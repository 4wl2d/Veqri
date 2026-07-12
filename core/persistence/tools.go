package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/core/tools"
	"github.com/veqri/veqri/internal/ids"
)

func (s *Store) StartToolInvocation(ctx context.Context, invocation tools.Invocation) (tools.Invocation, bool, error) {
	if existing, err := s.GetToolInvocationByIdempotencyKey(ctx, invocation.IdempotencyKey); err == nil {
		return existing, true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return tools.Invocation{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tools.Invocation{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	invocation.Status = "STARTED"
	invocation.StartedAt = &now
	_, err = tx.ExecContext(ctx, `INSERT INTO tool_invocations(id, task_id,
tool_name, input_json, risk, status, started_at, correlation_id, idempotency_key)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, invocation.ID, invocation.TaskID,
		invocation.ToolName, string(invocation.Input), string(invocation.Risk), invocation.Status,
		formatTime(now), invocation.CorrelationID, invocation.IdempotencyKey)
	if err != nil {
		_ = tx.Rollback()
		existing, lookupErr := s.GetToolInvocationByIdempotencyKey(ctx, invocation.IdempotencyKey)
		if lookupErr == nil {
			return existing, true, nil
		}
		return tools.Invocation{}, false, fmt.Errorf("start tool invocation: %w", err)
	}
	task, err := scanTask(tx.QueryRowContext(ctx, taskSelect+" WHERE t.id = ?", invocation.TaskID))
	if err != nil {
		return tools.Invocation{}, false, err
	}
	details, err := json.Marshal(map[string]any{"tool": invocation.ToolName, "input": "omitted"})
	if err != nil {
		return tools.Invocation{}, false, err
	}
	entry := observability.AuditEntry{
		ID: ids.New(), OccurredAt: now, ActorKind: "agent", ActorID: task.AssignedAgentID,
		Action: "tool.started", ResourceKind: "tool_invocation", ResourceID: invocation.ID,
		Decision: string(invocation.Risk), Details: details,
		CorrelationID: task.CorrelationID, TaskID: task.ID, ConversationID: task.ConversationID,
	}
	if err := insertAuditEntry(ctx, tx, entry); err != nil {
		return tools.Invocation{}, false, fmt.Errorf("audit tool start: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return tools.Invocation{}, false, err
	}
	return invocation, false, nil
}

func (s *Store) FinishToolInvocation(ctx context.Context, id string, output json.RawMessage, exitCode int, invocationErr error) (tools.Invocation, error) {
	status := "COMPLETED"
	errorText := ""
	if invocationErr != nil {
		status = "FAILED"
		errorText = invocationErr.Error()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tools.Invocation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	invocation, err := scanInvocation(tx.QueryRowContext(ctx, invocationSelect+" WHERE id = ?", id))
	if err != nil {
		return tools.Invocation{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE tool_invocations SET status = ?,
finished_at = ?, exit_code = ?, output_json = ?, error = ? WHERE id = ? AND status = 'STARTED'`,
		status, formatTime(time.Now().UTC()), exitCode, nullableRaw(output), errorText, id)
	if err != nil {
		return tools.Invocation{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return tools.Invocation{}, ErrConflict
	}
	task, err := scanTask(tx.QueryRowContext(ctx, taskSelect+" WHERE t.id = ?", invocation.TaskID))
	if err != nil {
		return tools.Invocation{}, err
	}
	var outputMetadata struct {
		TimedOut  bool `json:"timed_out"`
		Truncated bool `json:"truncated"`
		DryRun    bool `json:"dry_run"`
	}
	_ = json.Unmarshal(output, &outputMetadata)
	details, err := json.Marshal(map[string]any{
		"tool": invocation.ToolName, "exit_code": exitCode,
		"timed_out": outputMetadata.TimedOut, "truncated": outputMetadata.Truncated,
		"dry_run": outputMetadata.DryRun,
		"stdout":  "omitted", "stderr": "omitted",
	})
	if err != nil {
		return tools.Invocation{}, err
	}
	entry := observability.AuditEntry{
		ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: "agent", ActorID: task.AssignedAgentID,
		Action: "tool.finished", ResourceKind: "tool_invocation", ResourceID: invocation.ID,
		Decision: status, Details: details, CorrelationID: task.CorrelationID,
		TaskID: task.ID, ConversationID: task.ConversationID,
	}
	if err := insertAuditEntry(ctx, tx, entry); err != nil {
		return tools.Invocation{}, fmt.Errorf("audit tool finish: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return tools.Invocation{}, err
	}
	return s.GetToolInvocation(ctx, id)
}

func (s *Store) GetToolInvocation(ctx context.Context, id string) (tools.Invocation, error) {
	return scanInvocation(s.db.QueryRowContext(ctx, invocationSelect+" WHERE id = ?", id))
}

func (s *Store) GetToolInvocationByIdempotencyKey(ctx context.Context, key string) (tools.Invocation, error) {
	return scanInvocation(s.db.QueryRowContext(ctx, invocationSelect+" WHERE idempotency_key = ?", key))
}

const invocationSelect = `SELECT id, task_id, tool_name, input_json, risk, status,
started_at, finished_at, exit_code, output_json, error, correlation_id, idempotency_key
FROM tool_invocations`

func scanInvocation(scanner rowScanner) (tools.Invocation, error) {
	var item tools.Invocation
	var input, risk string
	var startedAt, finishedAt, output sql.NullString
	var exitCode sql.NullInt64
	if err := scanner.Scan(&item.ID, &item.TaskID, &item.ToolName, &input, &risk,
		&item.Status, &startedAt, &finishedAt, &exitCode, &output, &item.Error,
		&item.CorrelationID, &item.IdempotencyKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return tools.Invocation{}, ErrNotFound
		}
		return tools.Invocation{}, err
	}
	item.Input = json.RawMessage(input)
	item.Risk = tools.Risk(risk)
	var err error
	item.StartedAt, err = parseOptionalTime(startedAt)
	if err != nil {
		return tools.Invocation{}, err
	}
	item.FinishedAt, err = parseOptionalTime(finishedAt)
	if err != nil {
		return tools.Invocation{}, err
	}
	if exitCode.Valid {
		value := int(exitCode.Int64)
		item.ExitCode = &value
	}
	if output.Valid {
		item.Output = json.RawMessage(output.String)
	}
	return item, nil
}
