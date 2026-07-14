package api

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/internal/config"
	"github.com/veqri/veqri/internal/stream"
)

func TestPeriodicMaintenanceInjectedTickRunsCleanupWhenRetentionDisabled(t *testing.T) {
	if maintenanceSweepInterval != 6*time.Hour {
		t.Fatalf("maintenance interval = %s, want 6h", maintenanceSweepInterval)
	}
	ctx := context.Background()
	store := openLifecycleMaintenanceTestStore(t)
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	seedLifecycleMaintenanceRows(t, store, now, "disabled")
	server := lifecycleMaintenanceTestServer(store, 0)

	ticks := make(chan time.Time, 1)
	ticks <- now
	close(ticks)
	server.runPeriodicMaintenance(ctx, ticks)

	assertLifecycleMaintenanceCount(t, store, "pairing_sessions", 0)
	assertLifecycleMaintenanceCount(t, store, "desktop_action_results", 0)
	assertLifecycleMaintenanceCount(t, store, "turns", 1)
}

func TestPeriodicMaintenanceInjectedTickRunsCleanupAndEnabledRetention(t *testing.T) {
	ctx := context.Background()
	store := openLifecycleMaintenanceTestStore(t)
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	seedLifecycleMaintenanceRows(t, store, now, "enabled")
	server := lifecycleMaintenanceTestServer(store, 1)

	ticks := make(chan time.Time, 1)
	ticks <- now
	close(ticks)
	server.runPeriodicMaintenance(ctx, ticks)

	assertLifecycleMaintenanceCount(t, store, "pairing_sessions", 0)
	assertLifecycleMaintenanceCount(t, store, "desktop_action_results", 0)
	assertLifecycleMaintenanceCount(t, store, "turns", 0)
	var retentionAudits int
	if err := store.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM audit_entries WHERE action = 'retention.sweep'").Scan(&retentionAudits); err != nil {
		t.Fatal(err)
	}
	if retentionAudits != 1 {
		t.Fatalf("retention sweep audit count = %d, want 1", retentionAudits)
	}
}

func openLifecycleMaintenanceTestStore(t testing.TB) *persistence.Store {
	t.Helper()
	store, err := persistence.Open(context.Background(), filepath.Join(t.TempDir(), "maintenance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close maintenance store: %v", err)
		}
	})
	return store
}

func lifecycleMaintenanceTestServer(store *persistence.Store, retentionDays int) *Server {
	return &Server{
		config: config.Config{RetentionDays: retentionDays}, store: store,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), hub: stream.New(),
	}
}

func seedLifecycleMaintenanceRows(t testing.TB, store *persistence.Store, now time.Time, suffix string) {
	t.Helper()
	ctx := context.Background()
	format := func(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO pairing_sessions(
id, code_hash, expires_at, created_at) VALUES(?, ?, ?, ?)`,
		"pairing-"+suffix, []byte("hash-"+suffix), format(now.Add(-25*time.Hour)),
		format(now.Add(-26*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO desktop_action_results(
request_id, status, result_json, created_at, completed_at)
VALUES(?, 'COMPLETED', '{"accepted":true}', ?, ?)`, "desktop-"+suffix,
		format(now.Add(-9*24*time.Hour)), format(now.Add(-8*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	conversationID := "conversation-" + suffix
	old := format(now.Add(-2 * 24 * time.Hour))
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO conversations(
id, external_key, title, transcript_retention, created_at, updated_at)
VALUES(?, ?, 'old content', 1, ?, ?)`, conversationID, "maintenance:"+suffix, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO turns(
id, conversation_id, role, text, final, correlation_id, created_at)
VALUES(?, ?, 'user', 'old retained content', 1, ?, ?)`, "turn-"+suffix,
		conversationID, "correlation-"+suffix, old); err != nil {
		t.Fatal(err)
	}
}

func assertLifecycleMaintenanceCount(t testing.TB, store *persistence.Store, table string, want int) {
	t.Helper()
	query := map[string]string{
		"pairing_sessions":       "SELECT COUNT(*) FROM pairing_sessions",
		"desktop_action_results": "SELECT COUNT(*) FROM desktop_action_results",
		"turns":                  "SELECT COUNT(*) FROM turns",
	}[table]
	var got int
	if err := store.DB().QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}
