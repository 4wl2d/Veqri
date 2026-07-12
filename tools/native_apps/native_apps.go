// Package native_apps exposes a small typed surface over official operating
// system launchers. It never accepts a command string and never falls back to
// shell, AppleScript, accessibility, or visual automation.
//
// Supported operations:
//   - macOS: application launch through /usr/bin/open and Shortcuts through the
//     official shortcuts CLI when installed.
//   - Linux: desktop-file launch through gtk-launch. D-Bus availability is
//     detected and reported, but arbitrary D-Bus calls are intentionally not
//     exposed by this typed adapter.
//   - Windows: packaged application launch through explorer.exe AppsFolder.
package native_apps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	coretools "github.com/veqri/veqri/core/tools"
)

const (
	defaultTimeout        = 30 * time.Second
	defaultMaxOutputBytes = 1 << 20
	maxArguments          = 128
	maxStringBytes        = 4096
)

var (
	ErrUnsupportedPlatform = errors.New("native application platform is unsupported")
	ErrUnsupportedFeature  = errors.New("native application feature is unsupported")
)

type Operation string

const (
	OperationLaunchApplication Operation = "launch_application"
	OperationRunShortcut       Operation = "run_shortcut"
)

type Features struct {
	Platform          string `json:"platform"`
	ApplicationLaunch bool   `json:"application_launch"`
	MacOSOpen         bool   `json:"macos_open"`
	MacOSShortcuts    bool   `json:"macos_shortcuts"`
	LinuxGTKLaunch    bool   `json:"linux_gtk_launch"`
	LinuxDBus         bool   `json:"linux_dbus"`
	WindowsAppsFolder bool   `json:"windows_apps_folder"`
	WindowsExplorer   bool   `json:"windows_explorer"`
}

type UnsupportedError struct {
	Platform  string
	Operation Operation
	Reason    string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("%s on %s: %s", e.Operation, e.Platform, e.Reason)
}

func (e *UnsupportedError) Unwrap() error { return ErrUnsupportedFeature }

type Input struct {
	Operation      Operation `json:"operation"`
	ApplicationID  string    `json:"application_id,omitempty"`
	Arguments      []string  `json:"arguments,omitempty"`
	ShortcutName   string    `json:"shortcut_name,omitempty"`
	InputPath      string    `json:"input_path,omitempty"`
	OutputPath     string    `json:"output_path,omitempty"`
	TimeoutSeconds int       `json:"timeout_seconds,omitempty"`
	DryRun         bool      `json:"dry_run,omitempty"`
}

type Output struct {
	Operation Operation `json:"operation"`
	Platform  string    `json:"platform"`
	Backend   string    `json:"backend"`
	Binary    string    `json:"binary"`
	Args      []string  `json:"args"`
	Stdout    string    `json:"stdout,omitempty"`
	Stderr    string    `json:"stderr,omitempty"`
	ExitCode  int       `json:"exit_code"`
	TimedOut  bool      `json:"timed_out"`
	Cancelled bool      `json:"cancelled"`
	Truncated bool      `json:"truncated"`
	DryRun    bool      `json:"dry_run"`
	Features  Features  `json:"features"`
}

type LookupFunc func(string) (string, error)

type CommandResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
}

type CommandRunner interface {
	Run(context.Context, string, []string, int) (CommandResult, error)
}

type Config struct {
	Platform       string
	Lookup         LookupFunc
	Runner         CommandRunner
	DefaultTimeout time.Duration
	MaxOutputBytes int
}

type Executor struct {
	platform       string
	features       Features
	binaries       map[string]string
	runner         CommandRunner
	defaultTimeout time.Duration
	maxOutput      int
}

var _ coretools.Executor = (*Executor)(nil)

func New() (*Executor, error) { return NewWithConfig(Config{}) }

func NewWithConfig(config Config) (*Executor, error) {
	platform := strings.ToLower(strings.TrimSpace(config.Platform))
	if platform == "" {
		platform = runtime.GOOS
	}
	if platform != "darwin" && platform != "linux" && platform != "windows" {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedPlatform, platform)
	}
	lookup := config.Lookup
	if lookup == nil {
		lookup = defaultPlatformLookup(platform)
	}
	runner := config.Runner
	if runner == nil {
		runner = execRunner{}
	}
	if config.DefaultTimeout == 0 {
		config.DefaultTimeout = defaultTimeout
	}
	if config.DefaultTimeout < time.Millisecond || config.DefaultTimeout > time.Hour {
		return nil, errors.New("native application default timeout must be between 1 millisecond and 1 hour")
	}
	if config.MaxOutputBytes == 0 {
		config.MaxOutputBytes = defaultMaxOutputBytes
	}
	if config.MaxOutputBytes < 1 || config.MaxOutputBytes > 1<<30 {
		return nil, errors.New("native application output limit must be between 1 byte and 1 GiB")
	}
	binaries := make(map[string]string)
	find := func(name string) bool {
		path, err := lookup(name)
		if err != nil || strings.TrimSpace(path) == "" || strings.ContainsRune(path, 0) {
			return false
		}
		binaries[name] = path
		return true
	}
	features := Features{Platform: platform}
	switch platform {
	case "darwin":
		features.MacOSOpen = find("open")
		features.MacOSShortcuts = find("shortcuts")
		features.ApplicationLaunch = features.MacOSOpen
	case "linux":
		features.LinuxGTKLaunch = find("gtk-launch")
		features.LinuxDBus = find("gdbus")
		if !features.LinuxDBus {
			features.LinuxDBus = find("busctl")
		}
		features.ApplicationLaunch = features.LinuxGTKLaunch
	case "windows":
		features.WindowsExplorer = find("explorer.exe")
		features.WindowsAppsFolder = features.WindowsExplorer
		features.ApplicationLaunch = features.WindowsAppsFolder
	}
	return &Executor{
		platform: platform, features: features, binaries: binaries, runner: runner,
		defaultTimeout: config.DefaultTimeout, maxOutput: config.MaxOutputBytes,
	}, nil
}

func (e *Executor) Features() Features { return e.features }

func (e *Executor) Definition() coretools.Definition {
	return coretools.Definition{
		Name: "native_apps", Description: "Launches native applications and macOS Shortcuts through typed official OS CLIs",
		InputSchema:    json.RawMessage(`{"type":"object","additionalProperties":false,"required":["operation"],"properties":{"operation":{"enum":["launch_application","run_shortcut"]},"application_id":{"type":"string","maxLength":4096},"arguments":{"type":"array","maxItems":128,"items":{"type":"string","maxLength":4096}},"shortcut_name":{"type":"string","maxLength":4096},"input_path":{"type":"string","maxLength":4096},"output_path":{"type":"string","maxLength":4096},"timeout_seconds":{"type":"integer","minimum":1,"maximum":3600},"dry_run":{"type":"boolean"}}}`),
		OutputSchema:   json.RawMessage(`{"type":"object","required":["operation","platform","backend","binary","args","exit_code","features"]}`),
		RequiredScopes: []string{"tool.native_apps.launch"}, Risk: coretools.RiskStateChanging,
		ApprovalRequired: true, DefaultTimeout: e.defaultTimeout,
		SupportsCancellation: true, SupportsStreaming: false,
		SupportedOS: []string{"darwin", "linux", "windows"},
	}
}

func (e *Executor) ParseAndValidate(raw json.RawMessage) (Input, error) {
	var input Input
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return Input{}, fmt.Errorf("decode native application input: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Input{}, errors.New("native application input must contain exactly one JSON object")
	}
	if input.TimeoutSeconds == 0 {
		input.TimeoutSeconds = int(e.defaultTimeout.Seconds())
		if input.TimeoutSeconds < 1 {
			input.TimeoutSeconds = 1
		}
	}
	if input.TimeoutSeconds < 1 || input.TimeoutSeconds > 3600 {
		return Input{}, errors.New("timeout_seconds must be between 1 and 3600")
	}
	if len(input.Arguments) > maxArguments {
		return Input{}, fmt.Errorf("application arguments cannot exceed %d items", maxArguments)
	}
	for _, value := range append([]string{input.ApplicationID, input.ShortcutName, input.InputPath, input.OutputPath}, input.Arguments...) {
		if len(value) > maxStringBytes || strings.ContainsRune(value, 0) {
			return Input{}, errors.New("native application strings cannot exceed 4096 bytes or contain NUL")
		}
	}
	switch input.Operation {
	case OperationLaunchApplication:
		if strings.TrimSpace(input.ApplicationID) == "" {
			return Input{}, errors.New("application_id is required for launch_application")
		}
		if input.ShortcutName != "" || input.InputPath != "" || input.OutputPath != "" {
			return Input{}, errors.New("shortcut fields are valid only for run_shortcut")
		}
		if e.platform != "darwin" && len(input.Arguments) > 0 {
			return Input{}, &UnsupportedError{Platform: e.platform, Operation: input.Operation, Reason: "application arguments are supported only by the macOS open adapter"}
		}
		if e.platform == "windows" && !validWindowsAppID(input.ApplicationID) {
			return Input{}, errors.New("Windows application_id must be an AppsFolder application user model ID")
		}
		if e.platform == "linux" && !validLinuxDesktopID(input.ApplicationID) {
			return Input{}, errors.New("Linux application_id must be a desktop-file ID")
		}
	case OperationRunShortcut:
		if strings.TrimSpace(input.ShortcutName) == "" {
			return Input{}, errors.New("shortcut_name is required for run_shortcut")
		}
		if input.ApplicationID != "" || len(input.Arguments) > 0 {
			return Input{}, errors.New("application fields are valid only for launch_application")
		}
		if e.platform != "darwin" {
			return Input{}, &UnsupportedError{Platform: e.platform, Operation: input.Operation, Reason: "Shortcuts is a macOS-only official CLI feature"}
		}
		if strings.HasPrefix(strings.TrimSpace(input.ShortcutName), "-") {
			return Input{}, errors.New("shortcut_name cannot begin with a hyphen")
		}
		var err error
		if input.InputPath != "" {
			input.InputPath, err = canonicalInputFile(input.InputPath)
			if err != nil {
				return Input{}, err
			}
		}
		if input.OutputPath != "" {
			input.OutputPath, err = canonicalOutputFile(input.OutputPath)
			if err != nil {
				return Input{}, err
			}
		}
	default:
		return Input{}, fmt.Errorf("unsupported native application operation %q", input.Operation)
	}
	return input, nil
}

func (e *Executor) Execute(ctx context.Context, raw json.RawMessage, _ func(coretools.Progress)) (json.RawMessage, error) {
	input, err := e.ParseAndValidate(raw)
	if err != nil {
		return nil, err
	}
	binary, args, backend, err := e.command(input)
	if err != nil {
		return nil, err
	}
	output := Output{
		Operation: input.Operation, Platform: e.platform, Backend: backend,
		Binary: binary, Args: append([]string(nil), args...), DryRun: input.DryRun, Features: e.features,
	}
	if input.DryRun {
		return json.Marshal(output)
	}
	runContext, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
	defer cancel()
	result, runErr := e.runner.Run(runContext, binary, args, e.maxOutput)
	output.Stdout = result.Stdout
	output.Stderr = result.Stderr
	output.ExitCode = result.ExitCode
	output.Truncated = result.Truncated
	output.TimedOut = errors.Is(runContext.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	output.Cancelled = ctx.Err() != nil
	encoded, encodeErr := json.Marshal(output)
	if encodeErr != nil {
		return nil, encodeErr
	}
	if ctx.Err() != nil {
		return encoded, ctx.Err()
	}
	if output.TimedOut {
		return encoded, context.DeadlineExceeded
	}
	if runErr != nil {
		return encoded, fmt.Errorf("native application %s failed: %w", backend, runErr)
	}
	return encoded, nil
}

func (e *Executor) command(input Input) (binary string, args []string, backend string, err error) {
	switch e.platform {
	case "darwin":
		if input.Operation == OperationLaunchApplication {
			if !e.features.MacOSOpen {
				return "", nil, "", &UnsupportedError{Platform: e.platform, Operation: input.Operation, Reason: "the official open CLI was not found"}
			}
			args = []string{"-a", input.ApplicationID}
			if len(input.Arguments) > 0 {
				args = append(args, "--args")
				args = append(args, input.Arguments...)
			}
			return e.binaries["open"], args, "macos.open", nil
		}
		if !e.features.MacOSShortcuts {
			return "", nil, "", &UnsupportedError{Platform: e.platform, Operation: input.Operation, Reason: "the official shortcuts CLI was not found"}
		}
		args = []string{"run", input.ShortcutName}
		if input.InputPath != "" {
			args = append(args, "--input-path", input.InputPath)
		}
		if input.OutputPath != "" {
			args = append(args, "--output-path", input.OutputPath)
		}
		return e.binaries["shortcuts"], args, "macos.shortcuts", nil
	case "linux":
		if input.Operation != OperationLaunchApplication || !e.features.LinuxGTKLaunch {
			return "", nil, "", &UnsupportedError{Platform: e.platform, Operation: input.Operation, Reason: "gtk-launch was not found; arbitrary D-Bus invocation is not exposed"}
		}
		return e.binaries["gtk-launch"], []string{"--", input.ApplicationID}, "linux.gtk-launch", nil
	case "windows":
		if input.Operation != OperationLaunchApplication || !e.features.WindowsAppsFolder {
			return "", nil, "", &UnsupportedError{Platform: e.platform, Operation: input.Operation, Reason: "explorer.exe AppsFolder activation was not found"}
		}
		return e.binaries["explorer.exe"], []string{"shell:AppsFolder\\" + input.ApplicationID}, "windows.appsfolder", nil
	default:
		return "", nil, "", fmt.Errorf("%w: %s", ErrUnsupportedPlatform, e.platform)
	}
}

func canonicalInputFile(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve shortcut input path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve shortcut input path symlinks: %w", err)
	}
	if info, statErr := os.Stat(resolved); statErr != nil || !info.Mode().IsRegular() {
		return "", errors.New("shortcut input path must be a regular file")
	}
	return resolved, nil
}

func canonicalOutputFile(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve shortcut output path: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("resolve shortcut output directory symlinks: %w", err)
	}
	if info, statErr := os.Stat(parent); statErr != nil || !info.IsDir() {
		return "", errors.New("shortcut output directory must exist")
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func validWindowsAppID(value string) bool {
	if len(value) < 1 || len(value) > maxStringBytes || strings.ContainsAny(value, "\\/:\r\n\x00") {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._-!", character) {
			continue
		}
		return false
	}
	return true
}

func validLinuxDesktopID(value string) bool {
	if len(value) < 1 || len(value) > maxStringBytes || strings.HasPrefix(value, "-") {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._-", character) {
			continue
		}
		return false
	}
	return true
}

func defaultPlatformLookup(platform string) LookupFunc {
	return func(name string) (string, error) {
		if platform != runtime.GOOS {
			return "", exec.ErrNotFound
		}
		var fixed string
		switch platform {
		case "darwin":
			switch name {
			case "open":
				fixed = "/usr/bin/open"
			case "shortcuts":
				fixed = "/usr/bin/shortcuts"
			}
		case "windows":
			if name == "explorer.exe" && os.Getenv("SystemRoot") != "" {
				fixed = filepath.Join(os.Getenv("SystemRoot"), "explorer.exe")
			}
		}
		if fixed != "" {
			info, err := os.Stat(fixed)
			if err != nil || !info.Mode().IsRegular() {
				return "", exec.ErrNotFound
			}
			return fixed, nil
		}
		if platform == "linux" {
			return exec.LookPath(name)
		}
		return "", exec.ErrNotFound
	}
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, binary string, args []string, maxOutput int) (CommandResult, error) {
	command := exec.CommandContext(ctx, binary, args...)
	stdout := &limitedBuffer{limit: maxOutput}
	stderr := &limitedBuffer{limit: maxOutput}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if ctx.Err() != nil {
			exitCode = -1
		}
	}
	return CommandResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode, Truncated: stdout.Truncated() || stderr.Truncated()}, err
}

type limitedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining < len(data) {
		b.truncated = true
		if remaining < 0 {
			remaining = 0
		}
		data = data[:remaining]
	}
	_, _ = b.buffer.Write(data)
	return original, nil
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
