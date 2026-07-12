package agents

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/internal/stream"
	"github.com/veqri/veqri/tools/shell"
)

func TestPersistedShellTaskIsReclassifiedBeforeExecution(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, filepath.Join(t.TempDir(), "runtime-security.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	executor, err := shell.New([]string{workspace}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binaryContents, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatal(err)
	}
	privilegedAlias := filepath.Join(workspace, "runas.exe")
	if err := os.WriteFile(privilegedAlias, binaryContents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(privilegedAlias, 0o700); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(shell.Input{Command: privilegedAlias, Args: []string{"/savecred", "/user:Administrator", "whoami"}, WorkingDir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := tasks.Task{
		ID: "legacy-approved-runas", RootTaskID: "legacy-approved-runas", TaskType: "shell",
		Goal: "legacy approved privileged command", Input: input, AssignedAgentID: "builtin.automation",
		AllowedTools: []string{"shell"}, ApprovalPolicy: "APPROVED_ONCE", Status: tasks.StatusRunning,
		Progress: 50, CreatedAt: now, StartedAt: &now, MaxRetries: 0,
		CorrelationID: "legacy-runas-correlation", IdempotencyKey: "legacy-runas-idempotency",
	}
	if _, duplicate, err := store.CreateTask(ctx, task); err != nil || duplicate {
		t.Fatalf("CreateTask() = duplicate %v, %v", duplicate, err)
	}
	runtime := NewRuntime(
		store,
		NewRegistry(),
		executor,
		stream.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		1,
	)

	runtime.processShell(ctx, task)

	updated, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != tasks.StatusFailed || updated.Error != "privilege escalation is denied at execution time" {
		t.Fatalf("legacy privileged task = %s (%q), want failed execution-time denial", updated.Status, updated.Error)
	}
	var invocationCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM tool_invocations WHERE task_id = ?", task.ID).Scan(&invocationCount); err != nil {
		t.Fatal(err)
	}
	if invocationCount != 0 {
		t.Fatalf("legacy privileged task created %d invocation(s)", invocationCount)
	}
}

func TestLegacyApprovedPATHCommandWithoutDigestFailsClosed(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, filepath.Join(t.TempDir(), "runtime-path-security.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	executor, err := shell.New([]string{workspace}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	goBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	legacyCommand := "legacy-path-tool"
	if runtime.GOOS == "windows" {
		legacyCommand += ".exe"
	}
	pathTarget := filepath.Join(workspace, legacyCommand)
	binaryContents, err := os.ReadFile(goBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathTarget, binaryContents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pathTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	// This models a PATH target selected only after an old approval was
	// persisted. The legacy payload has neither an absolute canonical command
	// nor the content digest that a current approval binds.
	t.Setenv("PATH", workspace+string(os.PathListSeparator)+os.Getenv("PATH"))
	input, err := json.Marshal(shell.Input{
		Command: legacyCommand, Args: []string{"-test.run=^$"}, WorkingDir: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := tasks.Task{
		ID: "legacy-approved-path", RootTaskID: "legacy-approved-path", TaskType: "shell",
		Goal: "legacy PATH command", Input: input, AssignedAgentID: "builtin.automation",
		AllowedTools: []string{"shell"}, ApprovalPolicy: "APPROVED_ONCE", Status: tasks.StatusRunning,
		Progress: 50, CreatedAt: now, StartedAt: &now, MaxRetries: 0,
		CorrelationID: "legacy-path-correlation", IdempotencyKey: "legacy-path-idempotency",
	}
	if _, duplicate, err := store.CreateTask(ctx, task); err != nil || duplicate {
		t.Fatalf("CreateTask() = duplicate %v, %v", duplicate, err)
	}
	runtime := NewRuntime(
		store,
		NewRegistry(),
		executor,
		stream.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		1,
	)

	runtime.processShell(ctx, task)

	updated, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	const wantError = "approved shell invocation is not canonical; request a new approval"
	if updated.Status != tasks.StatusFailed || updated.Error != wantError {
		t.Fatalf("legacy PATH task = %s (%q), want failed canonical denial", updated.Status, updated.Error)
	}
	var invocationCount int
	if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM tool_invocations WHERE task_id = ?", task.ID).Scan(&invocationCount); err != nil {
		t.Fatal(err)
	}
	if invocationCount != 0 {
		t.Fatalf("legacy PATH task created %d invocation(s)", invocationCount)
	}
}
