package stdio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	coreagents "github.com/veqri/veqri/core/agents"
	"github.com/veqri/veqri/core/tasks"
)

func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("VEQRI_STDIO_HELPER")
	if mode == "" {
		return
	}
	var request Request
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		os.Exit(11)
	}
	if request.Type != "run" || request.Version != 1 || request.Task.ID == "" {
		os.Exit(12)
	}
	switch mode {
	case "success":
		_, _ = fmt.Fprintln(os.Stdout, `{"type":"progress","progress":{"percent":25,"message":"started"}}`)
		_, _ = fmt.Fprintf(os.Stdout, `{"type":"result","result":{"structured":{"task_id":%q},"written_summary":"complete","spoken_summary":"complete","artifacts":null,"partial":false}}`+"\n", request.Task.ID)
	case "sleep":
		_, _ = fmt.Fprintln(os.Stdout, `{"type":"progress","progress":{"percent":1,"message":"sleeping"}}`)
		time.Sleep(10 * time.Second)
	case "large":
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", 4096))
	case "fail":
		_, _ = fmt.Fprint(os.Stderr, strings.Repeat("stdio-error-secret-must-not-persist", 1000))
		os.Exit(14)
	default:
		os.Exit(13)
	}
	os.Exit(0)
}

func newHelperRunner(t *testing.T, mode string, configure func(*Config)) *Runner {
	t.Helper()
	config := Config{
		Command: os.Args[0], Args: []string{"-test.run=TestHelperProcess"},
		Environment: map[string]string{"VEQRI_STDIO_HELPER": mode},
		Definition:  coreagents.Definition{ID: "helper"},
	}
	if configure != nil {
		configure(&config)
	}
	runner, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func TestRunnerUsesStructuredJSONProtocol(t *testing.T) {
	runner := newHelperRunner(t, "success", nil)
	var progress []coreagents.Progress
	result, err := runner.Run(context.Background(), tasks.Task{ID: "task-stdio"}, func(value coreagents.Progress) {
		progress = append(progress, value)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 1 || progress[0].Percent != 25 {
		t.Fatalf("progress = %#v", progress)
	}
	if result.WrittenSummary != "complete" || string(result.Structured) != `{"task_id":"task-stdio"}` {
		t.Fatalf("result = %#v", result)
	}
	definition := runner.Definition()
	if definition.ExecutionMode != coreagents.ModeStdio || !definition.SupportsCancellation || !definition.SupportsStreaming {
		t.Fatalf("definition = %#v", definition)
	}
	if runner.CancellationScope() == "" {
		t.Fatal("cancellation scope was not reported")
	}
}

func TestRunnerCancellationKillsProcess(t *testing.T) {
	runner := newHelperRunner(t, "sleep", nil)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, tasks.Task{ID: "cancel-stdio"}, func(coreagents.Progress) {
			select {
			case <-started:
			default:
				close(started)
			}
		})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("helper did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stdio agent was not cancelled")
	}
}

func TestRunnerEnforcesOutputLimit(t *testing.T) {
	runner := newHelperRunner(t, "large", func(config *Config) {
		config.MaxOutputBytes = 128
	})
	_, err := runner.Run(context.Background(), tasks.Task{ID: "large-stdio"}, nil)
	if !errors.Is(err, ErrOutputLimit) || !errors.Is(err, ErrProtocol) {
		t.Fatalf("Run error = %v", err)
	}
}

func TestRunnerDoesNotExposeStderrOnProcessFailure(t *testing.T) {
	runner := newHelperRunner(t, "fail", nil)
	_, err := runner.Run(context.Background(), tasks.Task{ID: "failed-stdio"}, nil)
	if err == nil || !strings.Contains(err.Error(), "code 14") || strings.Contains(err.Error(), "stdio-error-secret") {
		t.Fatalf("Run error exposed stdio stderr: %v", err)
	}
}

func TestNewRejectsShellInterpreter(t *testing.T) {
	if _, err := New(Config{Command: "sh", Args: []string{"-c", "echo unsafe"}}); !errors.Is(err, ErrShellUnsupported) {
		t.Fatalf("New error = %v", err)
	}
}
