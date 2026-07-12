package shell

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	coretools "github.com/veqri/veqri/core/tools"
)

const (
	maxOutputBytes      = 1 << 20
	maxCommandBytes     = 1 << 10
	maxWorkingDirBytes  = 2 << 10
	maxArgumentCount    = 128
	maxArgumentBytes    = 4 << 10
	maxArgumentsBytes   = 8 << 10
	maxEnvironmentCount = 64
	maxEnvironmentBytes = 2 << 10
	maxEnvironmentValue = 1 << 10
	maxExecutableBytes  = 256 << 20
)

type Input struct {
	Command          string            `json:"command"`
	Args             []string          `json:"args"`
	WorkingDir       string            `json:"working_directory,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
	TimeoutSeconds   int               `json:"timeout_seconds,omitempty"`
	DryRun           bool              `json:"dry_run,omitempty"`
	ExecutableSHA256 string            `json:"executable_sha256,omitempty"`
}

type Output struct {
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	WorkingDir string   `json:"working_directory"`
	Stdout     string   `json:"stdout"`
	Stderr     string   `json:"stderr"`
	ExitCode   int      `json:"exit_code"`
	TimedOut   bool     `json:"timed_out"`
	Truncated  bool     `json:"truncated"`
	DryRun     bool     `json:"dry_run"`
}

type Executor struct {
	workspaces     []string
	allowedEnv     map[string]bool
	redactedValues []string
	// beforeLaunch is a package-private test seam. Production callers cannot
	// replace it; tests use it to prove that changing the approved source path
	// after staging cannot change the bytes that are executed.
	beforeLaunch func(originalCommand, stagedCommand string)
}

func New(workspaces []string, additionalAllowedEnv []string, redactedValues []string) (*Executor, error) {
	if len(workspaces) == 0 {
		return nil, errors.New("at least one shell workspace is required")
	}
	resolved := make([]string, 0, len(workspaces))
	for _, workspace := range workspaces {
		absolute, err := filepath.Abs(workspace)
		if err != nil {
			return nil, fmt.Errorf("resolve workspace: %w", err)
		}
		resolvedPath, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, fmt.Errorf("resolve workspace symlinks: %w", err)
		}
		resolved = append(resolved, filepath.Clean(resolvedPath))
	}
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true,
		"LC_ALL": true, "CI": true, "TERM": true, "NO_COLOR": true,
	}
	for _, name := range additionalAllowedEnv {
		allowed[name] = true
	}
	return &Executor{workspaces: resolved, allowedEnv: allowed, redactedValues: redactedValues}, nil
}

func (e *Executor) Definition() coretools.Definition {
	return coretools.Definition{
		Name:                 "shell",
		Description:          "Executes one binary with a structured argument array inside an allowed workspace",
		InputSchema:          json.RawMessage(`{"type":"object","required":["command","args"],"properties":{"command":{"type":"string"},"args":{"type":"array","items":{"type":"string"}},"working_directory":{"type":"string"},"environment":{"type":"object","additionalProperties":{"type":"string"}},"timeout_seconds":{"type":"integer","minimum":1,"maximum":3600},"dry_run":{"type":"boolean"},"executable_sha256":{"type":"string","pattern":"^[a-fA-F0-9]{64}$"}}}`),
		OutputSchema:         json.RawMessage(`{"type":"object","required":["exit_code","stdout","stderr"]}`),
		RequiredScopes:       []string{"tool.shell.execute"},
		Risk:                 coretools.RiskStateChanging,
		ApprovalRequired:     true,
		DefaultTimeout:       2 * time.Minute,
		SupportsCancellation: true,
		SupportsStreaming:    true,
		SupportedOS:          []string{"darwin", "linux", "windows"},
	}
}

func (e *Executor) Workspaces() []string { return append([]string(nil), e.workspaces...) }

func (e *Executor) ParseAndValidate(raw json.RawMessage) (Input, coretools.Risk, error) {
	var input Input
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return Input{}, "", fmt.Errorf("decode shell input: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Input{}, "", errors.New("shell input must contain exactly one JSON object")
		}
		return Input{}, "", fmt.Errorf("decode trailing shell input: %w", err)
	}
	if input.Command == "" || strings.ContainsRune(input.Command, 0) {
		return Input{}, "", errors.New("command is required and cannot contain NUL")
	}
	if len(input.Command) > maxCommandBytes {
		return Input{}, "", fmt.Errorf("command exceeds %d bytes", maxCommandBytes)
	}
	if input.Args == nil {
		return Input{}, "", errors.New("args is required and must be an array")
	}
	if len(input.Args) > maxArgumentCount {
		return Input{}, "", fmt.Errorf("args exceeds %d entries", maxArgumentCount)
	}
	if filepath.Base(input.Command) != input.Command && !filepath.IsAbs(input.Command) {
		return Input{}, "", errors.New("command must be a binary name or absolute path")
	}
	argumentBytes := 0
	for _, argument := range input.Args {
		if strings.ContainsRune(argument, 0) {
			return Input{}, "", errors.New("arguments cannot contain NUL")
		}
		if len(argument) > maxArgumentBytes {
			return Input{}, "", fmt.Errorf("an argument exceeds %d bytes", maxArgumentBytes)
		}
		argumentBytes += len(argument)
		if argumentBytes > maxArgumentsBytes {
			return Input{}, "", fmt.Errorf("arguments exceed %d total bytes", maxArgumentsBytes)
		}
	}
	if input.ExecutableSHA256 != "" {
		if len(input.ExecutableSHA256) != sha256.Size*2 {
			return Input{}, "", errors.New("executable_sha256 must contain 64 hexadecimal characters")
		}
		for _, character := range input.ExecutableSHA256 {
			if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
				return Input{}, "", errors.New("executable_sha256 must contain 64 hexadecimal characters")
			}
		}
	}
	resolvedCommand, executableDigest, err := resolveExecutable(input.Command, input.ExecutableSHA256)
	if err != nil {
		return Input{}, "", err
	}
	input.Command = resolvedCommand
	input.ExecutableSHA256 = executableDigest
	base := strings.ToLower(filepath.Base(input.Command))
	if deniedInterpreter(base) {
		return Input{}, "", fmt.Errorf("shell interpreter %q is denied; structured invocation is required", base)
	}
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = 120
	}
	if input.TimeoutSeconds < 1 || input.TimeoutSeconds > 3600 {
		return Input{}, "", errors.New("timeout_seconds must be between 1 and 3600")
	}
	workingDir, err := e.validateWorkingDirectory(input.WorkingDir)
	if err != nil {
		return Input{}, "", err
	}
	input.WorkingDir = workingDir
	if len(input.WorkingDir) > maxWorkingDirBytes {
		return Input{}, "", fmt.Errorf("working_directory exceeds %d bytes", maxWorkingDirBytes)
	}
	if len(input.Environment) > maxEnvironmentCount {
		return Input{}, "", fmt.Errorf("environment exceeds %d entries", maxEnvironmentCount)
	}
	environmentBytes := 0
	for name, value := range input.Environment {
		upper := strings.ToUpper(name)
		if strings.EqualFold(name, "PATH") {
			return Input{}, "", errors.New("PATH cannot be overridden by a tool request")
		}
		if !e.allowedEnv[name] || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, 0) {
			return Input{}, "", fmt.Errorf("environment variable %q is not allowed", name)
		}
		if len(value) > maxEnvironmentValue {
			return Input{}, "", fmt.Errorf("environment variable %q exceeds %d bytes", name, maxEnvironmentValue)
		}
		environmentBytes += len(name) + len(value)
		if environmentBytes > maxEnvironmentBytes {
			return Input{}, "", fmt.Errorf("environment exceeds %d total bytes", maxEnvironmentBytes)
		}
		if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "PRIVATE_KEY") {
			return Input{}, "", fmt.Errorf("secret-like environment variable %q requires a secret reference", name)
		}
	}
	risk := Classify(input)
	if risk == coretools.RiskReadOnly && !trustedExecutablePath(input.Command) {
		risk = coretools.RiskStateChanging
	}
	return input, risk, nil
}

func (e *Executor) Execute(ctx context.Context, raw json.RawMessage, progress func(coretools.Progress)) (json.RawMessage, error) {
	input, risk, err := e.ParseAndValidate(raw)
	if err != nil {
		return nil, err
	}
	if risk == coretools.RiskPrivileged {
		return nil, errors.New("privilege escalation is denied at execution time")
	}
	if input.DryRun {
		return json.Marshal(Output{Command: e.redact(input.Command), Args: e.redactArguments(input.Args), WorkingDir: input.WorkingDir, ExitCode: 0, DryRun: true})
	}
	executionCommand := input.Command
	cleanupExecutionCommand := func() {}
	// Apple platform binaries are killed by AMFI when byte-copied outside the
	// sealed system location. Root-protected system directories are not
	// replaceable by Veqri or its non-privileged child processes, so canonical
	// path plus digest revalidation is sufficient there. Every executable from
	// a mutable/untrusted location is launched only from a private staged copy.
	if !trustedExecutablePath(input.Command) {
		executionCommand, cleanupExecutionCommand, err = stageApprovedExecutable(input.Command, input.ExecutableSHA256)
		if err != nil {
			return nil, fmt.Errorf("stage approved shell executable: %w", err)
		}
	}
	defer cleanupExecutionCommand()
	if e.beforeLaunch != nil {
		e.beforeLaunch(input.Command, executionCommand)
	}
	commandContext, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
	defer cancel()
	command := exec.CommandContext(commandContext, executionCommand, input.Args...)
	// Cmd.Path selects the immutable staged bytes. Keep argv[0] equal to the
	// canonical command the user approved so native multicall binaries and
	// audit-visible invocation semantics do not change.
	command.Args[0] = input.Command
	configureProcessCancellation(command)
	command.Dir = input.WorkingDir
	command.Env = e.filteredEnvironment(input.Environment)
	stdoutBuffer := newLimitedBuffer(maxOutputBytes)
	stderrBuffer := newLimitedBuffer(maxOutputBytes)
	command.Stdout = io.MultiWriter(stdoutBuffer, progressWriter{stream: "stdout", emit: progress, redact: e.redact})
	command.Stderr = io.MultiWriter(stderrBuffer, progressWriter{stream: "stderr", emit: progress, redact: e.redact})
	err = command.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if commandContext.Err() != nil {
			exitCode = -1
		} else {
			return nil, fmt.Errorf("execute binary: %w", err)
		}
	}
	result := Output{
		Command: e.redact(input.Command), Args: e.redactArguments(input.Args), WorkingDir: input.WorkingDir,
		Stdout: e.redact(stdoutBuffer.String()), Stderr: e.redact(stderrBuffer.String()),
		ExitCode: exitCode, TimedOut: errors.Is(commandContext.Err(), context.DeadlineExceeded),
		Truncated: stdoutBuffer.Truncated() || stderrBuffer.Truncated(),
	}
	encoded, encodeErr := json.Marshal(result)
	if encodeErr != nil {
		return nil, encodeErr
	}
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return encoded, fmt.Errorf("shell command timed out: %w", context.DeadlineExceeded)
	}
	if err != nil {
		return encoded, fmt.Errorf("command exited with code %d", exitCode)
	}
	return encoded, nil
}

// stageApprovedExecutable copies and hashes from one open source descriptor.
// Once that descriptor is open, replacing the source pathname cannot change
// the bytes copied into the private execution directory. The digest-named
// directory is made non-writable before launch and removed after the process
// exits.
func stageApprovedExecutable(command, approvedDigest string) (string, func(), error) {
	if len(approvedDigest) != sha256.Size*2 {
		return "", nil, errors.New("approved executable digest is required")
	}
	source, err := os.Open(command)
	if err != nil {
		return "", nil, fmt.Errorf("open approved executable: %w", err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, errors.New("approved executable must remain a regular file")
	}
	if info.Size() < 1 || info.Size() > maxExecutableBytes {
		return "", nil, fmt.Errorf("approved executable size must be between 1 and %d bytes", maxExecutableBytes)
	}

	digestKey := strings.ToLower(approvedDigest)
	stageDir, err := os.MkdirTemp("", "veqri-shell-"+digestKey[:16]+"-")
	if err != nil {
		return "", nil, fmt.Errorf("create private executable directory: %w", err)
	}
	cleanup := func() {
		// Restore owner-write permission solely so cleanup also succeeds on
		// platforms that honor read-only directory/file attributes.
		_ = os.Chmod(stageDir, 0o700)
		_ = os.Chmod(filepath.Join(stageDir, filepath.Base(command)), 0o600)
		_ = os.RemoveAll(stageDir)
	}
	fail := func(stageErr error) (string, func(), error) {
		cleanup()
		return "", nil, stageErr
	}
	if err := os.Chmod(stageDir, 0o700); err != nil {
		return fail(fmt.Errorf("protect private executable directory: %w", err))
	}
	stagedCommand := filepath.Join(stageDir, filepath.Base(command))
	destination, err := os.OpenFile(stagedCommand, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(fmt.Errorf("create private executable copy: %w", err))
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(destination, hasher), source)
	if copyErr == nil {
		copyErr = destination.Sync()
	}
	closeErr := destination.Close()
	if copyErr != nil {
		return fail(fmt.Errorf("copy approved executable: %w", copyErr))
	}
	if closeErr != nil {
		return fail(fmt.Errorf("close approved executable copy: %w", closeErr))
	}
	if written != info.Size() {
		return fail(errors.New("approved executable changed while it was staged"))
	}
	actualDigest := fmt.Sprintf("%x", hasher.Sum(nil))
	if !strings.EqualFold(approvedDigest, actualDigest) {
		return fail(errors.New("shell executable changed after approval"))
	}
	if err := os.Chmod(stagedCommand, 0o500); err != nil {
		return fail(fmt.Errorf("protect approved executable copy: %w", err))
	}
	if err := verifyExecutableDigest(stagedCommand, approvedDigest); err != nil {
		return fail(err)
	}
	if err := os.Chmod(stageDir, 0o500); err != nil {
		return fail(fmt.Errorf("seal private executable directory: %w", err))
	}
	return stagedCommand, cleanup, nil
}

func verifyExecutableDigest(path, approvedDigest string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("verify approved executable copy: %w", err)
	}
	defer file.Close()
	hasher := sha256.New()
	written, err := io.Copy(hasher, io.LimitReader(file, maxExecutableBytes+1))
	if err != nil {
		return fmt.Errorf("verify approved executable copy: %w", err)
	}
	if written > maxExecutableBytes || !strings.EqualFold(approvedDigest, fmt.Sprintf("%x", hasher.Sum(nil))) {
		return errors.New("private executable copy failed digest verification")
	}
	return nil
}

func Classify(input Input) coretools.Risk {
	base := strings.ToLower(filepath.Base(input.Command))
	if privilegedCommands[base] {
		return coretools.RiskPrivileged
	}
	if destructiveCommands[base] {
		return coretools.RiskDestructive
	}
	if readOnlyCommands[base] {
		if !strictReadOnlyShape(base, input.Args) {
			return coretools.RiskStateChanging
		}
		return coretools.RiskReadOnly
	}
	return coretools.RiskStateChanging
}

// strictReadOnlyShape is intentionally conservative. Any operand or option we
// have not proved incapable of reaching outside the selected workspace (or of
// writing output) is routed through explicit approval. The command may still
// be used; it simply never inherits the local read-only auto-allow rule.
func strictReadOnlyShape(base string, arguments []string) bool {
	switch base {
	case "pwd", "whoami":
		return len(arguments) == 0
	case "uname":
		for _, argument := range arguments {
			if !map[string]bool{"-a": true, "-s": true, "-n": true, "-r": true, "-v": true, "-m": true, "-p": true, "-i": true, "-o": true}[argument] {
				return false
			}
		}
		return true
	case "ls":
		// With no operands ls is confined by the validated working directory.
		return len(arguments) == 0
	default:
		// cat/head/tail/wc/rg/grep/stat accept filesystem operands. With no
		// arguments they cannot escape the validated working directory.
		return len(arguments) == 0
	}
}

func resolveExecutable(command, approvedDigest string) (string, string, error) {
	resolved, err := exec.LookPath(command)
	if err != nil {
		return "", "", fmt.Errorf("resolve shell executable: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", "", fmt.Errorf("resolve shell executable path: %w", err)
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", "", fmt.Errorf("resolve shell executable symlinks: %w", err)
	}
	if len(resolved) > maxCommandBytes {
		return "", "", fmt.Errorf("resolved command exceeds %d bytes", maxCommandBytes)
	}
	if deniedExecutableExtension(strings.ToLower(filepath.Ext(resolved))) {
		return "", "", errors.New("script and command-wrapper executables are denied; invoke a native binary directly")
	}
	file, err := os.Open(resolved)
	if err != nil {
		if Classify(Input{Command: resolved}) == coretools.RiskPrivileged {
			// Some protected system escalators (notably macOS sudo) are
			// execute-only. Their canonical basename is enough to deny them;
			// they can never reach approval or execution.
			return filepath.Clean(resolved), strings.Repeat("0", sha256.Size*2), nil
		}
		return "", "", fmt.Errorf("open shell executable: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", "", errors.New("shell executable must be a regular file")
	}
	if info.Size() < 1 || info.Size() > maxExecutableBytes {
		return "", "", fmt.Errorf("shell executable size must be between 1 and %d bytes", maxExecutableBytes)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", "", errors.New("shell executable is not executable")
	}
	first := make([]byte, 2)
	read, readErr := io.ReadFull(file, first)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return "", "", fmt.Errorf("inspect shell executable: %w", readErr)
	}
	if read == 2 && bytes.Equal(first, []byte("#!")) {
		return "", "", errors.New("script interpreters are denied; invoke a native binary directly")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", "", fmt.Errorf("inspect shell executable: %w", err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", "", fmt.Errorf("hash shell executable: %w", err)
	}
	digest := fmt.Sprintf("%x", hasher.Sum(nil))
	if approvedDigest != "" && !strings.EqualFold(approvedDigest, digest) {
		return "", "", errors.New("shell executable changed after approval")
	}
	return filepath.Clean(resolved), digest, nil
}

func trustedExecutablePath(resolved string) bool {
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	trustedDirectories := []string{"/bin", "/usr/bin", "/sbin", "/usr/sbin"}
	if runtime.GOOS == "windows" {
		if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
			trustedDirectories = []string{filepath.Join(systemRoot, "System32")}
		}
	}
	for _, directory := range trustedDirectories {
		relative, err := filepath.Rel(directory, resolved)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func deniedExecutableExtension(extension string) bool {
	return map[string]bool{
		".bat": true, ".cmd": true, ".ps1": true, ".vbs": true,
		".vbe": true, ".js": true, ".jse": true, ".wsf": true, ".wsh": true,
	}[extension]
}

var readOnlyCommands = map[string]bool{
	"pwd": true, "ls": true, "cat": true, "head": true, "tail": true,
	"wc": true, "rg": true, "grep": true, "stat": true, "uname": true,
	"whoami": true,
}

var destructiveCommands = map[string]bool{
	"rm": true, "rmdir": true, "dd": true, "mkfs": true, "diskutil": true,
	"shutdown": true, "reboot": true, "kill": true, "killall": true,
	"truncate": true, "format": true, "del": true, "erase": true,
}

var privilegedCommands = map[string]bool{
	"sudo": true, "sudo.exe": true, "su": true, "doas": true, "pkexec": true,
	// Windows runas can reuse cached credentials with /savecred and therefore
	// must never pass through the ordinary approval-only path.
	"runas": true, "runas.exe": true,
	// Generic launchers can otherwise hide a privileged executable from the
	// structured command classifier. Call the intended binary directly.
	"env": true, "xargs": true, "nice": true, "nohup": true, "setsid": true,
}

func deniedInterpreter(base string) bool {
	return map[string]bool{
		"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true,
		"cmd": true, "cmd.exe": true, "powershell": true, "powershell.exe": true,
		"pwsh": true, "osascript": true,
	}[base]
}

func (e *Executor) validateWorkingDirectory(value string) (string, error) {
	if value == "" {
		return e.workspaces[0], nil
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	resolved = filepath.Clean(resolved)
	for _, workspace := range e.workspaces {
		relative, err := filepath.Rel(workspace, resolved)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return resolved, nil
		}
	}
	return "", errors.New("working directory is outside allowed workspaces")
}

func (e *Executor) filteredEnvironment(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if found && e.allowedEnv[name] {
			upper := strings.ToUpper(name)
			if !strings.Contains(upper, "TOKEN") && !strings.Contains(upper, "SECRET") && !strings.Contains(upper, "PASSWORD") && !strings.Contains(upper, "PRIVATE_KEY") {
				values[name] = value
			}
		}
	}
	for name, value := range overrides {
		values[name] = value
	}
	keys := make([]string, 0, len(values))
	for name := range values {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, name := range keys {
		result = append(result, name+"="+values[name])
	}
	return result
}

func (e *Executor) redact(value string) string {
	for _, secret := range e.redactedValues {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func (e *Executor) redactArguments(arguments []string) []string {
	result := make([]string, len(arguments))
	for index, argument := range arguments {
		result[index] = e.redact(argument)
	}
	return result
}

type limitedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func newLimitedBuffer(limit int) *limitedBuffer { return &limitedBuffer{limit: limit} }

func (b *limitedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	originalLength := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = true
		return originalLength, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(value)
	return originalLength, nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func (b *limitedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

type progressWriter struct {
	stream string
	emit   func(coretools.Progress)
	redact func(string) string
}

func (w progressWriter) Write(value []byte) (int, error) {
	if w.emit != nil && len(value) > 0 {
		w.emit(coretools.Progress{Stream: w.stream, Data: w.redact(string(value))})
	}
	return len(value), nil
}
