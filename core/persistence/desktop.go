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

type ConversationSummary struct {
	Conversation conversation.Conversation `json:"conversation"`
	TurnCount    int                       `json:"turn_count"`
	ActiveTasks  int                       `json:"active_tasks"`
	LastMessage  string                    `json:"last_message"`
	Correlation  string                    `json:"correlation_id"`
}

func (s *Store) ListConversationSummaries(ctx context.Context, limit int) ([]ConversationSummary, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.external_key, c.title,
c.transcript_retention, c.created_at, c.updated_at,
(SELECT COUNT(*) FROM turns tr WHERE tr.conversation_id = c.id),
(SELECT COUNT(*) FROM tasks ta WHERE ta.conversation_id = c.id AND ta.status NOT IN ('COMPLETED','PARTIALLY_COMPLETED','FAILED','CANCELLED','TIMED_OUT')),
COALESCE((SELECT tr.text FROM turns tr WHERE tr.conversation_id = c.id ORDER BY tr.created_at DESC LIMIT 1), ''),
COALESCE((SELECT tr.correlation_id FROM turns tr WHERE tr.conversation_id = c.id ORDER BY tr.created_at DESC LIMIT 1), '')
FROM conversations c ORDER BY c.updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ConversationSummary
	for rows.Next() {
		var item ConversationSummary
		var retain bool
		var createdAt, updatedAt string
		if err := rows.Scan(&item.Conversation.ID, &item.Conversation.ExternalKey,
			&item.Conversation.Title, &retain, &createdAt, &updatedAt, &item.TurnCount,
			&item.ActiveTasks, &item.LastMessage, &item.Correlation); err != nil {
			return nil, err
		}
		item.Conversation.TranscriptRetention = retain
		item.Conversation.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		item.Conversation.UpdatedAt, err = parseTime(updatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ListVoiceSessions(ctx context.Context, activeOnly bool, limit int) ([]conversation.VoiceSession, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	query := `SELECT id, conversation_id, device_id, state, transport, interrupted,
started_at, ended_at, correlation_id, direction, muted, push_to_talk, audio_route FROM voice_sessions`
	args := []any{}
	if activeOnly {
		query += " WHERE state NOT IN ('ENDED','FAILED')"
	}
	query += " ORDER BY started_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []conversation.VoiceSession
	for rows.Next() {
		var session conversation.VoiceSession
		var deviceID, endedAt sql.NullString
		var state, startedAt string
		if err := rows.Scan(&session.ID, &session.ConversationID, &deviceID, &state,
			&session.Transport, &session.Interrupted, &startedAt, &endedAt, &session.CorrelationID,
			&session.Direction, &session.Muted, &session.PushToTalk, &session.AudioRoute); err != nil {
			return nil, err
		}
		session.DeviceID = deviceID.String
		session.State = conversation.DialogState(state)
		session.StartedAt, err = parseTime(startedAt)
		if err != nil {
			return nil, err
		}
		session.EndedAt, err = parseOptionalTime(endedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, session)
	}
	return result, rows.Err()
}

func (s *Store) TaskDependencies(ctx context.Context, taskID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT depends_on_task_id FROM task_dependencies WHERE task_id = ? ORDER BY depends_on_task_id", taskID)
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

func (s *Store) GetSetting(ctx context.Context, key string, target any) error {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value_json FROM settings WHERE key = ?", key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), target)
}

func (s *Store) SetSetting(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings(key, value_json, updated_at)
VALUES(?, ?, ?) ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json,
updated_at = excluded.updated_at`, key, string(raw), formatTime(time.Now().UTC()))
	return err
}

// SetSettingWithAudit commits a security-sensitive setting and its audit
// record together. Callers must not apply the in-memory setting unless this
// transaction succeeds.
func (s *Store) SetSettingWithAudit(ctx context.Context, key string, value any,
	entry observability.AuditEntry) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value_json, updated_at)
VALUES(?, ?, ?) ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json,
updated_at = excluded.updated_at`, key, string(raw), formatTime(time.Now().UTC())); err != nil {
		return err
	}
	if err := insertAuditEntry(ctx, tx, entry); err != nil {
		return fmt.Errorf("audit setting update: %w", err)
	}
	return tx.Commit()
}

func (s *Store) StartDesktopAction(ctx context.Context, requestID string) (existing json.RawMessage, started bool, err error) {
	_, err = s.db.ExecContext(ctx, `INSERT INTO desktop_action_results(request_id, status,
created_at) VALUES(?, 'STARTED', ?)`, requestID, formatTime(time.Now().UTC()))
	if err == nil {
		return nil, true, nil
	}
	var status string
	var result sql.NullString
	lookupErr := s.db.QueryRowContext(ctx, `SELECT status, result_json FROM desktop_action_results
WHERE request_id = ?`, requestID).Scan(&status, &result)
	if lookupErr != nil {
		return nil, false, fmt.Errorf("start desktop action: %w", err)
	}
	if status == "COMPLETED" && result.Valid {
		return json.RawMessage(result.String), false, nil
	}
	return nil, false, ErrConflict
}

func (s *Store) CompleteDesktopAction(ctx context.Context, requestID string, response any) error {
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE desktop_action_results SET status = 'COMPLETED',
result_json = ?, completed_at = ? WHERE request_id = ? AND status = 'STARTED'`,
		string(raw), formatTime(time.Now().UTC()), requestID)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrConflict
	}
	return nil
}
