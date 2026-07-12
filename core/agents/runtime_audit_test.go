package agents

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/internal/stream"
)

type auditProbeRunner struct {
	runs atomic.Int32
}

func (r *auditProbeRunner) Definition() Definition {
	return Definition{ID: "audit.probe", DisplayName: "Audit probe", ConcurrencyLimit: 1,
		Health: HealthHealthy, ExecutionMode: ModeBuiltin}
}

func (r *auditProbeRunner) Run(context.Context, tasks.Task, func(Progress)) (Result, error) {
	r.runs.Add(1)
	return Result{Structured: json.RawMessage(`{"ok":true}`), WrittenSummary: "done"}, nil
}

func TestAgentIsNotInvokedWhenStartAuditCannotBePersisted(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, filepath.Join(t.TempDir(), "state", "veqri.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	task := tasks.Task{ID: "task-agent-audit", RootTaskID: "task-agent-audit", Goal: "probe",
		TaskType: "dialog", Input: json.RawMessage(`{}`), AssignedAgentID: "audit.probe",
		AllowedTools: []string{}, ApprovalPolicy: "test", Status: tasks.StatusRunning,
		CreatedAt: time.Now().UTC(), TimeoutSeconds: 30, Artifacts: []tasks.Artifact{},
		CorrelationID: "correlation-agent-audit", IdempotencyKey: "agent-audit", Version: 1}
	if _, duplicate, err := store.CreateTask(ctx, task); err != nil || duplicate {
		t.Fatalf("CreateTask() = duplicate %v, %v", duplicate, err)
	}
	if _, err := store.DB().ExecContext(ctx, `CREATE TRIGGER reject_agent_start_audit
BEFORE INSERT ON audit_entries WHEN NEW.action = 'agent.started'
BEGIN SELECT RAISE(FAIL, 'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}

	runner := &auditProbeRunner{}
	registry := NewRegistry()
	if err := registry.Register(runner); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime := NewRuntime(store, registry, nil, stream.New(), logger, 1)
	runtime.processAgent(ctx, task)

	if got := runner.runs.Load(); got != 0 {
		t.Fatalf("agent ran %d time(s) despite failed mandatory start audit", got)
	}
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != tasks.StatusFailed {
		t.Fatalf("task status = %s, want FAILED after execution was blocked", stored.Status)
	}
	entries, err := store.ListAuditEntries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Action != "agent.failed" {
		t.Fatalf("audit entries = %+v, want only the audited blocked failure", entries)
	}
}
