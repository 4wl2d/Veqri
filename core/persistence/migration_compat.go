package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// applyMigration owns the complete transaction for both compatibility repair
// and the checksummed migration contents. Compatibility work must never commit
// unless the corresponding migration and schema_migrations row also commit.
func (s *Store) applyMigration(ctx context.Context, migration embeddedMigration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", migration.version, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := prepareMigrationCompatibility(ctx, tx, migration.version); err != nil {
		return fmt.Errorf("prepare migration %d compatibility: %w", migration.version, err)
	}
	if _, err = tx.ExecContext(ctx, string(migration.contents)); err != nil {
		return fmt.Errorf("apply migration %d: %w", migration.version, err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at, checksum)
VALUES(?, ?, ?)`, migration.version, formatTime(time.Now().UTC()), migration.checksum); err != nil {
		return fmt.Errorf("record migration %d: %w", migration.version, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", migration.version, err)
	}
	return nil
}

func prepareMigrationCompatibility(ctx context.Context, tx *sql.Tx, version int) error {
	switch version {
	case 2:
		return reconcileV1ActiveVoiceSessions(ctx, tx)
	default:
		return nil
	}
}

type migrationVoiceSession struct {
	id        string
	deviceID  string
	startedAt time.Time
}

// Schema v1 allowed several nonterminal sessions for one device. Migration 2
// introduces the database invariant, so retain the newest session and mark
// every older duplicate failed before creating the unique index. ID descending
// is the stable tie-breaker when persisted start times are equal.
func reconcileV1ActiveVoiceSessions(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, device_id, started_at
FROM voice_sessions
WHERE device_id IS NOT NULL AND state NOT IN ('ENDED', 'FAILED')`)
	if err != nil {
		return fmt.Errorf("list active v1 voice sessions: %w", err)
	}
	var sessions []migrationVoiceSession
	for rows.Next() {
		var session migrationVoiceSession
		var startedAt string
		if err := rows.Scan(&session.id, &session.deviceID, &startedAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan active v1 voice session: %w", err)
		}
		session.startedAt, err = parseTime(startedAt)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("parse active v1 voice session %s start time: %w", session.id, err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close active v1 voice sessions: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate active v1 voice sessions: %w", err)
	}

	newest := make(map[string]migrationVoiceSession)
	for _, session := range sessions {
		current, found := newest[session.deviceID]
		if !found || session.startedAt.After(current.startedAt) ||
			(session.startedAt.Equal(current.startedAt) && session.id > current.id) {
			newest[session.deviceID] = session
		}
	}
	for _, session := range sessions {
		if newest[session.deviceID].id == session.id {
			continue
		}
		result, err := tx.ExecContext(ctx, `UPDATE voice_sessions
SET state = 'FAILED', ended_at = COALESCE(ended_at, started_at)
WHERE id = ? AND state NOT IN ('ENDED', 'FAILED')`, session.id)
		if err != nil {
			return fmt.Errorf("terminalize duplicate v1 voice session %s: %w", session.id, err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("inspect duplicate v1 voice session %s repair: %w", session.id, err)
		}
		if changed != 1 {
			return fmt.Errorf("duplicate v1 voice session %s changed concurrently", session.id)
		}
	}
	return nil
}
