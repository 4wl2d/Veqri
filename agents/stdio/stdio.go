// Package stdio runs local agents using a bounded JSON-lines protocol.
// Commands are always an executable plus an argument slice; shell command
// strings and shell interpreters are intentionally unsupported.
package stdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	coreagents "github.com/veqri/veqri/core/agents"
	"github.com/veqri/veqri/core/tasks"
)

const (
	defaultMaxOutputBytes = 4 << 20
	defaultMaxFrameBytes  = 256 << 10
)

var (
	ErrShellUnsupported = errors.New("stdio agents do not support shell interpreters or shell command strings")
	ErrOutputLimit      = errors.New("stdio agent output exceeds the configured limit")
	ErrProtocol         = errors.New("stdio agent protocol error")
)

type Config struct {
	Command            string
	Args               []string
	WorkingDirectory   string
	Environment        map[string]string
	InheritEnvironment bool
	Definition         coreagents.Definition
	MaxOutputBytes     int
	MaxFrameBytes      int
}

type Runner struct {
	command          string
	args             []string
	workingDirectory string
	environment      []string
	definition       coreagents.Definition
	maxOutput        int
	maxFrame         int
}

var _ coreagents.Runner = (*Runner)(nil)

type Request struct {
	Type    string     `json:"type"`
	Version int        `json:"version"`
	Task    tasks.Task `json:"task"`
}

type Frame struct {
	Type     string               `json:"type"`
	Progress *coreagents.Progress `json:"progress,omitempty"`
	Result   *coreagents.Result   `json:"result,omitempty"`
	Error    string               `json:"error,omitempty"`
}

func New(config Config) (*Runner, error) {
	command := strings.TrimSpace(config.Command)
	if command == "" || strings.ContainsRune(command, 0) {
		return nil, errors.New("stdio agent command is required and cannot contain NUL")
	}
	if deniedInterpreter(filepath.Base(command)) {
		return nil, ErrShellUnsupported
	}
	for _, argument := range config.Args {
		if strings.ContainsRune(argument, 0) {
			return nil, errors.New("stdio agent arguments cannot contain NUL")
		}
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("locate stdio agent executable: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, fmt.Errorf("resolve stdio agent executable: %w", err)
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return nil, fmt.Errorf("resolve stdio agent executable symlinks: %w", err)
	}
	if deniedInterpreter(filepath.Base(resolved)) {
		return nil, ErrShellUnsupported
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("stdio agent executable is not a regular file")
	}
	workingDirectory := config.WorkingDirectory
	if workingDirectory != "" {
		workingDirectory, err = filepath.Abs(workingDirectory)
		if err != nil {
			return nil, fmt.Errorf("resolve stdio agent working directory: %w", err)
		}
		workingDirectory, err = filepath.EvalSymlinks(workingDirectory)
		if err != nil {
			return nil, fmt.Errorf("resolve stdio agent working directory symlinks: %w", err)
		}
		if info, statErr := os.Stat(workingDirectory); statErr != nil || !info.IsDir() {
			return nil, errors.New("stdio agent working directory is not a directory")
		}
	}
	if config.MaxOutputBytes == 0 {
		config.MaxOutputBytes = defaultMaxOutputBytes
	}
	if config.MaxOutputBytes < 1 || config.MaxOutputBytes > 1<<30 {
		return nil, errors.New("stdio agent output limit must be between 1 byte and 1 GiB")
	}
	if config.MaxFrameBytes == 0 {
		config.MaxFrameBytes = defaultMaxFrameBytes
		if config.MaxFrameBytes > config.MaxOutputBytes {
			config.MaxFrameBytes = config.MaxOutputBytes
		}
	}
	if config.MaxFrameBytes < 1 || config.MaxFrameBytes > config.MaxOutputBytes {
		return nil, errors.New("stdio agent frame limit must be positive and no larger than the output limit")
	}
	environment, err := buildEnvironment(config.InheritEnvironment, config.Environment)
	if err != nil {
		return nil, err
	}
	definition := config.Definition
	if definition.ID == "" {
		definition.ID = "stdio-agent"
	}
	if definition.DisplayName == "" {
		definition.DisplayName = definition.ID
	}
	definition.ExecutionMode = coreagents.ModeStdio
	definition.SupportsCancellation = true
	definition.SupportsStreaming = true
	if definition.Health == "" {
		definition.Health = coreagents.HealthUnknown
	}
	definition.UpdatedAt = time.Now().UTC()
	return &Runner{
		command: resolved, args: append([]string(nil), config.Args...),
		workingDirectory: workingDirectory, environment: environment,
		definition: definition, maxOutput: config.MaxOutputBytes, maxFrame: config.MaxFrameBytes,
	}, nil
}

func (r *Runner) Definition() coreagents.Definition { return r.definition }

func (r *Runner) Retryable(runErr error) bool {
	return runErr != nil && !errors.Is(runErr, context.Canceled) &&
		!errors.Is(runErr, context.DeadlineExceeded) && !errors.Is(runErr, ErrProtocol) &&
		!errors.Is(runErr, ErrOutputLimit) && !errors.Is(runErr, ErrShellUnsupported)
}

// CancellationScope is "process_tree" on Unix and
// "direct_process_only" on Windows, where job-object integration is not yet
// available in this adapter.
func (r *Runner) CancellationScope() string { return processCancellationScope }

func (r *Runner) Run(ctx context.Context, task tasks.Task, progress func(coreagents.Progress)) (coreagents.Result, error) {
	if err := ctx.Err(); err != nil {
		return coreagents.Result{}, err
	}
	processContext, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(processContext, r.command, r.args...)
	configureProcess(command)
	command.Dir = r.workingDirectory
	command.Env = append([]string(nil), r.environment...)
	stdin, err := command.StdinPipe()
	if err != nil {
		return coreagents.Result{}, fmt.Errorf("create stdio agent stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return coreagents.Result{}, fmt.Errorf("create stdio agent stdout: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return coreagents.Result{}, fmt.Errorf("create stdio agent stderr: %w", err)
	}
	if err := command.Start(); err != nil {
		return coreagents.Result{}, fmt.Errorf("start stdio agent: %w", err)
	}

	stderrBuffer := &boundedBuffer{limit: r.maxOutput}
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(stderrBuffer, stderr)
		close(stderrDone)
	}()

	encodeErr := json.NewEncoder(stdin).Encode(Request{Type: "run", Version: 1, Task: task})
	closeErr := stdin.Close()
	if encodeErr != nil || closeErr != nil {
		cancel()
		_ = command.Wait()
		<-stderrDone
		if encodeErr != nil {
			return coreagents.Result{}, fmt.Errorf("write stdio agent request: %w", encodeErr)
		}
		return coreagents.Result{}, fmt.Errorf("close stdio agent request: %w", closeErr)
	}

	result, protocolErr := r.decodeFrames(stdout, progress)
	if protocolErr != nil {
		cancel()
	}
	waitErr := command.Wait()
	<-stderrDone
	if ctx.Err() != nil {
		return coreagents.Result{}, ctx.Err()
	}
	if stderrBuffer.Truncated() {
		return coreagents.Result{}, ErrOutputLimit
	}
	if protocolErr != nil {
		return coreagents.Result{}, fmt.Errorf("%w: %w", ErrProtocol, protocolErr)
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			// Stderr is untrusted adapter output and may contain secrets. Keep it
			// bounded in memory for drain/limit enforcement, but never place it in
			// the persisted or logged task error.
			return coreagents.Result{}, fmt.Errorf("stdio agent exited with code %d", exitErr.ExitCode())
		}
		return coreagents.Result{}, fmt.Errorf("wait for stdio agent: %w", waitErr)
	}
	if result == nil {
		return coreagents.Result{}, fmt.Errorf("%w: process exited without a result frame", ErrProtocol)
	}
	if err := validateResult(*result); err != nil {
		return coreagents.Result{}, fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	return *result, nil
}

func (r *Runner) decodeFrames(reader io.Reader, progress func(coreagents.Progress)) (*coreagents.Result, error) {
	budget := &outputReader{reader: reader, remaining: int64(r.maxOutput)}
	scanner := bufio.NewScanner(budget)
	scanner.Buffer(make([]byte, 4096), r.maxFrame)
	var result *coreagents.Result
	for scanner.Scan() {
		data := bytes.TrimSpace(scanner.Bytes())
		if len(data) == 0 {
			continue
		}
		var frame Frame
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&frame); err != nil {
			return nil, fmt.Errorf("decode frame: %w", err)
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, errors.New("frame must contain exactly one JSON object")
		}
		switch frame.Type {
		case "progress":
			if frame.Progress == nil || frame.Result != nil || frame.Error != "" || frame.Progress.Percent < 0 || frame.Progress.Percent > 100 {
				return nil, errors.New("invalid progress frame")
			}
			if progress != nil {
				progress(*frame.Progress)
			}
		case "result":
			if frame.Result == nil || frame.Progress != nil || frame.Error != "" || result != nil {
				return nil, errors.New("invalid or duplicate result frame")
			}
			copy := *frame.Result
			result = &copy
		case "error":
			if strings.TrimSpace(frame.Error) == "" || frame.Progress != nil || frame.Result != nil {
				return nil, errors.New("invalid error frame")
			}
			return nil, errors.New(frame.Error)
		default:
			return nil, fmt.Errorf("unsupported frame type %q", frame.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, ErrOutputLimit) || strings.Contains(err.Error(), "token too long") {
			return nil, ErrOutputLimit
		}
		return nil, err
	}
	return result, nil
}

func validateResult(result coreagents.Result) error {
	if len(result.Structured) > 0 && !json.Valid(result.Structured) {
		return errors.New("result.structured is not valid JSON")
	}
	if len(result.Structured) == 0 && strings.TrimSpace(result.WrittenSummary) == "" && strings.TrimSpace(result.SpokenSummary) == "" {
		return errors.New("result is empty")
	}
	return nil
}

func deniedInterpreter(base string) bool {
	base = strings.ToLower(strings.TrimSpace(base))
	return map[string]bool{
		"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true,
		"cmd": true, "cmd.exe": true, "powershell": true, "powershell.exe": true,
		"pwsh": true, "osascript": true,
	}[base]
}

func buildEnvironment(inherit bool, overrides map[string]string) ([]string, error) {
	values := make(map[string]string)
	if inherit {
		for _, entry := range os.Environ() {
			name, value, ok := strings.Cut(entry, "=")
			if ok {
				values[name] = value
			}
		}
	}
	for name, value := range overrides {
		if name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("invalid stdio agent environment variable %q", name)
		}
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
	return result, nil
}

type outputReader struct {
	reader    io.Reader
	remaining int64
}

func (r *outputReader) Read(buffer []byte) (int, error) {
	if r.remaining < 0 {
		return 0, ErrOutputLimit
	}
	maximum := int64(len(buffer))
	if maximum > r.remaining+1 {
		maximum = r.remaining + 1
	}
	n, err := r.reader.Read(buffer[:maximum])
	if int64(n) > r.remaining {
		r.remaining = -1
		return 0, ErrOutputLimit
	}
	r.remaining -= int64(n)
	return n, err
}

type boundedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
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

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func (b *boundedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
