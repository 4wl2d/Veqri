package local_events

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
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultProcessOutputBytes = 1 << 20

var ErrProcessShellUnsupported = errors.New("process-completion adapters require a binary and argument array; shell interpreters are unsupported")

type ProcessConfig struct {
	Command            string
	Args               []string
	WorkingDirectory   string
	Environment        map[string]string
	InheritEnvironment bool
	EventType          string
	ConversationKey    string
	IdempotencyKey     string
	CreateTask         bool
	Timeout            time.Duration
	MaxOutputBytes     int
}

type ProcessCompletion struct {
	Command           string    `json:"command"`
	Args              []string  `json:"args"`
	ExitCode          int       `json:"exit_code"`
	Stdout            string    `json:"stdout,omitempty"`
	Stderr            string    `json:"stderr,omitempty"`
	StartedAt         time.Time `json:"started_at"`
	FinishedAt        time.Time `json:"finished_at"`
	TimedOut          bool      `json:"timed_out"`
	Cancelled         bool      `json:"cancelled"`
	Truncated         bool      `json:"truncated"`
	CancellationScope string    `json:"cancellation_scope"`
}

type ProcessAdapter struct {
	command          string
	args             []string
	workingDirectory string
	environment      []string
	eventType        string
	conversationKey  string
	idempotencyKey   string
	createTask       bool
	timeout          time.Duration
	maxOutput        int
}

func NewProcessAdapter(config ProcessConfig) (*ProcessAdapter, error) {
	command := strings.TrimSpace(config.Command)
	if command == "" || strings.ContainsRune(command, 0) {
		return nil, errors.New("process-completion command is required and cannot contain NUL")
	}
	if deniedProcessInterpreter(filepath.Base(command)) {
		return nil, ErrProcessShellUnsupported
	}
	for _, argument := range config.Args {
		if strings.ContainsRune(argument, 0) {
			return nil, errors.New("process-completion arguments cannot contain NUL")
		}
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("locate process-completion executable: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, fmt.Errorf("resolve process-completion executable: %w", err)
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return nil, fmt.Errorf("resolve process-completion executable symlinks: %w", err)
	}
	if deniedProcessInterpreter(filepath.Base(resolved)) {
		return nil, ErrProcessShellUnsupported
	}
	workingDirectory := config.WorkingDirectory
	if workingDirectory != "" {
		workingDirectory, err = filepath.Abs(workingDirectory)
		if err != nil {
			return nil, fmt.Errorf("resolve process-completion working directory: %w", err)
		}
		workingDirectory, err = filepath.EvalSymlinks(workingDirectory)
		if err != nil {
			return nil, fmt.Errorf("resolve process-completion working directory symlinks: %w", err)
		}
		if info, statErr := os.Stat(workingDirectory); statErr != nil || !info.IsDir() {
			return nil, errors.New("process-completion working directory is not a directory")
		}
	}
	if config.EventType == "" {
		config.EventType = "process.completed"
	}
	if !validEventType(config.EventType) {
		return nil, errors.New("invalid process-completion event type")
	}
	if config.Timeout < 0 || config.Timeout > 24*time.Hour {
		return nil, errors.New("process-completion timeout cannot be negative or exceed 24 hours")
	}
	if config.MaxOutputBytes == 0 {
		config.MaxOutputBytes = defaultProcessOutputBytes
	}
	if config.MaxOutputBytes < 1 || config.MaxOutputBytes > 1<<30 {
		return nil, errors.New("process-completion output limit must be between 1 byte and 1 GiB")
	}
	environment, err := processEnvironment(config.InheritEnvironment, config.Environment)
	if err != nil {
		return nil, err
	}
	return &ProcessAdapter{
		command: resolved, args: append([]string(nil), config.Args...),
		workingDirectory: workingDirectory, environment: environment,
		eventType: config.EventType, conversationKey: config.ConversationKey,
		idempotencyKey: config.IdempotencyKey, createTask: config.CreateTask,
		timeout: config.Timeout, maxOutput: config.MaxOutputBytes,
	}, nil
}

// Run waits for the configured process and returns a completion event for any
// process that successfully started, including non-zero exits. Start failures
// and caller cancellation are returned as errors.
func (a *ProcessAdapter) Run(ctx context.Context) (Event, error) {
	processContext := ctx
	cancel := func() {}
	if a.timeout > 0 {
		processContext, cancel = context.WithTimeout(ctx, a.timeout)
	}
	defer cancel()
	command := exec.CommandContext(processContext, a.command, a.args...)
	configureLocalProcess(command)
	command.Dir = a.workingDirectory
	command.Env = append([]string(nil), a.environment...)
	stdout := &processBuffer{limit: a.maxOutput}
	stderr := &processBuffer{limit: a.maxOutput}
	command.Stdout = stdout
	command.Stderr = stderr
	started := time.Now().UTC()
	if err := command.Start(); err != nil {
		return Event{}, fmt.Errorf("start completion process: %w", err)
	}
	waitErr := command.Wait()
	finished := time.Now().UTC()
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if processContext.Err() != nil {
			exitCode = -1
		} else {
			return Event{}, fmt.Errorf("wait for completion process: %w", waitErr)
		}
	}
	completion := ProcessCompletion{
		Command: a.command, Args: append([]string(nil), a.args...), ExitCode: exitCode,
		Stdout: stdout.String(), Stderr: stderr.String(), StartedAt: started, FinishedAt: finished,
		TimedOut:  errors.Is(processContext.Err(), context.DeadlineExceeded) && ctx.Err() == nil,
		Cancelled: ctx.Err() != nil, Truncated: stdout.Truncated() || stderr.Truncated(),
		CancellationScope: localProcessCancellationScope,
	}
	encoded, err := json.Marshal(completion)
	if err != nil {
		return Event{}, fmt.Errorf("encode process completion: %w", err)
	}
	idempotencyKey := a.idempotencyKey
	if idempotencyKey == "" {
		idempotencyKey, err = secureNonce()
		if err != nil {
			return Event{}, fmt.Errorf("create process-completion idempotency key: %w", err)
		}
	}
	event := Event{
		Type: a.eventType, Data: encoded, ConversationKey: a.conversationKey,
		IdempotencyKey: idempotencyKey, CreateTask: a.createTask,
	}
	if ctx.Err() != nil {
		return event, ctx.Err()
	}
	return event, nil
}

func (a *ProcessAdapter) RunAndEmit(ctx context.Context, emit func(Event) error) error {
	if emit == nil {
		return errors.New("process-completion event callback is required")
	}
	event, err := a.Run(ctx)
	if err != nil {
		return err
	}
	return emit(event)
}

func (a *ProcessAdapter) CancellationScope() string { return localProcessCancellationScope }

func deniedProcessInterpreter(base string) bool {
	base = strings.ToLower(strings.TrimSpace(base))
	return map[string]bool{
		"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true,
		"cmd": true, "cmd.exe": true, "powershell": true, "powershell.exe": true,
		"pwsh": true, "osascript": true,
	}[base]
}

func processEnvironment(inherit bool, overrides map[string]string) ([]string, error) {
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
			return nil, fmt.Errorf("invalid process-completion environment variable %q", name)
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

type processBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *processBuffer) Write(data []byte) (int, error) {
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

func (b *processBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func (b *processBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

var _ io.Writer = (*processBuffer)(nil)
