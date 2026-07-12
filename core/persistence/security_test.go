package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/veqri/veqri/core/approvals"
	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/core/tools"
	"github.com/veqri/veqri/internal/auth"
)

var (
	persistencePast   = time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)
	persistenceNow    = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	persistenceFuture = time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC)
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "veqri.db")
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

func TestMigrationsAreCompleteIdempotentAndIntegrityChecked(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.IntegrityCheck(ctx); err != nil {
		t.Fatalf("IntegrityCheck(): %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate(): %v", err)
	}

	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	var expectedVersions int
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".sql" {
			expectedVersions++
		}
	}
	var applied int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&applied); err != nil {
		t.Fatalf("count schema migrations: %v", err)
	}
	if applied != expectedVersions {
		t.Fatalf("applied migrations = %d, embedded migrations = %d", applied, expectedVersions)
	}

	wantTables := []string{
		"approvals", "devices", "events", "pairing_sessions", "schema_migrations",
		"task_dependencies", "tasks", "tool_invocations", "voice_sessions", "webhook_replay_nonces",
	}
	rows, err := store.db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		t.Fatalf("list schema tables: %v", err)
	}
	defer rows.Close()
	gotTables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		gotTables[name] = true
	}
	for _, name := range wantTables {
		if !gotTables[name] {
			t.Errorf("required table %q is missing", name)
		}
	}

	var foreignKeys int
	if err := store.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign key pragma: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}
}

func testEvent(id, connector, instance, key string) events.Envelope {
	return events.Envelope{
		ID: id, Type: "message.received", Version: 1,
		Source:          events.Source{Kind: "test", ConnectorID: connector, InstanceID: instance},
		Actor:           events.Actor{ID: "actor-1"},
		OccurredAt:      persistenceNow,
		ReceivedAt:      persistenceNow.Add(time.Second),
		ConversationKey: "test:conversation",
		CorrelationID:   "correlation-1",
		IdempotencyKey:  key,
		TrustLevel:      events.TrustTrusted,
		ReplyTarget:     events.ReplyTarget{ConnectorID: connector, ChannelID: "channel-1"},
		Payload:         json.RawMessage(`{"text":"hello"}`),
	}
}

func TestEventIngestionIsSourceScopedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)

	original := testEvent("event-1", "connector-1", "instance-1", "key-1")
	id, duplicate, err := store.IngestEvent(ctx, original)
	if err != nil || duplicate || id != original.ID {
		t.Fatalf("first IngestEvent() = (%q, %v, %v)", id, duplicate, err)
	}

	retry := testEvent("event-2", "connector-1", "instance-1", "key-1")
	retry.Payload = json.RawMessage(`{"text":"mutated retry"}`)
	id, duplicate, err = store.IngestEvent(ctx, retry)
	if err != nil || !duplicate || id != original.ID {
		t.Fatalf("retry IngestEvent() = (%q, %v, %v), want original ID and duplicate", id, duplicate, err)
	}

	stored, err := store.GetEvent(ctx, original.ID)
	if err != nil {
		t.Fatalf("GetEvent(): %v", err)
	}
	if string(stored.Payload) != string(original.Payload) {
		t.Fatalf("duplicate overwrote payload: got %s, want %s", stored.Payload, original.Payload)
	}

	otherScope := testEvent("event-3", "connector-1", "instance-2", "key-1")
	id, duplicate, err = store.IngestEvent(ctx, otherScope)
	if err != nil || duplicate || id != otherScope.ID {
		t.Fatalf("other source scope IngestEvent() = (%q, %v, %v)", id, duplicate, err)
	}
}

func TestEventIngestionRejectsInvalidEnvelopeBeforePersistence(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	event := testEvent("event-invalid", "connector-1", "instance-1", "key-invalid")
	event.TrustLevel = "superuser"
	if _, _, err := store.IngestEvent(ctx, event); err == nil {
		t.Fatal("invalid event was persisted")
	}
	if _, err := store.GetEvent(ctx, event.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetEvent(invalid) error = %v, want ErrNotFound", err)
	}
}

func approvalTask(id string) tasks.Task {
	return tasks.Task{
		ID: id, RootTaskID: id, Goal: "test approval", TaskType: "shell",
		Input: json.RawMessage(`{}`), AllowedTools: []string{"shell"},
		ApprovalPolicy: "always", Status: tasks.StatusWaitingForApproval,
		CreatedAt: persistenceNow, TimeoutSeconds: 300, Artifacts: []tasks.Artifact{},
		CorrelationID: "correlation-" + id, IdempotencyKey: "idempotency-" + id,
	}
}

func createApprovalFixture(t *testing.T, store *Store, suffix string, expiresAt time.Time) {
	t.Helper()
	ctx := context.Background()
	task := approvalTask("task-" + suffix)
	if _, duplicate, err := store.CreateTask(ctx, task); err != nil || duplicate {
		t.Fatalf("CreateTask() = (duplicate %v, %v)", duplicate, err)
	}
	approval := approvals.Approval{
		ID: "approval-" + suffix, TaskID: task.ID, ToolName: "shell",
		ToolArguments:   json.RawMessage(`{"command":"git","args":["status"]}`),
		RequestedScopes: []string{"tool.shell.execute"}, Risk: tools.RiskStateChanging,
		Reason: "test", Status: approvals.StatusPending, RequestedAt: persistenceNow,
		ExpiresAt: expiresAt, CorrelationID: task.CorrelationID,
	}
	if err := store.CreateApproval(ctx, approval); err != nil {
		t.Fatalf("CreateApproval(): %v", err)
	}
}

func TestApprovalDecisionIsSingleUse(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	createApprovalFixture(t, store, "single-use", persistenceFuture)

	approval, task, err := store.DecideApproval(ctx, "approval-single-use", "admin-1", true)
	if err != nil {
		t.Fatalf("DecideApproval(approve): %v", err)
	}
	if approval.Status != approvals.StatusConsumed || approval.ConsumedAt == nil || approval.DecidedAt == nil {
		t.Fatalf("approved approval not atomically consumed: %+v", approval)
	}
	if approval.DecidedBy != "admin-1" || task.Status != tasks.StatusQueued {
		t.Fatalf("approval/task result = (%+v, %s)", approval, task.Status)
	}
	if _, _, err := store.DecideApproval(ctx, approval.ID, "admin-2", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("second DecideApproval() error = %v, want ErrConflict", err)
	}
	stored, err := store.GetApproval(ctx, approval.ID)
	if err != nil {
		t.Fatalf("GetApproval(): %v", err)
	}
	if stored.DecidedBy != "admin-1" || stored.Status != approvals.StatusConsumed {
		t.Fatalf("repeated decision changed approval: %+v", stored)
	}
}

func TestApprovalDenyCancelsTask(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	createApprovalFixture(t, store, "deny", persistenceFuture)
	approval, task, err := store.DecideApproval(ctx, "approval-deny", "admin-1", false)
	if err != nil {
		t.Fatalf("DecideApproval(deny): %v", err)
	}
	if approval.Status != approvals.StatusDenied || approval.ConsumedAt != nil {
		t.Fatalf("denied approval = %+v", approval)
	}
	if task.Status != tasks.StatusCancelled || task.FinishedAt == nil {
		t.Fatalf("denied task = %+v", task)
	}
}

func TestExpiredApprovalCannotBeConsumed(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	createApprovalFixture(t, store, "expired", persistencePast)
	if _, _, err := store.DecideApproval(ctx, "approval-expired", "admin-1", true); !errors.Is(err, ErrExpired) {
		t.Fatalf("DecideApproval(expired) error = %v, want ErrExpired", err)
	}
	approval, err := store.GetApproval(ctx, "approval-expired")
	if err != nil {
		t.Fatalf("GetApproval(): %v", err)
	}
	task, err := store.GetTask(ctx, "task-expired")
	if err != nil {
		t.Fatalf("GetTask(): %v", err)
	}
	if approval.Status != approvals.StatusExpired || task.Status != tasks.StatusTimedOut {
		t.Fatalf("expired approval/task states = (%s, %s)", approval.Status, task.Status)
	}
}

func TestPairingCredentialIsSingleUseAndRevocationIsImmediate(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	codeHash := auth.HashPairingCode("pairing-secret", "123456")
	if err := store.CreatePairingSession(ctx, "pairing-1", codeHash, persistenceFuture); err != nil {
		t.Fatalf("CreatePairingSession(): %v", err)
	}
	device := Device{ID: "device-1", Name: "test device", Platform: "android", Capabilities: `{}`}
	credential := "device-credential-with-sufficient-entropy"
	if err := store.ClaimPairingSession(ctx, codeHash, device, credential); err != nil {
		t.Fatalf("ClaimPairingSession(): %v", err)
	}
	if err := store.ClaimPairingSession(ctx, codeHash, Device{ID: "device-2", Capabilities: `{}`}, "other"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second ClaimPairingSession() error = %v, want ErrConflict", err)
	}
	if id, err := store.VerifyDeviceCredential(ctx, credential); err != nil || id != device.ID {
		t.Fatalf("VerifyDeviceCredential() = (%q, %v)", id, err)
	}
	if _, err := store.VerifyDeviceCredential(ctx, credential+"-wrong"); err == nil {
		t.Fatal("wrong device credential was accepted")
	}
	if err := store.RevokeDevice(ctx, device.ID); err != nil {
		t.Fatalf("RevokeDevice(): %v", err)
	}
	if _, err := store.VerifyDeviceCredential(ctx, credential); err == nil {
		t.Fatal("revoked device credential was accepted")
	}
	devices, err := store.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices(): %v", err)
	}
	if len(devices) != 1 || devices[0].RevokedAt == nil {
		t.Fatalf("revoked device not recorded: %+v", devices)
	}
}

func TestExpiredPairingSessionCannotBeClaimed(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	hash := auth.HashPairingCode("pairing-secret", "expired")
	if err := store.CreatePairingSession(ctx, "pairing-expired", hash, persistencePast); err != nil {
		t.Fatalf("CreatePairingSession(): %v", err)
	}
	err := store.ClaimPairingSession(ctx, hash, Device{ID: "device-expired", Capabilities: `{}`}, "credential")
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("ClaimPairingSession(expired) error = %v, want ErrExpired", err)
	}
}

func TestWebhookNonceReplayIsScopedToConnector(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const nonce = "0123456789abcdef"
	duplicate, err := store.UseWebhookNonce(ctx, "connector-a", nonce, persistenceNow)
	if err != nil || duplicate {
		t.Fatalf("first UseWebhookNonce() = (%v, %v)", duplicate, err)
	}
	duplicate, err = store.UseWebhookNonce(ctx, "connector-a", nonce, persistenceNow.Add(time.Second))
	if err != nil || !duplicate {
		t.Fatalf("replay UseWebhookNonce() = (%v, %v), want duplicate", duplicate, err)
	}
	duplicate, err = store.UseWebhookNonce(ctx, "connector-b", nonce, persistenceNow.Add(time.Second))
	if err != nil || duplicate {
		t.Fatalf("other connector UseWebhookNonce() = (%v, %v)", duplicate, err)
	}
}

func TestMigrationVersionsAreStrictlyOrderedAndUnique(t *testing.T) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		version := entry.Name()
		if index := len("0000"); len(version) > index {
			version = version[:index]
		}
		if seen[version] {
			t.Fatalf("duplicate migration version %q", version)
		}
		seen[version] = true
		names = append(names, entry.Name())
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	for index := range names {
		if names[index] != sorted[index] {
			t.Fatalf("embedded migration order = %v, want lexical order %v", names, sorted)
		}
	}
}

func TestMigrationChecksumAndDowngradeProtection(t *testing.T) {
	ctx := context.Background()
	t.Run("checksum mismatch", func(t *testing.T) {
		store := openTestStore(t)
		if _, err := store.db.ExecContext(ctx, "UPDATE schema_migrations SET checksum = 'tampered'"); err != nil {
			t.Fatal(err)
		}
		if err := store.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatalf("Migrate() error = %v, want checksum mismatch", err)
		}
	})
	t.Run("newer database", func(t *testing.T) {
		store := openTestStore(t)
		if _, err := store.db.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at, checksum)
VALUES(9999, ?, 'future')`, formatTime(time.Now().UTC())); err != nil {
			t.Fatal(err)
		}
		if err := store.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "newer than") {
			t.Fatalf("Migrate() error = %v, want newer schema rejection", err)
		}
	})
}
