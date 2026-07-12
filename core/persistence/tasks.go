package persistence

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/veqri/veqri/core/approvals"
	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/delivery"
	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/internal/ids"
)

func (s *Store) CreateTask(ctx context.Context, task tasks.Task) (tasks.Task, bool, error) {
	err := insertTask(ctx, s.db, task)
	if err == nil {
		return task, false, nil
	}
	existing, lookupErr := s.GetTaskByIdempotencyKey(ctx, task.IdempotencyKey)
	if lookupErr == nil {
		return existing, true, nil
	}
	return tasks.Task{}, false, fmt.Errorf("create task: %w", err)
}

// CreateTaskWithApproval makes the executable waiting task and its single-use
// approval visible together. On an idempotent replay it returns the original
// task and approval; it also repairs records produced by older non-atomic
// versions that stopped between the two inserts.
func (s *Store) CreateTaskWithApproval(ctx context.Context, task tasks.Task,
	requested *approvals.Approval) (tasks.Task, *approvals.Approval, bool, error) {
	if existing, err := s.GetTaskByIdempotencyKey(ctx, task.IdempotencyKey); err == nil {
		approval, repairErr := s.approvalForExistingTask(ctx, existing, requested)
		return existing, approval, true, repairErr
	} else if !errors.Is(err, ErrNotFound) {
		return tasks.Task{}, nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tasks.Task{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertTask(ctx, tx, task); err != nil {
		_ = tx.Rollback()
		if existing, lookupErr := s.GetTaskByIdempotencyKey(ctx, task.IdempotencyKey); lookupErr == nil {
			approval, repairErr := s.approvalForExistingTask(ctx, existing, requested)
			return existing, approval, true, repairErr
		}
		return tasks.Task{}, nil, false, fmt.Errorf("insert task with approval: %w", err)
	}
	if requested != nil {
		requested.TaskID = task.ID
		if requested.ID == "" {
			requested.ID = "approval:" + task.ID
		}
		if err := insertApproval(ctx, tx, *requested); err != nil {
			return tasks.Task{}, nil, false, fmt.Errorf("insert task approval: %w", err)
		}
		if err := insertAuditEntry(ctx, tx, approvalRequestedAudit(task, *requested)); err != nil {
			return tasks.Task{}, nil, false, fmt.Errorf("audit task approval request: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return tasks.Task{}, nil, false, err
	}
	return task, requested, false, nil
}

func (s *Store) approvalForExistingTask(ctx context.Context, task tasks.Task,
	requested *approvals.Approval) (*approvals.Approval, error) {
	if requested == nil {
		return nil, nil
	}
	existing, err := s.GetApprovalByTask(ctx, task.ID)
	if err == nil {
		return &existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if task.Status != tasks.StatusWaitingForApproval {
		return nil, nil
	}
	if len(task.AllowedTools) != 1 || requested.ToolName != task.AllowedTools[0] ||
		!bytes.Equal(requested.ToolArguments, task.Input) {
		return nil, fmt.Errorf("%w: approval replay does not match the persisted task command", ErrConflict)
	}
	repaired := *requested
	repaired.ID = "approval:" + task.ID
	repaired.TaskID = task.ID
	repaired.CorrelationID = task.CorrelationID
	repaired.ToolArguments = append(json.RawMessage(nil), task.Input...)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertApproval(ctx, tx, repaired); err != nil {
		return nil, err
	}
	if err := insertAuditEntry(ctx, tx, approvalRequestedAudit(task, repaired)); err != nil {
		return nil, fmt.Errorf("audit repaired approval request: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &repaired, nil
}

func approvalRequestedAudit(task tasks.Task, approval approvals.Approval) observability.AuditEntry {
	details, _ := json.Marshal(map[string]any{"tool": approval.ToolName, "risk": approval.Risk})
	return observability.AuditEntry{
		ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: "policy", ActorID: "core",
		Action: "approval.requested", ResourceKind: "approval", ResourceID: approval.ID,
		Decision:      "REQUIRE_APPROVAL",
		Details:       details,
		CorrelationID: task.CorrelationID, TaskID: task.ID, ConversationID: task.ConversationID,
	}
}

type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertTask(ctx context.Context, executor sqlExecer, task tasks.Task) error {
	allowedTools, err := json.Marshal(task.AllowedTools)
	if err != nil {
		return fmt.Errorf("marshal allowed tools: %w", err)
	}
	artifacts, err := json.Marshal(task.Artifacts)
	if err != nil {
		return fmt.Errorf("marshal artifacts: %w", err)
	}
	if task.Version == 0 {
		task.Version = 1
	}
	_, err = executor.ExecContext(ctx, `INSERT INTO tasks(
id, parent_task_id, root_task_id, conversation_id, goal, task_type, input_json,
assigned_agent_id, allowed_tools_json, approval_policy, status, progress,
progress_message, created_at, started_at, finished_at, retry_count, max_retries,
timeout_seconds, error, result_json, user_facing_summary, artifacts_json,
correlation_id, causation_id, idempotency_key, version, priority, dismissed)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ID, task.ParentTaskID, task.RootTaskID, nullableString(task.ConversationID), task.Goal,
		task.TaskType, string(task.Input), task.AssignedAgentID, string(allowedTools),
		task.ApprovalPolicy, string(task.Status), task.Progress, task.ProgressMessage,
		formatTime(task.CreatedAt), optionalTime(task.StartedAt), optionalTime(task.FinishedAt),
		task.RetryCount, task.MaxRetries, task.TimeoutSeconds, task.Error,
		nullableRaw(task.Result), task.UserFacingSummary, string(artifacts),
		task.CorrelationID, task.CausationID, task.IdempotencyKey, task.Version,
		task.Priority, task.Dismissed)
	return err
}

// CreateTaskGraph commits all nodes and dependencies together. The first task
// must be the root so parent foreign keys are satisfiable.
func (s *Store) CreateTaskGraph(ctx context.Context, taskList []tasks.Task, dependencies []tasks.Dependency) (tasks.Task, bool, error) {
	if len(taskList) == 0 {
		return tasks.Task{}, false, errors.New("task graph requires at least one task")
	}
	root := taskList[0]
	if existing, err := s.GetTaskByIdempotencyKey(ctx, root.IdempotencyKey); err == nil {
		return existing, true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return tasks.Task{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tasks.Task{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, task := range taskList {
		if task.RootTaskID != root.ID {
			return tasks.Task{}, false, errors.New("all graph nodes must reference the first task as root")
		}
		if err := insertTask(ctx, tx, task); err != nil {
			return tasks.Task{}, false, fmt.Errorf("insert task graph node %s: %w", task.ID, err)
		}
	}
	for _, dependency := range dependencies {
		if dependency.TaskID == dependency.DependsOnTaskID {
			return tasks.Task{}, false, errors.New("task cannot depend on itself")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_dependencies(task_id,
depends_on_task_id) VALUES(?, ?)`, dependency.TaskID, dependency.DependsOnTaskID); err != nil {
			return tasks.Task{}, false, fmt.Errorf("insert task dependency: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return tasks.Task{}, false, err
	}
	return root, false, nil
}

func (s *Store) AddTaskDependency(ctx context.Context, taskID, dependsOn string) error {
	if taskID == dependsOn {
		return errors.New("task cannot depend on itself")
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO task_dependencies(task_id,
depends_on_task_id) VALUES(?, ?)`, taskID, dependsOn)
	if err != nil {
		return fmt.Errorf("add task dependency: %w", err)
	}
	return nil
}

func (s *Store) GetTask(ctx context.Context, id string) (tasks.Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, taskSelect+" WHERE t.id = ?", id))
}

func (s *Store) GetTaskByIdempotencyKey(ctx context.Context, key string) (tasks.Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, taskSelect+" WHERE t.idempotency_key = ?", key))
}

// RetryTask atomically retries one task attempt and invalidates any terminal
// synthesis outcome that was derived from it. A queued synthesis root is
// version-touched in the same transaction so it cannot be claimed between the
// eligibility check and the dependency reset.
//
// Retrying a synthesis root retries its eligible failed dependencies as well;
// this prevents an explicit retry from merely re-synthesizing unchanged
// terminal failures.
func (s *Store) RetryTask(ctx context.Context, id string) (tasks.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tasks.Task{}, err
	}
	defer func() { _ = tx.Rollback() }()

	selected, err := scanTask(tx.QueryRowContext(ctx, taskSelect+" WHERE t.id = ?", id))
	if err != nil {
		return tasks.Task{}, err
	}
	eligible, err := explicitRetryEligible(ctx, tx, selected)
	if err != nil {
		return tasks.Task{}, err
	}
	if !eligible {
		return tasks.Task{}, fmt.Errorf("%w: selected task is not eligible for explicit retry", ErrConflict)
	}

	if selected.ID == selected.RootTaskID {
		selected, err = retryRootTask(ctx, tx, selected)
	} else {
		selected, err = retryGraphChild(ctx, tx, selected)
	}
	if err != nil {
		return tasks.Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return tasks.Task{}, err
	}
	return selected, nil
}

func retryRootTask(ctx context.Context, tx *sql.Tx, root tasks.Task) (tasks.Task, error) {
	var graphNodes int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE root_task_id = ?", root.ID).Scan(&graphNodes); err != nil {
		return tasks.Task{}, err
	}
	if graphNodes <= 1 {
		return resetExplicitRetryTask(ctx, tx, root, "Explicit retry requested")
	}
	if root.TaskType != "synthesis" {
		return tasks.Task{}, fmt.Errorf("%w: graph root is not a synthesis task", ErrConflict)
	}

	dependencies, err := directRootDependencies(ctx, tx, root)
	if err != nil {
		return tasks.Task{}, err
	}
	retryable := make([]tasks.Task, 0, len(dependencies))
	for _, dependency := range dependencies {
		eligible, eligibilityErr := explicitRetryEligible(ctx, tx, dependency)
		if eligibilityErr != nil {
			return tasks.Task{}, eligibilityErr
		}
		if eligible {
			retryable = append(retryable, dependency)
		}
	}
	if len(retryable) == 0 {
		if root.Status == tasks.StatusPartiallyCompleted {
			return tasks.Task{}, fmt.Errorf("%w: partial synthesis graph has no eligible failed dependency", ErrConflict)
		}
		// A failed/timed-out/cancelled synthesizer may itself be the failed
		// operation even when every dependency completed. Retrying that root is
		// useful and does not replay an unchanged partial synthesis result.
		return resetExplicitRetryTask(ctx, tx, root, "Explicit retry requested")
	}

	updatedRoot, err := resetExplicitRetryTask(ctx, tx, root, "Waiting for retried graph dependencies")
	if err != nil {
		return tasks.Task{}, err
	}
	for _, dependency := range retryable {
		if _, err := resetExplicitRetryTask(ctx, tx, dependency, "Explicit graph retry requested"); err != nil {
			return tasks.Task{}, err
		}
	}
	return updatedRoot, nil
}

func retryGraphChild(ctx context.Context, tx *sql.Tx, selected tasks.Task) (tasks.Task, error) {
	root, err := scanTask(tx.QueryRowContext(ctx, taskSelect+" WHERE t.id = ?", selected.RootTaskID))
	if err != nil {
		return tasks.Task{}, err
	}
	if root.ID != root.RootTaskID || root.TaskType != "synthesis" {
		return tasks.Task{}, fmt.Errorf("%w: task graph has no synthesis root", ErrConflict)
	}
	var directDependency bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM task_dependencies WHERE task_id = ? AND depends_on_task_id = ?
)`, root.ID, selected.ID).Scan(&directDependency); err != nil {
		return tasks.Task{}, err
	}
	if !directDependency {
		return tasks.Task{}, fmt.Errorf("%w: selected task is not a synthesis-root dependency", ErrConflict)
	}

	switch root.Status {
	case tasks.StatusQueued:
		if err := guardQueuedSynthesisRoot(ctx, tx, root); err != nil {
			return tasks.Task{}, err
		}
	case tasks.StatusFailed, tasks.StatusTimedOut, tasks.StatusCancelled, tasks.StatusPartiallyCompleted:
		eligible, eligibilityErr := explicitRetryEligible(ctx, tx, root)
		if eligibilityErr != nil {
			return tasks.Task{}, eligibilityErr
		}
		if !eligible {
			return tasks.Task{}, fmt.Errorf("%w: synthesis root cannot be re-armed", ErrConflict)
		}
		if _, err := resetExplicitRetryTask(ctx, tx, root, "Waiting for retried graph dependencies"); err != nil {
			return tasks.Task{}, err
		}
	default:
		return tasks.Task{}, fmt.Errorf("%w: synthesis root is %s", ErrConflict, root.Status)
	}

	return resetExplicitRetryTask(ctx, tx, selected, "Explicit retry requested")
}

func directRootDependencies(ctx context.Context, tx *sql.Tx, root tasks.Task) ([]tasks.Task, error) {
	rows, err := tx.QueryContext(ctx, taskSelect+` JOIN task_dependencies d
ON d.depends_on_task_id = t.id
WHERE d.task_id = ? AND t.root_task_id = ? ORDER BY t.created_at, t.id`, root.ID, root.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []tasks.Task
	for rows.Next() {
		dependency, scanErr := scanTask(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, dependency)
	}
	return result, rows.Err()
}

func explicitRetryEligible(ctx context.Context, tx *sql.Tx, task tasks.Task) (bool, error) {
	if task.Dismissed || task.Goal == expiredContentMarker || task.TaskType == "shell" ||
		task.RetryCount >= task.MaxRetries || !explicitRetryStatus(task.Status) {
		return false, nil
	}
	if task.ConversationID == "" {
		return true, nil
	}
	var retained bool
	err := tx.QueryRowContext(ctx, "SELECT transcript_retention FROM conversations WHERE id = ?", task.ConversationID).Scan(&retained)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return retained, err
}

func explicitRetryStatus(status tasks.Status) bool {
	switch status {
	case tasks.StatusFailed, tasks.StatusTimedOut, tasks.StatusCancelled, tasks.StatusPartiallyCompleted:
		return true
	default:
		return false
	}
}

func resetExplicitRetryTask(ctx context.Context, tx *sql.Tx, task tasks.Task, message string) (tasks.Task, error) {
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, progress = 0,
progress_message = ?, started_at = NULL, finished_at = NULL, error = '', result_json = NULL,
user_facing_summary = '', artifacts_json = '[]', retry_count = retry_count + 1,
version = version + 1
WHERE id = ? AND version = ? AND dismissed = 0 AND goal <> ? AND task_type <> 'shell'
AND retry_count < max_retries AND status IN (?, ?, ?, ?)
AND (conversation_id IS NULL OR EXISTS (
SELECT 1 FROM conversations c WHERE c.id = tasks.conversation_id AND c.transcript_retention = 1
))`, string(tasks.StatusQueued), message, task.ID, task.Version, expiredContentMarker,
		string(tasks.StatusFailed), string(tasks.StatusTimedOut), string(tasks.StatusCancelled),
		string(tasks.StatusPartiallyCompleted))
	if err != nil {
		return tasks.Task{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return tasks.Task{}, ErrConflict
	}
	task.Status = tasks.StatusQueued
	task.Progress = 0
	task.ProgressMessage = message
	task.StartedAt = nil
	task.FinishedAt = nil
	task.Error = ""
	task.Result = nil
	task.UserFacingSummary = ""
	task.Artifacts = []tasks.Artifact{}
	task.RetryCount++
	task.Version++
	return task, nil
}

func guardQueuedSynthesisRoot(ctx context.Context, tx *sql.Tx, root tasks.Task) error {
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET progress_message = ?, version = version + 1
WHERE id = ? AND version = ? AND status = ? AND task_type = 'synthesis' AND dismissed = 0
AND goal <> ? AND (conversation_id IS NULL OR EXISTS (
SELECT 1 FROM conversations c WHERE c.id = tasks.conversation_id AND c.transcript_retention = 1
))`, "Waiting for retried graph dependencies", root.ID, root.Version, string(tasks.StatusQueued),
		expiredContentMarker)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrConflict
	}
	return nil
}

const taskSelect = `SELECT t.id, t.parent_task_id, t.root_task_id, t.conversation_id,
t.goal, t.task_type, t.input_json, t.assigned_agent_id, t.allowed_tools_json,
t.approval_policy, t.status, t.progress, t.progress_message, t.created_at,
t.started_at, t.finished_at, t.retry_count, t.max_retries, t.timeout_seconds,
t.error, t.result_json, t.user_facing_summary, t.artifacts_json, t.correlation_id,
t.causation_id, t.idempotency_key, t.version, t.priority, t.dismissed FROM tasks t`

func (s *Store) ListTasks(ctx context.Context, statuses []tasks.Status, limit int) ([]tasks.Task, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	query := taskSelect + " WHERE t.dismissed = 0"
	args := make([]any, 0, len(statuses)+1)
	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, status := range statuses {
			placeholders[i] = "?"
			args = append(args, string(status))
		}
		query += " AND t.status IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY t.priority DESC, t.created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	var result []tasks.Task
	for rows.Next() {
		item, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// ListTasksByRecency is intended for bounded client history windows. Unlike
// ListTasks, it deliberately ignores operator priority because terminal history
// should be filled with the most recently updated work after active tasks have
// already been selected separately.
func (s *Store) ListTasksByRecency(ctx context.Context, statuses []tasks.Status, limit int) ([]tasks.Task, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	query := taskSelect + " WHERE t.dismissed = 0"
	args := make([]any, 0, len(statuses)+1)
	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, status := range statuses {
			placeholders[i] = "?"
			args = append(args, string(status))
		}
		query += " AND t.status IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY COALESCE(t.finished_at, t.started_at, t.created_at) DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks by recency: %w", err)
	}
	defer rows.Close()
	var result []tasks.Task
	for rows.Next() {
		item, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) GetTaskGraph(ctx context.Context, rootID string) ([]tasks.Task, []tasks.Dependency, error) {
	rows, err := s.db.QueryContext(ctx, taskSelect+" WHERE t.root_task_id = ? ORDER BY t.created_at", rootID)
	if err != nil {
		return nil, nil, err
	}
	var taskList []tasks.Task
	for rows.Next() {
		item, scanErr := scanTask(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, nil, scanErr
		}
		taskList = append(taskList, item)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	depRows, err := s.db.QueryContext(ctx, `SELECT d.task_id, d.depends_on_task_id
FROM task_dependencies d JOIN tasks t ON t.id = d.task_id WHERE t.root_task_id = ?`, rootID)
	if err != nil {
		return nil, nil, err
	}
	defer depRows.Close()
	var dependencies []tasks.Dependency
	for depRows.Next() {
		var dependency tasks.Dependency
		if err := depRows.Scan(&dependency.TaskID, &dependency.DependsOnTaskID); err != nil {
			return nil, nil, err
		}
		dependencies = append(dependencies, dependency)
	}
	return taskList, dependencies, depRows.Err()
}

func (s *Store) ClaimNextTask(ctx context.Context) (tasks.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tasks.Task{}, err
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, taskSelect+` WHERE t.status = ? AND t.dismissed = 0
AND NOT EXISTS (
  SELECT 1 FROM task_dependencies d JOIN tasks dependency ON dependency.id = d.depends_on_task_id
  WHERE d.task_id = t.id AND dependency.status NOT IN (?, ?, ?, ?, ?)
)
ORDER BY t.priority DESC, t.created_at LIMIT 1`, string(tasks.StatusQueued), string(tasks.StatusCompleted),
		string(tasks.StatusPartiallyCompleted), string(tasks.StatusFailed), string(tasks.StatusCancelled), string(tasks.StatusTimedOut))
	task, err := scanTask(row)
	if err != nil {
		return tasks.Task{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, version = version + 1
WHERE id = ? AND status = ? AND version = ?`, string(tasks.StatusAssigned), task.ID,
		string(tasks.StatusQueued), task.Version)
	if err != nil {
		return tasks.Task{}, fmt.Errorf("claim task: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return tasks.Task{}, ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return tasks.Task{}, err
	}
	task.Status = tasks.StatusAssigned
	task.Version++
	return task, nil
}

func (s *Store) StartTask(ctx context.Context, id string, version int64) (tasks.Task, error) {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = ?, started_at = COALESCE(started_at, ?),
progress = CASE WHEN progress < 1 THEN 1 ELSE progress END, version = version + 1
WHERE id = ? AND status = ? AND version = ?`, string(tasks.StatusRunning), formatTime(now), id,
		string(tasks.StatusAssigned), version)
	if err != nil {
		return tasks.Task{}, fmt.Errorf("start task: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return tasks.Task{}, ErrConflict
	}
	return s.GetTask(ctx, id)
}

func (s *Store) UpdateTaskProgress(ctx context.Context, id string, percent int, message string) (tasks.Task, error) {
	if percent < 0 || percent > 99 {
		return tasks.Task{}, errors.New("running task progress must be between 0 and 99")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET progress = ?, progress_message = ?,
version = version + 1 WHERE id = ? AND status IN (?, ?, ?)`, percent, message, id,
		string(tasks.StatusRunning), string(tasks.StatusWaitingForChildren), string(tasks.StatusCancelRequested))
	if err != nil {
		return tasks.Task{}, fmt.Errorf("update task progress: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return tasks.Task{}, ErrConflict
	}
	return s.GetTask(ctx, id)
}

func (s *Store) CompleteTask(ctx context.Context, id string, resultJSON json.RawMessage, summary string, partial bool) (tasks.Task, error) {
	return s.CompleteTaskWithArtifacts(ctx, id, resultJSON, summary, partial, nil)
}

func (s *Store) CompleteTaskWithArtifacts(ctx context.Context, id string, resultJSON json.RawMessage,
	summary string, partial bool, artifacts []tasks.Artifact) (tasks.Task, error) {
	status := tasks.StatusCompleted
	if partial {
		status = tasks.StatusPartiallyCompleted
	}
	artifactsJSON, err := json.Marshal(artifacts)
	if err != nil {
		return tasks.Task{}, fmt.Errorf("marshal completion artifacts: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tasks.Task{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanTask(tx.QueryRowContext(ctx, taskSelect+" WHERE t.id = ?", id))
	if err != nil {
		return tasks.Task{}, err
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, progress = 100,
progress_message = ?, result_json = ?, user_facing_summary = ?, finished_at = ?,
artifacts_json = ?, version = version + 1 WHERE id = ? AND status IN (?, ?, ?, ?)`, string(status), "Complete",
		nullableRaw(resultJSON), summary, formatTime(now), string(artifactsJSON), id, string(tasks.StatusRunning),
		string(tasks.StatusWaitingForChildren), string(tasks.StatusCancelRequested), string(tasks.StatusAssigned))
	if err != nil {
		return tasks.Task{}, fmt.Errorf("complete task: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return tasks.Task{}, ErrConflict
	}
	if err := insertAuditEntry(ctx, tx, taskTerminalAudit(current, completionAuditAction(current),
		"task", current.ID, string(status), json.RawMessage(`{"result":"persisted"}`))); err != nil {
		return tasks.Task{}, fmt.Errorf("audit task completion: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return tasks.Task{}, err
	}
	return s.GetTask(ctx, id)
}

// CompletionOutcome contains durable records that must become visible in the
// same commit as a root task's terminal status. This is the local outbox
// boundary: a crash can leave all of these absent or all of them committed,
// but can never mark a task complete while losing its final answer.
type CompletionOutcome struct {
	Turn      *conversation.Turn
	Delivery  *delivery.Delivery
	Artifacts []tasks.Artifact
}

func (s *Store) CompleteTaskWithOutcome(ctx context.Context, id string, resultJSON json.RawMessage,
	summary string, partial bool, outcome CompletionOutcome) (tasks.Task, error) {
	status := tasks.StatusCompleted
	if partial {
		status = tasks.StatusPartiallyCompleted
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tasks.Task{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanTask(tx.QueryRowContext(ctx, taskSelect+" WHERE t.id = ?", id))
	if err != nil {
		return tasks.Task{}, err
	}
	if current.ID != current.RootTaskID {
		return tasks.Task{}, errors.New("completion outcome is only valid for a root task")
	}
	now := time.Now().UTC()
	artifactsJSON, err := json.Marshal(outcome.Artifacts)
	if err != nil {
		return tasks.Task{}, fmt.Errorf("marshal completion artifacts: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, progress = 100,
progress_message = ?, result_json = ?, user_facing_summary = ?, finished_at = ?,
artifacts_json = ?, version = version + 1 WHERE id = ? AND status IN (?, ?, ?, ?)`, string(status), "Complete",
		nullableRaw(resultJSON), summary, formatTime(now), string(artifactsJSON), id, string(tasks.StatusRunning),
		string(tasks.StatusWaitingForChildren), string(tasks.StatusCancelRequested), string(tasks.StatusAssigned))
	if err != nil {
		return tasks.Task{}, fmt.Errorf("complete task with outcome: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return tasks.Task{}, ErrConflict
	}
	if outcome.Turn != nil {
		if outcome.Turn.ConversationID == "" || outcome.Turn.ConversationID != current.ConversationID {
			return tasks.Task{}, errors.New("completion turn does not match task conversation")
		}
		var retain bool
		if err := tx.QueryRowContext(ctx, "SELECT transcript_retention FROM conversations WHERE id = ?",
			current.ConversationID).Scan(&retain); err != nil {
			return tasks.Task{}, fmt.Errorf("load transcript policy: %w", err)
		}
		if err := insertTurn(ctx, tx, *outcome.Turn, retain); err != nil {
			return tasks.Task{}, err
		}
	}
	if outcome.Delivery != nil {
		if outcome.Delivery.TaskID != id {
			return tasks.Task{}, errors.New("completion delivery does not match task")
		}
		if err := insertDelivery(ctx, tx, *outcome.Delivery, true); err != nil {
			return tasks.Task{}, fmt.Errorf("insert completion delivery: %w", err)
		}
	}
	if err := insertAuditEntry(ctx, tx, taskTerminalAudit(current, completionAuditAction(current),
		"task", current.ID, string(status), json.RawMessage(`{"result":"persisted"}`))); err != nil {
		return tasks.Task{}, fmt.Errorf("audit completion outcome: %w", err)
	}
	if outcome.Delivery != nil {
		deliveryAudit := taskTerminalAudit(current, "delivery.created", "delivery", outcome.Delivery.ID,
			string(outcome.Delivery.Status), json.RawMessage(`{"target":"connector","content":"redacted"}`))
		deliveryAudit.ConnectorID = outcome.Delivery.Target.ConnectorID
		if err := insertAuditEntry(ctx, tx, deliveryAudit); err != nil {
			return tasks.Task{}, fmt.Errorf("audit completion delivery: %w", err)
		}
	}
	if current.ConversationID != "" {
		var retain bool
		var externalKey string
		if err := tx.QueryRowContext(ctx, `SELECT transcript_retention, external_key
FROM conversations WHERE id = ?`, current.ConversationID).Scan(&retain, &externalKey); err != nil {
			return tasks.Task{}, err
		}
		if !retain {
			if _, err := tx.ExecContext(ctx, "UPDATE events SET payload_json = '{}' WHERE conversation_key = ?", externalKey); err != nil {
				return tasks.Task{}, err
			}
			safe, err := terminalTaskGraphSafeToScrub(ctx, tx, current.RootTaskID)
			if err != nil {
				return tasks.Task{}, err
			}
			if safe {
				if err := scrubTerminalTaskGraphContent(ctx, tx, current.RootTaskID); err != nil {
					return tasks.Task{}, err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return tasks.Task{}, err
	}
	return s.GetTask(ctx, id)
}

func (s *Store) FailTask(ctx context.Context, id string, taskErr error, timedOut bool) (tasks.Task, error) {
	status := tasks.StatusFailed
	if timedOut {
		status = tasks.StatusTimedOut
	}
	errorText := "unknown task failure"
	if taskErr != nil {
		errorText = boundedTaskError(taskErr.Error())
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tasks.Task{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanTask(tx.QueryRowContext(ctx, taskSelect+" WHERE t.id = ?", id))
	if err != nil {
		return tasks.Task{}, err
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, error = ?,
finished_at = ?, version = version + 1 WHERE id = ? AND status NOT IN (?, ?, ?, ?, ?)`,
		string(status), errorText, formatTime(now), id, string(tasks.StatusCompleted),
		string(tasks.StatusPartiallyCompleted), string(tasks.StatusFailed),
		string(tasks.StatusCancelled), string(tasks.StatusTimedOut))
	if err != nil {
		return tasks.Task{}, fmt.Errorf("fail task: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return tasks.Task{}, ErrConflict
	}
	action := "agent.failed"
	if current.TaskType == "shell" {
		action = "tool.task_failed"
	}
	if err := insertAuditEntry(ctx, tx, taskTerminalAudit(current, action, "task", current.ID, string(status),
		json.RawMessage(`{"error":"redacted; inspect task error with authorization"}`))); err != nil {
		return tasks.Task{}, fmt.Errorf("audit task failure: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return tasks.Task{}, err
	}
	return s.GetTask(ctx, id)
}

const maxPersistedTaskErrorBytes = 2048

func boundedTaskError(value string) string {
	value = strings.ToValidUTF8(strings.TrimSpace(value), "�")
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "unknown task failure"
	}
	if len(value) <= maxPersistedTaskErrorBytes {
		return value
	}
	limit := maxPersistedTaskErrorBytes - len("…")
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit] + "…"
}

func completionAuditAction(task tasks.Task) string {
	if task.TaskType == "shell" {
		return "tool.task_completed"
	}
	return "agent.completed"
}

func taskTerminalAudit(task tasks.Task, action, resourceKind, resourceID, decision string,
	details json.RawMessage) observability.AuditEntry {
	return observability.AuditEntry{
		ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: "agent", ActorID: task.AssignedAgentID,
		Action: action, ResourceKind: resourceKind, ResourceID: resourceID, Decision: decision,
		Details: details, CorrelationID: task.CorrelationID, TaskID: task.ID,
		ConversationID: task.ConversationID,
	}
}

func (s *Store) RequeueTaskForAutomaticRetry(ctx context.Context, id string, runErr error) (tasks.Task, error) {
	errorText := "transient agent failure"
	if runErr != nil {
		errorText = boundedTaskError(runErr.Error())
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tasks.Task{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanTask(tx.QueryRowContext(ctx, taskSelect+" WHERE t.id = ?", id))
	if err != nil {
		return tasks.Task{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, progress = 0,
progress_message = ?, started_at = NULL, error = ?, retry_count = retry_count + 1,
version = version + 1 WHERE id = ? AND status = ? AND task_type <> 'shell'
AND retry_count < max_retries`, string(tasks.StatusQueued), "Transient failure; bounded retry queued",
		errorText, id, string(tasks.StatusRunning))
	if err != nil {
		return tasks.Task{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return tasks.Task{}, ErrConflict
	}
	if err := insertAuditEntry(ctx, tx, taskTerminalAudit(current, "agent.retry_scheduled", "task",
		current.ID, "RETRY", json.RawMessage(`{"error":"redacted","bounded":true}`))); err != nil {
		return tasks.Task{}, fmt.Errorf("audit automatic retry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return tasks.Task{}, err
	}
	return s.GetTask(ctx, id)
}

func (s *Store) SetTaskPriority(ctx context.Context, id string, priority int) (tasks.Task, error) {
	if priority < -100 || priority > 100 {
		return tasks.Task{}, errors.New("task priority must be between -100 and 100")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET priority = ?, version = version + 1
WHERE id = ? AND dismissed = 0`, priority, id)
	if err != nil {
		return tasks.Task{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return tasks.Task{}, ErrNotFound
	}
	return s.GetTask(ctx, id)
}

func (s *Store) DismissTask(ctx context.Context, id string) (tasks.Task, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET dismissed = 1, version = version + 1
WHERE id = ? AND dismissed = 0 AND status IN (?, ?, ?, ?, ?)`, id,
		string(tasks.StatusCompleted), string(tasks.StatusPartiallyCompleted), string(tasks.StatusFailed),
		string(tasks.StatusCancelled), string(tasks.StatusTimedOut))
	if err != nil {
		return tasks.Task{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return tasks.Task{}, ErrConflict
	}
	return s.GetTask(ctx, id)
}

func (s *Store) RequestTaskCancellation(ctx context.Context, id string) (tasks.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tasks.Task{}, err
	}
	defer func() { _ = tx.Rollback() }()
	task, err := scanTask(tx.QueryRowContext(ctx, taskSelect+" WHERE t.id = ?", id))
	if err != nil {
		return tasks.Task{}, err
	}
	if task.Status.Terminal() {
		return task, nil
	}
	next := tasks.StatusCancelRequested
	finishedAt := any(nil)
	if task.Status == tasks.StatusCreated || task.Status == tasks.StatusQueued || task.Status == tasks.StatusWaitingForApproval || task.Status == tasks.StatusBlocked {
		next = tasks.StatusCancelled
		finishedAt = formatTime(time.Now().UTC())
	}
	if !tasks.CanTransition(task.Status, next) {
		return tasks.Task{}, fmt.Errorf("%w: cannot cancel task in %s", ErrConflict, task.Status)
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, finished_at = COALESCE(?, finished_at),
version = version + 1 WHERE id = ? AND version = ?`, string(next), finishedAt, id, task.Version)
	if err != nil {
		return tasks.Task{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return tasks.Task{}, ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return tasks.Task{}, err
	}
	return s.GetTask(ctx, id)
}

// RequestTaskGraphCancellation applies cancellation to every non-terminal node
// in the root graph so delegated children cannot outlive the user's intent.
func (s *Store) RequestTaskGraphCancellation(ctx context.Context, id string) (tasks.Task, []string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tasks.Task{}, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	task, err := scanTask(tx.QueryRowContext(ctx, taskSelect+" WHERE t.id = ?", id))
	if err != nil {
		return tasks.Task{}, nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM tasks WHERE root_task_id = ?", task.RootTaskID)
	if err != nil {
		return tasks.Task{}, nil, err
	}
	ids := []string{}
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			_ = rows.Close()
			return tasks.Task{}, nil, err
		}
		ids = append(ids, taskID)
	}
	if err := rows.Close(); err != nil {
		return tasks.Task{}, nil, err
	}
	now := formatTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, finished_at = ?,
version = version + 1 WHERE root_task_id = ? AND status IN (?, ?, ?, ?)`,
		string(tasks.StatusCancelled), now, task.RootTaskID, string(tasks.StatusCreated),
		string(tasks.StatusQueued), string(tasks.StatusWaitingForApproval), string(tasks.StatusBlocked)); err != nil {
		return tasks.Task{}, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, version = version + 1
WHERE root_task_id = ? AND status IN (?, ?, ?)`, string(tasks.StatusCancelRequested),
		task.RootTaskID, string(tasks.StatusAssigned), string(tasks.StatusRunning),
		string(tasks.StatusWaitingForChildren)); err != nil {
		return tasks.Task{}, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE approvals SET status = ?, decided_at = ?,
decided_by = 'task-graph-cancel' WHERE status = ? AND task_id IN (
SELECT id FROM tasks WHERE root_task_id = ?)`, string(approvals.StatusDenied), now,
		string(approvals.StatusPending), task.RootTaskID); err != nil {
		return tasks.Task{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return tasks.Task{}, nil, err
	}
	updated, err := s.GetTask(ctx, id)
	return updated, ids, err
}

func (s *Store) MarkTaskCancelled(ctx context.Context, id string) (tasks.Task, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = ?, finished_at = ?,
version = version + 1 WHERE id = ? AND status = ?`, string(tasks.StatusCancelled),
		formatTime(time.Now().UTC()), id, string(tasks.StatusCancelRequested))
	if err != nil {
		return tasks.Task{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return tasks.Task{}, ErrConflict
	}
	task, err := s.GetTask(ctx, id)
	if err != nil {
		return tasks.Task{}, err
	}
	if err := s.ScrubTerminalTaskGraph(context.WithoutCancel(ctx), task.RootTaskID); err != nil {
		return tasks.Task{}, err
	}
	return task, nil
}

// RecoverInterruptedTasks makes safe work runnable after a daemon restart and
// prevents automatic replay of a state-changing shell invocation with unknown
// outcome.
func (s *Store) RecoverInterruptedTasks(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	now := formatTime(time.Now().UTC())
	const uncertainTaskError = "tool outcome uncertain after restart; manual inspection required"
	const uncertainInvocationError = "tool outcome uncertain after restart"
	uncertain, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, error = ?,
finished_at = ?, version = version + 1 WHERE task_type = 'shell' AND status IN (?, ?, ?)
AND EXISTS (SELECT 1 FROM tool_invocations i WHERE i.task_id = tasks.id AND i.status = 'STARTED')`,
		string(tasks.StatusFailed), uncertainTaskError,
		now, string(tasks.StatusAssigned), string(tasks.StatusRunning), string(tasks.StatusCancelRequested))
	if err != nil {
		return 0, fmt.Errorf("mark uncertain invocations: %w", err)
	}
	terminalizedInvocations, err := tx.ExecContext(ctx, `UPDATE tool_invocations
SET status = 'FAILED', finished_at = ?, exit_code = NULL, output_json = NULL, error = ?
WHERE status = 'STARTED' AND task_id IN (
  SELECT id FROM tasks WHERE task_type = 'shell' AND status = ? AND error = ? AND finished_at = ?
)`, now, uncertainInvocationError, string(tasks.StatusFailed), uncertainTaskError, now)
	if err != nil {
		return 0, fmt.Errorf("terminalize uncertain tool invocations: %w", err)
	}
	uncertainCount, _ := uncertain.RowsAffected()
	terminalizedInvocationCount, _ := terminalizedInvocations.RowsAffected()
	if terminalizedInvocationCount < uncertainCount {
		return 0, fmt.Errorf("terminalize uncertain tool invocations: updated %d invocations for %d tasks",
			terminalizedInvocationCount, uncertainCount)
	}
	cancelled, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, finished_at = ?,
progress_message = ?, version = version + 1 WHERE status = ?`, string(tasks.StatusCancelled),
		now, "Cancellation recovered after restart", string(tasks.StatusCancelRequested))
	if err != nil {
		return 0, fmt.Errorf("recover cancellations: %w", err)
	}
	exhausted, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, error = ?, finished_at = ?,
version = version + 1 WHERE status IN (?, ?) AND retry_count >= max_retries`,
		string(tasks.StatusFailed), "automatic retry limit exhausted during restart recovery", now,
		string(tasks.StatusAssigned), string(tasks.StatusRunning))
	if err != nil {
		return 0, fmt.Errorf("fail exhausted recovered tasks: %w", err)
	}
	recovered, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, progress_message = ?,
retry_count = retry_count + 1, version = version + 1
WHERE status IN (?, ?) AND NOT (task_type = 'shell' AND EXISTS (
SELECT 1 FROM tool_invocations i WHERE i.task_id = tasks.id AND i.status = 'STARTED'))
AND retry_count < max_retries`,
		string(tasks.StatusQueued), "Recovered after restart", string(tasks.StatusAssigned),
		string(tasks.StatusRunning))
	if err != nil {
		return 0, fmt.Errorf("recover tasks: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := s.ScrubTerminalNonRetainedTaskGraphs(context.WithoutCancel(ctx)); err != nil {
		return 0, fmt.Errorf("scrub recovered non-retained tasks: %w", err)
	}
	cancelledCount, _ := cancelled.RowsAffected()
	exhaustedCount, _ := exhausted.RowsAffected()
	recoveredCount, _ := recovered.RowsAffected()
	return uncertainCount + cancelledCount + exhaustedCount + recoveredCount, nil
}

func scanTask(scanner rowScanner) (tasks.Task, error) {
	var task tasks.Task
	var parent, conversationID, startedAt, finishedAt, resultJSON, causation sql.NullString
	var inputJSON, allowedToolsJSON, artifactsJSON, status, createdAt string
	err := scanner.Scan(&task.ID, &parent, &task.RootTaskID, &conversationID,
		&task.Goal, &task.TaskType, &inputJSON, &task.AssignedAgentID, &allowedToolsJSON,
		&task.ApprovalPolicy, &status, &task.Progress, &task.ProgressMessage, &createdAt,
		&startedAt, &finishedAt, &task.RetryCount, &task.MaxRetries, &task.TimeoutSeconds,
		&task.Error, &resultJSON, &task.UserFacingSummary, &artifactsJSON,
		&task.CorrelationID, &causation, &task.IdempotencyKey, &task.Version,
		&task.Priority, &task.Dismissed)
	if errors.Is(err, sql.ErrNoRows) {
		return tasks.Task{}, ErrNotFound
	}
	if err != nil {
		return tasks.Task{}, fmt.Errorf("scan task: %w", err)
	}
	if parent.Valid {
		task.ParentTaskID = &parent.String
	}
	if conversationID.Valid {
		task.ConversationID = conversationID.String
	}
	if causation.Valid {
		task.CausationID = &causation.String
	}
	task.Status = tasks.Status(status)
	task.Input = json.RawMessage(inputJSON)
	if resultJSON.Valid {
		task.Result = json.RawMessage(resultJSON.String)
	}
	if err := json.Unmarshal([]byte(allowedToolsJSON), &task.AllowedTools); err != nil {
		return tasks.Task{}, fmt.Errorf("decode task tools: %w", err)
	}
	if err := json.Unmarshal([]byte(artifactsJSON), &task.Artifacts); err != nil {
		return tasks.Task{}, fmt.Errorf("decode task artifacts: %w", err)
	}
	var parseErr error
	task.CreatedAt, parseErr = parseTime(createdAt)
	if parseErr != nil {
		return tasks.Task{}, parseErr
	}
	task.StartedAt, parseErr = parseOptionalTime(startedAt)
	if parseErr != nil {
		return tasks.Task{}, parseErr
	}
	task.FinishedAt, parseErr = parseOptionalTime(finishedAt)
	if parseErr != nil {
		return tasks.Task{}, parseErr
	}
	return task, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableRaw(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}
