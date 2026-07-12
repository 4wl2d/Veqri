package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/veqri/veqri/core/observability"
)

func (s *Store) AddAuditEntry(ctx context.Context, entry observability.AuditEntry) error {
	return insertAuditEntry(ctx, s.db, entry)
}

func insertAuditEntry(ctx context.Context, executor sqlExecer, entry observability.AuditEntry) error {
	if len(entry.Details) == 0 {
		entry.Details = json.RawMessage(`{}`)
	}
	_, err := executor.ExecContext(ctx, `INSERT INTO audit_entries(id, occurred_at,
actor_kind, actor_id, action, resource_kind, resource_id, decision, details_json,
correlation_id, task_id, conversation_id, connector_id)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, entry.ID, formatTime(entry.OccurredAt),
		entry.ActorKind, entry.ActorID, entry.Action, entry.ResourceKind, entry.ResourceID,
		nullableString(entry.Decision), string(entry.Details), entry.CorrelationID,
		nullableString(entry.TaskID), nullableString(entry.ConversationID), nullableString(entry.ConnectorID))
	if err != nil {
		return fmt.Errorf("add audit entry: %w", err)
	}
	return nil
}

func (s *Store) ListAuditEntries(ctx context.Context, limit int) ([]observability.AuditEntry, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, occurred_at, actor_kind, actor_id,
action, resource_kind, resource_id, decision, details_json, correlation_id,
task_id, conversation_id, connector_id FROM audit_entries ORDER BY occurred_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []observability.AuditEntry
	for rows.Next() {
		var item observability.AuditEntry
		var occurredAt, details string
		var decision, taskID, conversationID, connectorID sql.NullString
		if err := rows.Scan(&item.ID, &occurredAt, &item.ActorKind, &item.ActorID,
			&item.Action, &item.ResourceKind, &item.ResourceID, &decision, &details,
			&item.CorrelationID, &taskID, &conversationID, &connectorID); err != nil {
			return nil, err
		}
		item.OccurredAt, err = parseTime(occurredAt)
		if err != nil {
			return nil, err
		}
		item.Details = json.RawMessage(details)
		item.Decision = decision.String
		item.TaskID = taskID.String
		item.ConversationID = conversationID.String
		item.ConnectorID = connectorID.String
		result = append(result, item)
	}
	return result, rows.Err()
}

type Diagnostics struct {
	DatabaseOK       bool           `json:"database_ok"`
	Counts           map[string]int `json:"counts"`
	PendingTasks     int            `json:"pending_tasks"`
	PendingApprovals int            `json:"pending_approvals"`
	FailedDeliveries int            `json:"failed_deliveries"`
	GeneratedAt      time.Time      `json:"generated_at"`
	DatabaseDetail   string         `json:"database_detail"`
	JournalMode      string         `json:"journal_mode"`
	ForeignKeys      bool           `json:"foreign_keys"`
	MigrationVersion int            `json:"migration_version"`
}

func (s *Store) Diagnostics(ctx context.Context) (Diagnostics, error) {
	result := Diagnostics{DatabaseOK: true, Counts: make(map[string]int), GeneratedAt: time.Now().UTC(),
		DatabaseDetail: "quick_check ok"}
	if err := s.Ping(ctx); err != nil {
		result.DatabaseOK = false
		return result, err
	}
	if err := s.IntegrityCheck(ctx); err != nil {
		result.DatabaseOK = false
		result.DatabaseDetail = err.Error()
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&result.JournalMode); err != nil {
		return result, err
	}
	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return result, err
	}
	result.ForeignKeys = foreignKeys == 1
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&result.MigrationVersion); err != nil {
		return result, err
	}
	for _, table := range []string{"devices", "connectors", "conversations", "turns", "events", "tasks", "agents", "tool_invocations", "approvals", "deliveries", "audit_entries", "voice_sessions"} {
		var count int
		// Table names come only from this hard-coded list.
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil { //nolint:gosec
			return result, err
		}
		result.Counts[table] = count
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE status IN ('CREATED','QUEUED','ASSIGNED','RUNNING','WAITING_FOR_CHILDREN','WAITING_FOR_APPROVAL','BLOCKED','CANCEL_REQUESTED')`).Scan(&result.PendingTasks); err != nil {
		return result, err
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM approvals WHERE status = 'PENDING'").Scan(&result.PendingApprovals); err != nil {
		return result, err
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM deliveries WHERE status = 'FAILED'").Scan(&result.FailedDeliveries); err != nil {
		return result, err
	}
	return result, nil
}
