package persistence

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/delivery"
	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/core/tasks"
)

func TestAskWorkRollsBackEveryIngressRecordOnGraphFailure(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now().UTC()
	event := testEvent("event-atomic", "connector", "instance", "atomic")
	conversationRecord := conversation.Conversation{ID: "conversation-atomic", ExternalKey: event.ConversationKey,
		Title: "Atomic", TranscriptRetention: true, CreatedAt: now, UpdatedAt: now}
	root := testRootTask("task-atomic", conversationRecord.ID, event.ID, now)
	_, _, _, err := store.CreateAskWork(ctx, AskWork{
		Event: event, Conversation: conversationRecord,
		Turn: conversation.Turn{ID: "turn:user:" + event.ID, Role: conversation.RoleUser,
			Text: "atomic", Final: true, CorrelationID: event.CorrelationID, CreatedAt: now},
		Tasks:        []tasks.Task{root},
		Dependencies: []tasks.Dependency{{TaskID: root.ID, DependsOnTaskID: root.ID}},
		Audit: &observability.AuditEntry{ID: "audit-atomic", OccurredAt: now,
			ActorKind: "test", ActorID: "actor", Action: "ingress.accepted",
			ResourceKind: "event", ResourceID: event.ID, Decision: "ALLOW", Details: json.RawMessage(`{}`)},
	})
	if err == nil {
		t.Fatal("invalid graph unexpectedly committed")
	}
	for table := range map[string]bool{"events": true, "conversations": true, "turns": true, "tasks": true, "audit_entries": true} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s retained %d row(s) after rollback", table, count)
		}
	}
}

func TestRootCompletionAndOutcomeCommitTogetherAndRespectRetention(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now().UTC()
	conversationRecord, err := store.GetOrCreateConversation(ctx, "private:atomic", "Private", false, "conversation-private")
	if err != nil {
		t.Fatal(err)
	}
	root := testRootTask("task-private", conversationRecord.ID, "event-private", now)
	root.Status = tasks.StatusRunning
	if _, _, err := store.CreateTask(ctx, root); err != nil {
		t.Fatal(err)
	}
	turn := conversation.Turn{ID: "turn:assistant:" + root.ID, ConversationID: conversationRecord.ID,
		Role: conversation.RoleAssistant, Text: "private answer", Final: true,
		CorrelationID: root.CorrelationID, CreatedAt: now}
	badDelivery := delivery.Delivery{ID: "delivery-bad", TaskID: "other-task",
		Target: delivery.Target{Kind: "test", ConnectorID: "test"}, Status: delivery.StatusPending,
		IdempotencyKey: "delivery-bad", CreatedAt: now, CorrelationID: root.CorrelationID}
	if _, err := store.CompleteTaskWithOutcome(ctx, root.ID, json.RawMessage(`{"private":true}`),
		"private answer", false, CompletionOutcome{Turn: &turn, Delivery: &badDelivery}); err == nil {
		t.Fatal("mismatched delivery unexpectedly committed")
	}
	unchanged, _ := store.GetTask(ctx, root.ID)
	if unchanged.Status != tasks.StatusRunning {
		t.Fatalf("task status after rollback = %s", unchanged.Status)
	}
	validDelivery := badDelivery
	validDelivery.ID = "delivery-valid"
	validDelivery.TaskID = root.ID
	validDelivery.IdempotencyKey = "delivery-valid"
	completed, err := store.CompleteTaskWithOutcome(ctx, root.ID, json.RawMessage(`{"private":true}`),
		"private answer", false, CompletionOutcome{Turn: &turn, Delivery: &validDelivery})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != tasks.StatusCompleted || len(completed.Result) != 0 || completed.Goal != "[transcript retention disabled]" {
		t.Fatalf("non-retained completion was not scrubbed: %+v", completed)
	}
	turns, err := store.ListTurns(ctx, conversationRecord.ID, 10)
	if err != nil || len(turns) != 1 || turns[0].Text != "[transcript retention disabled]" {
		t.Fatalf("stored turns = %+v, %v", turns, err)
	}
	var deliveries int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM deliveries WHERE task_id = ?", root.ID).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("deliveries = %d, %v", deliveries, err)
	}
}

func TestVoiceSessionUniquenessAndControlsAreDurable(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now().UTC()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO devices(id, name, platform, credential_hash,
capabilities_json, created_at, last_seen_at, key_version) VALUES(?, ?, ?, ?, '{}', ?, ?, 1)`,
		"device-one", "Phone", "android", []byte("hash"), formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	firstConversation, _ := store.GetOrCreateConversation(ctx, "voice:one", "One", true, "voice-conversation-one")
	secondConversation, _ := store.GetOrCreateConversation(ctx, "voice:two", "Two", true, "voice-conversation-two")
	first := conversation.VoiceSession{ID: "voice-one", ConversationID: firstConversation.ID,
		DeviceID: "device-one", State: conversation.StateListening, Transport: "simulated",
		StartedAt: now, CorrelationID: "voice-correlation-one", Direction: "OUTGOING", AudioRoute: "EARPIECE"}
	if err := store.CreateVoiceSession(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID = "voice-two"
	second.ConversationID = secondConversation.ID
	if err := store.CreateVoiceSession(ctx, second); err == nil {
		t.Fatal("second active session for one device unexpectedly succeeded")
	}
	updated, err := store.UpdateVoicePreferences(ctx, first.ID, true, true, "SPEAKER")
	if err != nil || !updated.Muted || !updated.PushToTalk || updated.AudioRoute != "SPEAKER" {
		t.Fatalf("persisted voice controls = %+v, %v", updated, err)
	}
	if _, err := store.TransitionVoiceSession(ctx, first.ID, conversation.StateEnded, false); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateVoiceSession(ctx, second); err != nil {
		t.Fatalf("new session after ending prior call: %v", err)
	}
}

func TestExplicitRetryHonorsTaskTypeAndMaximum(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now().UTC()
	eventID := "retry-event"
	task := testRootTask("retry-task", "", eventID, now)
	task.Status = tasks.StatusFailed
	task.MaxRetries = 1
	if _, _, err := store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	retried, err := store.RetryTask(ctx, task.ID)
	if err != nil || retried.Status != tasks.StatusQueued || retried.RetryCount != 1 {
		t.Fatalf("first RetryTask = %+v, %v", retried, err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE tasks SET status = 'FAILED' WHERE id = ?", task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RetryTask(ctx, task.ID); err == nil {
		t.Fatal("retry beyond max_retries unexpectedly succeeded")
	}

	shell := testRootTask("retry-shell", "", "retry-shell-event", now)
	shell.Status = tasks.StatusFailed
	shell.TaskType = "shell"
	shell.MaxRetries = 3
	if _, _, err := store.CreateTask(ctx, shell); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RetryTask(ctx, shell.ID); err == nil {
		t.Fatal("shell retry unexpectedly succeeded")
	}

	dismissed := testRootTask("retry-dismissed", "", "retry-dismissed-event", now)
	dismissed.Status = tasks.StatusFailed
	dismissed.MaxRetries = 1
	if _, _, err := store.CreateTask(ctx, dismissed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DismissTask(ctx, dismissed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RetryTask(ctx, dismissed.ID); err == nil {
		t.Fatal("dismissed task unexpectedly became retryable")
	}
	storedDismissed, err := store.GetTask(ctx, dismissed.ID)
	if err != nil || !storedDismissed.Dismissed || storedDismissed.Status != tasks.StatusFailed || storedDismissed.RetryCount != 0 {
		t.Fatalf("dismissed task changed after rejected retry: %+v, %v", storedDismissed, err)
	}
}

func TestTaskPriorityControlsSchedulingAndTerminalDismissal(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now().UTC()
	low := testRootTask("priority-low", "", "priority-low-event", now)
	high := testRootTask("priority-high", "", "priority-high-event", now.Add(time.Second))
	terminal := testRootTask("priority-terminal", "", "priority-terminal-event", now.Add(2*time.Second))
	terminal.Status = tasks.StatusCompleted
	terminal.Progress = 100
	terminal.FinishedAt = &now
	for _, task := range []tasks.Task{low, high, terminal} {
		if _, _, err := store.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SetTaskPriority(ctx, high.ID, 80); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetTaskPriority(ctx, low.ID, -20); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetTaskPriority(ctx, low.ID, 101); err == nil {
		t.Fatal("out-of-range priority unexpectedly succeeded")
	}
	claimed, err := store.ClaimNextTask(ctx)
	if err != nil || claimed.ID != high.ID {
		t.Fatalf("ClaimNextTask() = %+v, %v; want highest priority task", claimed, err)
	}
	if _, err := store.DismissTask(ctx, low.ID); err == nil {
		t.Fatal("active task dismissal unexpectedly succeeded")
	}
	dismissed, err := store.DismissTask(ctx, terminal.ID)
	if err != nil || !dismissed.Dismissed {
		t.Fatalf("DismissTask() = %+v, %v", dismissed, err)
	}
	listed, err := store.ListTasks(ctx, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range listed {
		if task.ID == terminal.ID {
			t.Fatal("dismissed task remained in the default list")
		}
	}
}

func testRootTask(id, conversationID, eventID string, now time.Time) tasks.Task {
	return tasks.Task{ID: id, RootTaskID: id, ConversationID: conversationID, Goal: "goal",
		TaskType: "dialog", Input: json.RawMessage(`{"text":"goal"}`), AssignedAgentID: "test",
		AllowedTools: []string{}, ApprovalPolicy: "test", Status: tasks.StatusQueued,
		CreatedAt: now, MaxRetries: 2, TimeoutSeconds: 30, Artifacts: []tasks.Artifact{},
		CorrelationID: "correlation-" + id, CausationID: &eventID,
		IdempotencyKey: "event:" + eventID + ":root", Version: 1}
}
