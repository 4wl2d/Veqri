package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/veqri/veqri/core/approvals"
	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/core/tools"
	"github.com/veqri/veqri/internal/ids"
)

func (s *Store) CreateApproval(ctx context.Context, approval approvals.Approval) error {
	return insertApproval(ctx, s.db, approval)
}

func insertApproval(ctx context.Context, executor sqlExecer, approval approvals.Approval) error {
	scopes, err := json.Marshal(approval.RequestedScopes)
	if err != nil {
		return err
	}
	_, err = executor.ExecContext(ctx, `INSERT INTO approvals(id, task_id, tool_name,
tool_arguments_json, requested_scopes_json, risk, reason, status, requested_at,
expires_at, correlation_id) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, approval.ID,
		approval.TaskID, approval.ToolName, string(approval.ToolArguments), string(scopes),
		string(approval.Risk), approval.Reason, string(approval.Status),
		formatTime(approval.RequestedAt), formatTime(approval.ExpiresAt), approval.CorrelationID)
	if err != nil {
		return fmt.Errorf("create approval: %w", err)
	}
	return nil
}

func (s *Store) GetApproval(ctx context.Context, id string) (approvals.Approval, error) {
	return scanApproval(s.db.QueryRowContext(ctx, approvalSelect+" WHERE id = ?", id))
}

func (s *Store) GetApprovalByTask(ctx context.Context, taskID string) (approvals.Approval, error) {
	return scanApproval(s.db.QueryRowContext(ctx, approvalSelect+" WHERE task_id = ? ORDER BY requested_at DESC LIMIT 1", taskID))
}

const approvalSelect = `SELECT id, task_id, tool_name, tool_arguments_json,
requested_scopes_json, risk, reason, status, requested_at, expires_at, decided_at,
decided_by, consumed_at, correlation_id FROM approvals`

func (s *Store) ListApprovals(ctx context.Context, pendingOnly bool, limit int) ([]approvals.Approval, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	query := approvalSelect
	args := []any{}
	if pendingOnly {
		query += " WHERE status = ?"
		args = append(args, string(approvals.StatusPending))
	}
	query += " ORDER BY requested_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []approvals.Approval
	for rows.Next() {
		item, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ListApprovalsForTaskGraph(ctx context.Context, rootTaskID string) ([]approvals.Approval, error) {
	rows, err := s.db.QueryContext(ctx, approvalSelect+` WHERE task_id IN (
SELECT id FROM tasks WHERE root_task_id = ?) ORDER BY requested_at`, rootTaskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []approvals.Approval
	for rows.Next() {
		item, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// DecideApproval atomically consumes a single-use decision and moves the task
// to QUEUED (approve) or CANCELLED (deny). Repeated decisions are rejected.
func (s *Store) DecideApproval(ctx context.Context, id, decidedBy string, approved bool) (approvals.Approval, tasks.Task, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return approvals.Approval{}, tasks.Task{}, err
	}
	defer func() { _ = tx.Rollback() }()
	approval, err := scanApproval(tx.QueryRowContext(ctx, approvalSelect+" WHERE id = ?", id))
	if err != nil {
		return approvals.Approval{}, tasks.Task{}, err
	}
	if approval.Status != approvals.StatusPending {
		return approvals.Approval{}, tasks.Task{}, ErrConflict
	}
	if !approval.ExpiresAt.After(now) {
		if _, err := tx.ExecContext(ctx, "UPDATE approvals SET status = ?, decided_at = ? WHERE id = ? AND status = ?",
			string(approvals.StatusExpired), formatTime(now), id, string(approvals.StatusPending)); err != nil {
			return approvals.Approval{}, tasks.Task{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, error = ?, finished_at = ?,
version = version + 1 WHERE id = ? AND status = ?`, string(tasks.StatusTimedOut),
			"approval expired", formatTime(now), approval.TaskID, string(tasks.StatusWaitingForApproval)); err != nil {
			return approvals.Approval{}, tasks.Task{}, err
		}
		task, err := scanTask(tx.QueryRowContext(ctx, taskSelect+" WHERE t.id = ?", approval.TaskID))
		if err != nil {
			return approvals.Approval{}, tasks.Task{}, err
		}
		if err := insertAuditEntry(ctx, tx, approvalDecisionAudit(approval, task, decidedBy, "EXPIRED", now)); err != nil {
			return approvals.Approval{}, tasks.Task{}, fmt.Errorf("audit expired approval decision: %w", err)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return approvals.Approval{}, tasks.Task{}, commitErr
		}
		_ = s.ScrubTerminalTaskGraph(context.WithoutCancel(ctx), task.RootTaskID)
		return approvals.Approval{}, tasks.Task{}, ErrExpired
	}
	approvalStatus := approvals.StatusDenied
	taskStatus := tasks.StatusCancelled
	if approved {
		approvalStatus = approvals.StatusConsumed
		taskStatus = tasks.StatusQueued
	}
	result, err := tx.ExecContext(ctx, `UPDATE approvals SET status = ?, decided_at = ?,
decided_by = ?, consumed_at = ? WHERE id = ? AND status = ?`, string(approvalStatus),
		formatTime(now), decidedBy, optionalConsumed(now, approved), id, string(approvals.StatusPending))
	if err != nil {
		return approvals.Approval{}, tasks.Task{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return approvals.Approval{}, tasks.Task{}, ErrConflict
	}
	finishedAt := any(nil)
	if !approved {
		finishedAt = formatTime(now)
	}
	result, err = tx.ExecContext(ctx, `UPDATE tasks SET status = ?, finished_at = ?,
version = version + 1 WHERE id = ? AND status = ?`, string(taskStatus), finishedAt,
		approval.TaskID, string(tasks.StatusWaitingForApproval))
	if err != nil {
		return approvals.Approval{}, tasks.Task{}, err
	}
	changed, _ = result.RowsAffected()
	if changed != 1 {
		return approvals.Approval{}, tasks.Task{}, ErrConflict
	}
	updatedTask, err := scanTask(tx.QueryRowContext(ctx, taskSelect+" WHERE t.id = ?", approval.TaskID))
	if err != nil {
		return approvals.Approval{}, tasks.Task{}, err
	}
	decision := "DENY"
	if approved {
		decision = "ALLOW"
	}
	if err := insertAuditEntry(ctx, tx, approvalDecisionAudit(approval, updatedTask, decidedBy, decision, now)); err != nil {
		return approvals.Approval{}, tasks.Task{}, fmt.Errorf("audit approval decision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return approvals.Approval{}, tasks.Task{}, err
	}
	updatedApproval, err := s.GetApproval(ctx, id)
	if err != nil {
		return approvals.Approval{}, tasks.Task{}, err
	}
	updatedTask, err = s.GetTask(ctx, approval.TaskID)
	if err == nil && !approved {
		_ = s.ScrubTerminalTaskGraph(ctx, updatedTask.RootTaskID)
	}
	return updatedApproval, updatedTask, err
}

func approvalDecisionAudit(approval approvals.Approval, task tasks.Task, decidedBy, decision string,
	now time.Time) observability.AuditEntry {
	actorKind := "principal"
	actorID := decidedBy
	if kind, id, ok := strings.Cut(decidedBy, ":"); ok && kind != "" && id != "" {
		actorKind = kind
		actorID = id
	}
	details, _ := json.Marshal(map[string]any{"tool": approval.ToolName, "risk": approval.Risk})
	return observability.AuditEntry{
		ID: ids.New(), OccurredAt: now, ActorKind: actorKind, ActorID: actorID,
		Action: "approval.decide", ResourceKind: "approval", ResourceID: approval.ID,
		Decision: decision, Details: details, CorrelationID: approval.CorrelationID,
		TaskID: task.ID, ConversationID: task.ConversationID,
	}
}

type ApprovalTaskChange struct {
	Approval approvals.Approval
	Task     tasks.Task
}

func (s *Store) ExpireApprovals(ctx context.Context) ([]ApprovalTaskChange, error) {
	now := formatTime(time.Now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, approvalSelect+` WHERE status = ? AND expires_at <= ?`,
		string(approvals.StatusPending), now)
	if err != nil {
		return nil, err
	}
	var expiring []approvals.Approval
	for rows.Next() {
		approval, err := scanApproval(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		expiring = append(expiring, approval)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE approvals SET status = ?, decided_at = ?
WHERE status = ? AND expires_at <= ?`, string(approvals.StatusExpired), now,
		string(approvals.StatusPending), now)
	if err != nil {
		return nil, err
	}
	for _, approval := range expiring {
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, error = ?, finished_at = ?,
version = version + 1 WHERE id = ? AND status = ?`, string(tasks.StatusTimedOut),
			"approval expired", now, approval.TaskID, string(tasks.StatusWaitingForApproval)); err != nil {
			return nil, err
		}
		task, err := scanTask(tx.QueryRowContext(ctx, taskSelect+" WHERE t.id = ?", approval.TaskID))
		if err != nil {
			return nil, err
		}
		if err := insertAuditEntry(ctx, tx, approvalDecisionAudit(approval, task, "policy:core", "EXPIRED", time.Now().UTC())); err != nil {
			return nil, fmt.Errorf("audit approval expiry: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	changes := make([]ApprovalTaskChange, 0, len(expiring))
	for _, expired := range expiring {
		if task, taskErr := s.GetTask(ctx, expired.TaskID); taskErr == nil {
			_ = s.ScrubTerminalTaskGraph(ctx, task.RootTaskID)
			if approval, approvalErr := s.GetApproval(ctx, expired.ID); approvalErr == nil {
				changes = append(changes, ApprovalTaskChange{Approval: approval, Task: task})
			}
		}
	}
	return changes, nil
}

func scanApproval(scanner rowScanner) (approvals.Approval, error) {
	var item approvals.Approval
	var toolArguments, scopes, risk, status, requestedAt, expiresAt string
	var decidedAt, consumedAt sql.NullString
	if err := scanner.Scan(&item.ID, &item.TaskID, &item.ToolName, &toolArguments,
		&scopes, &risk, &item.Reason, &status, &requestedAt, &expiresAt, &decidedAt,
		&item.DecidedBy, &consumedAt, &item.CorrelationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return approvals.Approval{}, ErrNotFound
		}
		return approvals.Approval{}, err
	}
	item.ToolArguments = json.RawMessage(toolArguments)
	if err := json.Unmarshal([]byte(scopes), &item.RequestedScopes); err != nil {
		return approvals.Approval{}, err
	}
	item.Risk = tools.Risk(risk)
	item.Status = approvals.Status(status)
	var err error
	item.RequestedAt, err = parseTime(requestedAt)
	if err != nil {
		return approvals.Approval{}, err
	}
	item.ExpiresAt, err = parseTime(expiresAt)
	if err != nil {
		return approvals.Approval{}, err
	}
	item.DecidedAt, err = parseOptionalTime(decidedAt)
	if err != nil {
		return approvals.Approval{}, err
	}
	item.ConsumedAt, err = parseOptionalTime(consumedAt)
	if err != nil {
		return approvals.Approval{}, err
	}
	return item, nil
}

func optionalConsumed(now time.Time, consumed bool) any {
	if !consumed {
		return nil
	}
	return formatTime(now)
}
