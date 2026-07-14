package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/delivery"
	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/core/tasks"
	coretools "github.com/veqri/veqri/core/tools"
	"github.com/veqri/veqri/internal/ids"
	"github.com/veqri/veqri/internal/stream"
	"github.com/veqri/veqri/tools/shell"
)

type Runtime struct {
	store        *persistence.Store
	registry     *Registry
	shell        *shell.Executor
	hub          *stream.Hub
	logger       *slog.Logger
	workerCount  int
	wake         chan struct{}
	activeMu     sync.Mutex
	active       map[string]context.CancelFunc
	toolGate     func() bool
	agentGate    func(string) bool
	deliveryGate func(string) bool
}

func (r *Runtime) SetExecutionGates(toolAllowed func() bool, agentAllowed func(string) bool) {
	r.toolGate = toolAllowed
	r.agentGate = agentAllowed
}

func (r *Runtime) SetDeliveryGate(connectorAllowed func(string) bool) {
	r.deliveryGate = connectorAllowed
}

func NewRuntime(store *persistence.Store, registry *Registry, shellExecutor *shell.Executor, hub *stream.Hub, logger *slog.Logger, workerCount int) *Runtime {
	return &Runtime{
		store: store, registry: registry, shell: shellExecutor, hub: hub,
		logger: logger, workerCount: workerCount, wake: make(chan struct{}, 1),
		active: make(map[string]context.CancelFunc),
	}
}

func (r *Runtime) Start(ctx context.Context) error {
	recovered, err := r.store.RecoverInterruptedTasks(ctx)
	if err != nil {
		return fmt.Errorf("recover task queue: %w", err)
	}
	if recovered > 0 {
		r.logger.Warn("recovered interrupted tasks", "count", recovered)
	}
	for index := 0; index < r.workerCount; index++ {
		go r.worker(ctx, index)
	}
	go r.approvalExpiryLoop(ctx)
	r.Wake()
	return nil
}

func (r *Runtime) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runtime) Cancel(ctx context.Context, taskID string) (tasks.Task, error) {
	return r.cancel(ctx, taskID, nil)
}

// CancelWithAudit makes the durable graph cancellation and the initiating
// actor's audit entry one persistence transaction before signalling workers.
func (r *Runtime) CancelWithAudit(ctx context.Context, taskID string,
	audit observability.AuditEntry) (tasks.Task, error) {
	return r.cancel(ctx, taskID, &audit)
}

func (r *Runtime) cancel(ctx context.Context, taskID string,
	audit *observability.AuditEntry) (tasks.Task, error) {
	var (
		task    tasks.Task
		taskIDs []string
		err     error
	)
	if audit == nil {
		task, taskIDs, err = r.store.RequestTaskGraphCancellation(ctx, taskID)
	} else {
		task, taskIDs, err = r.store.RequestTaskGraphCancellationWithAudit(ctx, taskID, *audit)
	}
	if err != nil {
		return tasks.Task{}, err
	}
	for _, activeTaskID := range taskIDs {
		r.activeMu.Lock()
		cancel := r.active[activeTaskID]
		r.activeMu.Unlock()
		if cancel != nil {
			cancel()
		}
		if updated, getErr := r.store.GetTask(context.WithoutCancel(ctx), activeTaskID); getErr == nil {
			r.hub.Publish(stream.Event{Type: "task.changed", TaskID: updated.ID,
				ConversationID: updated.ConversationID, CorrelationID: updated.CorrelationID, Payload: updated})
		}
	}
	if approvalList, listErr := r.store.ListApprovalsForTaskGraph(context.WithoutCancel(ctx), task.RootTaskID); listErr == nil {
		for _, approval := range approvalList {
			r.hub.Publish(stream.Event{Type: "approval.changed", TaskID: approval.TaskID,
				CorrelationID: approval.CorrelationID, Payload: approval})
		}
	}
	_ = r.store.ScrubTerminalTaskGraph(context.WithoutCancel(ctx), task.RootTaskID)
	r.hub.Publish(stream.Event{Type: "task.cancel_requested", TaskID: task.ID,
		ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: task})
	return task, nil
}

func (r *Runtime) worker(ctx context.Context, workerID int) {
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.wake:
		}
		for {
			task, err := r.store.ClaimNextTask(ctx)
			if errors.Is(err, persistence.ErrNotFound) || errors.Is(err, persistence.ErrConflict) {
				break
			}
			if err != nil {
				r.logger.Error("claim task", "worker", workerID, "error", err)
				break
			}
			r.process(ctx, task)
		}
	}
}

func (r *Runtime) process(parent context.Context, claimed tasks.Task) {
	task, err := r.store.StartTask(parent, claimed.ID, claimed.Version)
	if err != nil {
		r.logger.Error("start task", "task_id", claimed.ID, "error", err)
		return
	}
	timeout := time.Duration(task.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	r.activeMu.Lock()
	r.active[task.ID] = cancel
	r.activeMu.Unlock()
	defer func() {
		cancel()
		r.activeMu.Lock()
		delete(r.active, task.ID)
		r.activeMu.Unlock()
	}()
	r.hub.Publish(stream.Event{Type: "task.running", TaskID: task.ID,
		ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: task})
	if task.TaskType == "shell" {
		r.processShell(ctx, task)
		return
	}
	r.processAgent(ctx, task)
}

func (r *Runtime) processAgent(ctx context.Context, task tasks.Task) {
	if r.agentGate != nil && !r.agentGate(task.AssignedAgentID) {
		r.finishFailure(ctx, task, errors.New("agent kill switch prevents execution"))
		return
	}
	if err := r.audit(context.WithoutCancel(ctx), task, "agent.started", "task", task.ID, "ALLOW",
		json.RawMessage(`{"execution":"delegated"}`)); err != nil {
		r.logger.Error("mandatory agent start audit failed", "task_id", task.ID, "error", err)
		r.finishFailure(ctx, task, errors.New("mandatory audit unavailable before agent execution"))
		return
	}
	var lastConnectorAnnouncement time.Time
	result, runErr := r.registry.Run(ctx, task, func(progress Progress) {
		updated, err := r.store.UpdateTaskProgress(context.WithoutCancel(ctx), task.ID, progress.Percent, progress.Message)
		if err == nil {
			r.hub.Publish(stream.Event{Type: "task.progress", TaskID: task.ID,
				ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: updated})
			now := time.Now().UTC()
			shouldAnnounce := lastConnectorAnnouncement.IsZero() || now.Sub(lastConnectorAnnouncement) >= 2*time.Second || progress.Percent >= 95
			if routing := routingFromTask(task); routing.ReplyTarget.ConnectorID != "" && shouldAnnounce {
				lastConnectorAnnouncement = now
				r.hub.Publish(stream.Event{Type: "connector.progress", TaskID: task.ID,
					ConversationID: task.ConversationID, CorrelationID: task.CorrelationID,
					Payload: map[string]any{"target": routing.ReplyTarget, "agent_id": task.AssignedAgentID,
						"progress": progress.Percent, "message": progress.Message}})
			}
		}
	})
	if runErr != nil {
		if r.registry.Retryable(task.AssignedAgentID, runErr) {
			retried, retryErr := r.store.RequeueTaskForAutomaticRetry(context.WithoutCancel(ctx), task.ID, runErr)
			if retryErr == nil {
				r.hub.Publish(stream.Event{Type: "task.retry_scheduled", TaskID: retried.ID,
					ConversationID: retried.ConversationID, CorrelationID: retried.CorrelationID, Payload: retried})
				r.Wake()
				return
			}
		}
		r.finishFailure(ctx, task, runErr)
		return
	}
	_, err := r.completeAndPublish(context.WithoutCancel(ctx), task, result.Structured,
		result.WrittenSummary, result.SpokenSummary, result.Partial, result.Artifacts)
	if err != nil {
		r.logger.Error("complete task", "task_id", task.ID, "error", err)
		r.finishFailure(ctx, task, fmt.Errorf("persist audited task completion: %w", err))
		return
	}
}

func (r *Runtime) processShell(ctx context.Context, task tasks.Task) {
	if r.toolGate != nil && !r.toolGate() {
		r.finishFailure(ctx, task, errors.New("emergency stop prevents new tool execution"))
		return
	}
	if r.shell == nil {
		r.finishFailure(ctx, task, errors.New("shell executor is not configured"))
		return
	}
	parsed, risk, err := r.shell.ParseAndValidate(task.Input)
	if err != nil {
		r.finishFailure(ctx, task, err)
		return
	}
	// Reclassify at the last possible boundary. This prevents an approved task
	// persisted by an older binary from executing after a security upgrade adds
	// its executable (for example Windows runas.exe) to the privilege denylist.
	if risk == coretools.RiskPrivileged {
		r.finishFailure(ctx, task, errors.New("privilege escalation is denied at execution time"))
		return
	}
	canonicalInput, err := json.Marshal(parsed)
	if err != nil {
		r.finishFailure(ctx, task, errors.New("canonical shell invocation could not be encoded"))
		return
	}
	// Every executable approval created by the current API persists this exact
	// canonical representation: absolute symlink-resolved path plus content
	// digest. A legacy task containing a PATH lookup or missing digest has no
	// approval bound to the executable that would run now, so fail closed.
	if !parsed.DryRun && !bytes.Equal(task.Input, canonicalInput) {
		r.finishFailure(ctx, task, errors.New("approved shell invocation is not canonical; request a new approval"))
		return
	}
	invocation := coretools.Invocation{
		ID: ids.New(), TaskID: task.ID, ToolName: "shell", Input: canonicalInput,
		Risk: risk, CorrelationID: task.CorrelationID, IdempotencyKey: "shell:" + task.ID,
	}
	started, duplicate, err := r.store.StartToolInvocation(context.WithoutCancel(ctx), invocation)
	if err != nil {
		r.finishFailure(ctx, task, err)
		return
	}
	if duplicate {
		if started.Status == "COMPLETED" {
			_, completeErr := r.completeAndPublish(context.WithoutCancel(ctx), task, started.Output,
				"Command already completed", "The command completed.", false, nil)
			if completeErr != nil {
				r.logger.Error("complete recovered shell task", "task_id", task.ID, "error", completeErr)
				r.finishFailure(ctx, task, fmt.Errorf("persist audited recovered shell completion: %w", completeErr))
			}
			return
		}
		r.finishFailure(ctx, task, errors.New("tool invocation already started; refusing duplicate execution"))
		return
	}
	output, executeErr := r.shell.Execute(ctx, canonicalInput, func(progress coretools.Progress) {
		r.hub.Publish(stream.Event{Type: "tool.output", TaskID: task.ID,
			ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: progress})
	})
	exitCode := extractExitCode(output)
	_, finishErr := r.store.FinishToolInvocation(context.WithoutCancel(ctx), started.ID, output, exitCode, executeErr)
	if finishErr != nil {
		r.finishFailure(ctx, task, finishErr)
		return
	}
	if executeErr != nil {
		r.finishFailure(ctx, task, executeErr)
		return
	}
	summary := fmt.Sprintf("Command completed with exit code %d", exitCode)
	_, err = r.completeAndPublish(context.WithoutCancel(ctx), task, output, summary, summary+".", false, nil)
	if err != nil {
		r.logger.Error("complete shell task", "task_id", task.ID, "error", err)
		r.finishFailure(ctx, task, fmt.Errorf("persist audited shell completion: %w", err))
		return
	}
}

func (r *Runtime) finishFailure(ctx context.Context, task tasks.Task, taskErr error) {
	if errors.Is(taskErr, context.Canceled) {
		cancelled, err := r.store.MarkTaskCancelled(context.WithoutCancel(ctx), task.ID)
		if err == nil {
			_ = r.store.ScrubTerminalTaskGraph(context.WithoutCancel(ctx), cancelled.RootTaskID)
			r.hub.Publish(stream.Event{Type: "task.cancelled", TaskID: task.ID,
				ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: cancelled})
		}
		return
	}
	failed, err := r.store.FailTask(context.WithoutCancel(ctx), task.ID, taskErr, errors.Is(taskErr, context.DeadlineExceeded))
	if err != nil {
		r.logger.Error("fail task", "task_id", task.ID, "task_error_class", fmt.Sprintf("%T", taskErr), "persistence_error", err)
		return
	}
	_ = r.store.ScrubTerminalTaskGraph(context.WithoutCancel(ctx), failed.RootTaskID)
	r.hub.Publish(stream.Event{Type: "task.failed", TaskID: task.ID,
		ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: failed})
}

func (r *Runtime) completeAndPublish(ctx context.Context, task tasks.Task, resultJSON json.RawMessage,
	written, spoken string, partial bool, artifacts []tasks.Artifact) (tasks.Task, error) {
	if task.ID != task.RootTaskID {
		completed, err := r.store.CompleteTaskWithArtifacts(ctx, task.ID, resultJSON, written, partial, artifacts)
		if err != nil {
			return tasks.Task{}, err
		}
		r.hub.Publish(stream.Event{Type: "task.completed", TaskID: task.ID,
			ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: completed})
		r.Wake()
		return completed, nil
	}
	now := time.Now().UTC()
	attemptSuffix := taskOutcomeAttemptSuffix(task)
	var finalTurn *conversation.Turn
	if task.ConversationID != "" {
		turn := conversation.Turn{ID: "turn:assistant:" + task.ID + attemptSuffix, ConversationID: task.ConversationID,
			Role: conversation.RoleAssistant, Text: written, Final: true,
			CorrelationID: task.CorrelationID, CreatedAt: now}
		finalTurn = &turn
	}
	finalDelivery := connectorDelivery(task, now, r.deliveryGate)
	completed, err := r.store.CompleteTaskWithOutcome(ctx, task.ID, resultJSON, written, partial,
		persistence.CompletionOutcome{Turn: finalTurn, Delivery: finalDelivery, Artifacts: artifacts})
	if err != nil {
		return tasks.Task{}, err
	}
	if finalTurn != nil {
		r.hub.Publish(stream.Event{Type: "conversation.turn.final", TaskID: task.ID,
			ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: *finalTurn})
	}
	r.hub.Publish(stream.Event{Type: "task.completed", TaskID: task.ID,
		ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: completed})
	if finalDelivery != nil {
		eventType := "delivery.pending"
		if finalDelivery.Status == delivery.StatusDelivered {
			eventType = "connector.reply"
		}
		r.hub.Publish(stream.Event{Type: eventType, TaskID: task.ID,
			ConversationID: task.ConversationID, CorrelationID: task.CorrelationID,
			Payload: map[string]any{"delivery": *finalDelivery, "text": written,
				"simulated": finalDelivery.DeliveredAt != nil}})
	}
	if spoken != "" {
		r.hub.Publish(stream.Event{Type: "voice.tts.text", TaskID: task.ID,
			ConversationID: task.ConversationID, CorrelationID: task.CorrelationID,
			Payload: map[string]any{"text": spoken, "simulated": true}})
	}
	r.Wake()
	return completed, nil
}

type taskRouting struct {
	Source      events.Source      `json:"source"`
	ReplyTarget events.ReplyTarget `json:"reply_target"`
}

func routingFromTask(task tasks.Task) taskRouting {
	var routing taskRouting
	_ = json.Unmarshal(task.Input, &routing)
	return routing
}

func connectorDelivery(task tasks.Task, now time.Time, connectorAllowed func(string) bool) *delivery.Delivery {
	routing := routingFromTask(task)
	if routing.ReplyTarget.ConnectorID == "" {
		return nil
	}
	deliveryStatus := delivery.StatusPending
	var deliveredAt *time.Time
	allowed := connectorAllowed == nil || connectorAllowed(routing.ReplyTarget.ConnectorID)
	if allowed && routing.ReplyTarget.ConnectorID == routing.Source.Kind+"-simulator" &&
		(routing.Source.Kind == "slack" || routing.Source.Kind == "mattermost" || routing.Source.Kind == "teams") {
		deliveryStatus = delivery.StatusDelivered
		deliveredAt = &now
	}
	attemptSuffix := taskOutcomeAttemptSuffix(task)
	item := delivery.Delivery{
		ID: "delivery:" + task.ID + ":connector-final" + attemptSuffix, TaskID: task.ID,
		Target: delivery.Target{Kind: routing.Source.Kind,
			ConnectorID: routing.ReplyTarget.ConnectorID, ChannelID: routing.ReplyTarget.ChannelID,
			ThreadID: routing.ReplyTarget.ThreadID},
		Priority: 50, Status: deliveryStatus, IdempotencyKey: "task:" + task.ID + ":connector-final" + attemptSuffix,
		CreatedAt: now, DeliveredAt: deliveredAt, CorrelationID: task.CorrelationID,
	}
	if !allowed {
		item.LastError = "connector kill switch active; delivery retained for inspection"
	}
	return &item
}

// Initial outcomes retain their established identifiers. A retried attempt gets
// a distinct durable turn and delivery identity so its result cannot be hidden
// by INSERT OR IGNORE records from an earlier partial completion.
func taskOutcomeAttemptSuffix(task tasks.Task) string {
	if task.RetryCount <= 0 {
		return ""
	}
	return fmt.Sprintf(":retry:%d", task.RetryCount)
}

func (r *Runtime) audit(ctx context.Context, task tasks.Task, action, kind, resourceID, decision string, details json.RawMessage) error {
	return r.store.AddAuditEntry(ctx, observability.AuditEntry{
		ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: "agent",
		ActorID: task.AssignedAgentID, Action: action, ResourceKind: kind,
		ResourceID: resourceID, Decision: decision, Details: details,
		CorrelationID: task.CorrelationID, TaskID: task.ID, ConversationID: task.ConversationID,
	})
}

func (r *Runtime) approvalExpiryLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if changes, err := r.store.ExpireApprovals(ctx); err != nil {
				r.logger.Error("expire approvals", "error", err)
			} else {
				for _, change := range changes {
					r.hub.Publish(stream.Event{Type: "approval.expired", TaskID: change.Task.ID,
						ConversationID: change.Task.ConversationID, CorrelationID: change.Approval.CorrelationID,
						Payload: map[string]any{"approval": change.Approval, "task": change.Task}})
					r.hub.Publish(stream.Event{Type: "task.timed_out", TaskID: change.Task.ID,
						ConversationID: change.Task.ConversationID, CorrelationID: change.Task.CorrelationID,
						Payload: change.Task})
				}
			}
		}
	}
}

func extractExitCode(output json.RawMessage) int {
	var value struct {
		ExitCode int `json:"exit_code"`
	}
	_ = json.Unmarshal(output, &value)
	return value.ExitCode
}
