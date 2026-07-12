package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/veqri/veqri/core/approvals"
	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/delivery"
	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/core/tasks"
	coretools "github.com/veqri/veqri/core/tools"
)

func TestRetentionSweepUsesStrictRollingCutoffAndProtectsActiveGraphs(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, time.March, 10, 12, 0, 0, 0, time.UTC)
	sweptAt := cutoff.Add(24 * time.Hour)
	old := cutoff.Add(-time.Hour)
	recent := cutoff.Add(time.Hour)

	insertRetentionConversation(t, store, "conversation-stale", "retention:stale", "Private stale title", old)
	insertRetentionConversation(t, store, "conversation-rolling", "retention:rolling", "Current rolling title", recent)
	insertRetentionConversation(t, store, "conversation-active", "retention:active", "Active title", old)

	addRetentionTurn(t, store, "turn-stale-old", "conversation-stale", "stale secret", old)
	addRetentionTurn(t, store, "turn-rolling-old", "conversation-rolling", "rolling old secret", old)
	addRetentionTurn(t, store, "turn-rolling-boundary", "conversation-rolling", "boundary stays", cutoff)
	addRetentionTurn(t, store, "turn-rolling-recent", "conversation-rolling", "recent stays", recent)
	addRetentionTurn(t, store, "turn-active-old", "conversation-active", "active context stays", old)

	terminalEvent := testEvent("event-retention-terminal", "connector", "instance", "retention-terminal")
	terminalEvent.ConversationKey = "retention:stale"
	terminalEvent.OccurredAt, terminalEvent.ReceivedAt = old, old
	terminalEvent.Actor.DisplayName = "Private actor"
	terminalEvent.Payload = json.RawMessage(`{"secret":"terminal event"}`)
	if _, _, err := store.IngestEvent(ctx, terminalEvent); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkEventProcessed(ctx, terminalEvent.ID, errors.New("private processing detail")); err != nil {
		t.Fatal(err)
	}

	activeEvent := testEvent("event-retention-active", "connector", "instance", "retention-active")
	activeEvent.ConversationKey = "retention:active"
	activeEvent.OccurredAt, activeEvent.ReceivedAt = old, old
	activeEvent.Payload = json.RawMessage(`{"secret":"active event"}`)
	if _, _, err := store.IngestEvent(ctx, activeEvent); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkEventProcessed(ctx, activeEvent.ID, nil); err != nil {
		t.Fatal(err)
	}

	terminal := testRootTask("task-retention-terminal", "conversation-stale", terminalEvent.ID, old)
	terminal.Status = tasks.StatusFailed
	terminal.Progress = 100
	terminal.FinishedAt = &old
	terminal.Goal = "private terminal goal"
	terminal.Input = json.RawMessage(`{"secret":"terminal input"}`)
	terminal.Result = json.RawMessage(`{"secret":"terminal result"}`)
	terminal.ProgressMessage = "private progress"
	terminal.Error = "private task error"
	terminal.UserFacingSummary = "private summary"
	terminal.Artifacts = []tasks.Artifact{{ID: "artifact-json", Name: "private artifact", URI: "/outside/unmanaged.txt"}}
	if _, _, err := store.CreateTask(ctx, terminal); err != nil {
		t.Fatal(err)
	}

	active := testRootTask("task-retention-active", "conversation-active", activeEvent.ID, old)
	active.Status = tasks.StatusRunning
	active.Goal = "active private goal"
	active.Input = json.RawMessage(`{"secret":"active input"}`)
	if _, _, err := store.CreateTask(ctx, active); err != nil {
		t.Fatal(err)
	}

	if _, err := store.db.ExecContext(ctx, `INSERT INTO tool_invocations(id, task_id, tool_name,
input_json, risk, status, started_at, finished_at, exit_code, output_json, error,
correlation_id, idempotency_key) VALUES(?, ?, 'shell', ?, 'STATE_CHANGING', 'COMPLETED',
?, ?, 0, ?, ?, ?, ?)`, "invocation-retention", terminal.ID,
		`{"secret":"tool input"}`, formatTime(old), formatTime(old), `{"secret":"tool output"}`,
		"private tool error", terminal.CorrelationID, "invocation-retention"); err != nil {
		t.Fatal(err)
	}
	approval := approvals.Approval{ID: "approval-retention", TaskID: terminal.ID, ToolName: "shell",
		ToolArguments: json.RawMessage(`{"secret":"approval arguments"}`), RequestedScopes: []string{"tool.shell.execute"},
		Risk: coretools.RiskStateChanging, Reason: "private approval reason", Status: approvals.StatusConsumed,
		RequestedAt: old, ExpiresAt: recent, CorrelationID: terminal.CorrelationID}
	if err := store.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	deliveredAt := old
	if _, err := store.CreateDelivery(ctx, delivery.Delivery{ID: "delivery-retention", TaskID: terminal.ID,
		Target: delivery.Target{Kind: "slack", ConnectorID: "private-connector", ChannelID: "private-channel"},
		Status: delivery.StatusDelivered, LastError: "private delivery error", IdempotencyKey: "delivery-retention",
		CreatedAt: old, DeliveredAt: &deliveredAt, CorrelationID: terminal.CorrelationID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO artifacts(id, task_id, name, media_type,
uri, description, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)`, "artifact-retention", terminal.ID,
		"private artifact", "text/plain", "/outside/unmanaged.txt", "private description", formatTime(old)); err != nil {
		t.Fatal(err)
	}

	addRetentionAudit(t, store, "audit-old-system", old, "", "old.system")
	addRetentionAudit(t, store, "audit-old-terminal", old, terminal.ID, "old.terminal")
	addRetentionAudit(t, store, "audit-old-active", old, active.ID, "old.active")
	if err := store.AddAuditEntry(ctx, observability.AuditEntry{ID: "audit-old-active-correlation", OccurredAt: old,
		ActorKind: "test", ActorID: "retention", Action: "old.active.correlation", ResourceKind: "test",
		ResourceID: "active-policy", Decision: "ALLOW", Details: json.RawMessage(`{}`),
		CorrelationID: active.CorrelationID}); err != nil {
		t.Fatal(err)
	}
	addRetentionAudit(t, store, "audit-boundary", cutoff, "", "boundary")
	addRetentionAudit(t, store, "audit-recent", recent, "", "recent")

	result, err := store.ApplyRetentionSweep(ctx, cutoff, sweptAt)
	if err != nil {
		t.Fatal(err)
	}
	if result.TurnsDeleted != 2 || result.ConversationsScrubbed != 1 || result.EventsScrubbed != 1 ||
		result.TasksScrubbed != 1 || result.ToolInvocationsScrubbed != 1 || result.ApprovalsScrubbed != 1 ||
		result.DeliveriesScrubbed != 1 || result.ArtifactMetadataDeleted != 1 || result.AuditEntriesDeleted != 2 {
		t.Fatalf("unexpected retention counts: %+v", result)
	}

	staleConversation, err := store.GetConversation(ctx, "conversation-stale")
	if err != nil {
		t.Fatal(err)
	}
	if !staleConversation.TranscriptRetention || staleConversation.Title != expiredContentMarker {
		t.Fatalf("automatic expiry changed future retention or left title content: %+v", staleConversation)
	}
	rollingTurns, err := store.ListTurns(ctx, "conversation-rolling", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rollingTurns) != 2 || rollingTurns[0].Text != "boundary stays" || rollingTurns[1].Text != "recent stays" {
		t.Fatalf("strict rolling cutoff retained wrong turns: %+v", rollingTurns)
	}
	activeTurns, err := store.ListTurns(ctx, "conversation-active", 10)
	if err != nil || len(activeTurns) != 1 || activeTurns[0].Text != "active context stays" {
		t.Fatalf("active graph context was disrupted: %+v, %v", activeTurns, err)
	}

	storedTerminal, _ := store.GetTask(ctx, terminal.ID)
	if storedTerminal.Goal != expiredContentMarker || string(storedTerminal.Input) != "{}" ||
		len(storedTerminal.Result) != 0 || storedTerminal.UserFacingSummary != expiredContentMarker || len(storedTerminal.Artifacts) != 0 {
		t.Fatalf("terminal task content was not scrubbed: %+v", storedTerminal)
	}
	if _, err := store.RetryTask(ctx, terminal.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired task was retryable without its original content: %v", err)
	}
	storedActive, _ := store.GetTask(ctx, active.ID)
	if storedActive.Goal != active.Goal || string(storedActive.Input) != string(active.Input) {
		t.Fatalf("active task content changed: %+v", storedActive)
	}
	storedInvocation, err := store.GetToolInvocation(ctx, "invocation-retention")
	if err != nil || string(storedInvocation.Input) != "{}" || len(storedInvocation.Output) != 0 || storedInvocation.Error != "" {
		t.Fatalf("tool invocation retention result = %+v, %v", storedInvocation, err)
	}
	storedApproval, err := store.GetApproval(ctx, approval.ID)
	if err != nil || string(storedApproval.ToolArguments) != "{}" || storedApproval.Reason != expiredContentMarker {
		t.Fatalf("approval retention result = %+v, %v", storedApproval, err)
	}
	var deliveryTarget, deliveryError string
	if err := store.db.QueryRowContext(ctx, "SELECT target_json, last_error FROM deliveries WHERE id = ?", "delivery-retention").
		Scan(&deliveryTarget, &deliveryError); err != nil || deliveryTarget != "{}" || deliveryError != "" {
		t.Fatalf("delivery retention result = (%q, %q, %v)", deliveryTarget, deliveryError, err)
	}
	var artifactCount int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM artifacts WHERE task_id = ?", terminal.ID).Scan(&artifactCount); err != nil || artifactCount != 0 {
		t.Fatalf("artifact metadata count = %d, %v", artifactCount, err)
	}

	terminalStoredEvent, _ := store.GetEvent(ctx, terminalEvent.ID)
	activeStoredEvent, _ := store.GetEvent(ctx, activeEvent.ID)
	if string(terminalStoredEvent.Payload) != "{}" || terminalStoredEvent.Actor.DisplayName != "" ||
		string(activeStoredEvent.Payload) != string(activeEvent.Payload) {
		t.Fatalf("event retention terminal=%+v active=%+v", terminalStoredEvent, activeStoredEvent)
	}
	entries, err := store.ListAuditEntries(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	actions := make(map[string]bool)
	for _, entry := range entries {
		actions[entry.Action] = true
	}
	if actions["old.system"] || actions["old.terminal"] || !actions["old.active"] || !actions["old.active.correlation"] ||
		!actions["boundary"] || !actions["recent"] || !actions["retention.sweep"] {
		t.Fatalf("unexpected retained audit actions: %+v", actions)
	}

	restoredConversation, err := store.GetOrCreateConversation(ctx, staleConversation.ExternalKey,
		"Restored future title", true, "unused-replacement-id")
	if err != nil || restoredConversation.ID != staleConversation.ID || restoredConversation.Title != "Restored future title" ||
		!restoredConversation.TranscriptRetention {
		t.Fatalf("future conversation retention was not restored cleanly: %+v, %v", restoredConversation, err)
	}
	addRetentionTurn(t, store, "turn-stale-future", staleConversation.ID, "future content is retained", sweptAt.Add(time.Hour))
	futureTurns, err := store.ListTurns(ctx, staleConversation.ID, 10)
	if err != nil || len(futureTurns) != 1 || futureTurns[0].Text != "future content is retained" {
		t.Fatalf("future retention did not survive rolling expiry: %+v, %v", futureTurns, err)
	}
}

func TestRetentionSweepRollsBackIfSweepAuditCannotBeWritten(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, time.April, 10, 0, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour)
	insertRetentionConversation(t, store, "conversation-audit-rollback", "retention:audit-rollback", "rollback title", old)
	addRetentionTurn(t, store, "turn-audit-rollback", "conversation-audit-rollback", "rollback secret", old)
	addRetentionAudit(t, store, "audit-rollback-old", old, "", "old.rollback")
	rejectAuditAction(t, store, "reject_retention_sweep_audit", "retention.sweep")

	if _, err := store.ApplyRetentionSweep(ctx, cutoff, cutoff.Add(time.Hour)); err == nil {
		t.Fatal("retention sweep committed without its mandatory audit fact")
	}
	storedConversation, err := store.GetConversation(ctx, "conversation-audit-rollback")
	if err != nil || storedConversation.Title != "rollback title" {
		t.Fatalf("conversation changed despite rolled-back sweep: %+v, %v", storedConversation, err)
	}
	turns, err := store.ListTurns(ctx, storedConversation.ID, 10)
	if err != nil || len(turns) != 1 || turns[0].Text != "rollback secret" {
		t.Fatalf("turns changed despite rolled-back sweep: %+v, %v", turns, err)
	}
	entries, err := store.ListAuditEntries(ctx, 10)
	if err != nil || len(entries) != 1 || entries[0].Action != "old.rollback" {
		t.Fatalf("old audit was not restored by rollback: %+v, %v", entries, err)
	}
}

func TestRetentionSweepDefersTerminalGraphWithPendingDelivery(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, time.May, 10, 0, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour)
	insertRetentionConversation(t, store, "conversation-pending-delivery", "retention:pending-delivery", "pending title", old)
	addRetentionTurn(t, store, "turn-pending-delivery", "conversation-pending-delivery", "needed for pending delivery", old)
	task := testRootTask("task-pending-delivery", "conversation-pending-delivery", "event-pending-delivery", old)
	task.Status = tasks.StatusCompleted
	task.FinishedAt = &old
	task.Goal = "pending delivery goal"
	if _, _, err := store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDelivery(ctx, delivery.Delivery{ID: "delivery-pending-retention", TaskID: task.ID,
		Target: delivery.Target{Kind: "slack", ConnectorID: "connector", ChannelID: "channel"},
		Status: delivery.StatusPending, IdempotencyKey: "delivery-pending-retention", CreatedAt: old,
		CorrelationID: task.CorrelationID}); err != nil {
		t.Fatal(err)
	}
	addRetentionAudit(t, store, "audit-pending-delivery", old, task.ID, "pending.delivery")

	result, err := store.ApplyRetentionSweep(ctx, cutoff, cutoff.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if result.TurnsDeleted != 0 || result.ConversationsScrubbed != 0 || result.TasksScrubbed != 0 ||
		result.DeliveriesScrubbed != 0 || result.AuditEntriesDeleted != 0 {
		t.Fatalf("pending delivery was not deferred: %+v", result)
	}
	storedTask, _ := store.GetTask(ctx, task.ID)
	turns, _ := store.ListTurns(ctx, "conversation-pending-delivery", 10)
	entries, _ := store.ListAuditEntries(ctx, 10)
	if storedTask.Goal != task.Goal || len(turns) != 1 || turns[0].Text != "needed for pending delivery" {
		t.Fatalf("pending delivery context changed: task=%+v turns=%+v", storedTask, turns)
	}
	found := false
	for _, entry := range entries {
		found = found || entry.Action == "pending.delivery"
	}
	if !found {
		t.Fatal("task-linked audit for pending delivery expired prematurely")
	}
}

func insertRetentionConversation(t *testing.T, store *Store, id, externalKey, title string, updatedAt time.Time) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO conversations(
id, external_key, title, transcript_retention, created_at, updated_at) VALUES(?, ?, ?, 1, ?, ?)`,
		id, externalKey, title, formatTime(updatedAt), formatTime(updatedAt)); err != nil {
		t.Fatal(err)
	}
}

func addRetentionTurn(t *testing.T, store *Store, id, conversationID, text string, createdAt time.Time) {
	t.Helper()
	if err := store.AddTurn(context.Background(), conversation.Turn{ID: id, ConversationID: conversationID,
		Role: conversation.RoleUser, Text: text, Final: true, CorrelationID: "correlation-" + id,
		CreatedAt: createdAt}, true); err != nil {
		t.Fatal(err)
	}
}

func addRetentionAudit(t *testing.T, store *Store, id string, occurredAt time.Time, taskID, action string) {
	t.Helper()
	if err := store.AddAuditEntry(context.Background(), observability.AuditEntry{ID: id, OccurredAt: occurredAt,
		ActorKind: "test", ActorID: "retention", Action: action, ResourceKind: "test", ResourceID: id,
		Decision: "ALLOW", Details: json.RawMessage(`{}`), CorrelationID: "correlation-" + id,
		TaskID: taskID}); err != nil {
		t.Fatal(err)
	}
}
