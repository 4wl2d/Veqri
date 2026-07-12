package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/observability"
)

func (s *Store) GetOrCreateConversation(ctx context.Context, externalKey, title string, retain bool, id string) (conversation.Conversation, error) {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO conversations(
id, external_key, title, transcript_retention, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(external_key) DO UPDATE SET
updated_at = excluded.updated_at,
title = CASE WHEN conversations.transcript_retention = 1 AND conversations.title = ?
THEN excluded.title ELSE conversations.title END`,
		id, externalKey, title, retain, formatTime(now), formatTime(now), expiredContentMarker)
	if err != nil {
		return conversation.Conversation{}, fmt.Errorf("upsert conversation: %w", err)
	}
	return s.GetConversationByExternalKey(ctx, externalKey)
}

func (s *Store) GetConversation(ctx context.Context, id string) (conversation.Conversation, error) {
	return s.scanConversation(s.db.QueryRowContext(ctx, `SELECT id, external_key, title,
transcript_retention, created_at, updated_at FROM conversations WHERE id = ?`, id))
}

func (s *Store) GetConversationByExternalKey(ctx context.Context, key string) (conversation.Conversation, error) {
	return s.scanConversation(s.db.QueryRowContext(ctx, `SELECT id, external_key, title,
transcript_retention, created_at, updated_at FROM conversations WHERE external_key = ?`, key))
}

// ListDeviceConversationIDs returns only the Android device's default dialog
// and voice-session dialogs. Tasks and approvals are owner-wide, but turns are
// scoped because the Android client currently presents one active thread.
func (s *Store) ListDeviceConversationIDs(ctx context.Context, deviceID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM conversations WHERE external_key = ?
UNION SELECT conversation_id FROM voice_sessions WHERE device_id = ? AND state NOT IN ('ENDED','FAILED')`,
		"android:"+deviceID+":default", deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func (s *Store) DeviceOwnsConversation(ctx context.Context, deviceID, conversationID string) (bool, error) {
	if conversationID == "" {
		return false, nil
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations c WHERE c.id = ? AND
(c.external_key = ? OR EXISTS (SELECT 1 FROM voice_sessions v
WHERE v.conversation_id = c.id AND v.device_id = ?))`, conversationID,
		"android:"+deviceID+":default", deviceID).Scan(&count)
	return count > 0, err
}

type rowScanner interface{ Scan(dest ...any) error }

func (s *Store) scanConversation(row rowScanner) (conversation.Conversation, error) {
	var result conversation.Conversation
	var retain bool
	var createdAt, updatedAt string
	if err := row.Scan(&result.ID, &result.ExternalKey, &result.Title, &retain, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return conversation.Conversation{}, ErrNotFound
		}
		return conversation.Conversation{}, fmt.Errorf("scan conversation: %w", err)
	}
	result.TranscriptRetention = retain
	var err error
	result.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return conversation.Conversation{}, err
	}
	result.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return conversation.Conversation{}, err
	}
	return result, nil
}

func (s *Store) AddTurn(ctx context.Context, turn conversation.Turn, retain bool) error {
	return insertTurn(ctx, s.db, turn, retain)
}

func insertTurn(ctx context.Context, executor sqlExecer, turn conversation.Turn, retain bool) error {
	text := turn.Text
	if !retain {
		text = "[transcript retention disabled]"
	}
	_, err := executor.ExecContext(ctx, `INSERT OR IGNORE INTO turns(
id, conversation_id, role, text, final, correlation_id, created_at)
VALUES(?, ?, ?, ?, ?, ?, ?)`, turn.ID, turn.ConversationID, string(turn.Role), text,
		turn.Final, turn.CorrelationID, formatTime(turn.CreatedAt))
	if err != nil {
		return fmt.Errorf("add turn: %w", err)
	}
	return nil
}

// SetTranscriptRetention changes the authoritative Core retention policy. A
// transition to disabled also removes every stored turn in the same
// transaction so callers cannot accidentally leave historical text behind.
func (s *Store) SetTranscriptRetention(ctx context.Context, conversationID string, enabled bool) error {
	return s.setTranscriptRetention(ctx, conversationID, enabled, nil)
}

// SetTranscriptRetentionWithAudit commits the conversation policy, canonical
// scrub, and mandatory audit fact together. A caller must not publish success
// unless this method returns nil.
func (s *Store) SetTranscriptRetentionWithAudit(ctx context.Context, conversationID string, enabled bool,
	entry observability.AuditEntry) error {
	return s.setTranscriptRetention(ctx, conversationID, enabled, &entry)
}

func (s *Store) setTranscriptRetention(ctx context.Context, conversationID string, enabled bool,
	entry *observability.AuditEntry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := setTranscriptRetentionTx(ctx, tx, conversationID, enabled, time.Now().UTC()); err != nil {
		return err
	}
	if entry != nil {
		if err := insertAuditEntry(ctx, tx, *entry); err != nil {
			return fmt.Errorf("audit transcript retention update: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) ListTurns(ctx context.Context, conversationID string, limit int) ([]conversation.Turn, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, conversation_id, role, text,
final, correlation_id, created_at FROM (
  SELECT id, conversation_id, role, text, final, correlation_id, created_at
  FROM turns WHERE conversation_id = ? ORDER BY created_at DESC LIMIT ?
) recent ORDER BY created_at ASC`, conversationID, limit)
	if err != nil {
		return nil, fmt.Errorf("list turns: %w", err)
	}
	defer rows.Close()
	var result []conversation.Turn
	for rows.Next() {
		var turn conversation.Turn
		var role, createdAt string
		if err := rows.Scan(&turn.ID, &turn.ConversationID, &role, &turn.Text,
			&turn.Final, &turn.CorrelationID, &createdAt); err != nil {
			return nil, fmt.Errorf("scan turn: %w", err)
		}
		turn.Role = conversation.TurnRole(role)
		turn.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		result = append(result, turn)
	}
	return result, rows.Err()
}

func (s *Store) DeleteTranscript(ctx context.Context, conversationID string) error {
	return s.SetTranscriptRetention(ctx, conversationID, false)
}

// SetDeviceTranscriptRetention atomically changes the Android device default,
// applies the same policy to its current conversation when supplied, performs
// the canonical scrub, and records the mandatory success audit.
func (s *Store) SetDeviceTranscriptRetention(ctx context.Context, deviceID, conversationID string,
	enabled bool, entry observability.AuditEntry) error {
	raw, err := json.Marshal(map[string]bool{"transcript_retention": enabled})
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if conversationID != "" {
		var owned int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations c WHERE c.id = ? AND
(c.external_key = ? OR EXISTS (SELECT 1 FROM voice_sessions v
WHERE v.conversation_id = c.id AND v.device_id = ?))`, conversationID,
			"android:"+deviceID+":default", deviceID).Scan(&owned); err != nil {
			return fmt.Errorf("verify device conversation ownership: %w", err)
		}
		if owned != 1 {
			return ErrNotFound
		}
		if err := setTranscriptRetentionTx(ctx, tx, conversationID, enabled, time.Now().UTC()); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value_json, updated_at)
VALUES(?, ?, ?) ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json,
updated_at = excluded.updated_at`, "device:"+deviceID+":privacy", string(raw),
		formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("save device transcript retention: %w", err)
	}
	if err := insertAuditEntry(ctx, tx, entry); err != nil {
		return fmt.Errorf("audit device transcript retention update: %w", err)
	}
	return tx.Commit()
}

func setTranscriptRetentionTx(ctx context.Context, tx *sql.Tx, conversationID string, enabled bool,
	updatedAt time.Time) error {
	var externalKey string
	if err := tx.QueryRowContext(ctx, "SELECT external_key FROM conversations WHERE id = ?", conversationID).
		Scan(&externalKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if enabled {
		result, err := tx.ExecContext(ctx, `UPDATE conversations SET transcript_retention = 1,
updated_at = ? WHERE id = ?`, formatTime(updatedAt), conversationID)
		if err != nil {
			return fmt.Errorf("enable transcript retention: %w", err)
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return ErrNotFound
		}
		return nil
	}
	return disableTranscriptRetentionTx(ctx, tx, conversationID, externalKey, updatedAt)
}

func disableTranscriptRetentionTx(ctx context.Context, tx *sql.Tx, conversationID, externalKey string,
	updatedAt time.Time) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM turns WHERE conversation_id = ?", conversationID); err != nil {
		return fmt.Errorf("delete turns: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE conversations SET transcript_retention = 0,
		title = '[transcript retention disabled]', updated_at = ? WHERE id = ?`,
		formatTime(updatedAt), conversationID)
	if err != nil {
		return fmt.Errorf("disable transcript retention: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE events SET payload_json = '{}'
WHERE conversation_key = ?`, externalKey); err != nil {
		return fmt.Errorf("scrub conversation events: %w", err)
	}
	if err := scrubTerminalConversationTasks(ctx, tx, conversationID); err != nil {
		return err
	}
	return nil
}

func scrubTerminalConversationTasks(ctx context.Context, tx *sql.Tx, conversationID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT root_task_id FROM tasks WHERE conversation_id = ?`, conversationID)
	if err != nil {
		return fmt.Errorf("list conversation task graphs: %w", err)
	}
	var roots []string
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			_ = rows.Close()
			return err
		}
		roots = append(roots, root)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, root := range roots {
		safe, err := terminalTaskGraphSafeToScrub(ctx, tx, root)
		if err != nil {
			return err
		}
		if safe {
			if err := scrubTerminalTaskGraphContent(ctx, tx, root); err != nil {
				return err
			}
		}
	}
	return nil
}

// ScrubTerminalTaskGraph removes retained prompt/result content once every
// node in a non-retained graph is terminal.
func (s *Store) ScrubTerminalTaskGraph(ctx context.Context, rootTaskID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var conversationID, externalKey string
	var retain bool
	err = tx.QueryRowContext(ctx, `SELECT c.id, c.external_key, c.transcript_retention
FROM tasks t JOIN conversations c ON c.id = t.conversation_id WHERE t.id = ?`, rootTaskID).
		Scan(&conversationID, &externalKey, &retain)
	if errors.Is(err, sql.ErrNoRows) || retain {
		return nil
	}
	if err != nil {
		return err
	}
	safe, err := terminalTaskGraphSafeToScrub(ctx, tx, rootTaskID)
	if err != nil {
		return err
	}
	if !safe {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "UPDATE events SET payload_json = '{}' WHERE conversation_key = ?", externalKey); err != nil {
		return err
	}
	if err := scrubTerminalTaskGraphContent(ctx, tx, rootTaskID); err != nil {
		return err
	}
	return tx.Commit()
}

func terminalTaskGraphSafeToScrub(ctx context.Context, tx *sql.Tx, rootTaskID string) (bool, error) {
	var protected int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE root_task_id = ?
AND status NOT IN ('COMPLETED','PARTIALLY_COMPLETED','FAILED','CANCELLED','TIMED_OUT'))`,
		rootTaskID).Scan(&protected)
	if err != nil {
		return false, fmt.Errorf("check terminal task graph scrub safety: %w", err)
	}
	return protected == 0, nil
}

func scrubTerminalTaskGraphContent(ctx context.Context, executor sqlExecer, rootTaskID string) error {
	if _, err := executor.ExecContext(ctx, `DELETE FROM artifacts WHERE task_id IN (
SELECT id FROM tasks WHERE root_task_id = ?)`, rootTaskID); err != nil {
		return fmt.Errorf("delete task graph artifact metadata: %w", err)
	}
	if _, err := executor.ExecContext(ctx, `UPDATE tool_invocations
SET input_json = '{}', output_json = NULL, error = '' WHERE task_id IN (
SELECT id FROM tasks WHERE root_task_id = ?)`, rootTaskID); err != nil {
		return fmt.Errorf("scrub task graph tool invocations: %w", err)
	}
	if _, err := executor.ExecContext(ctx, `UPDATE approvals
SET tool_arguments_json = '{}', requested_scopes_json = '[]', reason = '[transcript retention disabled]'
WHERE task_id IN (SELECT id FROM tasks WHERE root_task_id = ?)
AND status IN ('DENIED','EXPIRED','CONSUMED')`, rootTaskID); err != nil {
		return fmt.Errorf("scrub task graph approvals: %w", err)
	}
	if _, err := executor.ExecContext(ctx, `UPDATE deliveries SET target_json = '{}', last_error = ''
WHERE task_id IN (SELECT id FROM tasks WHERE root_task_id = ?) AND status IN ('DELIVERED','FAILED')`, rootTaskID); err != nil {
		return fmt.Errorf("scrub task graph deliveries: %w", err)
	}
	if _, err := executor.ExecContext(ctx, `UPDATE tasks
SET goal = '[transcript retention disabled]', input_json = '{}', progress_message = '', error = '',
result_json = NULL, user_facing_summary = '[transcript retention disabled]', artifacts_json = '[]'
WHERE root_task_id = ?`, rootTaskID); err != nil {
		return fmt.Errorf("scrub task graph tasks: %w", err)
	}
	return nil
}

// ScrubTerminalNonRetainedTaskGraphs is a restart-safe privacy reconciliation.
// It deliberately scans all eligible roots so a prior scrub failure is retried
// even when recovery already committed the terminal task status.
func (s *Store) ScrubTerminalNonRetainedTaskGraphs(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT t.root_task_id
FROM tasks t JOIN conversations c ON c.id = t.conversation_id
WHERE c.transcript_retention = 0 AND NOT EXISTS (
  SELECT 1 FROM tasks active WHERE active.root_task_id = t.root_task_id
  AND active.status NOT IN ('COMPLETED','PARTIALLY_COMPLETED','FAILED','CANCELLED','TIMED_OUT')
)`)
	if err != nil {
		return fmt.Errorf("list terminal non-retained task graphs: %w", err)
	}
	var rootTaskIDs []string
	for rows.Next() {
		var rootTaskID string
		if err := rows.Scan(&rootTaskID); err != nil {
			_ = rows.Close()
			return err
		}
		rootTaskIDs = append(rootTaskIDs, rootTaskID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, rootTaskID := range rootTaskIDs {
		if err := s.ScrubTerminalTaskGraph(ctx, rootTaskID); err != nil {
			return fmt.Errorf("scrub terminal non-retained task graph %s: %w", rootTaskID, err)
		}
	}
	return nil
}
