package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/veqri/veqri/core/events"
)

// IngestEvent durably records an event before any processing. It returns the
// existing event ID and duplicate=true when source-scoped idempotency matches.
func (s *Store) IngestEvent(ctx context.Context, event events.Envelope) (eventID string, duplicate bool, err error) {
	err = insertEvent(ctx, s.db, event)
	if err == nil {
		return event.ID, false, nil
	}
	existing, lookupErr := s.GetEventBySourceIdempotency(ctx, event.Source, event.IdempotencyKey)
	if lookupErr == nil {
		return existing.ID, true, nil
	}
	return "", false, fmt.Errorf("insert event: %w", err)
}

func insertEvent(ctx context.Context, executor sqlExecer, event events.Envelope) error {
	if err := event.Validate(); err != nil {
		return err
	}
	reply, err := json.Marshal(event.ReplyTarget)
	if err != nil {
		return fmt.Errorf("marshal reply target: %w", err)
	}
	_, err = executor.ExecContext(ctx, `INSERT INTO events(
id, type, version, source_kind, source_connector_id, source_instance_id,
actor_id, actor_display_name, occurred_at, received_at, conversation_key,
correlation_id, causation_id, idempotency_key, trust_level, reply_target_json,
payload_json, processing_status)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'PENDING')`,
		event.ID, event.Type, event.Version, event.Source.Kind, event.Source.ConnectorID,
		event.Source.InstanceID, event.Actor.ID, event.Actor.DisplayName,
		formatTime(event.OccurredAt), formatTime(event.ReceivedAt), event.ConversationKey,
		event.CorrelationID, event.CausationID, event.IdempotencyKey, string(event.TrustLevel),
		string(reply), string(event.Payload))
	return err
}

func (s *Store) GetEventBySourceIdempotency(ctx context.Context, source events.Source, key string) (events.Envelope, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM events
WHERE source_kind = ? AND source_connector_id = ? AND source_instance_id = ? AND idempotency_key = ?`,
		source.Kind, source.ConnectorID, source.InstanceID, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return events.Envelope{}, ErrNotFound
	}
	if err != nil {
		return events.Envelope{}, err
	}
	return s.GetEvent(ctx, id)
}

func (s *Store) GetEvent(ctx context.Context, id string) (events.Envelope, error) {
	var result events.Envelope
	var occurredAt, receivedAt string
	var causation sql.NullString
	var trust string
	var replyJSON, payloadJSON string
	err := s.db.QueryRowContext(ctx, `SELECT id, type, version, source_kind,
source_connector_id, source_instance_id, actor_id, actor_display_name,
occurred_at, received_at, conversation_key, correlation_id, causation_id,
idempotency_key, trust_level, reply_target_json, payload_json
FROM events WHERE id = ?`, id).Scan(
		&result.ID, &result.Type, &result.Version, &result.Source.Kind,
		&result.Source.ConnectorID, &result.Source.InstanceID, &result.Actor.ID,
		&result.Actor.DisplayName, &occurredAt, &receivedAt, &result.ConversationKey,
		&result.CorrelationID, &causation, &result.IdempotencyKey, &trust,
		&replyJSON, &payloadJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return events.Envelope{}, ErrNotFound
	}
	if err != nil {
		return events.Envelope{}, fmt.Errorf("get event: %w", err)
	}
	result.OccurredAt, err = parseTime(occurredAt)
	if err != nil {
		return events.Envelope{}, err
	}
	result.ReceivedAt, err = parseTime(receivedAt)
	if err != nil {
		return events.Envelope{}, err
	}
	if causation.Valid {
		result.CausationID = &causation.String
	}
	result.TrustLevel = events.TrustLevel(trust)
	if err := json.Unmarshal([]byte(replyJSON), &result.ReplyTarget); err != nil {
		return events.Envelope{}, fmt.Errorf("decode reply target: %w", err)
	}
	result.Payload = json.RawMessage(payloadJSON)
	return result, nil
}

func (s *Store) ListPendingEvents(ctx context.Context, limit int) ([]events.Envelope, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM events WHERE processing_status = 'PENDING'
ORDER BY received_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]events.Envelope, 0, len(ids))
	for _, id := range ids {
		event, err := s.GetEvent(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, nil
}

func (s *Store) MarkEventProcessed(ctx context.Context, id string, processingErr error) error {
	status := "PROCESSED"
	errorText := ""
	if processingErr != nil {
		status = "FAILED"
		errorText = processingErr.Error()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE events SET processing_status = ?, processing_error = ?, processed_at = ? WHERE id = ?`,
		status, errorText, formatTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("mark event processed: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")
var ErrExpired = errors.New("expired")
