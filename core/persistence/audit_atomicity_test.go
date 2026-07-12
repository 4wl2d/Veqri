package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/veqri/veqri/core/approvals"
	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/delivery"
	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/core/tasks"
	coretools "github.com/veqri/veqri/core/tools"
	"github.com/veqri/veqri/internal/auth"
)

func rejectAuditAction(t *testing.T, store *Store, triggerName, action string) {
	t.Helper()
	if strings.ContainsAny(triggerName+action, "'\"") {
		t.Fatal("test trigger identifiers and actions must not contain quotes")
	}
	statement := "CREATE TRIGGER " + triggerName + " BEFORE INSERT ON audit_entries " +
		"WHEN NEW.action = '" + action + "' BEGIN SELECT RAISE(FAIL, 'injected audit failure'); END"
	if _, err := store.db.ExecContext(context.Background(), statement); err != nil {
		t.Fatalf("install audit failure trigger: %v", err)
	}
}

func TestApprovalRequestRollsBackWhenMandatoryAuditFails(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	rejectAuditAction(t, store, "reject_approval_request_audit", "approval.requested")

	task := approvalTask("audit-request")
	approval := approvals.Approval{
		ID: "approval:audit-request", ToolName: "shell",
		ToolArguments:   json.RawMessage(`{"command":"git","args":["status"]}`),
		RequestedScopes: []string{"tool.shell.execute"}, Risk: coretools.RiskStateChanging,
		Reason: "test", Status: approvals.StatusPending, RequestedAt: persistenceNow,
		ExpiresAt: persistenceFuture, CorrelationID: task.CorrelationID,
	}
	if _, _, _, err := store.CreateTaskWithApproval(ctx, task, &approval); err == nil {
		t.Fatal("approval task committed without its mandatory audit record")
	}
	for _, table := range []string{"tasks", "approvals", "audit_entries"} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s retained %d row(s) after audit failure", table, count)
		}
	}
}

func TestLegacyApprovalRepairRejectsMismatchedReplayArguments(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	persisted := approvalTask("legacy-repair-exact")
	persisted.Input = json.RawMessage(`{"command":"rm","args":["valuable-file"]}`)
	if _, duplicate, err := store.CreateTask(ctx, persisted); err != nil || duplicate {
		t.Fatalf("CreateTask() = duplicate %v, %v", duplicate, err)
	}

	replay := persisted
	replay.Input = json.RawMessage(`{"command":"git","args":["status"]}`)
	mismatched := approvals.Approval{
		ID: "approval:replay", ToolName: "shell", ToolArguments: replay.Input,
		RequestedScopes: []string{"tool.shell.execute"}, Risk: coretools.RiskStateChanging,
		Reason: "replayed request", Status: approvals.StatusPending,
		RequestedAt: persistenceNow, ExpiresAt: persistenceFuture,
		CorrelationID: "replay-correlation",
	}
	_, repaired, duplicate, err := store.CreateTaskWithApproval(ctx, replay, &mismatched)
	if !duplicate || repaired != nil || !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched replay = approval %+v, duplicate %v, error %v", repaired, duplicate, err)
	}
	if _, err := store.GetApprovalByTask(ctx, persisted.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mismatched replay created an approval: %v", err)
	}

	exact := mismatched
	exact.ToolArguments = append(json.RawMessage(nil), persisted.Input...)
	_, repaired, duplicate, err = store.CreateTaskWithApproval(ctx, persisted, &exact)
	if err != nil || !duplicate || repaired == nil {
		t.Fatalf("exact replay repair = approval %+v, duplicate %v, error %v", repaired, duplicate, err)
	}
	if string(repaired.ToolArguments) != string(persisted.Input) {
		t.Fatalf("repaired approval arguments = %s, want persisted task input %s", repaired.ToolArguments, persisted.Input)
	}
}

func TestApprovalDecisionRollsBackWhenMandatoryAuditFails(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	createApprovalFixture(t, store, "audit-decision", persistenceFuture)
	rejectAuditAction(t, store, "reject_approval_decision_audit", "approval.decide")

	if _, _, err := store.DecideApproval(ctx, "approval-audit-decision", "admin:operator", true); err == nil {
		t.Fatal("approval decision committed without its mandatory audit record")
	}
	approval, err := store.GetApproval(ctx, "approval-audit-decision")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(ctx, approval.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if approval.Status != approvals.StatusPending || task.Status != tasks.StatusWaitingForApproval {
		t.Fatalf("failed audit changed approval/task state: %s / %s", approval.Status, task.Status)
	}
}

func TestApprovalExpiryRollsBackWhenMandatoryAuditFails(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	createApprovalFixture(t, store, "audit-expiry", persistencePast)
	rejectAuditAction(t, store, "reject_approval_expiry_audit", "approval.decide")

	if _, err := store.ExpireApprovals(ctx); err == nil {
		t.Fatal("approval expiry committed without its mandatory audit record")
	}
	approval, err := store.GetApproval(ctx, "approval-audit-expiry")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(ctx, approval.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if approval.Status != approvals.StatusPending || task.Status != tasks.StatusWaitingForApproval {
		t.Fatalf("failed expiry audit changed approval/task state: %s / %s", approval.Status, task.Status)
	}
}

func TestToolStartAndFinishRequireTransactionalAudit(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		ctx := context.Background()
		store := openTestStore(t)
		task := auditShellTask("tool-start-audit")
		if _, _, err := store.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		rejectAuditAction(t, store, "reject_tool_start_audit", "tool.started")
		invocation := auditInvocation(task, "invocation-start", "start-secret")
		if _, _, err := store.StartToolInvocation(ctx, invocation); err == nil {
			t.Fatal("tool invocation became executable without a start audit")
		}
		if _, err := store.GetToolInvocation(ctx, invocation.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("tool invocation survived failed start audit: %v", err)
		}
	})

	t.Run("finish", func(t *testing.T) {
		ctx := context.Background()
		store := openTestStore(t)
		task := auditShellTask("tool-finish-audit")
		if _, _, err := store.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		invocation := auditInvocation(task, "invocation-finish", "TOPSECRET-ARGUMENT")
		if _, duplicate, err := store.StartToolInvocation(ctx, invocation); err != nil || duplicate {
			t.Fatalf("StartToolInvocation() = duplicate %v, %v", duplicate, err)
		}
		rejectAuditAction(t, store, "reject_tool_finish_audit", "tool.finished")
		output := json.RawMessage(`{"exit_code":0,"stdout":"TOPSECRET-OUTPUT"}`)
		if _, err := store.FinishToolInvocation(ctx, invocation.ID, output, 0, nil); err == nil {
			t.Fatal("tool outcome committed without its finish audit")
		}
		stored, err := store.GetToolInvocation(ctx, invocation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != "STARTED" || len(stored.Output) != 0 || stored.FinishedAt != nil {
			t.Fatalf("failed finish audit did not preserve uncertain STARTED state: %+v", stored)
		}
		entries, err := store.ListAuditEntries(ctx, 20)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(entries)
		if strings.Contains(string(encoded), "TOPSECRET") {
			t.Fatalf("command secret leaked into audit entries: %s", encoded)
		}
	})
}

func TestCompletionDeliveryAndFailureStateRequireAudit(t *testing.T) {
	t.Run("completion and delivery", func(t *testing.T) {
		ctx := context.Background()
		store := openTestStore(t)
		now := time.Now().UTC()
		conversationRecord, err := store.GetOrCreateConversation(ctx, "audit:delivery", "Audit", true, "conversation-audit-delivery")
		if err != nil {
			t.Fatal(err)
		}
		task := testRootTask("task-audit-delivery", conversationRecord.ID, "event-audit-delivery", now)
		task.Status = tasks.StatusRunning
		if _, _, err := store.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		rejectAuditAction(t, store, "reject_delivery_audit", "delivery.created")
		turn := conversation.Turn{ID: "turn:audit-delivery", ConversationID: conversationRecord.ID,
			Role: conversation.RoleAssistant, Text: "answer", Final: true,
			CorrelationID: task.CorrelationID, CreatedAt: now}
		item := delivery.Delivery{ID: "delivery-audit", TaskID: task.ID,
			Target: delivery.Target{Kind: "slack", ConnectorID: "slack-live", ChannelID: "channel"},
			Status: delivery.StatusPending, IdempotencyKey: "delivery-audit", CreatedAt: now,
			CorrelationID: task.CorrelationID}
		_, err = store.CompleteTaskWithOutcome(ctx, task.ID, json.RawMessage(`{"ok":true}`), "answer", false,
			CompletionOutcome{Turn: &turn, Delivery: &item})
		if err == nil {
			t.Fatal("completion and delivery committed without the delivery audit")
		}
		stored, _ := store.GetTask(ctx, task.ID)
		if stored.Status != tasks.StatusRunning {
			t.Fatalf("task status = %s after rolled-back delivery audit", stored.Status)
		}
		turns, err := store.ListTurns(ctx, conversationRecord.ID, 10)
		if err != nil || len(turns) != 0 {
			t.Fatalf("completion turn survived audit rollback: %+v, %v", turns, err)
		}
		var deliveries, audits int
		_ = store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM deliveries WHERE task_id = ?", task.ID).Scan(&deliveries)
		_ = store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_entries WHERE task_id = ?", task.ID).Scan(&audits)
		if deliveries != 0 || audits != 0 {
			t.Fatalf("rolled-back completion retained deliveries=%d audits=%d", deliveries, audits)
		}
	})

	t.Run("failure", func(t *testing.T) {
		ctx := context.Background()
		store := openTestStore(t)
		task := testRootTask("task-audit-failure", "", "event-audit-failure", time.Now().UTC())
		task.Status = tasks.StatusRunning
		if _, _, err := store.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		rejectAuditAction(t, store, "reject_task_failure_audit", "agent.failed")
		if _, err := store.FailTask(ctx, task.ID, errors.New("private failure"), false); err == nil {
			t.Fatal("task failure committed without its mandatory audit")
		}
		stored, _ := store.GetTask(ctx, task.ID)
		if stored.Status != tasks.StatusRunning || stored.Error != "" {
			t.Fatalf("failed audit mutated task: %+v", stored)
		}
	})
}

func TestSecuritySettingRollsBackWhenAuditFails(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.SetSetting(ctx, "emergency_stop", false); err != nil {
		t.Fatal(err)
	}
	rejectAuditAction(t, store, "reject_setting_audit", "core.emergency_stop.set")
	entry := observability.AuditEntry{ID: "audit-setting", OccurredAt: time.Now().UTC(),
		ActorKind: "admin", ActorID: "operator", Action: "core.emergency_stop.set",
		ResourceKind: "core", ResourceID: "local-core", Decision: "ALLOW",
		Details: json.RawMessage(`{"enabled":true}`), CorrelationID: "setting-audit"}
	if err := store.SetSettingWithAudit(ctx, "emergency_stop", true, entry); err == nil {
		t.Fatal("security setting committed without its audit record")
	}
	var enabled bool
	if err := store.GetSetting(ctx, "emergency_stop", &enabled); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("emergency stop setting changed despite audit failure")
	}
}

func TestDevicePairAndRevokeRollbackWhenMandatoryAuditFails(t *testing.T) {
	t.Run("pair", func(t *testing.T) {
		ctx := context.Background()
		store := openTestStore(t)
		codeHash := auth.HashPairingCode("pairing-secret", "87654321")
		if err := store.CreatePairingSession(ctx, "pairing-audit", codeHash, persistenceFuture); err != nil {
			t.Fatal(err)
		}
		rejectAuditAction(t, store, "reject_device_pair_audit", "device.paired")
		entry := observability.AuditEntry{ID: "audit-device-pair", OccurredAt: time.Now().UTC(),
			ActorKind: "device", ActorID: "device-audit-pair", Action: "device.paired",
			ResourceKind: "device", ResourceID: "device-audit-pair", Decision: "ALLOW",
			Details: json.RawMessage(`{"credential_stored":"hash-only"}`), CorrelationID: "pair-audit"}
		err := store.ClaimPairingSessionWithAudit(ctx, codeHash, Device{
			ID: "device-audit-pair", Name: "Audit phone", Platform: "android", Capabilities: `{}`,
		}, "pair-audit-credential", false, entry)
		if err == nil {
			t.Fatal("device pairing committed without its mandatory audit")
		}
		var consumed sql.NullString
		if err := store.db.QueryRowContext(ctx, "SELECT consumed_at FROM pairing_sessions WHERE id = ?", "pairing-audit").Scan(&consumed); err != nil {
			t.Fatal(err)
		}
		if consumed.Valid {
			t.Fatal("audit failure consumed the one-time pairing code")
		}
		var devices, settings int
		_ = store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM devices WHERE id = ?", "device-audit-pair").Scan(&devices)
		_ = store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settings WHERE key = ?", "device:device-audit-pair:privacy").Scan(&settings)
		if devices != 0 || settings != 0 {
			t.Fatalf("audit failure retained devices=%d settings=%d", devices, settings)
		}
	})

	t.Run("revoke", func(t *testing.T) {
		ctx := context.Background()
		store := openTestStore(t)
		const credential = "device-revoke-audit-credential"
		pairCredentialRotationDevice(t, store, "device-audit-revoke", credential)
		rejectAuditAction(t, store, "reject_device_revoke_audit", "device.revoked")
		entry := observability.AuditEntry{ID: "audit-device-revoke", OccurredAt: time.Now().UTC(),
			ActorKind: "admin", ActorID: "local-admin", Action: "device.revoked",
			ResourceKind: "device", ResourceID: "device-audit-revoke", Decision: "ALLOW",
			Details: json.RawMessage(`{"credential":"revoked"}`), CorrelationID: "revoke-audit"}
		if err := store.RevokeDeviceWithAudit(ctx, "device-audit-revoke", entry); err == nil {
			t.Fatal("device revocation committed without its mandatory audit")
		}
		if deviceID, err := store.VerifyDeviceCredential(ctx, credential); err != nil || deviceID != "device-audit-revoke" {
			t.Fatalf("audit failure revoked credential: device=%q error=%v", deviceID, err)
		}
	})
}

func TestTaskFailureTextIsBoundedAndValid(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	task := auditShellTask("bounded-task-error")
	if _, _, err := store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	oversized := strings.Repeat("external-agent-error-🚀", 1000)
	failed, err := store.FailTask(ctx, task.ID, errors.New(oversized), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed.Error) > maxPersistedTaskErrorBytes || !utf8.ValidString(failed.Error) || !strings.HasSuffix(failed.Error, "…") {
		t.Fatalf("bounded task error bytes=%d suffix=%q", len(failed.Error), failed.Error[len(failed.Error)-3:])
	}
}

func auditShellTask(id string) tasks.Task {
	return tasks.Task{ID: id, RootTaskID: id, Goal: "shell", TaskType: "shell",
		Input: json.RawMessage(`{}`), AssignedAgentID: "builtin.shell", AllowedTools: []string{"shell"},
		ApprovalPolicy: "test", Status: tasks.StatusRunning, CreatedAt: time.Now().UTC(),
		TimeoutSeconds: 30, Artifacts: []tasks.Artifact{}, CorrelationID: "correlation-" + id,
		IdempotencyKey: "idempotency-" + id, Version: 1}
}

func auditInvocation(task tasks.Task, id, secret string) coretools.Invocation {
	return coretools.Invocation{ID: id, TaskID: task.ID, ToolName: "shell",
		Input: json.RawMessage(`{"command":"echo","args":["` + secret + `"]}`),
		Risk:  coretools.RiskStateChanging, CorrelationID: task.CorrelationID,
		IdempotencyKey: "idempotency-" + id}
}
