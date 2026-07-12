package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/internal/ids"
)

const expiredContentMarker = "[expired by retention policy]"

const eligibleRetentionRoots = `WITH eligible_roots(root_task_id) AS (
  SELECT graph.root_task_id
  FROM tasks graph
  GROUP BY graph.root_task_id
  HAVING SUM(CASE WHEN graph.status NOT IN ('COMPLETED','PARTIALLY_COMPLETED','FAILED','CANCELLED','TIMED_OUT') THEN 1 ELSE 0 END) = 0
  AND MAX(julianday(COALESCE(graph.finished_at, graph.created_at))) < julianday(?)
  AND NOT EXISTS (
    SELECT 1 FROM tasks delivery_task JOIN deliveries d ON d.task_id = delivery_task.id
    WHERE delivery_task.root_task_id = graph.root_task_id AND d.status NOT IN ('DELIVERED','FAILED')
  )
  AND NOT EXISTS (
    SELECT 1 FROM tasks invocation_task JOIN tool_invocations invocation ON invocation.task_id = invocation_task.id
    WHERE invocation_task.root_task_id = graph.root_task_id AND invocation.status NOT IN ('COMPLETED','FAILED')
  )
  AND NOT EXISTS (
    SELECT 1 FROM tasks approval_task JOIN approvals approval ON approval.task_id = approval_task.id
    WHERE approval_task.root_task_id = graph.root_task_id AND approval.status NOT IN ('DENIED','EXPIRED','CONSUMED')
  )
) `

// RetentionSweepResult contains counts only. It deliberately carries no
// deleted identifiers or content so it is safe to persist in the audit log.
type RetentionSweepResult struct {
	Cutoff                  time.Time `json:"cutoff"`
	SweptAt                 time.Time `json:"swept_at"`
	TurnsDeleted            int64     `json:"turns_deleted"`
	ConversationsScrubbed   int64     `json:"conversations_scrubbed"`
	EventsScrubbed          int64     `json:"events_scrubbed"`
	TasksScrubbed           int64     `json:"tasks_scrubbed"`
	ToolInvocationsScrubbed int64     `json:"tool_invocations_scrubbed"`
	ApprovalsScrubbed       int64     `json:"approvals_scrubbed"`
	DeliveriesScrubbed      int64     `json:"deliveries_scrubbed"`
	ArtifactMetadataDeleted int64     `json:"artifact_metadata_deleted"`
	AuditEntriesDeleted     int64     `json:"audit_entries_deleted"`
}

// ApplyRetentionSweep atomically removes or scrubs content strictly older
// than cutoff. It never changes conversations.transcript_retention, so a
// retained conversation continues retaining future turns after old turns
// expire. Active graphs and terminal graphs with pending approval, delivery,
// or uncertain tool work are excluded.
func (s *Store) ApplyRetentionSweep(ctx context.Context, cutoff, sweptAt time.Time) (RetentionSweepResult, error) {
	result := RetentionSweepResult{Cutoff: cutoff.UTC(), SweptAt: sweptAt.UTC()}
	if cutoff.IsZero() || sweptAt.IsZero() {
		return RetentionSweepResult{}, errors.New("retention cutoff and sweep time are required")
	}
	if cutoff.After(sweptAt) {
		return RetentionSweepResult{}, errors.New("retention cutoff cannot be after sweep time")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RetentionSweepResult{}, fmt.Errorf("begin retention sweep: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	cutoffValue := formatTime(cutoff)

	result.TurnsDeleted, err = retentionExec(ctx, tx, `DELETE FROM turns
WHERE julianday(created_at) < julianday(?)
AND NOT EXISTS (
  SELECT 1 FROM tasks protected
  WHERE protected.conversation_id = turns.conversation_id AND (
    protected.status NOT IN ('COMPLETED','PARTIALLY_COMPLETED','FAILED','CANCELLED','TIMED_OUT')
    OR EXISTS (
      SELECT 1 FROM tasks delivery_task JOIN deliveries d ON d.task_id = delivery_task.id
      WHERE delivery_task.root_task_id = protected.root_task_id AND d.status NOT IN ('DELIVERED','FAILED')
    )
    OR EXISTS (
      SELECT 1 FROM tasks invocation_task JOIN tool_invocations invocation ON invocation.task_id = invocation_task.id
      WHERE invocation_task.root_task_id = protected.root_task_id AND invocation.status NOT IN ('COMPLETED','FAILED')
    )
    OR EXISTS (
      SELECT 1 FROM tasks approval_task JOIN approvals approval ON approval.task_id = approval_task.id
      WHERE approval_task.root_task_id = protected.root_task_id AND approval.status NOT IN ('DENIED','EXPIRED','CONSUMED')
    )
  )
)`, cutoffValue)
	if err != nil {
		return RetentionSweepResult{}, fmt.Errorf("expire transcript turns: %w", err)
	}

	result.ConversationsScrubbed, err = retentionExec(ctx, tx, `UPDATE conversations
SET title = ?
WHERE transcript_retention = 1 AND title <> ? AND julianday(updated_at) < julianday(?)
AND NOT EXISTS (
  SELECT 1 FROM tasks protected
  WHERE protected.conversation_id = conversations.id AND (
    protected.status NOT IN ('COMPLETED','PARTIALLY_COMPLETED','FAILED','CANCELLED','TIMED_OUT')
    OR EXISTS (
      SELECT 1 FROM tasks delivery_task JOIN deliveries d ON d.task_id = delivery_task.id
      WHERE delivery_task.root_task_id = protected.root_task_id AND d.status NOT IN ('DELIVERED','FAILED')
    )
    OR EXISTS (
      SELECT 1 FROM tasks invocation_task JOIN tool_invocations invocation ON invocation.task_id = invocation_task.id
      WHERE invocation_task.root_task_id = protected.root_task_id AND invocation.status NOT IN ('COMPLETED','FAILED')
    )
    OR EXISTS (
      SELECT 1 FROM tasks approval_task JOIN approvals approval ON approval.task_id = approval_task.id
      WHERE approval_task.root_task_id = protected.root_task_id AND approval.status NOT IN ('DENIED','EXPIRED','CONSUMED')
    )
  )
)`, expiredContentMarker, expiredContentMarker, cutoffValue)
	if err != nil {
		return RetentionSweepResult{}, fmt.Errorf("scrub expired conversation titles: %w", err)
	}

	result.EventsScrubbed, err = retentionExec(ctx, tx, `UPDATE events
SET payload_json = '{}', reply_target_json = '{}', actor_display_name = '', processing_error = ''
WHERE processing_status <> 'PENDING' AND julianday(received_at) < julianday(?)
AND (payload_json <> '{}' OR reply_target_json <> '{}' OR actor_display_name <> '' OR COALESCE(processing_error, '') <> '')
AND NOT EXISTS (
  SELECT 1 FROM tasks protected LEFT JOIN conversations conversation ON conversation.id = protected.conversation_id
  WHERE (protected.causation_id = events.id OR conversation.external_key = events.conversation_key)
  AND (
    protected.status NOT IN ('COMPLETED','PARTIALLY_COMPLETED','FAILED','CANCELLED','TIMED_OUT')
    OR EXISTS (
      SELECT 1 FROM tasks delivery_task JOIN deliveries d ON d.task_id = delivery_task.id
      WHERE delivery_task.root_task_id = protected.root_task_id AND d.status NOT IN ('DELIVERED','FAILED')
    )
    OR EXISTS (
      SELECT 1 FROM tasks invocation_task JOIN tool_invocations invocation ON invocation.task_id = invocation_task.id
      WHERE invocation_task.root_task_id = protected.root_task_id AND invocation.status NOT IN ('COMPLETED','FAILED')
    )
    OR EXISTS (
      SELECT 1 FROM tasks approval_task JOIN approvals approval ON approval.task_id = approval_task.id
      WHERE approval_task.root_task_id = protected.root_task_id AND approval.status NOT IN ('DENIED','EXPIRED','CONSUMED')
    )
  )
)`, cutoffValue)
	if err != nil {
		return RetentionSweepResult{}, fmt.Errorf("scrub expired event content: %w", err)
	}

	result.ArtifactMetadataDeleted, err = retentionExec(ctx, tx, eligibleRetentionRoots+`DELETE FROM artifacts
WHERE task_id IN (
  SELECT task.id FROM tasks task WHERE task.root_task_id IN (SELECT root_task_id FROM eligible_roots)
)`, cutoffValue)
	if err != nil {
		return RetentionSweepResult{}, fmt.Errorf("delete expired artifact metadata: %w", err)
	}

	result.ToolInvocationsScrubbed, err = retentionExec(ctx, tx, eligibleRetentionRoots+`UPDATE tool_invocations
SET input_json = '{}', output_json = NULL, error = ''
WHERE task_id IN (
  SELECT task.id FROM tasks task WHERE task.root_task_id IN (SELECT root_task_id FROM eligible_roots)
) AND (input_json <> '{}' OR output_json IS NOT NULL OR error <> '')`, cutoffValue)
	if err != nil {
		return RetentionSweepResult{}, fmt.Errorf("scrub expired tool invocations: %w", err)
	}

	result.ApprovalsScrubbed, err = retentionExec(ctx, tx, eligibleRetentionRoots+`UPDATE approvals
SET tool_arguments_json = '{}', reason = ?
WHERE task_id IN (
  SELECT task.id FROM tasks task WHERE task.root_task_id IN (SELECT root_task_id FROM eligible_roots)
) AND (tool_arguments_json <> '{}' OR reason <> ?)`, cutoffValue, expiredContentMarker, expiredContentMarker)
	if err != nil {
		return RetentionSweepResult{}, fmt.Errorf("scrub expired approvals: %w", err)
	}

	result.DeliveriesScrubbed, err = retentionExec(ctx, tx, eligibleRetentionRoots+`UPDATE deliveries
SET target_json = '{}', last_error = ''
WHERE task_id IN (
  SELECT task.id FROM tasks task WHERE task.root_task_id IN (SELECT root_task_id FROM eligible_roots)
) AND status IN ('DELIVERED','FAILED') AND (target_json <> '{}' OR last_error <> '')`, cutoffValue)
	if err != nil {
		return RetentionSweepResult{}, fmt.Errorf("scrub expired deliveries: %w", err)
	}

	result.TasksScrubbed, err = retentionExec(ctx, tx, eligibleRetentionRoots+`UPDATE tasks
SET goal = ?, input_json = '{}', progress_message = '', error = '', result_json = NULL,
    user_facing_summary = ?, artifacts_json = '[]'
WHERE root_task_id IN (SELECT root_task_id FROM eligible_roots)
AND (goal <> ? OR input_json <> '{}' OR progress_message <> '' OR error <> '' OR result_json IS NOT NULL
     OR user_facing_summary <> ? OR artifacts_json <> '[]')`, cutoffValue,
		expiredContentMarker, expiredContentMarker, expiredContentMarker, expiredContentMarker)
	if err != nil {
		return RetentionSweepResult{}, fmt.Errorf("scrub expired task content: %w", err)
	}

	result.AuditEntriesDeleted, err = retentionExec(ctx, tx, eligibleRetentionRoots+`DELETE FROM audit_entries
WHERE julianday(occurred_at) < julianday(?)
AND (
  (task_id IS NULL AND NOT EXISTS (
    SELECT 1 FROM tasks correlated
    WHERE correlated.correlation_id = audit_entries.correlation_id
    AND correlated.root_task_id NOT IN (SELECT root_task_id FROM eligible_roots)
  ))
  OR task_id IN (
    SELECT task.id FROM tasks task WHERE task.root_task_id IN (SELECT root_task_id FROM eligible_roots)
  )
)`, cutoffValue, cutoffValue)
	if err != nil {
		return RetentionSweepResult{}, fmt.Errorf("expire audit entries: %w", err)
	}

	details, err := json.Marshal(result)
	if err != nil {
		return RetentionSweepResult{}, fmt.Errorf("encode retention sweep audit: %w", err)
	}
	correlationID := ids.New()
	if err := insertAuditEntry(ctx, tx, observability.AuditEntry{
		ID: ids.New(), OccurredAt: sweptAt.UTC(), ActorKind: "system", ActorID: "core",
		Action: "retention.sweep", ResourceKind: "core", ResourceID: "local-core",
		Decision: "ALLOW", Details: details, CorrelationID: correlationID,
	}); err != nil {
		return RetentionSweepResult{}, fmt.Errorf("audit retention sweep: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RetentionSweepResult{}, fmt.Errorf("commit retention sweep: %w", err)
	}
	return result, nil
}

func retentionExec(ctx context.Context, tx *sql.Tx, query string, arguments ...any) (int64, error) {
	result, err := tx.ExecContext(ctx, query, arguments...)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return count, nil
}
