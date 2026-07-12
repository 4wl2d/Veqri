package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreagents "github.com/veqri/veqri/core/agents"
	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/tests/integration/testfixture"
)

func TestFailedChildRetryResynthesizesAndPersistsNewFinalOutcome(t *testing.T) {
	flaky := &failOnceGraphRunner{}
	fixture := testfixture.New(t, testfixture.Options{
		WorkerCount: 2,
		Runners:     []coreagents.Runner{flaky},
	})
	ctx := context.Background()
	conversationRecord, err := fixture.Store.GetOrCreateConversation(ctx,
		"integration:graph-retry", "Graph retry", true, "conversation-graph-retry")
	if err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{
  "text":"recover delegated work",
  "source":{"kind":"slack"},
  "reply_target":{"connector_id":"slack-simulator","channel_id":"channel","thread_id":"thread"}
}`)
	now := time.Now().UTC()
	rootID := "integration-retry-root"
	causationID := "integration-retry-event"
	root := tasks.Task{
		ID: rootID, RootTaskID: rootID, ConversationID: conversationRecord.ID,
		Goal: "recover delegated work", TaskType: "synthesis", Input: input,
		AssignedAgentID: "builtin.synthesizer", AllowedTools: []string{},
		ApprovalPolicy: "policy-engine", Status: tasks.StatusQueued,
		ProgressMessage: "Waiting for delegated workers", CreatedAt: now,
		MaxRetries: 2, TimeoutSeconds: 30, Artifacts: []tasks.Artifact{},
		CorrelationID: "correlation-graph-retry", CausationID: &causationID,
		IdempotencyKey: "integration:graph-retry:root", Version: 1,
	}
	parentID := root.ID
	child := tasks.Task{
		ID: "integration-retry-child", ParentTaskID: &parentID, RootTaskID: root.ID,
		ConversationID: conversationRecord.ID, Goal: root.Goal, TaskType: "coding", Input: input,
		AssignedAgentID: "builtin.coding", AllowedTools: []string{},
		ApprovalPolicy: "policy-engine", Status: tasks.StatusQueued,
		ProgressMessage: "Waiting for worker", CreatedAt: now.Add(time.Millisecond),
		MaxRetries: 2, TimeoutSeconds: 30, Artifacts: []tasks.Artifact{},
		CorrelationID: root.CorrelationID, CausationID: &causationID,
		IdempotencyKey: "integration:graph-retry:child", Version: 1,
	}
	if _, duplicate, err := fixture.Store.CreateTaskGraph(ctx, []tasks.Task{root, child}, []tasks.Dependency{
		{TaskID: root.ID, DependsOnTaskID: child.ID},
	}); err != nil || duplicate {
		t.Fatalf("CreateTaskGraph() = duplicate %v, %v", duplicate, err)
	}
	fixture.Runtime.Wake()

	partial := fixture.WaitTask(t, root.ID, tasks.StatusPartiallyCompleted)
	failedChild, err := fixture.Store.GetTask(ctx, child.ID)
	if err != nil || failedChild.Status != tasks.StatusFailed || flaky.calls.Load() != 1 {
		t.Fatalf("first child attempt = %+v, calls=%d, err=%v", failedChild, flaky.calls.Load(), err)
	}
	if !strings.Contains(partial.UserFacingSummary, "Failed or incomplete subtasks") {
		t.Fatalf("first synthesis did not persist its partial failure: %q", partial.UserFacingSummary)
	}

	retriedChild, err := fixture.Store.RetryTask(ctx, child.ID)
	if err != nil {
		t.Fatalf("RetryTask(child): %v", err)
	}
	rearmedRoot, err := fixture.Store.GetTask(ctx, root.ID)
	if err != nil || retriedChild.Status != tasks.StatusQueued || rearmedRoot.Status != tasks.StatusQueued ||
		rearmedRoot.RetryCount != 1 {
		t.Fatalf("graph retry state: child=%+v root=%+v err=%v", retriedChild, rearmedRoot, err)
	}
	fixture.Runtime.Wake()

	completed := fixture.WaitTask(t, root.ID, tasks.StatusCompleted)
	completedChild, err := fixture.Store.GetTask(ctx, child.ID)
	if err != nil || completedChild.Status != tasks.StatusCompleted || flaky.calls.Load() != 2 {
		t.Fatalf("retried child = %+v, calls=%d, err=%v", completedChild, flaky.calls.Load(), err)
	}
	if completed.RetryCount != 1 || !strings.Contains(completed.UserFacingSummary, "child recovered on retry") ||
		strings.Contains(completed.UserFacingSummary, "Failed or incomplete subtasks") {
		t.Fatalf("root did not persist a fresh successful synthesis: %+v", completed)
	}

	turns, err := fixture.Store.ListTurns(ctx, conversationRecord.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantTurns := map[string]string{
		"turn:assistant:" + root.ID:              partial.UserFacingSummary,
		"turn:assistant:" + root.ID + ":retry:1": completed.UserFacingSummary,
	}
	if len(turns) != len(wantTurns) {
		t.Fatalf("persisted retry turns = %+v", turns)
	}
	for _, turn := range turns {
		if turn.Role != conversation.RoleAssistant || !turn.Final || wantTurns[turn.ID] != turn.Text {
			t.Errorf("unexpected persisted retry turn: %+v", turn)
		}
	}

	rows, err := fixture.Store.DB().QueryContext(ctx, `SELECT id, idempotency_key, status
FROM deliveries WHERE task_id = ? ORDER BY id`, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	wantDeliveries := map[string]string{
		"delivery:" + root.ID + ":connector-final":         "task:" + root.ID + ":connector-final",
		"delivery:" + root.ID + ":connector-final:retry:1": "task:" + root.ID + ":connector-final:retry:1",
	}
	seen := make(map[string]bool, len(wantDeliveries))
	for rows.Next() {
		var id, key, status string
		if err := rows.Scan(&id, &key, &status); err != nil {
			t.Fatal(err)
		}
		if wantDeliveries[id] != key || status != "DELIVERED" {
			t.Errorf("unexpected persisted retry delivery: id=%q key=%q status=%q", id, key, status)
		}
		seen[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(wantDeliveries) {
		t.Fatalf("persisted retry delivery IDs = %v, want %v", seen, wantDeliveries)
	}
}

type failOnceGraphRunner struct {
	calls atomic.Int32
}

func (r *failOnceGraphRunner) Definition() coreagents.Definition {
	return coreagents.Definition{
		ID: "builtin.coding", DisplayName: "Fail-once coding fixture",
		Description:  "fails the first graph attempt and succeeds the explicit retry",
		Capabilities: []string{"deterministic"}, AcceptedTaskTypes: []string{"coding"},
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		TrustLevel: coreagents.TrustTrusted, ConcurrencyLimit: 1, Health: coreagents.HealthHealthy,
		ExecutionMode: coreagents.ModeBuiltin, SupportsCancellation: true, SupportsStreaming: true,
		UpdatedAt: time.Now().UTC(),
	}
}

func (r *failOnceGraphRunner) Run(_ context.Context, _ tasks.Task,
	progress func(coreagents.Progress)) (coreagents.Result, error) {
	attempt := r.calls.Add(1)
	progress(coreagents.Progress{Percent: 75, Message: "Executing deterministic graph attempt"})
	if attempt == 1 {
		return coreagents.Result{}, errors.New("deterministic first-attempt failure")
	}
	return coreagents.Result{
		Structured:     json.RawMessage(`{"attempt":2,"status":"recovered"}`),
		WrittenSummary: "child recovered on retry", SpokenSummary: "The child recovered on retry.",
	}, nil
}
