package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/veqri/veqri/core/approvals"
	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/internal/auth"
	"github.com/veqri/veqri/internal/ids"
	"github.com/veqri/veqri/internal/stream"
)

type desktopSettings struct {
	Theme                     string `json:"theme"`
	StartAtLogin              bool   `json:"start_at_login"`
	CloseToTray               bool   `json:"close_to_tray"`
	DesktopNotifications      bool   `json:"desktop_notifications"`
	TranscriptRetentionDays   int    `json:"transcript_retention_days"`
	AuditRetentionDays        int    `json:"audit_retention_days"`
	AnnounceBackgroundResults bool   `json:"announce_background_results"`
	QuietHoursEnabled         bool   `json:"quiet_hours_enabled"`
	QuietHoursStart           string `json:"quiet_hours_start"`
	QuietHoursEnd             string `json:"quiet_hours_end"`
	LANAccessEnabled          bool   `json:"lan_access_enabled"`
	RedactDiagnostics         bool   `json:"redact_diagnostics"`
}

func defaultDesktopSettings(retention int) desktopSettings {
	return desktopSettings{
		Theme: "system", CloseToTray: true, DesktopNotifications: true,
		TranscriptRetentionDays: retention, AuditRetentionDays: retention,
		AnnounceBackgroundResults: true, QuietHoursStart: "22:00", QuietHoursEnd: "07:00",
		RedactDiagnostics: true,
	}
}

func (s *Server) handleDesktopSnapshot(writer http.ResponseWriter, request *http.Request) {
	snapshot, err := s.desktopSnapshot(request.Context())
	if err != nil {
		s.logger.Error("desktop snapshot", "error", err)
		writeError(writer, http.StatusInternalServerError, "desktop_snapshot", "could not build desktop snapshot")
		return
	}
	writeJSON(writer, http.StatusOK, snapshot)
}

func (s *Server) desktopSnapshot(ctx context.Context) (map[string]any, error) {
	now := time.Now().UTC()
	devices, err := s.store.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	voiceSessions, err := s.store.ListVoiceSessions(ctx, false, 100)
	if err != nil {
		return nil, err
	}
	conversationSummaries, err := s.store.ListConversationSummaries(ctx, 200)
	if err != nil {
		return nil, err
	}
	taskList, err := s.store.ListTasks(ctx, nil, 500)
	if err != nil {
		return nil, err
	}
	approvalList, err := s.store.ListApprovals(ctx, false, 500)
	if err != nil {
		return nil, err
	}
	auditList, err := s.store.ListAuditEntries(ctx, 500)
	if err != nil {
		return nil, err
	}
	diagnostics, err := s.store.Diagnostics(ctx)
	if err != nil {
		return nil, err
	}
	settings := defaultDesktopSettings(s.config.RetentionDays)
	if err := s.store.GetSetting(ctx, "desktop", &settings); err != nil && !errors.Is(err, persistence.ErrNotFound) {
		return nil, err
	}
	// Core environment/service configuration is authoritative for retention;
	// an older desktop settings row must never report a value that is not
	// actually enforced by the background sweeper.
	settings.TranscriptRetentionDays = s.config.RetentionDays
	settings.AuditRetentionDays = s.config.RetentionDays
	deviceNames := make(map[string]string)
	desktopDevices := make([]map[string]any, 0, len(devices))
	for _, device := range devices {
		deviceNames[device.ID] = device.Name
		status := "offline"
		if device.RevokedAt != nil {
			status = "revoked"
		} else if device.LastSeenAt != nil && time.Since(*device.LastSeenAt) < 2*time.Minute {
			status = "online"
		}
		capabilities := []string{}
		var capabilityMap map[string]any
		if json.Unmarshal([]byte(device.Capabilities), &capabilityMap) == nil {
			for key, value := range capabilityMap {
				if enabled, ok := value.(bool); ok && enabled {
					capabilities = append(capabilities, key)
				}
			}
		}
		lastSeen := device.CreatedAt
		if device.LastSeenAt != nil {
			lastSeen = *device.LastSeenAt
		}
		desktopDevices = append(desktopDevices, map[string]any{
			"id": device.ID, "name": device.Name, "platform": "android", "model": "Android device",
			"status": status, "paired_at": device.CreatedAt, "last_seen_at": lastSeen,
			"app_version": "unknown", "key_version": device.KeyVersion,
			"capabilities": capabilities, "network": "lan",
		})
	}
	desktopVoice := make([]map[string]any, 0, len(voiceSessions))
	for _, session := range voiceSessions {
		transport := "webrtc"
		codec := "opus"
		if strings.Contains(session.Transport, "simulated") {
			transport = "simulated"
			codec = "text/utf-8-simulated"
		}
		desktopVoice = append(desktopVoice, map[string]any{
			"id": session.ID, "conversation_id": session.ConversationID, "device_id": session.DeviceID,
			"device_name": deviceNames[session.DeviceID], "state": session.State,
			"started_at": session.StartedAt, "duration_seconds": int64(now.Sub(session.StartedAt).Seconds()),
			"transport": transport, "codec": codec, "round_trip_ms": 0,
			"packet_loss_percent": 0, "active_task_count": countActiveConversationTasks(taskList, session.ConversationID),
			"partial_transcript": "",
		})
	}
	desktopConversations := make([]map[string]any, 0, len(conversationSummaries))
	for _, summary := range conversationSummaries {
		desktopConversations = append(desktopConversations, map[string]any{
			"id": summary.Conversation.ID, "title": summary.Conversation.Title,
			"source": desktopConversationSource(summary.Conversation.ExternalKey), "participant": "Owner",
			"updated_at": summary.Conversation.UpdatedAt, "turn_count": summary.TurnCount,
			"active_task_count": summary.ActiveTasks, "retention": retentionName(summary.Conversation.TranscriptRetention),
			"last_message": summary.LastMessage, "correlation_id": summary.Correlation,
		})
	}
	desktopTasks := make([]map[string]any, 0, len(taskList))
	activeByAgent := make(map[string]int)
	for _, task := range taskList {
		dependencies, depErr := s.store.TaskDependencies(ctx, task.ID)
		if depErr != nil {
			return nil, depErr
		}
		if dependencies == nil {
			dependencies = []string{}
		}
		if !task.Status.Terminal() {
			activeByAgent[task.AssignedAgentID]++
		}
		var parent any
		if task.ParentTaskID != nil {
			parent = *task.ParentTaskID
		}
		var started, finished any
		if task.StartedAt != nil {
			started = *task.StartedAt
		}
		if task.FinishedAt != nil {
			finished = *task.FinishedAt
		}
		var taskError any
		if task.Error != "" {
			taskError = task.Error
		}
		desktopTasks = append(desktopTasks, map[string]any{
			"id": task.ID, "parent_task_id": parent, "root_task_id": task.RootTaskID,
			"goal": task.Goal, "status": task.Status, "progress_percent": task.Progress,
			"assigned_agent_id": nullableDesktop(task.AssignedAgentID), "assigned_agent_name": nullableDesktop(agentName(task.AssignedAgentID)),
			"current_tool": currentTool(task), "allowed_tools": task.AllowedTools,
			"created_at": task.CreatedAt, "started_at": started, "finished_at": finished,
			"retry_count": task.RetryCount, "max_retries": task.MaxRetries, "error": taskError,
			"priority": task.Priority, "dismissed": task.Dismissed,
			"summary": task.UserFacingSummary, "dependencies": dependencies, "artifacts": task.Artifacts,
			"correlation_id": task.CorrelationID,
		})
	}
	desktopAgents := make([]map[string]any, 0)
	for _, definition := range s.registry.Definitions() {
		desktopAgents = append(desktopAgents, map[string]any{
			"id": definition.ID, "name": definition.DisplayName, "description": definition.Description,
			"capabilities": definition.Capabilities, "tool_scopes": definition.ToolScopes,
			"trust_level": definition.TrustLevel, "execution_mode": desktopExecutionMode(string(definition.ExecutionMode)),
			"health": definition.Health, "active_tasks": activeByAgent[definition.ID],
			"concurrency_limit": definition.ConcurrencyLimit, "supports_streaming": definition.SupportsStreaming,
			"supports_cancellation": definition.SupportsCancellation, "kill_switch": s.policy.AgentDisabled(definition.ID), "latency_ms": 0,
		})
	}
	taskByID := make(map[string]tasks.Task)
	for _, task := range taskList {
		taskByID[task.ID] = task
	}
	desktopApprovals := make([]map[string]any, 0, len(approvalList))
	for _, approval := range approvalList {
		task := taskByID[approval.TaskID]
		var arguments map[string]any
		_ = json.Unmarshal(approval.ToolArguments, &arguments)
		desktopApprovals = append(desktopApprovals, map[string]any{
			"id": approval.ID, "task_id": approval.TaskID, "task_goal": task.Goal,
			"requested_by_agent": task.AssignedAgentID, "tool_name": approval.ToolName,
			"permission": strings.Join(approval.RequestedScopes, ", "), "risk": approval.Risk,
			"arguments": arguments, "command_preview": commandPreview(approval), "reason": approval.Reason,
			"requested_at": approval.RequestedAt, "expires_at": approval.ExpiresAt,
			"status": strings.ToLower(string(approval.Status)),
		})
	}
	desktopAudit := make([]map[string]any, 0, len(auditList))
	for _, entry := range auditList {
		desktopAudit = append(desktopAudit, map[string]any{
			"id": entry.ID, "occurred_at": entry.OccurredAt, "category": auditCategory(entry.ResourceKind),
			"action": entry.Action, "actor": entry.ActorKind + ":" + entry.ActorID,
			"target": entry.ResourceKind + ":" + entry.ResourceID, "decision": auditDecision(entry.Decision),
			"summary": sanitizedAuditSummary(entry.Details), "correlation_id": entry.CorrelationID, "redacted": true,
		})
	}
	databaseSize := int64(0)
	if info, statErr := os.Stat(s.config.DatabasePath); statErr == nil {
		databaseSize = info.Size()
	}
	queued, running, blocked := taskQueueCounts(taskList)
	coreStatus := "healthy"
	if !diagnostics.DatabaseOK || !diagnostics.ForeignKeys || !strings.EqualFold(diagnostics.JournalMode, "wal") {
		coreStatus = "degraded"
	}
	return map[string]any{
		"protocol_version": 1, "revision": now.UnixMilli(), "generated_at": now,
		"core": map[string]any{
			"status": coreStatus, "version": "0.1.0", "protocol_version": 1,
			"started_at": s.startedAt, "uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
			"bind_address": s.config.Address, "service_mode": "foreground",
			"database": map[string]any{"status": coreStatus, "path": s.config.DatabasePath,
				"size_bytes": databaseSize, "wal_enabled": strings.EqualFold(diagnostics.JournalMode, "wal"),
				"migration_version": diagnostics.MigrationVersion},
			"queue":          map[string]int{"queued": queued, "running": running, "blocked": blocked},
			"emergency_stop": s.policy.EmergencyStop(), "cpu_percent": nil, "memory_bytes": nil,
		},
		"devices": desktopDevices, "voice_sessions": desktopVoice,
		"conversations": desktopConversations, "tasks": desktopTasks, "agents": desktopAgents,
		"tools": s.desktopTools(), "policies": desktopPolicies(now), "approvals": desktopApprovals,
		"connectors": s.desktopConnectors(), "providers": s.desktopProviders(), "audit_entries": desktopAudit,
		"diagnostics": s.desktopDiagnostics(now, diagnostics, len(voiceSessions)), "settings": settings,
	}, nil
}

func (s *Server) handleDesktopAction(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		RequestID string          `json:"request_id"`
		Action    json.RawMessage `json:"action"`
	}
	if !decodeJSON(writer, request, &body) {
		return
	}
	if body.RequestID == "" || len(body.RequestID) > 128 {
		writeError(writer, http.StatusBadRequest, "request_id", "request_id is required")
		return
	}
	existing, started, err := s.store.StartDesktopAction(request.Context(), body.RequestID)
	if err != nil {
		writeError(writer, persistenceStatus(err), "desktop_action", "action is already in progress or could not be started")
		return
	}
	if !started {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(existing)
		return
	}
	message, artifactPath, actionErr := s.performDesktopAction(request.Context(), body.RequestID, body.Action)
	s.auditDesktopAction(request.Context(), body.RequestID, body.Action, actionErr)
	if actionErr != nil {
		// Keep STARTED: a retry is refused instead of risking duplicate side effects.
		writeError(writer, persistenceStatus(actionErr), "desktop_action", actionErr.Error())
		return
	}
	response := map[string]any{
		"request_id": body.RequestID, "accepted": true, "occurred_at": time.Now().UTC(),
		"revision": time.Now().UTC().UnixMilli(), "message": message, "artifact_path": artifactPath,
	}
	if err := s.store.CompleteDesktopAction(request.Context(), body.RequestID, response); err != nil {
		writeError(writer, http.StatusInternalServerError, "desktop_action_result", "action completed but its response could not be recorded")
		return
	}
	s.hub.Publish(stream.Event{Type: "snapshot.changed", Payload: map[string]any{"request_id": body.RequestID}})
	writeJSON(writer, http.StatusOK, response)
}

func (s *Server) auditDesktopAction(ctx context.Context, requestID string, raw json.RawMessage, actionErr error) {
	var action struct {
		Type        string `json:"type"`
		ApprovalID  string `json:"approval_id"`
		TaskID      string `json:"task_id"`
		DeviceID    string `json:"device_id"`
		SessionID   string `json:"session_id"`
		ConnectorID string `json:"connector_id"`
		AgentID     string `json:"agent_id"`
	}
	_ = json.Unmarshal(raw, &action)
	if len(action.Type) > 80 {
		action.Type = action.Type[:80]
	}
	decision := "ALLOW"
	result := "completed"
	if actionErr != nil {
		decision = "ERROR"
		result = "failed"
	}
	resourceKind, resourceID := "desktop_action", requestID
	switch {
	case action.ApprovalID != "":
		resourceKind, resourceID = "approval", action.ApprovalID
	case action.TaskID != "":
		resourceKind, resourceID = "task", action.TaskID
	case action.DeviceID != "":
		resourceKind, resourceID = "device", action.DeviceID
	case action.SessionID != "":
		resourceKind, resourceID = "voice_session", action.SessionID
	case action.ConnectorID != "":
		resourceKind, resourceID = "connector", action.ConnectorID
	case action.AgentID != "":
		resourceKind, resourceID = "agent", action.AgentID
	}
	_ = s.store.AddAuditEntry(ctx, observability.AuditEntry{
		ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: "admin", ActorID: "desktop",
		Action: "desktop.action." + action.Type, ResourceKind: resourceKind,
		ResourceID: resourceID, Decision: decision,
		Details:       mustJSON(map[string]any{"type": action.Type, "result": result}),
		CorrelationID: requestID, TaskID: action.TaskID,
	})
}

func (s *Server) performDesktopAction(ctx context.Context, requestID string, raw json.RawMessage) (string, any, error) {
	var action struct {
		Type        string          `json:"type"`
		ApprovalID  string          `json:"approval_id"`
		Decision    string          `json:"decision"`
		TaskID      string          `json:"task_id"`
		DeviceID    string          `json:"device_id"`
		SessionID   string          `json:"session_id"`
		ConnectorID string          `json:"connector_id"`
		AgentID     string          `json:"agent_id"`
		Enabled     bool            `json:"enabled"`
		Patch       json.RawMessage `json:"patch"`
		Redact      bool            `json:"redact"`
		Priority    int             `json:"priority"`
	}
	if err := json.Unmarshal(raw, &action); err != nil {
		return "", nil, err
	}
	switch action.Type {
	case "approval.resolve":
		approved := action.Decision == "approved"
		if !approved && action.Decision != "denied" {
			return "", nil, errors.New("approval decision must be approved or denied")
		}
		approval, task, err := s.store.DecideApproval(ctx, action.ApprovalID, "desktop:local-admin", approved)
		if err != nil {
			return "", nil, err
		}
		s.hub.Publish(stream.Event{Type: "approval.changed", TaskID: task.ID,
			ConversationID: task.ConversationID, CorrelationID: approval.CorrelationID,
			Payload: map[string]any{"approval": approval, "task": task}})
		s.hub.Publish(stream.Event{Type: "task.changed", TaskID: task.ID,
			ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: task})
		if approved {
			s.runtime.Wake()
			return "Single-use approval granted.", nil, nil
		}
		return "Request denied; the tool was not executed.", nil, nil
	case "task.cancel":
		_, err := s.runtime.Cancel(ctx, action.TaskID)
		return "Task cancellation requested.", nil, err
	case "task.retry":
		_, err := s.store.RetryTask(ctx, action.TaskID)
		if err == nil {
			s.runtime.Wake()
		}
		return "Task queued for explicit retry.", nil, err
	case "task.reprioritize":
		task, err := s.store.SetTaskPriority(ctx, action.TaskID, action.Priority)
		if err == nil {
			s.hub.Publish(stream.Event{Type: "task.changed", TaskID: task.ID,
				ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: task})
			s.runtime.Wake()
		}
		return "Task priority updated.", nil, err
	case "task.dismiss":
		task, err := s.store.DismissTask(ctx, action.TaskID)
		if err == nil {
			s.hub.Publish(stream.Event{Type: "task.dismissed", TaskID: task.ID,
				ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: task})
		}
		return "Task dismissed from default lists.", nil, err
	case "device.revoke":
		entry := observability.AuditEntry{
			ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: "admin", ActorID: "desktop",
			Action: "device.revoked", ResourceKind: "device", ResourceID: action.DeviceID, Decision: "ALLOW",
			Details:       mustJSON(map[string]any{"credential": "revoked", "source": "desktop"}),
			CorrelationID: requestID,
		}
		if err := s.store.RevokeDeviceWithAudit(ctx, action.DeviceID, entry); err != nil {
			return "", nil, err
		}
		s.closeDeviceSockets(action.DeviceID)
		s.hub.Publish(stream.Event{Type: "device.revoked", Payload: map[string]any{"device_id": action.DeviceID}})
		return "Device credential revoked.", nil, nil
	case "voice.end":
		session, err := s.store.GetVoiceSession(ctx, action.SessionID)
		if err != nil {
			return "", nil, err
		}
		_, err = s.endVoiceSession(ctx, session)
		return "Voice session ended.", nil, err
	case "connector.retry":
		return "Connector retry requested; the adapter will reconnect when configured.", nil, nil
	case "connector.kill_switch.set":
		state := s.policy.KillSwitches()
		state.Connectors[action.ConnectorID] = action.Enabled
		entry := policyControlAudit("desktop", "connector.kill_switch.set", "connector",
			action.ConnectorID, action.Enabled)
		if err := s.store.SetSettingWithAudit(ctx, "kill_switches", state, entry); err != nil {
			return "", nil, err
		}
		s.policy.LoadKillSwitches(state)
		return enabledMessage("Connector kill switch", action.Enabled), nil, nil
	case "agent.kill_switch.set":
		state := s.policy.KillSwitches()
		state.Agents[action.AgentID] = action.Enabled
		entry := policyControlAudit("desktop", "agent.kill_switch.set", "agent", action.AgentID, action.Enabled)
		if err := s.store.SetSettingWithAudit(ctx, "kill_switches", state, entry); err != nil {
			return "", nil, err
		}
		s.policy.LoadKillSwitches(state)
		return enabledMessage("Agent kill switch", action.Enabled), nil, nil
	case "core.emergency_stop.set":
		entry := observability.AuditEntry{
			ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: "admin", ActorID: "desktop",
			Action: "core.emergency_stop.set", ResourceKind: "core", ResourceID: "local-core",
			Decision: "ALLOW", Details: mustJSON(map[string]any{"enabled": action.Enabled}),
			CorrelationID: ids.New(),
		}
		if err := s.store.SetSettingWithAudit(ctx, "emergency_stop", action.Enabled, entry); err != nil {
			return "", nil, err
		}
		s.policy.SetEmergencyStop(action.Enabled)
		return enabledMessage("Emergency stop", action.Enabled), nil, nil
	case "settings.update":
		settings, err := s.updateDesktopSettings(ctx, action.Patch)
		if err != nil {
			return "", nil, err
		}
		_ = settings
		return "Desktop settings saved.", nil, nil
	case "backup.create":
		path, err := s.createBackup(ctx)
		return "SQLite backup created.", path, err
	case "diagnostics.export":
		path, err := s.createDiagnosticsExport(ctx, action.Redact)
		return "Diagnostic export created.", path, err
	default:
		return "", nil, errors.New("unsupported desktop action")
	}
}

func policyControlAudit(actorID, action, resourceKind, resourceID string, enabled bool) observability.AuditEntry {
	return observability.AuditEntry{
		ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: "admin", ActorID: actorID,
		Action: action, ResourceKind: resourceKind, ResourceID: resourceID, Decision: "ALLOW",
		Details: mustJSON(map[string]any{"enabled": enabled}), CorrelationID: ids.New(),
	}
}

func (s *Server) updateDesktopSettings(ctx context.Context, patch json.RawMessage) (desktopSettings, error) {
	current := defaultDesktopSettings(s.config.RetentionDays)
	_ = s.store.GetSetting(ctx, "desktop", &current)
	var patchValues map[string]json.RawMessage
	if err := json.Unmarshal(patch, &patchValues); err != nil {
		return desktopSettings{}, err
	}
	currentRaw, _ := json.Marshal(current)
	var values map[string]json.RawMessage
	_ = json.Unmarshal(currentRaw, &values)
	for key, value := range patchValues {
		if _, exists := values[key]; !exists {
			return desktopSettings{}, fmt.Errorf("unknown desktop setting %q", key)
		}
		if key != "theme" {
			return desktopSettings{}, fmt.Errorf("desktop setting %q is read-only runtime configuration", key)
		}
		values[key] = value
	}
	merged, _ := json.Marshal(values)
	if err := json.Unmarshal(merged, &current); err != nil {
		return desktopSettings{}, err
	}
	if current.Theme != "dark" && current.Theme != "light" && current.Theme != "system" {
		return desktopSettings{}, errors.New("theme must be dark, light, or system")
	}
	current.TranscriptRetentionDays = s.config.RetentionDays
	current.AuditRetentionDays = s.config.RetentionDays
	return current, s.store.SetSetting(ctx, "desktop", current)
}

func (s *Server) createBackup(ctx context.Context) (string, error) {
	directory := filepath.Join(s.config.DataDir, "backups")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(directory, "veqri-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".db")
	if _, err := s.store.DB().ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Server) createDiagnosticsExport(ctx context.Context, redact bool) (string, error) {
	directory := filepath.Join(s.config.DataDir, "diagnostics")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	diagnostics, err := s.store.Diagnostics(ctx)
	if err != nil {
		return "", err
	}
	payload := map[string]any{
		"generated_at": time.Now().UTC(), "redacted": redact, "diagnostics": diagnostics,
		"platform": runtime.GOOS + "/" + runtime.GOARCH, "config": map[string]any{
			"bind_address": s.config.Address, "database": redactedPath(s.config.DatabasePath, redact),
			"media_transport": s.config.MediaTransport, "stt_provider": s.config.STTProvider,
			"tts_provider": s.config.TTSProvider,
		},
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(directory, "veqri-diagnostics-"+time.Now().UTC().Format("20060102T150405Z")+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Server) handleDesktopWebSocket(writer http.ResponseWriter, request *http.Request) {
	token := auth.BearerToken(request.Header.Get("Authorization"))
	if token == "" {
		token = websocketProtocolToken(request.Header.Get("Sec-WebSocket-Protocol"))
	}
	principal, err := s.authenticator.Authenticate(request.Context(), token)
	if err != nil || principal.Kind != "admin" {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "WebSocket authentication required")
		return
	}
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		Subprotocols: []string{"veqri.v1"}, OriginPatterns: desktopWebSocketOrigins(),
	})
	if err != nil {
		return
	}
	defer func() { _ = connection.Close(websocket.StatusNormalClosure, "desktop stream ended") }()
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	eventsChannel := s.hub.Subscribe(ctx, 128)
	if err := s.writeDesktopEvent(ctx, connection, "heartbeat", "", ""); err != nil {
		return
	}
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-eventsChannel:
			if !ok || s.writeDesktopEvent(ctx, connection, desktopEventType(event.Type), event.CorrelationID, event.TaskID) != nil {
				return
			}
		case <-heartbeat.C:
			if s.writeDesktopEvent(ctx, connection, "heartbeat", "", "") != nil {
				return
			}
		}
	}
}

func (s *Server) writeDesktopEvent(ctx context.Context, connection *websocket.Conn, eventType, correlationID, entityID string) error {
	revision := time.Now().UTC().UnixMilli()
	var correlation any
	if correlationID != "" {
		correlation = correlationID
	}
	data := map[string]any{"revision": revision}
	if entityID != "" {
		data["entity_id"] = entityID
	}
	return writeWebSocketJSON(ctx, connection, map[string]any{
		"id": ids.New(), "type": eventType, "occurred_at": time.Now().UTC(),
		"correlation_id": correlation, "sequence": s.desktopSequence.Add(1), "data": data,
	})
}

func desktopEventType(value string) string {
	switch {
	case strings.HasPrefix(value, "task.") || strings.HasPrefix(value, "tool."):
		return "task.changed"
	case strings.HasPrefix(value, "approval."):
		return "approval.changed"
	case strings.HasPrefix(value, "policy.") || strings.HasPrefix(value, "device."):
		return "core.changed"
	default:
		return "snapshot.changed"
	}
}

func (s *Server) desktopTools() []map[string]any {
	workspace := ""
	if s.shell != nil {
		if workspaces := s.shell.Workspaces(); len(workspaces) > 0 {
			workspace = workspaces[0]
		}
	}
	return []map[string]any{
		{"id": "shell", "name": "Structured shell", "description": "Runs one binary with a structured argument array", "risk": "STATE_CHANGING", "status": "approval_required", "scopes": []string{"tool.shell.execute"}, "workspace_boundary": workspace, "running_invocations": 0, "supported_os": []string{"darwin", "linux", "windows"}},
	}
}

func desktopPolicies(now time.Time) []map[string]any {
	return []map[string]any{
		{"id": "deny-privilege", "name": "Deny privilege escalation", "description": "Privilege escalation is denied by default", "priority": 1000, "decision": "DENY", "match_summary": "risk = PRIVILEGED", "enabled": true, "updated_at": now},
		{"id": "approve-mutation", "name": "Approve state changes", "description": "State-changing and destructive tools require a single-use approval", "priority": 900, "decision": "REQUIRE_APPROVAL", "match_summary": "risk >= STATE_CHANGING", "enabled": true, "updated_at": now},
		{"id": "allow-local-read", "name": "Allow local reads", "description": "Trusted local read-only operations are allowed", "priority": 100, "decision": "ALLOW", "match_summary": "trust = local AND risk = READ_ONLY", "enabled": true, "updated_at": now},
	}
}

func (s *Server) desktopConnectors() []map[string]any {
	return []map[string]any{
		{"id": "slack-simulator", "name": "Slack", "kind": "slack", "mode": "simulated", "health": "healthy", "enabled": true, "kill_switch": s.policy.ConnectorDisabled("slack-simulator"), "last_event_at": nil, "events_today": 0, "target_summary": "Deterministic simulator; Socket Mode recommended for live local-first ingress", "error": nil},
		{"id": "mattermost-simulator", "name": "Mattermost", "kind": "mattermost", "mode": "simulated", "health": "healthy", "enabled": true, "kill_switch": s.policy.ConnectorDisabled("mattermost-simulator"), "last_event_at": nil, "events_today": 0, "target_summary": "Deterministic simulator; bot WebSocket adapter boundary", "error": nil},
		{"id": "teams-simulator", "name": "Microsoft Teams", "kind": "teams", "mode": "simulated", "health": "healthy", "enabled": true, "kill_switch": s.policy.ConnectorDisabled("teams-simulator"), "last_event_at": nil, "events_today": 0, "target_summary": "Deterministic simulator; live Bot Connector JWT verifier required", "error": nil},
		{"id": "webhook-default", "name": "Signed webhook", "kind": "webhook", "mode": "live", "health": configuredHealth(s.config.WebhookSecret), "enabled": s.config.WebhookSecret != "", "kill_switch": s.policy.ConnectorDisabled("webhook-default"), "last_event_at": nil, "events_today": 0, "target_summary": "HMAC-SHA256 with timestamp and replay nonce", "error": configuredError(s.config.WebhookSecret)},
	}
}

func (s *Server) desktopProviders() []map[string]any {
	return []map[string]any{
		{"id": "ai-mock", "name": "Deterministic agents", "category": "ai", "adapter": "builtin", "mode": "simulated", "health": "healthy", "enabled": true, "secret_reference": nil, "latency_ms": 0, "detail": "Offline deterministic orchestration validation"},
		{"id": "stt-default", "name": "Speech-to-text", "category": "stt", "adapter": "mock", "mode": "simulated", "health": "healthy", "enabled": true, "secret_reference": nil, "latency_ms": 0, "detail": "Mock provider treats frames as UTF-8 fragments; no acoustic STT is active"},
		{"id": "tts-default", "name": "Text-to-speech", "category": "tts", "adapter": s.config.TTSProvider, "mode": providerMode(s.config.TTSProvider), "health": "healthy", "enabled": true, "secret_reference": nil, "latency_ms": 0, "detail": "Mock provider streams text chunks"},
		{"id": "media-default", "name": "Media transport", "category": "media", "adapter": "simulated", "mode": "simulated", "health": "healthy", "enabled": true, "secret_reference": nil, "latency_ms": 0, "detail": "Control-plane simulator; no WebRTC or acoustic audio is active"},
		{"id": "push-none", "name": "Android push", "category": "push", "adapter": "none", "mode": "local", "health": "disabled", "enabled": false, "secret_reference": nil, "latency_ms": nil, "detail": "LAN delivery cannot wake a sleeping app; configure an optional push adapter"},
	}
}

func (s *Server) desktopDiagnostics(now time.Time, diagnostics persistence.Diagnostics, activeVoice int) map[string]any {
	status := "healthy"
	if !diagnostics.DatabaseOK {
		status = "degraded"
	}
	streamStats := s.hub.Stats()
	backupCount, lastBackupAt, lastBackupPath := backupStats(s.config.DataDir)
	freeBytes, _ := availableDiskBytes(s.config.DataDir)
	return map[string]any{
		"generated_at": now,
		"checks": []map[string]any{
			{"id": "database", "name": "SQLite", "status": status, "detail": diagnostics.DatabaseDetail, "checked_at": now},
			{"id": "media", "name": "Media transport", "status": "healthy", "detail": "Simulated control plane; no acoustic media", "checked_at": now},
		},
		"event_stream": map[string]any{"connected_clients": streamStats.Subscribers, "last_event_id": streamStats.LastEventID, "backlog": streamStats.Queued},
		"webrtc":       map[string]any{"active_peers": activeVoice, "stun": "not configured", "turn": "not configured"},
		"storage":      map[string]any{"free_bytes": freeBytes, "backup_count": backupCount, "last_backup_at": lastBackupAt, "last_backup_path": lastBackupPath},
		"recent_logs":  []any{},
	}
}

func backupStats(dataDir string) (int, any, any) {
	directory := filepath.Join(dataDir, "backups")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, nil, nil
	}
	count := 0
	var latest time.Time
	var latestPath string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".db" {
			continue
		}
		count++
		if info, statErr := entry.Info(); statErr == nil && info.ModTime().After(latest) {
			latest = info.ModTime().UTC()
			latestPath = filepath.Join(directory, entry.Name())
		}
	}
	if latestPath == "" {
		return count, nil, nil
	}
	return count, latest, latestPath
}

func countActiveConversationTasks(items []tasks.Task, conversationID string) int {
	count := 0
	for _, item := range items {
		if item.ConversationID == conversationID && !item.Status.Terminal() {
			count++
		}
	}
	return count
}

func taskQueueCounts(items []tasks.Task) (queued, running, blocked int) {
	for _, item := range items {
		switch item.Status {
		case tasks.StatusCreated, tasks.StatusQueued, tasks.StatusAssigned:
			queued++
		case tasks.StatusRunning, tasks.StatusWaitingForChildren, tasks.StatusCancelRequested:
			running++
		case tasks.StatusBlocked, tasks.StatusWaitingForApproval:
			blocked++
		}
	}
	return
}

func desktopConversationSource(key string) string {
	prefix, _, _ := strings.Cut(key, ":")
	switch prefix {
	case "slack", "mattermost", "teams", "webhook":
		return prefix
	case "voice", "device", "android_voice":
		return "android"
	default:
		return "desktop"
	}
}

func retentionName(enabled bool) string {
	if enabled {
		return "retained"
	}
	return "disabled"
}

func nullableDesktop(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func currentTool(task tasks.Task) any {
	if task.TaskType == "shell" && !task.Status.Terminal() {
		return "shell"
	}
	return nil
}

func agentName(id string) string {
	if id == "" {
		return ""
	}
	value := strings.TrimPrefix(id, "builtin.")
	return strings.ToUpper(value[:1]) + value[1:]
}

func desktopExecutionMode(value string) string {
	switch value {
	case "builtin":
		return "built_in"
	case "subprocess":
		return "local_process"
	default:
		return value
	}
}

func commandPreview(approval approvals.Approval) any {
	var input struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if json.Unmarshal(approval.ToolArguments, &input) != nil || input.Command == "" {
		return nil
	}
	return strings.Join(append([]string{input.Command}, input.Args...), " ")
}

func auditCategory(resource string) string {
	switch resource {
	case "tool", "tool_invocation":
		return "tool"
	case "approval":
		return "approval"
	case "device":
		return "device"
	case "connector", "delivery":
		return "connector"
	case "task":
		return "task"
	default:
		return "security"
	}
}

func auditDecision(value string) string {
	switch strings.ToUpper(value) {
	case "ALLOW", "COMPLETED":
		return "allowed"
	case "DENY":
		return "denied"
	case "FAILED":
		return "failed"
	default:
		return "recorded"
	}
}

func sanitizedAuditSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "Recorded"
	}
	return "Details recorded with secret redaction"
}

func configuredHealth(secret string) string {
	if secret == "" {
		return "disabled"
	}
	return "healthy"
}

func configuredError(secret string) any {
	if secret == "" {
		return "Secret reference is not configured"
	}
	return nil
}

func providerMode(value string) string {
	if value == "mock" || strings.Contains(value, "simulated") {
		return "simulated"
	}
	return "local"
}

func enabledMessage(subject string, enabled bool) string {
	if enabled {
		return subject + " enabled."
	}
	return subject + " disabled."
}

func redactedPath(value string, redact bool) string {
	if redact {
		return filepath.Base(value)
	}
	return value
}
