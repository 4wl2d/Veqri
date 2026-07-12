package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/internal/auth"
	"github.com/veqri/veqri/internal/ids"
	"github.com/veqri/veqri/internal/stream"
)

type AskRequest struct {
	Text             string   `json:"text"`
	ConversationKey  string   `json:"conversation_key,omitempty"`
	IdempotencyKey   string   `json:"idempotency_key,omitempty"`
	AgentIDs         []string `json:"agent_ids,omitempty"`
	RetainTranscript *bool    `json:"retain_transcript,omitempty"`
}

type askContext struct {
	Request     AskRequest
	Source      events.Source
	Actor       events.Actor
	Trust       events.TrustLevel
	ReplyTarget events.ReplyTarget
	OccurredAt  time.Time
}

func (s *Server) handleAsk(writer http.ResponseWriter, request *http.Request) {
	var body AskRequest
	if !decodeJSON(writer, request, &body) {
		return
	}
	principal := principalFromContext(request.Context())
	if body.IdempotencyKey == "" {
		body.IdempotencyKey = request.Header.Get("Idempotency-Key")
	}
	if body.ConversationKey == "" {
		body.ConversationKey = principal.Kind + ":" + principal.ID + ":default"
	}
	trust := events.TrustLocal
	if principal.Kind == "device" {
		trust = events.TrustTrusted
	}
	task, duplicate, err := s.submitAsk(request.Context(), askContext{
		Request: body, Source: events.Source{Kind: principal.Kind},
		Actor: events.Actor{ID: principal.ID}, Trust: trust, OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		writeError(writer, http.StatusBadRequest, "ask_rejected", err.Error())
		return
	}
	status := http.StatusAccepted
	if duplicate {
		status = http.StatusOK
	}
	writeJSON(writer, status, map[string]any{"task": task, "duplicate": duplicate})
}

func (s *Server) submitAsk(ctx context.Context, input askContext) (tasks.Task, bool, error) {
	text := strings.TrimSpace(input.Request.Text)
	if text == "" || len(text) > 100_000 {
		return tasks.Task{}, false, errors.New("text must contain 1 to 100000 characters")
	}
	if input.Request.ConversationKey == "" {
		return tasks.Task{}, false, errors.New("conversation_key is required")
	}
	if input.Source.ConnectorID != "" && s.policy.ConnectorDisabled(input.Source.ConnectorID) {
		return tasks.Task{}, false, errors.New("connector kill switch is active")
	}
	agentIDs := input.Request.AgentIDs
	if len(agentIDs) == 0 {
		agentIDs = []string{"builtin.general"}
	}
	if len(agentIDs) > 8 {
		return tasks.Task{}, false, errors.New("at most eight agents may be delegated in one request")
	}
	available := make(map[string]bool)
	for _, definition := range s.registry.Definitions() {
		available[definition.ID] = true
	}
	for _, agentID := range agentIDs {
		if !available[agentID] {
			return tasks.Task{}, false, errors.New("requested agent is not registered: " + agentID)
		}
		if s.policy.AgentDisabled(agentID) {
			return tasks.Task{}, false, errors.New("requested agent kill switch is active: " + agentID)
		}
	}
	now := input.OccurredAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	correlationID := ids.New()
	eventID := ids.New()
	idempotency := input.Request.IdempotencyKey
	if idempotency == "" {
		idempotency = ids.New()
	}
	retainTranscript := s.config.TranscriptRetention
	if input.Request.RetainTranscript != nil {
		retainTranscript = *input.Request.RetainTranscript
	}
	payload, _ := json.Marshal(map[string]any{"text": text})
	if !retainTranscript {
		payload = json.RawMessage(`{"retention":"disabled"}`)
	}
	event := events.Envelope{
		ID: eventID, Type: "message.received", Version: 1, Source: input.Source,
		Actor: input.Actor, OccurredAt: now, ReceivedAt: time.Now().UTC(),
		ConversationKey: input.Request.ConversationKey, CorrelationID: correlationID,
		IdempotencyKey: idempotency, TrustLevel: input.Trust,
		ReplyTarget: input.ReplyTarget, Payload: payload,
	}
	title := truncateTitle(text, 80)
	if !retainTranscript {
		title = "[transcript retention disabled]"
	}
	conversationRecord := conversation.Conversation{
		ID: ids.New(), ExternalKey: input.Request.ConversationKey, Title: title,
		TranscriptRetention: retainTranscript, CreatedAt: now, UpdatedAt: time.Now().UTC(),
	}
	turn := conversation.Turn{ID: "turn:user:" + eventID, ConversationID: conversationRecord.ID,
		Role: conversation.RoleUser, Text: text, Final: true, CorrelationID: correlationID, CreatedAt: now}
	taskPayload, _ := json.Marshal(map[string]any{
		"text": text, "source": input.Source, "reply_target": input.ReplyTarget,
	})
	rootID := ids.New()
	rootAgent := agentIDs[0]
	rootTaskType := "dialog"
	if len(agentIDs) > 1 {
		rootAgent = "builtin.synthesizer"
		rootTaskType = "synthesis"
	}
	root := tasks.Task{
		ID: rootID, RootTaskID: rootID, ConversationID: conversationRecord.ID,
		Goal: text, TaskType: rootTaskType, Input: taskPayload, AssignedAgentID: rootAgent,
		AllowedTools: []string{}, ApprovalPolicy: "policy-engine", Status: tasks.StatusQueued,
		Progress: 0, ProgressMessage: waitingMessage(len(agentIDs)), CreatedAt: time.Now().UTC(),
		MaxRetries: 2, TimeoutSeconds: 300, Artifacts: []tasks.Artifact{},
		CorrelationID: correlationID, CausationID: &eventID,
		IdempotencyKey: "event:" + eventID + ":root", Version: 1,
	}
	graph := []tasks.Task{root}
	var dependencies []tasks.Dependency
	if len(agentIDs) > 1 {
		for index, agentID := range agentIDs {
			childID := ids.New()
			parentID := rootID
			child := tasks.Task{
				ID: childID, ParentTaskID: &parentID, RootTaskID: rootID,
				ConversationID: conversationRecord.ID, Goal: text,
				TaskType: agentTaskType(agentID), Input: taskPayload, AssignedAgentID: agentID,
				AllowedTools: []string{}, ApprovalPolicy: "policy-engine", Status: tasks.StatusQueued,
				ProgressMessage: "Waiting for worker", CreatedAt: time.Now().UTC(), MaxRetries: 2,
				TimeoutSeconds: 300, Artifacts: []tasks.Artifact{}, CorrelationID: correlationID,
				CausationID: &eventID, IdempotencyKey: "event:" + eventID + ":agent:" + strconv.Itoa(index), Version: 1,
			}
			graph = append(graph, child)
			dependencies = append(dependencies, tasks.Dependency{TaskID: rootID, DependsOnTaskID: childID})
		}
	}
	conversationRecord, created, duplicate, err := s.store.CreateAskWork(ctx, persistence.AskWork{
		Event: event, Conversation: conversationRecord,
		ApplyRetention: input.Request.RetainTranscript != nil,
		Turn:           turn, Tasks: graph, Dependencies: dependencies,
		Audit: &observability.AuditEntry{
			ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: input.Source.Kind,
			ActorID: input.Actor.ID, Action: "ingress.accepted", ResourceKind: "event",
			ResourceID: event.ID, Decision: "ALLOW",
			Details:     mustJSON(map[string]any{"event_type": event.Type, "connector_id": input.Source.ConnectorID}),
			ConnectorID: input.Source.ConnectorID,
		},
	})
	if err != nil {
		return tasks.Task{}, false, err
	}
	if duplicate {
		return created, true, nil
	}
	turn.ConversationID = conversationRecord.ID
	s.hub.Publish(stream.Event{Type: "conversation.turn.final", ConversationID: conversationRecord.ID,
		CorrelationID: correlationID, Payload: turn})
	s.hub.Publish(stream.Event{Type: "task.created", TaskID: created.ID,
		ConversationID: created.ConversationID, CorrelationID: correlationID, Payload: created})
	s.runtime.Wake()
	return created, false, nil
}

func (s *Server) recoverPendingEvents(ctx context.Context) {
	pending, err := s.store.ListPendingEvents(ctx, 1000)
	if err != nil {
		s.logger.Error("load pending ingress events", "error", err)
		return
	}
	for _, event := range pending {
		if event.Type != "message.received" {
			_ = s.store.MarkEventProcessed(ctx, event.ID, errors.New("unsupported pending event type"))
			continue
		}
		var payload struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || strings.TrimSpace(payload.Text) == "" {
			_ = s.store.MarkEventProcessed(ctx, event.ID, errors.New("pending event has no recoverable text"))
			continue
		}
		_, _, submitErr := s.submitAsk(ctx, askContext{
			Request: AskRequest{Text: payload.Text, ConversationKey: event.ConversationKey,
				IdempotencyKey: event.IdempotencyKey},
			Source: event.Source, Actor: event.Actor, Trust: event.TrustLevel,
			ReplyTarget: event.ReplyTarget, OccurredAt: event.OccurredAt,
		})
		if submitErr != nil {
			s.logger.Error("recover pending ingress event", "event_id", event.ID, "error", submitErr)
			continue
		}
		_ = s.store.MarkEventProcessed(ctx, event.ID, nil)
	}
}

func (s *Server) handleTasks(writer http.ResponseWriter, request *http.Request) {
	var statuses []tasks.Status
	for _, raw := range strings.Split(request.URL.Query().Get("status"), ",") {
		if value := strings.TrimSpace(raw); value != "" {
			statuses = append(statuses, tasks.Status(strings.ToUpper(value)))
		}
	}
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	items, err := s.store.ListTasks(request.Context(), statuses, limit)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "list_tasks", "could not list tasks")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"tasks": items})
}

func (s *Server) handleTask(writer http.ResponseWriter, request *http.Request) {
	task, err := s.store.GetTask(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, persistenceStatus(err), "task_not_found", "task was not found")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"task": task})
}

func (s *Server) handleTaskGraph(writer http.ResponseWriter, request *http.Request) {
	task, err := s.store.GetTask(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, persistenceStatus(err), "task_not_found", "task was not found")
		return
	}
	nodes, dependencies, err := s.store.GetTaskGraph(request.Context(), task.RootTaskID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "task_graph", "could not load task graph")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"root_task_id": task.RootTaskID, "tasks": nodes, "dependencies": dependencies})
}

func (s *Server) handleCancelTask(writer http.ResponseWriter, request *http.Request) {
	task, err := s.runtime.Cancel(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, persistenceStatus(err), "cancel_task", err.Error())
		return
	}
	principal := principalFromContext(request.Context())
	_ = s.store.AddAuditEntry(request.Context(), observability.AuditEntry{
		ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: principal.Kind, ActorID: principal.ID,
		Action: "task.cancel", ResourceKind: "task", ResourceID: task.ID, Decision: "ALLOW",
		Details:       mustJSON(map[string]any{"root_task_id": task.RootTaskID}),
		CorrelationID: task.CorrelationID, TaskID: task.ID, ConversationID: task.ConversationID,
	})
	writeJSON(writer, http.StatusOK, map[string]any{"task": task})
}

func (s *Server) handleTaskPriority(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Priority int `json:"priority"`
	}
	if !decodeJSON(writer, request, &body) {
		return
	}
	task, err := s.store.SetTaskPriority(request.Context(), request.PathValue("id"), body.Priority)
	if err != nil {
		writeError(writer, persistenceStatus(err), "task_priority", err.Error())
		return
	}
	s.auditTaskControl(request, task, "task.priority.set", map[string]any{"priority": task.Priority})
	s.hub.Publish(stream.Event{Type: "task.changed", TaskID: task.ID,
		ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: task})
	s.runtime.Wake()
	writeJSON(writer, http.StatusOK, map[string]any{"task": task})
}

func (s *Server) handleDismissTask(writer http.ResponseWriter, request *http.Request) {
	task, err := s.store.DismissTask(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, persistenceStatus(err), "task_dismiss", err.Error())
		return
	}
	s.auditTaskControl(request, task, "task.dismiss", map[string]any{"dismissed": true})
	s.hub.Publish(stream.Event{Type: "task.dismissed", TaskID: task.ID,
		ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: task})
	writeJSON(writer, http.StatusOK, map[string]any{"task": task})
}

func (s *Server) auditTaskControl(request *http.Request, task tasks.Task, action string, details map[string]any) {
	principal := principalFromContext(request.Context())
	_ = s.store.AddAuditEntry(request.Context(), observability.AuditEntry{
		ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: principal.Kind, ActorID: principal.ID,
		Action: action, ResourceKind: "task", ResourceID: task.ID, Decision: "ALLOW",
		Details: mustJSON(details), CorrelationID: task.CorrelationID,
		TaskID: task.ID, ConversationID: task.ConversationID,
	})
}

func (s *Server) handleTranscriptRetention(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(writer, request, &body) {
		return
	}
	if body.Enabled == nil {
		writeError(writer, http.StatusBadRequest, "retention_enabled", "enabled is required")
		return
	}
	conversationID := request.PathValue("id")
	principal := principalFromContext(request.Context())
	if principal.Kind == "device" {
		owned, err := s.store.DeviceOwnsConversation(request.Context(), principal.ID, conversationID)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "transcript_retention", "could not verify conversation ownership")
			return
		}
		if !owned {
			writeError(writer, http.StatusForbidden, "conversation_owner", "conversation does not belong to this device")
			return
		}
	}
	auditEntry := observability.AuditEntry{
		ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: principal.Kind, ActorID: principal.ID,
		Action: "conversation.transcript_retention.set", ResourceKind: "conversation",
		ResourceID: conversationID, Decision: "ALLOW",
		Details:       mustJSON(map[string]any{"enabled": *body.Enabled}),
		CorrelationID: ids.New(), ConversationID: conversationID,
	}
	if err := s.store.SetTranscriptRetentionWithAudit(request.Context(), conversationID, *body.Enabled, auditEntry); err != nil {
		writeError(writer, persistenceStatus(err), "transcript_retention", "could not update transcript retention")
		return
	}
	s.hub.Publish(stream.Event{Type: "conversation.retention_changed", ConversationID: conversationID,
		Payload: map[string]any{"enabled": *body.Enabled}})
	writeJSON(writer, http.StatusOK, map[string]any{"conversation_id": conversationID, "transcript_retention": *body.Enabled})
}

func truncateTitle(value string, maximum int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= maximum {
		return string(runes)
	}
	return string(runes[:maximum]) + "…"
}

func waitingMessage(agentCount int) string {
	if agentCount > 1 {
		return "Waiting for delegated child tasks"
	}
	return "Waiting for worker"
}

func agentTaskType(agentID string) string {
	switch {
	case strings.Contains(agentID, "coding"):
		return "coding"
	case strings.Contains(agentID, "research"):
		return "research"
	case strings.Contains(agentID, "automation"):
		return "automation"
	default:
		return "dialog"
	}
}

var _ = auth.Principal{}
