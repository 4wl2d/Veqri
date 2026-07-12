package persistence

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/core/tasks"
)

// AskWork is the durable ingress boundary for a user or connector request.
// The event, conversation policy, user turn, task graph, dependencies, and
// processed marker are committed as one SQLite transaction.
type AskWork struct {
	Event          events.Envelope
	Conversation   conversation.Conversation
	ApplyRetention bool
	Turn           conversation.Turn
	Tasks          []tasks.Task
	Dependencies   []tasks.Dependency
	Audit          *observability.AuditEntry
}

func (s *Store) CreateAskWork(ctx context.Context, work AskWork) (conversation.Conversation, tasks.Task, bool, error) {
	if len(work.Tasks) == 0 {
		return conversation.Conversation{}, tasks.Task{}, false, errors.New("ask work requires a task graph")
	}
	if err := work.Event.Validate(); err != nil {
		return conversation.Conversation{}, tasks.Task{}, false, err
	}
	recoverExisting := false
	if existingEvent, err := s.GetEventBySourceIdempotency(ctx, work.Event.Source, work.Event.IdempotencyKey); err == nil {
		if existingTask, taskErr := s.GetTaskByIdempotencyKey(ctx, "event:"+existingEvent.ID+":root"); taskErr == nil {
			existingConversation, conversationErr := s.GetConversation(ctx, existingTask.ConversationID)
			return existingConversation, existingTask, true, conversationErr
		}
		if existingEvent.Type != work.Event.Type || existingEvent.ConversationKey != work.Event.ConversationKey ||
			!bytes.Equal(existingEvent.Payload, work.Event.Payload) {
			return conversation.Conversation{}, tasks.Task{}, true,
				fmt.Errorf("%w: idempotency key reused with different event content", ErrConflict)
		}
		work.Event = existingEvent
		recoverExisting = true
		normalizeRecoveredAskWork(&work)
	} else if !errors.Is(err, ErrNotFound) {
		return conversation.Conversation{}, tasks.Task{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return conversation.Conversation{}, tasks.Task{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if !recoverExisting {
		if err := insertEvent(ctx, tx, work.Event); err != nil {
			_ = tx.Rollback()
			existingEvent, lookupErr := s.GetEventBySourceIdempotency(ctx, work.Event.Source, work.Event.IdempotencyKey)
			if lookupErr == nil {
				existingTask, taskErr := s.GetTaskByIdempotencyKey(ctx, "event:"+existingEvent.ID+":root")
				if taskErr == nil {
					existingConversation, conversationErr := s.GetConversation(ctx, existingTask.ConversationID)
					return existingConversation, existingTask, true, conversationErr
				}
				return conversation.Conversation{}, tasks.Task{}, true,
					fmt.Errorf("%w: event is pending recovery", ErrConflict)
			}
			return conversation.Conversation{}, tasks.Task{}, false, fmt.Errorf("insert ask event: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(
id, external_key, title, transcript_retention, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(external_key) DO UPDATE SET
updated_at = excluded.updated_at,
title = CASE WHEN conversations.transcript_retention = 1 AND conversations.title = ?
THEN excluded.title ELSE conversations.title END`,
		work.Conversation.ID, work.Conversation.ExternalKey, work.Conversation.Title,
		work.Conversation.TranscriptRetention, formatTime(work.Conversation.CreatedAt),
		formatTime(work.Conversation.UpdatedAt), expiredContentMarker); err != nil {
		return conversation.Conversation{}, tasks.Task{}, false, fmt.Errorf("upsert ask conversation: %w", err)
	}
	actualConversation, err := s.scanConversation(tx.QueryRowContext(ctx, `SELECT id, external_key, title,
transcript_retention, created_at, updated_at FROM conversations WHERE external_key = ?`, work.Conversation.ExternalKey))
	if err != nil {
		return conversation.Conversation{}, tasks.Task{}, false, err
	}
	if work.ApplyRetention {
		// An explicit disabled policy always runs the canonical scrub, even if
		// an older build already flipped the flag without removing every copy.
		if !work.Conversation.TranscriptRetention {
			if err := disableTranscriptRetentionTx(ctx, tx, actualConversation.ID,
				actualConversation.ExternalKey, work.Conversation.UpdatedAt); err != nil {
				return conversation.Conversation{}, tasks.Task{}, false, err
			}
			actualConversation.Title = "[transcript retention disabled]"
		} else if !actualConversation.TranscriptRetention {
			if _, err := tx.ExecContext(ctx, `UPDATE conversations SET transcript_retention = 1, updated_at = ? WHERE id = ?`,
				formatTime(work.Conversation.UpdatedAt), actualConversation.ID); err != nil {
				return conversation.Conversation{}, tasks.Task{}, false, err
			}
		}
		actualConversation.TranscriptRetention = work.Conversation.TranscriptRetention
	}
	work.Turn.ConversationID = actualConversation.ID
	if err := insertTurn(ctx, tx, work.Turn, actualConversation.TranscriptRetention); err != nil {
		return conversation.Conversation{}, tasks.Task{}, false, err
	}
	root := work.Tasks[0]
	for index := range work.Tasks {
		work.Tasks[index].ConversationID = actualConversation.ID
		if work.Tasks[index].RootTaskID != root.ID {
			return conversation.Conversation{}, tasks.Task{}, false,
				errors.New("all ask graph nodes must reference the first task as root")
		}
		if err := insertTask(ctx, tx, work.Tasks[index]); err != nil {
			return conversation.Conversation{}, tasks.Task{}, false,
				fmt.Errorf("insert ask task %s: %w", work.Tasks[index].ID, err)
		}
	}
	for _, dependency := range work.Dependencies {
		if dependency.TaskID == dependency.DependsOnTaskID {
			return conversation.Conversation{}, tasks.Task{}, false, errors.New("task cannot depend on itself")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_dependencies(task_id, depends_on_task_id) VALUES(?, ?)`,
			dependency.TaskID, dependency.DependsOnTaskID); err != nil {
			return conversation.Conversation{}, tasks.Task{}, false, fmt.Errorf("insert ask dependency: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE events SET processing_status = 'PROCESSED',
processing_error = '', processed_at = ? WHERE id = ?`, formatTime(work.Event.ReceivedAt), work.Event.ID); err != nil {
		return conversation.Conversation{}, tasks.Task{}, false, err
	}
	if work.Audit != nil {
		work.Audit.TaskID = root.ID
		work.Audit.ConversationID = actualConversation.ID
		work.Audit.CorrelationID = work.Event.CorrelationID
		if err := insertAuditEntry(ctx, tx, *work.Audit); err != nil {
			return conversation.Conversation{}, tasks.Task{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return conversation.Conversation{}, tasks.Task{}, false, err
	}
	root = work.Tasks[0]
	root.ConversationID = actualConversation.ID
	return actualConversation, root, false, nil
}

func normalizeRecoveredAskWork(work *AskWork) {
	eventID := work.Event.ID
	work.Turn.ID = "turn:user:" + eventID
	work.Turn.CorrelationID = work.Event.CorrelationID
	work.Turn.CreatedAt = work.Event.OccurredAt
	if work.Audit != nil {
		work.Audit.ResourceID = eventID
	}
	for index := range work.Tasks {
		work.Tasks[index].CorrelationID = work.Event.CorrelationID
		work.Tasks[index].CausationID = &eventID
		if index == 0 {
			work.Tasks[index].IdempotencyKey = "event:" + eventID + ":root"
		} else {
			work.Tasks[index].IdempotencyKey = fmt.Sprintf("event:%s:agent:%d", eventID, index-1)
		}
	}
}
