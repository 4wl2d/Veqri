package persistence

import (
	"context"
	"fmt"
	"time"
)

func (s *Store) UseWebhookNonce(ctx context.Context, connectorID, nonce string, receivedAt time.Time) (duplicate bool, err error) {
	_, err = s.db.ExecContext(ctx, `INSERT INTO webhook_replay_nonces(connector_id, nonce,
received_at) VALUES(?, ?, ?)`, connectorID, nonce, formatTime(receivedAt))
	if err == nil {
		_, _ = s.db.ExecContext(ctx, "DELETE FROM webhook_replay_nonces WHERE received_at < ?", formatTime(receivedAt.Add(-24*time.Hour)))
		return false, nil
	}
	var count int
	if lookupErr := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM webhook_replay_nonces
WHERE connector_id = ? AND nonce = ?`, connectorID, nonce).Scan(&count); lookupErr == nil && count > 0 {
		return true, nil
	}
	return false, fmt.Errorf("record webhook nonce: %w", err)
}
