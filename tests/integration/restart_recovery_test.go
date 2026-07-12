package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	coreagents "github.com/veqri/veqri/core/agents"
	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/delivery"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/tests/integration/testfixture"
)

func TestRestartRecoversTaskAndDoesNotDuplicateExecutionTaskOrDelivery(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "restart.db")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("create restart workspace: %v", err)
	}
	var executions atomic.Int32
	runner := &countingRunner{executions: &executions}
	options := testfixture.Options{
		DatabasePath: databasePath, Workspace: workspace, WorkerCount: 2,
		Runners: []coreagents.Runner{runner},
	}

	beforeRestart := options
	beforeRestart.NoWorkers = true
	beforeRestart.WorkerCount = 0
	firstCore := testfixture.New(t, beforeRestart)
	simulatorBody := map[string]any{
		"text": "finish after restart", "actor_id": "restart-user",
		"workspace_id": "restart-workspace", "channel_id": "restart-channel",
		"thread_id": "restart-thread", "message_id": "restart-message-001",
	}
	createdResponse := firstCore.JSON(t, http.MethodPost, "/v1/connectors/simulate/slack", firstCore.AdminToken, simulatorBody, nil)
	testfixture.RequireStatus(t, createdResponse, http.StatusAccepted)
	created := testfixture.Decode[struct {
		Task      tasks.Task `json:"task"`
		Duplicate bool       `json:"duplicate"`
	}](t, createdResponse)
	if created.Duplicate || created.Task.Status != tasks.StatusQueued {
		t.Fatalf("unexpected pre-restart task: %+v", created)
	}
	claimed, err := firstCore.Store.ClaimNextTask(context.Background())
	if err != nil {
		t.Fatalf("claim task before simulated crash: %v", err)
	}
	running, err := firstCore.Store.StartTask(context.Background(), claimed.ID, claimed.Version)
	if err != nil {
		t.Fatalf("start task before simulated crash: %v", err)
	}
	if running.Status != tasks.StatusRunning || executions.Load() != 0 {
		t.Fatalf("pre-restart task was not durably RUNNING without execution: task=%+v executions=%d", running, executions.Load())
	}
	assertTaskInvocationCount(t, firstCore, running.ID, 0)
	firstCore.Close()

	afterRestart := testfixture.New(t, options)
	duplicateWhileRecovering := afterRestart.JSON(t, http.MethodPost, "/v1/connectors/simulate/slack", afterRestart.AdminToken, simulatorBody, nil)
	testfixture.RequireStatus(t, duplicateWhileRecovering, http.StatusAccepted)
	duplicateResult := testfixture.Decode[struct {
		Task      tasks.Task `json:"task"`
		Duplicate bool       `json:"duplicate"`
	}](t, duplicateWhileRecovering)
	if !duplicateResult.Duplicate || duplicateResult.Task.ID != created.Task.ID {
		t.Fatalf("replayed trigger created another task: %s", duplicateWhileRecovering.Body)
	}
	completed := afterRestart.WaitTask(t, created.Task.ID, tasks.StatusCompleted)
	if completed.RetryCount != 1 || completed.ProgressMessage != "Complete" {
		t.Fatalf("task did not carry restart recovery metadata through completion: %+v", completed)
	}
	if executions.Load() != 1 {
		t.Fatalf("recovered agent executed %d times, want exactly once", executions.Load())
	}
	assertDelivery(t, afterRestart, completed.ID, delivery.StatusDelivered, delivery.Target{
		Kind: "slack", ConnectorID: "slack-simulator", ChannelID: "restart-channel", ThreadID: "restart-thread",
	})
	assertCounts(t, afterRestart, map[string]int{
		"SELECT COUNT(*) FROM events WHERE idempotency_key = 'restart-message-001'":      1,
		"SELECT COUNT(*) FROM tasks WHERE root_task_id = '" + completed.RootTaskID + "'": 1,
		"SELECT COUNT(*) FROM deliveries WHERE task_id = '" + completed.ID + "'":         1,
	})
	turns, err := afterRestart.Store.ListTurns(context.Background(), completed.ConversationID, 20)
	if err != nil {
		t.Fatalf("list turns after recovery: %v", err)
	}
	if len(turns) != 2 || turns[0].Role != conversation.RoleUser || turns[1].Role != conversation.RoleAssistant {
		t.Fatalf("restart duplicated or lost conversation turns: %+v", turns)
	}

	afterRestart.Close()
	inspectionOptions := options
	inspectionOptions.NoWorkers = true
	inspectionOptions.WorkerCount = 0
	thirdCore := testfixture.New(t, inspectionOptions)
	duplicateAfterDelivery := thirdCore.JSON(t, http.MethodPost, "/v1/connectors/simulate/slack", thirdCore.AdminToken, simulatorBody, nil)
	testfixture.RequireStatus(t, duplicateAfterDelivery, http.StatusAccepted)
	thirdResult := testfixture.Decode[struct {
		Task      tasks.Task `json:"task"`
		Duplicate bool       `json:"duplicate"`
	}](t, duplicateAfterDelivery)
	if !thirdResult.Duplicate || thirdResult.Task.ID != completed.ID {
		t.Fatalf("post-delivery restart replay created another task: %s", duplicateAfterDelivery.Body)
	}
	if executions.Load() != 1 {
		t.Fatalf("completed task re-executed on a later restart: %d executions", executions.Load())
	}
	assertCounts(t, thirdCore, map[string]int{
		"SELECT COUNT(*) FROM events WHERE idempotency_key = 'restart-message-001'":      1,
		"SELECT COUNT(*) FROM tasks WHERE root_task_id = '" + completed.RootTaskID + "'": 1,
		"SELECT COUNT(*) FROM deliveries WHERE task_id = '" + completed.ID + "'":         1,
	})
}

type countingRunner struct {
	executions *atomic.Int32
}

func (r *countingRunner) Definition() coreagents.Definition {
	return coreagents.Definition{
		ID: "builtin.general", DisplayName: "Restart counting agent",
		Description: "counts deterministic integration executions", Capabilities: []string{"dialog", "deterministic"},
		AcceptedTaskTypes: []string{"dialog"}, InputSchema: json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`), TrustLevel: coreagents.TrustTrusted,
		ConcurrencyLimit: 1, Health: coreagents.HealthHealthy, ExecutionMode: coreagents.ModeBuiltin,
		SupportsCancellation: true, SupportsStreaming: true, UpdatedAt: time.Now().UTC(),
	}
}

func (r *countingRunner) Run(ctx context.Context, task tasks.Task, progress func(coreagents.Progress)) (coreagents.Result, error) {
	select {
	case <-ctx.Done():
		return coreagents.Result{}, ctx.Err()
	default:
	}
	execution := r.executions.Add(1)
	progress(coreagents.Progress{Percent: 50, Message: "recovering deterministic result"})
	structured, _ := json.Marshal(map[string]any{"execution": execution, "task_id": task.ID})
	return coreagents.Result{
		Structured: structured, WrittenSummary: "Recovered task completed exactly once.",
		SpokenSummary: "Recovered task completed.",
	}, nil
}
