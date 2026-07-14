package persistence

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	pairingSessionCleanupAge      = 24 * time.Hour
	desktopActionResultCleanupAge = 7 * 24 * time.Hour
)

// StorageMaintenanceResult reports bounded housekeeping counts without
// exposing pairing hashes, request identifiers, or cached action results.
type StorageMaintenanceResult struct {
	SweptAt                     time.Time `json:"swept_at"`
	PairingSessionCutoff        time.Time `json:"pairing_session_cutoff"`
	DesktopActionResultCutoff   time.Time `json:"desktop_action_result_cutoff"`
	PairingSessionsDeleted      int64     `json:"pairing_sessions_deleted"`
	DesktopActionResultsDeleted int64     `json:"desktop_action_results_deleted"`
}

// ApplyStorageMaintenance atomically removes only fixed-lifetime pairing and
// completed desktop idempotency records. STARTED desktop actions are retained
// indefinitely because their outcome may be unresolved and replaying them can
// duplicate side effects. Task, tool, and delivery records are never touched.
func (s *Store) ApplyStorageMaintenance(ctx context.Context, sweptAt time.Time) (StorageMaintenanceResult, error) {
	if sweptAt.IsZero() {
		return StorageMaintenanceResult{}, errors.New("storage maintenance sweep time is required")
	}
	sweptAt = sweptAt.UTC()
	result := StorageMaintenanceResult{
		SweptAt:                   sweptAt,
		PairingSessionCutoff:      sweptAt.Add(-pairingSessionCleanupAge),
		DesktopActionResultCutoff: sweptAt.Add(-desktopActionResultCleanupAge),
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StorageMaintenanceResult{}, fmt.Errorf("begin storage maintenance: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	pairingResult, err := tx.ExecContext(ctx, `DELETE FROM pairing_sessions
WHERE julianday(expires_at) < julianday(?)`, formatTime(result.PairingSessionCutoff))
	if err != nil {
		return StorageMaintenanceResult{}, fmt.Errorf("delete expired pairing sessions: %w", err)
	}
	result.PairingSessionsDeleted, err = pairingResult.RowsAffected()
	if err != nil {
		return StorageMaintenanceResult{}, fmt.Errorf("count deleted pairing sessions: %w", err)
	}

	desktopResult, err := tx.ExecContext(ctx, `DELETE FROM desktop_action_results
WHERE status = 'COMPLETED' AND completed_at IS NOT NULL
AND julianday(completed_at) < julianday(?)`, formatTime(result.DesktopActionResultCutoff))
	if err != nil {
		return StorageMaintenanceResult{}, fmt.Errorf("delete completed desktop action results: %w", err)
	}
	result.DesktopActionResultsDeleted, err = desktopResult.RowsAffected()
	if err != nil {
		return StorageMaintenanceResult{}, fmt.Errorf("count deleted desktop action results: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return StorageMaintenanceResult{}, fmt.Errorf("commit storage maintenance: %w", err)
	}
	return result, nil
}
