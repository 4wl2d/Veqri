package persistence

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/veqri/veqri/core/delivery"
)

func (s *Store) CreateDelivery(ctx context.Context, item delivery.Delivery) (bool, error) {
	err := insertDelivery(ctx, s.db, item, false)
	if err == nil {
		return false, nil
	}
	var count int
	if lookupErr := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM deliveries WHERE idempotency_key = ?", item.IdempotencyKey).Scan(&count); lookupErr == nil && count > 0 {
		return true, nil
	}
	return false, fmt.Errorf("create delivery: %w", err)
}

func insertDelivery(ctx context.Context, executor sqlExecer, item delivery.Delivery, ignoreDuplicate bool) error {
	target, err := json.Marshal(item.Target)
	if err != nil {
		return err
	}
	insert := "INSERT"
	if ignoreDuplicate {
		insert = "INSERT OR IGNORE"
	}
	_, err = executor.ExecContext(ctx, insert+` INTO deliveries(id, task_id, target_json,
priority, status, attempt_count, last_error, idempotency_key, created_at,
delivered_at, correlation_id) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, item.ID,
		item.TaskID, string(target), item.Priority, string(item.Status), item.AttemptCount,
		item.LastError, item.IdempotencyKey, formatTime(item.CreatedAt),
		optionalTime(item.DeliveredAt), item.CorrelationID)
	return err
}
