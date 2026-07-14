package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/veqri/veqri/core/delivery"
	"github.com/veqri/veqri/core/tasks"
	coretools "github.com/veqri/veqri/core/tools"
)

func TestStorageMaintenanceUsesStrictCutoffsAndPreservesUnresolvedWork(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	sweptAt := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	pairingCutoff := sweptAt.Add(-24 * time.Hour)
	desktopCutoff := sweptAt.Add(-7 * 24 * time.Hour)

	insertMaintenancePairing(t, store, "pairing-old", pairingCutoff.Add(-time.Second), nil)
	consumedAt := pairingCutoff.Add(-time.Minute)
	insertMaintenancePairing(t, store, "pairing-old-consumed", pairingCutoff.Add(-time.Hour), &consumedAt)
	insertMaintenancePairing(t, store, "pairing-boundary", pairingCutoff, nil)
	insertMaintenancePairing(t, store, "pairing-recent", pairingCutoff.Add(time.Second), nil)

	insertMaintenanceDesktopResult(t, store, "desktop-old", "COMPLETED",
		desktopCutoff.Add(-30*24*time.Hour), timePointer(desktopCutoff.Add(-time.Second)))
	insertMaintenanceDesktopResult(t, store, "desktop-boundary", "COMPLETED",
		desktopCutoff.Add(-time.Hour), timePointer(desktopCutoff))
	insertMaintenanceDesktopResult(t, store, "desktop-recent", "COMPLETED",
		desktopCutoff, timePointer(desktopCutoff.Add(time.Second)))
	insertMaintenanceDesktopResult(t, store, "desktop-started-old", "STARTED",
		desktopCutoff.Add(-30*24*time.Hour), nil)

	old := sweptAt.Add(-60 * 24 * time.Hour)
	startedAt := old
	task := tasks.Task{
		ID: "maintenance-running-task", RootTaskID: "maintenance-running-task",
		Goal: "unresolved task content", TaskType: "shell",
		Input:           json.RawMessage(`{"command":"echo","args":["unresolved"]}`),
		AssignedAgentID: "maintenance-agent", AllowedTools: []string{"shell"},
		ApprovalPolicy: "test", Status: tasks.StatusRunning, CreatedAt: old,
		StartedAt: &startedAt, MaxRetries: 2, TimeoutSeconds: 300,
		Artifacts: []tasks.Artifact{}, CorrelationID: "maintenance-correlation",
		IdempotencyKey: "maintenance-task-idempotency", Version: 1,
	}
	if _, duplicate, err := store.CreateTask(ctx, task); err != nil || duplicate {
		t.Fatalf("CreateTask() = (duplicate %v, %v)", duplicate, err)
	}
	invocation := coretools.Invocation{
		ID: "maintenance-started-invocation", TaskID: task.ID, ToolName: "shell",
		Input: task.Input, Risk: coretools.RiskStateChanging,
		CorrelationID: task.CorrelationID, IdempotencyKey: "maintenance-invocation-idempotency",
	}
	if _, duplicate, err := store.StartToolInvocation(ctx, invocation); err != nil || duplicate {
		t.Fatalf("StartToolInvocation() = (duplicate %v, %v)", duplicate, err)
	}
	for _, item := range []delivery.Delivery{
		{ID: "maintenance-delivering", TaskID: task.ID,
			Target: delivery.Target{Kind: "test", ChannelID: "delivering-secret"},
			Status: delivery.StatusDelivering, IdempotencyKey: "maintenance-delivering-idempotency",
			CreatedAt: old, CorrelationID: task.CorrelationID},
		{ID: "maintenance-pending", TaskID: task.ID,
			Target: delivery.Target{Kind: "test", ChannelID: "pending-secret"},
			Status: delivery.StatusPending, IdempotencyKey: "maintenance-pending-idempotency",
			CreatedAt: old, CorrelationID: task.CorrelationID},
	} {
		if duplicate, err := store.CreateDelivery(ctx, item); err != nil || duplicate {
			t.Fatalf("CreateDelivery(%s) = (duplicate %v, %v)", item.ID, duplicate, err)
		}
	}

	taskBefore, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	invocationBefore, err := store.GetToolInvocation(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	deliveriesBefore := maintenanceDeliverySnapshot(t, store)

	result, err := store.ApplyStorageMaintenance(ctx, sweptAt)
	if err != nil {
		t.Fatal(err)
	}
	if result.SweptAt != sweptAt || result.PairingSessionCutoff != pairingCutoff ||
		result.DesktopActionResultCutoff != desktopCutoff {
		t.Fatalf("maintenance timestamps = %+v", result)
	}
	if result.PairingSessionsDeleted != 2 || result.DesktopActionResultsDeleted != 1 {
		t.Fatalf("maintenance counts = %+v", result)
	}

	assertMaintenanceIDs(t, store, "pairing_sessions", "id",
		[]string{"pairing-boundary", "pairing-recent"})
	assertMaintenanceIDs(t, store, "desktop_action_results", "request_id",
		[]string{"desktop-boundary", "desktop-recent", "desktop-started-old"})

	taskAfter, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	invocationAfter, err := store.GetToolInvocation(ctx, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(taskBefore, taskAfter) {
		t.Fatalf("unresolved task changed: before=%+v after=%+v", taskBefore, taskAfter)
	}
	if !reflect.DeepEqual(invocationBefore, invocationAfter) {
		t.Fatalf("unresolved invocation changed: before=%+v after=%+v", invocationBefore, invocationAfter)
	}
	if deliveriesAfter := maintenanceDeliverySnapshot(t, store); !reflect.DeepEqual(deliveriesBefore, deliveriesAfter) {
		t.Fatalf("unresolved deliveries changed: before=%v after=%v", deliveriesBefore, deliveriesAfter)
	}

	second, err := store.ApplyStorageMaintenance(ctx, sweptAt)
	if err != nil {
		t.Fatal(err)
	}
	if second.PairingSessionsDeleted != 0 || second.DesktopActionResultsDeleted != 0 {
		t.Fatalf("second maintenance sweep was not idempotent: %+v", second)
	}
}

func TestStorageMaintenanceRollsBackBothDeletes(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	sweptAt := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	insertMaintenancePairing(t, store, "pairing-rollback", sweptAt.Add(-25*time.Hour), nil)
	insertMaintenanceDesktopResult(t, store, "desktop-rollback", "COMPLETED",
		sweptAt.Add(-30*24*time.Hour), timePointer(sweptAt.Add(-8*24*time.Hour)))
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_desktop_maintenance
BEFORE DELETE ON desktop_action_results WHEN OLD.request_id = 'desktop-rollback'
BEGIN SELECT RAISE(FAIL, 'injected desktop cleanup failure'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ApplyStorageMaintenance(ctx, sweptAt); err == nil {
		t.Fatal("storage maintenance unexpectedly succeeded despite injected delete failure")
	}
	assertMaintenanceIDs(t, store, "pairing_sessions", "id", []string{"pairing-rollback"})
	assertMaintenanceIDs(t, store, "desktop_action_results", "request_id", []string{"desktop-rollback"})
}

func insertMaintenancePairing(t testing.TB, store *Store, id string, expiresAt time.Time, consumedAt *time.Time) {
	t.Helper()
	var consumed any
	if consumedAt != nil {
		consumed = formatTime(*consumedAt)
	}
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO pairing_sessions(
id, code_hash, expires_at, consumed_at, created_at) VALUES(?, ?, ?, ?, ?)`,
		id, []byte("hash-"+id), formatTime(expiresAt), consumed,
		formatTime(expiresAt.Add(-5*time.Minute))); err != nil {
		t.Fatal(err)
	}
}

func insertMaintenanceDesktopResult(t testing.TB, store *Store, requestID, status string,
	createdAt time.Time, completedAt *time.Time) {
	t.Helper()
	var completed any
	var result any
	if completedAt != nil {
		completed = formatTime(*completedAt)
		result = `{"accepted":true}`
	}
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO desktop_action_results(
request_id, status, result_json, created_at, completed_at) VALUES(?, ?, ?, ?, ?)`,
		requestID, status, result, formatTime(createdAt), completed); err != nil {
		t.Fatal(err)
	}
}

func timePointer(value time.Time) *time.Time { return &value }

func assertMaintenanceIDs(t testing.TB, store *Store, table, column string, want []string) {
	t.Helper()
	rows, err := store.db.QueryContext(context.Background(),
		fmt.Sprintf("SELECT %s FROM %s ORDER BY %s", column, table, column))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s IDs = %v, want %v", table, got, want)
	}
}

func maintenanceDeliverySnapshot(t testing.TB, store *Store) []string {
	t.Helper()
	rows, err := store.db.QueryContext(context.Background(), `SELECT id, status, target_json,
attempt_count, last_error, created_at, correlation_id, idempotency_key
FROM deliveries ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var snapshot []string
	for rows.Next() {
		var id, status, target, lastError, createdAt, correlationID, idempotencyKey string
		var attemptCount int
		if err := rows.Scan(&id, &status, &target, &attemptCount, &lastError,
			&createdAt, &correlationID, &idempotencyKey); err != nil {
			t.Fatal(err)
		}
		snapshot = append(snapshot, fmt.Sprintf("%s|%s|%s|%d|%s|%s|%s|%s",
			id, status, target, attemptCount, lastError, createdAt, correlationID, idempotencyKey))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
