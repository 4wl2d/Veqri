package agents

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/internal/stream"
)

func TestRetriedPartialRootPersistsAttemptScopedTranscriptAndDelivery(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, filepath.Join(t.TempDir(), "state", "veqri.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	conversationRecord, err := store.GetOrCreateConversation(ctx, "retry:partial-root", "Retry", true, "conversation-retry-root")
	if err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{
  "source":{"kind":"slack"},
  "reply_target":{"connector_id":"slack-simulator","channel_id":"channel","thread_id":"thread"}
}`)
	now := time.Now().UTC()
	task := tasks.Task{
		ID: "task-retry-root", RootTaskID: "task-retry-root", ConversationID: conversationRecord.ID,
		Goal: "retry root", TaskType: "dialog", Input: input, AssignedAgentID: "builtin.general",
		AllowedTools: []string{}, ApprovalPolicy: "policy-engine", Status: tasks.StatusRunning,
		CreatedAt: now, MaxRetries: 2, TimeoutSeconds: 30, Artifacts: []tasks.Artifact{},
		CorrelationID: "correlation-retry-root", IdempotencyKey: "retry-root", Version: 1,
	}
	if _, duplicate, err := store.CreateTask(ctx, task); err != nil || duplicate {
		t.Fatalf("CreateTask() = duplicate %v, %v", duplicate, err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime := NewRuntime(store, nil, nil, stream.New(), logger, 1)
	first, err := runtime.completeAndPublish(ctx, task, json.RawMessage(`{"attempt":1}`),
		"partial first answer", "partial first answer", true, nil)
	if err != nil || first.Status != tasks.StatusPartiallyCompleted {
		t.Fatalf("first partial completion = %+v, %v", first, err)
	}
	if _, err := store.RetryTask(ctx, task.ID); err != nil {
		t.Fatalf("RetryTask(): %v", err)
	}
	claimed, err := store.ClaimNextTask(ctx)
	if err != nil {
		t.Fatal(err)
	}
	running, err := store.StartTask(ctx, claimed.ID, claimed.Version)
	if err != nil || running.RetryCount != 1 {
		t.Fatalf("retry running task = %+v, %v", running, err)
	}
	completed, err := runtime.completeAndPublish(ctx, running, json.RawMessage(`{"attempt":2}`),
		"successful retry answer", "successful retry answer", false, nil)
	if err != nil || completed.Status != tasks.StatusCompleted || completed.UserFacingSummary != "successful retry answer" {
		t.Fatalf("retried completion = %+v, %v", completed, err)
	}

	turns, err := store.ListTurns(ctx, conversationRecord.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantTurns := map[string]string{
		"turn:assistant:task-retry-root":         "partial first answer",
		"turn:assistant:task-retry-root:retry:1": "successful retry answer",
	}
	if len(turns) != len(wantTurns) {
		t.Fatalf("persisted reconnect transcript = %+v", turns)
	}
	for _, turn := range turns {
		if turn.Role != conversation.RoleAssistant || wantTurns[turn.ID] != turn.Text {
			t.Errorf("unexpected persisted retry turn: %+v", turn)
		}
	}

	rows, err := store.DB().QueryContext(ctx, `SELECT id, idempotency_key, status
FROM deliveries WHERE task_id = ? ORDER BY id`, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	type persistedDelivery struct{ id, key, status string }
	var deliveries []persistedDelivery
	for rows.Next() {
		var item persistedDelivery
		if err := rows.Scan(&item.id, &item.key, &item.status); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		deliveries = append(deliveries, item)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	wantDeliveries := map[string]string{
		"delivery:task-retry-root:connector-final":         "task:task-retry-root:connector-final",
		"delivery:task-retry-root:connector-final:retry:1": "task:task-retry-root:connector-final:retry:1",
	}
	if len(deliveries) != len(wantDeliveries) {
		t.Fatalf("persisted deliveries = %+v", deliveries)
	}
	for _, item := range deliveries {
		if wantDeliveries[item.id] != item.key || item.status != "DELIVERED" {
			t.Errorf("unexpected persisted retry delivery: %+v", item)
		}
	}
}
