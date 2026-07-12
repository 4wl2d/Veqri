package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/veqri/veqri/core/approvals"
	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/core/policy"
	"github.com/veqri/veqri/core/tasks"
	coretools "github.com/veqri/veqri/core/tools"
	"github.com/veqri/veqri/internal/ids"
	"github.com/veqri/veqri/internal/stream"
)

func (s *Server) handleShell(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Input          json.RawMessage `json:"input"`
		ConversationID string          `json:"conversation_id,omitempty"`
		IdempotencyKey string          `json:"idempotency_key,omitempty"`
	}
	if !decodeJSON(writer, request, &body) {
		return
	}
	if body.IdempotencyKey == "" {
		body.IdempotencyKey = request.Header.Get("Idempotency-Key")
	}
	if body.IdempotencyKey == "" {
		body.IdempotencyKey = ids.New()
	}
	parsed, risk, err := s.shell.ParseAndValidate(body.Input)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_shell_request", err.Error())
		return
	}
	canonicalInput, err := json.Marshal(parsed)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_shell_request", "shell request could not be normalized")
		return
	}
	if parsed.DryRun {
		risk = coretools.RiskReadOnly
	}
	principal := principalFromContext(request.Context())
	trust := events.TrustLocal
	if principal.Kind == "device" {
		trust = events.TrustTrusted
	}
	decision := s.policy.Evaluate(policy.Request{
		Source: events.Source{Kind: principal.Kind}, TrustLevel: trust,
		ActorID: principal.ID, DeviceID: deviceID(principal.Kind, principal.ID),
		AgentID: "builtin.shell", ToolName: "shell", ToolArguments: canonicalInput,
		Workspace: parsed.WorkingDir, At: time.Now().UTC(), ConversationID: body.ConversationID,
		Risk: risk, RequestedScopes: []string{"tool.shell.execute"}, SideEffects: riskSideEffects(risk),
	})
	correlationID := ids.New()
	requestID := ids.New()
	if err := s.store.AddAuditEntry(request.Context(), observability.AuditEntry{
		ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: principal.Kind,
		ActorID: principal.ID, Action: "policy.evaluate", ResourceKind: "tool_request",
		ResourceID: requestID, Decision: string(decision.Decision),
		Details:       mustJSON(map[string]any{"tool": "shell", "risk": risk, "reason": decision.Reason, "workspace": parsed.WorkingDir}),
		CorrelationID: correlationID, ConversationID: body.ConversationID,
	}); err != nil {
		writeError(writer, http.StatusServiceUnavailable, "audit_unavailable", "policy decision could not be recorded; no tool task was created")
		return
	}
	if decision.Decision == policy.DecisionDeny {
		writeError(writer, http.StatusForbidden, "policy_denied", decision.Reason)
		return
	}
	status := tasks.StatusQueued
	if decision.Decision == policy.DecisionRequireApproval {
		status = tasks.StatusWaitingForApproval
	}
	taskID := ids.New()
	task := tasks.Task{
		ID: taskID, RootTaskID: taskID, ConversationID: body.ConversationID,
		Goal: shellGoal(parsed.Command, parsed.Args), TaskType: "shell", Input: canonicalInput,
		AssignedAgentID: "builtin.shell", AllowedTools: []string{"shell"},
		ApprovalPolicy: "policy-engine", Status: status, ProgressMessage: decision.Reason,
		CreatedAt: time.Now().UTC(), MaxRetries: 0, TimeoutSeconds: parsed.TimeoutSeconds + 10,
		Artifacts: []tasks.Artifact{}, CorrelationID: correlationID,
		IdempotencyKey: "shell-request:" + body.IdempotencyKey, Version: 1,
	}
	var requestedApproval *approvals.Approval
	if status == tasks.StatusWaitingForApproval {
		requestedApproval = &approvals.Approval{
			ID: "approval:" + task.ID, TaskID: task.ID, ToolName: "shell", ToolArguments: canonicalInput,
			RequestedScopes: []string{"tool.shell.execute"}, Risk: risk, Reason: decision.Reason,
			Status: approvals.StatusPending, RequestedAt: time.Now().UTC(),
			ExpiresAt: time.Now().UTC().Add(10 * time.Minute), CorrelationID: correlationID,
		}
	}
	created, approval, duplicate, err := s.store.CreateTaskWithApproval(request.Context(), task, requestedApproval)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "create_shell_task", "could not create shell task")
		return
	}
	if !duplicate && approval != nil {
		s.hub.Publish(stream.Event{Type: "approval.requested", TaskID: created.ID,
			ConversationID: created.ConversationID, CorrelationID: correlationID, Payload: approval})
	} else if !duplicate {
		s.runtime.Wake()
	}
	s.hub.Publish(stream.Event{Type: "task.created", TaskID: created.ID,
		ConversationID: created.ConversationID, CorrelationID: correlationID, Payload: created})
	statusCode := http.StatusAccepted
	if duplicate {
		statusCode = http.StatusOK
	}
	writeJSON(writer, statusCode, map[string]any{
		"task": created, "approval": approval, "policy": decision, "duplicate": duplicate,
	})
}

func (s *Server) handleApprovals(writer http.ResponseWriter, request *http.Request) {
	pendingOnly := request.URL.Query().Get("pending") != "false"
	items, err := s.store.ListApprovals(request.Context(), pendingOnly, 200)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "approvals", "could not list approvals")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"approvals": items})
}

func (s *Server) handleApprove(writer http.ResponseWriter, request *http.Request) {
	s.decideApproval(writer, request, true)
}

func (s *Server) handleDeny(writer http.ResponseWriter, request *http.Request) {
	s.decideApproval(writer, request, false)
}

func (s *Server) decideApproval(writer http.ResponseWriter, request *http.Request, approved bool) {
	principal := principalFromContext(request.Context())
	approval, task, err := s.store.DecideApproval(request.Context(), request.PathValue("id"), principal.Kind+":"+principal.ID, approved)
	if err != nil {
		writeError(writer, persistenceStatus(err), "approval_decision", err.Error())
		return
	}
	eventType := "approval.denied"
	if approved {
		eventType = "approval.approved"
		s.runtime.Wake()
	}
	s.hub.Publish(stream.Event{Type: eventType, TaskID: task.ID,
		ConversationID: task.ConversationID, CorrelationID: approval.CorrelationID,
		Payload: map[string]any{"approval": approval, "task": task}})
	s.hub.Publish(stream.Event{Type: "task.changed", TaskID: task.ID,
		ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: task})
	writeJSON(writer, http.StatusOK, map[string]any{"approval": approval, "task": task})
}

func (s *Server) handleEmergencyStop(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(writer, request, &body) {
		return
	}
	principal := principalFromContext(request.Context())
	entry := observability.AuditEntry{
		ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: principal.Kind, ActorID: principal.ID,
		Action: "core.emergency_stop.set", ResourceKind: "core", ResourceID: "local-core",
		Decision: "ALLOW", Details: mustJSON(map[string]any{"enabled": body.Enabled}),
		CorrelationID: ids.New(),
	}
	if err := s.store.SetSettingWithAudit(request.Context(), "emergency_stop", body.Enabled, entry); err != nil {
		writeError(writer, http.StatusServiceUnavailable, "audit_unavailable", "emergency stop was not changed because its audit record could not be persisted")
		return
	}
	s.policy.SetEmergencyStop(body.Enabled)
	s.hub.Publish(stream.Event{Type: "policy.emergency_stop", Payload: map[string]any{"enabled": body.Enabled}})
	writeJSON(writer, http.StatusOK, map[string]any{"emergency_stop": body.Enabled})
}

func deviceID(kind, id string) string {
	if kind == "device" {
		return id
	}
	return ""
}

func riskSideEffects(risk coretools.Risk) []string {
	if risk == coretools.RiskReadOnly {
		return nil
	}
	return []string{strings.ToLower(string(risk))}
}

func shellGoal(command string, arguments []string) string {
	if len(arguments) == 0 {
		return "Run " + command
	}
	return "Run " + command + " with " + strings.Join(arguments, " ")
}
