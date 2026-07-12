package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/veqri/veqri/core/approvals"
	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/core/tasks"
	coretools "github.com/veqri/veqri/core/tools"
)

func TestCreateAskWorkExplicitDisableRunsCanonicalScrub(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now().UTC()
	conversationRecord, err := store.GetOrCreateConversation(ctx, "ask:private", "Private title", true, "conversation-private")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddTurn(ctx, conversation.Turn{
		ID: "old-turn", ConversationID: conversationRecord.ID, Role: conversation.RoleUser,
		Text: "old private turn", Final: true, CorrelationID: "old-correlation", CreatedAt: now,
	}, true); err != nil {
		t.Fatal(err)
	}
	oldEvent := testEvent("old-event", "test", "instance", "old-event")
	oldEvent.ConversationKey = conversationRecord.ExternalKey
	oldEvent.Payload = json.RawMessage(`{"text":"old private event"}`)
	if _, _, err := store.IngestEvent(ctx, oldEvent); err != nil {
		t.Fatal(err)
	}
	oldTask := testRootTask("old-task", conversationRecord.ID, oldEvent.ID, now)
	oldTask.Status = tasks.StatusCompleted
	oldTask.Progress = 100
	oldTask.FinishedAt = &now
	oldTask.Goal = "old private goal"
	oldTask.Input = json.RawMessage(`{"text":"old private input"}`)
	oldTask.Result = json.RawMessage(`{"text":"old private result"}`)
	oldTask.UserFacingSummary = "old private summary"
	if _, _, err := store.CreateTask(ctx, oldTask); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO tool_invocations(id, task_id, tool_name,
input_json, risk, status, started_at, finished_at, output_json, error, correlation_id, idempotency_key)
VALUES(?, ?, 'shell', ?, 'STATE_CHANGING', 'COMPLETED', ?, ?, ?, ?, ?, ?)`,
		"old-invocation", oldTask.ID, `{"command":"private"}`, formatTime(now), formatTime(now),
		`{"output":"private"}`, "private tool error", oldTask.CorrelationID, "old-invocation"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO approvals(id, task_id, tool_name,
tool_arguments_json, requested_scopes_json, risk, reason, status, requested_at, expires_at,
decided_at, decided_by, consumed_at, correlation_id) VALUES(?, ?, 'shell', ?, ?, 'STATE_CHANGING',
?, 'CONSUMED', ?, ?, ?, 'device', ?, ?)`, "old-approval", oldTask.ID,
		`{"command":"private"}`, `["tool.private"]`, "private approval reason", formatTime(now),
		formatTime(now.Add(time.Minute)), formatTime(now), formatTime(now), oldTask.CorrelationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO deliveries(id, task_id, target_json, priority,
status, attempt_count, last_error, idempotency_key, created_at, delivered_at, correlation_id)
VALUES(?, ?, ?, 1, 'DELIVERED', 1, ?, ?, ?, ?, ?)`, "old-delivery", oldTask.ID,
		`{"callback_url":"https://private.invalid"}`, "private delivery error", "old-delivery",
		formatTime(now), formatTime(now), oldTask.CorrelationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO artifacts(id, task_id, name, media_type, uri,
description, created_at) VALUES(?, ?, ?, 'text/plain', ?, ?, ?)`, "old-artifact", oldTask.ID,
		"private artifact", "file:///private", "private artifact description", formatTime(now)); err != nil {
		t.Fatal(err)
	}

	newEvent := testEvent("new-event", "test", "instance", "new-event")
	newEvent.ConversationKey = conversationRecord.ExternalKey
	newEvent.Payload = json.RawMessage(`{"retention":"disabled"}`)
	newConversation := conversation.Conversation{
		ID: "ignored-new-id", ExternalKey: conversationRecord.ExternalKey,
		Title: "[transcript retention disabled]", TranscriptRetention: false,
		CreatedAt: now, UpdatedAt: now.Add(time.Second),
	}
	newTask := testRootTask("new-task", newConversation.ID, newEvent.ID, now.Add(time.Second))
	actual, _, _, err := store.CreateAskWork(ctx, AskWork{
		Event: newEvent, Conversation: newConversation, ApplyRetention: true,
		Turn: conversation.Turn{ID: "turn:user:" + newEvent.ID, Role: conversation.RoleUser,
			Text: "new private request", Final: true, CorrelationID: newEvent.CorrelationID, CreatedAt: now},
		Tasks: []tasks.Task{newTask},
	})
	if err != nil {
		t.Fatal(err)
	}
	if actual.TranscriptRetention || actual.Title != "[transcript retention disabled]" {
		t.Fatalf("conversation policy was not scrubbed canonically: %+v", actual)
	}
	turns, err := store.ListTurns(ctx, actual.ID, 10)
	if err != nil || len(turns) != 1 || turns[0].Text != "[transcript retention disabled]" {
		t.Fatalf("turns after explicit disable = %+v, %v", turns, err)
	}
	storedOldEvent, err := store.GetEvent(ctx, oldEvent.ID)
	if err != nil || string(storedOldEvent.Payload) != "{}" {
		t.Fatalf("old event payload after explicit disable = %s, %v", storedOldEvent.Payload, err)
	}
	storedOldTask, err := store.GetTask(ctx, oldTask.ID)
	if err != nil || storedOldTask.Goal != "[transcript retention disabled]" ||
		string(storedOldTask.Input) != "{}" || len(storedOldTask.Result) != 0 {
		t.Fatalf("old terminal task after explicit disable = %+v, %v", storedOldTask, err)
	}
	var invocationInput string
	var invocationOutput *string
	var invocationError string
	if err := store.db.QueryRowContext(ctx, `SELECT input_json, output_json, error FROM tool_invocations WHERE id = ?`,
		"old-invocation").Scan(&invocationInput, &invocationOutput, &invocationError); err != nil ||
		invocationInput != "{}" || invocationOutput != nil || invocationError != "" {
		t.Fatalf("terminal invocation was not scrubbed: (%q, %#v, %q, %v)", invocationInput, invocationOutput, invocationError, err)
	}
	var approvalArguments, approvalScopes, approvalReason string
	if err := store.db.QueryRowContext(ctx, `SELECT tool_arguments_json, requested_scopes_json, reason
FROM approvals WHERE id = ?`, "old-approval").Scan(&approvalArguments, &approvalScopes, &approvalReason); err != nil ||
		approvalArguments != "{}" || approvalScopes != "[]" || approvalReason != "[transcript retention disabled]" {
		t.Fatalf("terminal approval was not scrubbed: (%q, %q, %q, %v)", approvalArguments, approvalScopes, approvalReason, err)
	}
	var deliveryTarget, deliveryError string
	if err := store.db.QueryRowContext(ctx, `SELECT target_json, last_error FROM deliveries WHERE id = ?`,
		"old-delivery").Scan(&deliveryTarget, &deliveryError); err != nil || deliveryTarget != "{}" || deliveryError != "" {
		t.Fatalf("terminal delivery was not scrubbed: (%q, %q, %v)", deliveryTarget, deliveryError, err)
	}
	var artifacts int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE task_id = ?`, oldTask.ID).
		Scan(&artifacts); err != nil || artifacts != 0 {
		t.Fatalf("terminal artifact metadata count = %d, %v", artifacts, err)
	}
}

func TestDeviceRetentionSettingScrubAndAuditRollbackTogether(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now().UTC()
	conversationRecord, err := store.GetOrCreateConversation(ctx, "android:device-private:default",
		"Private title", true, "device-conversation")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddTurn(ctx, conversation.Turn{
		ID: "private-turn", ConversationID: conversationRecord.ID, Role: conversation.RoleUser,
		Text: "private", Final: true, CorrelationID: "private", CreatedAt: now,
	}, true); err != nil {
		t.Fatal(err)
	}
	rejectAuditAction(t, store, "reject_device_retention_audit",
		"device.command.conversation.set_transcript_retention")
	err = store.SetDeviceTranscriptRetention(ctx, "device-private", conversationRecord.ID, false,
		observability.AuditEntry{
			ID: "retention-audit", OccurredAt: now, ActorKind: "device", ActorID: "device-private",
			Action: "device.command.conversation.set_transcript_retention", ResourceKind: "conversation",
			ResourceID: conversationRecord.ID, Decision: "ALLOW", Details: json.RawMessage(`{"enabled":false}`),
			CorrelationID: "retention-command", ConversationID: conversationRecord.ID,
		})
	if err == nil {
		t.Fatal("retention update committed without its mandatory audit")
	}
	stored, err := store.GetConversation(ctx, conversationRecord.ID)
	if err != nil || !stored.TranscriptRetention || stored.Title != "Private title" {
		t.Fatalf("conversation changed despite audit rollback: %+v, %v", stored, err)
	}
	turns, err := store.ListTurns(ctx, conversationRecord.ID, 10)
	if err != nil || len(turns) != 1 || turns[0].Text != "private" {
		t.Fatalf("turn scrub escaped rollback: %+v, %v", turns, err)
	}
	var privacy map[string]bool
	if err := store.GetSetting(ctx, "device:device-private:privacy", &privacy); !errors.Is(err, ErrNotFound) {
		t.Fatalf("device privacy setting escaped rollback: %#v, %v", privacy, err)
	}
}

func TestExpiredApprovalDecisionScrubsTerminalNonRetainedGraph(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now().UTC()
	conversationRecord, err := store.GetOrCreateConversation(ctx, "approval:private", "Private", false,
		"approval-private-conversation")
	if err != nil {
		t.Fatal(err)
	}
	task := testRootTask("approval-private-task", conversationRecord.ID, "approval-private-event", now)
	task.Status = tasks.StatusWaitingForApproval
	task.Goal = "private approval goal"
	if _, _, err := store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	approval := approvals.Approval{
		ID: "approval-private", TaskID: task.ID, ToolName: "shell",
		ToolArguments:   json.RawMessage(`{"command":"git","args":["status"]}`),
		RequestedScopes: []string{"tool.shell.execute"}, Risk: coretools.RiskStateChanging,
		Reason: "test", Status: approvals.StatusPending, RequestedAt: now.Add(-time.Hour),
		ExpiresAt: now.Add(-time.Minute), CorrelationID: task.CorrelationID,
	}
	if err := store.CreateApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.DecideApproval(ctx, approval.ID, "admin", true); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired decision error = %v, want ErrExpired", err)
	}
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil || stored.Status != tasks.StatusTimedOut || stored.Goal != "[transcript retention disabled]" {
		t.Fatalf("expired non-retained task = %+v, %v", stored, err)
	}
}

func TestCancellationTerminalizationScrubsNonRetainedGraph(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now().UTC()
	conversationRecord, err := store.GetOrCreateConversation(ctx, "cancel:private", "Private", false,
		"cancel-private-conversation")
	if err != nil {
		t.Fatal(err)
	}
	task := testRootTask("cancel-private-task", conversationRecord.ID, "cancel-private-event", now)
	task.Status = tasks.StatusRunning
	task.Goal = "private cancellation goal"
	if _, _, err := store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	requested, _, err := store.RequestTaskGraphCancellation(ctx, task.ID)
	if err != nil || requested.Status != tasks.StatusCancelRequested {
		t.Fatalf("request cancellation = %+v, %v", requested, err)
	}
	if _, err := store.MarkTaskCancelled(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil || stored.Status != tasks.StatusCancelled || stored.Goal != "[transcript retention disabled]" {
		t.Fatalf("cancelled non-retained task = %+v, %v", stored, err)
	}
}

func TestRecoveryScrubsEveryTerminalNonRetainedGraph(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now().UTC()
	conversationRecord, err := store.GetOrCreateConversation(ctx, "recovery:private", "Private", false,
		"recovery-private-conversation")
	if err != nil {
		t.Fatal(err)
	}
	task := testRootTask("recovery-private-task", conversationRecord.ID, "recovery-private-event", now)
	task.Status = tasks.StatusRunning
	task.RetryCount = task.MaxRetries
	task.Goal = "private recovered goal"
	if _, _, err := store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecoverInterruptedTasks(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil || stored.Status != tasks.StatusFailed || stored.Goal != "[transcript retention disabled]" {
		t.Fatalf("recovered non-retained task = %+v, %v", stored, err)
	}
}

func TestRecoveryTerminalizesAndScrubsCrashedNonRetainedShellInvocation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "veqri.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if store != nil {
			_ = store.Close()
		}
	})

	now := time.Now().UTC()
	conversationRecord, err := store.GetOrCreateConversation(ctx, "recovery:shell-private",
		"Private shell conversation", false, "recovery-shell-private-conversation")
	if err != nil {
		t.Fatal(err)
	}
	task := testRootTask("recovery-shell-private-task", conversationRecord.ID,
		"recovery-shell-private-event", now)
	task.Status = tasks.StatusRunning
	task.TaskType = "shell"
	task.AllowedTools = []string{"shell"}
	task.Goal = "private shell recovery goal"
	task.Input = json.RawMessage(`{"command":"echo","args":["private-recovery-secret"]}`)
	if _, _, err := store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	invocation := coretools.Invocation{
		ID: "recovery-shell-private-invocation", TaskID: task.ID, ToolName: "shell",
		Input: task.Input, Risk: coretools.RiskStateChanging, CorrelationID: task.CorrelationID,
		IdempotencyKey: "recovery-shell-private-invocation",
	}
	if _, duplicate, err := store.StartToolInvocation(ctx, invocation); err != nil || duplicate {
		t.Fatalf("start private invocation: duplicate=%v err=%v", duplicate, err)
	}

	// Closing without finishing the invocation models a daemon crash after the
	// durable STARTED boundary and before the shell outcome was persisted.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = nil
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}

	recovered, err := store.RecoverInterruptedTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered task count = %d, want 1", recovered)
	}

	storedTask, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedTask.Status != tasks.StatusFailed || storedTask.FinishedAt == nil ||
		storedTask.Goal != "[transcript retention disabled]" || string(storedTask.Input) != "{}" ||
		storedTask.ProgressMessage != "" || storedTask.Error != "" || len(storedTask.Result) != 0 ||
		storedTask.UserFacingSummary != "[transcript retention disabled]" || len(storedTask.Artifacts) != 0 {
		t.Fatalf("recovered private shell task was not terminal and scrubbed exactly: %+v", storedTask)
	}
	storedInvocation, err := store.GetToolInvocation(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedInvocation.Status != "FAILED" || storedInvocation.FinishedAt == nil ||
		string(storedInvocation.Input) != "{}" || len(storedInvocation.Output) != 0 ||
		storedInvocation.Error != "" || storedInvocation.ExitCode != nil {
		t.Fatalf("recovered private invocation was not terminal and scrubbed exactly: %+v", storedInvocation)
	}
}
