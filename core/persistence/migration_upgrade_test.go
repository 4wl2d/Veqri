package persistence

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestSchemaV1DuplicateVoiceSessionsUpgradeDeterministicallyAndIdempotently(t *testing.T) {
	ctx := context.Background()
	path := createSchemaV1DuplicateVoiceFixture(t)

	store, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("upgrade schema-v1 fixture: %v", err)
	}
	assertSchemaV1VoiceReconciliation(t, store)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("idempotent migration after upgrade: %v", err)
	}
	assertSchemaV1VoiceReconciliation(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("close upgraded fixture: %v", err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen upgraded fixture: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertSchemaV1VoiceReconciliation(t, reopened)
}

func TestSchemaV1VoiceReconciliationRollsBackWithMigrationFailure(t *testing.T) {
	ctx := context.Background()
	path := createSchemaV1DuplicateVoiceFixture(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store := &Store{db: db}
	if err := store.ensureMigrationChecksumColumn(ctx); err != nil {
		t.Fatal(err)
	}

	broken := embeddedMigration{
		version: 2,
		contents: []byte(`CREATE TABLE migration_rollback_probe(id INTEGER);
THIS IS NOT VALID SQL;`),
		checksum: "injected-failure",
	}
	if err := store.applyMigration(ctx, broken); err == nil {
		t.Fatal("migration with invalid SQL unexpectedly succeeded")
	}

	var activeForDeviceA int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM voice_sessions
WHERE device_id = 'device-a' AND state NOT IN ('ENDED', 'FAILED')`).Scan(&activeForDeviceA); err != nil {
		t.Fatal(err)
	}
	if activeForDeviceA != 3 {
		t.Fatalf("compatibility repair escaped rolled-back migration: active device-a sessions = %d", activeForDeviceA)
	}
	var probeTables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
WHERE type = 'table' AND name = 'migration_rollback_probe'`).Scan(&probeTables); err != nil {
		t.Fatal(err)
	}
	if probeTables != 0 {
		t.Fatal("failed migration left its probe table behind")
	}
	var migrationRows int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = 2").Scan(&migrationRows); err != nil {
		t.Fatal(err)
	}
	if migrationRows != 0 {
		t.Fatal("failed migration recorded schema version 2")
	}
}

func createSchemaV1DuplicateVoiceFixture(t testing.TB) string {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "schema-v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := migrationFiles.ReadFile("migrations/0001_initial.sql")
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(migration)); err != nil {
		_ = db.Close()
		t.Fatalf("apply schema v1: %v", err)
	}
	fixture, err := os.ReadFile("testdata/schema_v1_duplicate_voice.sql")
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(fixture)); err != nil {
		_ = db.Close()
		t.Fatalf("load schema-v1 fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertSchemaV1VoiceReconciliation(t testing.TB, store *Store) {
	t.Helper()
	type sessionState struct {
		state     string
		startedAt string
		endedAt   sql.NullString
	}
	wantStates := map[string]string{
		"voice-old":    "FAILED",
		"voice-tie-a":  "FAILED",
		"voice-tie-z":  "RINGING",
		"voice-ended":  "ENDED",
		"voice-single": "SPEAKING",
	}
	rows, err := store.db.Query(`SELECT id, state, started_at, ended_at FROM voice_sessions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := make(map[string]bool)
	for rows.Next() {
		var id string
		var item sessionState
		if err := rows.Scan(&id, &item.state, &item.startedAt, &item.endedAt); err != nil {
			t.Fatal(err)
		}
		seen[id] = true
		if item.state != wantStates[id] {
			t.Errorf("session %s state = %s, want %s", id, item.state, wantStates[id])
		}
		if item.state == "FAILED" && (!item.endedAt.Valid || item.endedAt.String != item.startedAt) {
			t.Errorf("terminalized session %s ended_at = %q, want started_at %q", id, item.endedAt.String, item.startedAt)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(wantStates) {
		t.Fatalf("reconciled session count = %d, want %d", len(seen), len(wantStates))
	}
	var indexCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
WHERE type = 'index' AND name = 'idx_voice_one_active_device'`).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 1 {
		t.Fatal("active-device voice-session unique index was not created")
	}
	if _, err := store.db.Exec(`INSERT INTO voice_sessions(
id, conversation_id, device_id, state, transport, interrupted, started_at, correlation_id)
VALUES('voice-conflict', 'conversation-ended', 'device-a', 'LISTENING', 'simulated', 0,
'2026-01-01T13:00:00Z', 'correlation-conflict')`); err == nil {
		t.Fatal("unique index accepted a second active session for device-a")
	}
}
