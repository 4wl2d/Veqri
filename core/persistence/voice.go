package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/veqri/veqri/core/conversation"
)

func (s *Store) CreateVoiceSession(ctx context.Context, session conversation.VoiceSession) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO voice_sessions(id, conversation_id,
device_id, state, transport, interrupted, started_at, ended_at, correlation_id,
direction, muted, push_to_talk, audio_route)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, session.ID, session.ConversationID,
		nullableString(session.DeviceID), string(session.State), session.Transport,
		session.Interrupted, formatTime(session.StartedAt), optionalTime(session.EndedAt), session.CorrelationID,
		fallbackVoiceValue(session.Direction, "OUTGOING"), session.Muted, session.PushToTalk,
		fallbackVoiceValue(session.AudioRoute, "EARPIECE"))
	if err != nil {
		return fmt.Errorf("create voice session: %w", err)
	}
	return nil
}

func (s *Store) GetVoiceSession(ctx context.Context, id string) (conversation.VoiceSession, error) {
	return scanVoiceSession(s.db.QueryRowContext(ctx, `SELECT id, conversation_id, device_id, state,
transport, interrupted, started_at, ended_at, correlation_id, direction, muted,
push_to_talk, audio_route FROM voice_sessions WHERE id = ?`, id))
}

// GetActiveVoiceSessionForDevice makes the one-call-per-device invariant
// explicit to API callers. The partial unique index remains the race-safe
// backstop for simultaneous starts.
func (s *Store) GetActiveVoiceSessionForDevice(ctx context.Context, deviceID string) (conversation.VoiceSession, error) {
	return scanVoiceSession(s.db.QueryRowContext(ctx, `SELECT id, conversation_id, device_id, state,
transport, interrupted, started_at, ended_at, correlation_id, direction, muted,
push_to_talk, audio_route FROM voice_sessions
WHERE device_id = ? AND state NOT IN ('ENDED','FAILED') LIMIT 1`, deviceID))
}

func scanVoiceSession(row rowScanner) (conversation.VoiceSession, error) {
	var session conversation.VoiceSession
	var deviceID, endedAt sql.NullString
	var state, startedAt string
	err := row.Scan(&session.ID, &session.ConversationID, &deviceID, &state,
		&session.Transport, &session.Interrupted, &startedAt, &endedAt, &session.CorrelationID,
		&session.Direction, &session.Muted, &session.PushToTalk, &session.AudioRoute)
	if errors.Is(err, sql.ErrNoRows) {
		return conversation.VoiceSession{}, ErrNotFound
	}
	if err != nil {
		return conversation.VoiceSession{}, err
	}
	session.DeviceID = deviceID.String
	session.State = conversation.DialogState(state)
	session.StartedAt, err = parseTime(startedAt)
	if err != nil {
		return conversation.VoiceSession{}, err
	}
	session.EndedAt, err = parseOptionalTime(endedAt)
	if err != nil {
		return conversation.VoiceSession{}, err
	}
	return session, nil
}

func (s *Store) UpdateVoicePreferences(ctx context.Context, id string, muted, pushToTalk bool, audioRoute string) (conversation.VoiceSession, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE voice_sessions SET muted = ?, push_to_talk = ?, audio_route = ?
WHERE id = ? AND state NOT IN ('ENDED','FAILED')`, muted, pushToTalk,
		fallbackVoiceValue(audioRoute, "EARPIECE"), id)
	if err != nil {
		return conversation.VoiceSession{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return conversation.VoiceSession{}, ErrConflict
	}
	return s.GetVoiceSession(ctx, id)
}

func fallbackVoiceValue(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (s *Store) TransitionVoiceSession(ctx context.Context, id string, to conversation.DialogState, interrupted bool) (conversation.VoiceSession, error) {
	session, err := s.GetVoiceSession(ctx, id)
	if err != nil {
		return conversation.VoiceSession{}, err
	}
	if err := conversation.ValidateTransition(session.State, to); err != nil {
		return conversation.VoiceSession{}, err
	}
	endedAt := any(nil)
	if to == conversation.StateEnded {
		endedAt = formatTime(time.Now().UTC())
	}
	result, err := s.db.ExecContext(ctx, `UPDATE voice_sessions SET state = ?, interrupted = ?,
ended_at = COALESCE(?, ended_at) WHERE id = ? AND state = ?`, string(to), interrupted,
		endedAt, id, string(session.State))
	if err != nil {
		return conversation.VoiceSession{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return conversation.VoiceSession{}, ErrConflict
	}
	return s.GetVoiceSession(ctx, id)
}
