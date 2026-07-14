package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"github.com/veqri/veqri/core/approvals"
	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/internal/auth"
	"github.com/veqri/veqri/internal/ids"
	"github.com/veqri/veqri/internal/stream"
)

type deviceVoicePreferences struct {
	Muted      bool
	PushToTalk bool
	AudioRoute string
}

func (s *Server) handleDeviceWebSocket(writer http.ResponseWriter, request *http.Request) {
	token := auth.BearerToken(request.Header.Get("Authorization"))
	principal, err := s.authenticator.Authenticate(request.Context(), token)
	if err != nil || principal.Kind != "device" {
		writeError(writer, http.StatusUnauthorized, "device_auth", "paired device authentication required")
		return
	}
	if claimedID := request.Header.Get("X-Veqri-Device-Id"); claimedID != "" && claimedID != principal.ID {
		writeError(writer, http.StatusForbidden, "device_identity", "device identity does not match its credential")
		return
	}
	if !supportedProtocol(request.Header.Get("X-Veqri-Protocol-Version")) {
		writeError(writer, http.StatusUpgradeRequired, "protocol_version", "protocol version 1 is required")
		return
	}
	if !hasWebSocketProtocol(request.Header.Values("Sec-WebSocket-Protocol"), "veqri.v1") {
		writeError(writer, http.StatusUpgradeRequired, "websocket_protocol", "WebSocket subprotocol veqri.v1 is required")
		return
	}
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{"veqri.v1"}})
	if err != nil {
		return
	}
	connection.SetReadLimit(128 << 10)
	s.registerDeviceSocket(principal.ID, connection)
	defer s.unregisterDeviceSocket(principal.ID, connection)
	defer func() { _ = connection.Close(websocket.StatusNormalClosure, "device stream ended") }()
	if status, reason := s.deviceSocketCredentialCloseStatus(request.Context(), principal.ID, token); status != 0 {
		_ = connection.Close(status, reason)
		return
	}
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	eventsChannel := s.hub.Subscribe(ctx, 256)
	catchup, err := s.androidCatchup(ctx, principal.ID)
	if err != nil {
		_ = connection.Close(websocket.StatusInternalError, "could not load durable catch-up")
		return
	}
	if writeWebSocketJSON(ctx, connection, catchup) != nil {
		return
	}
	direct := make(chan any, 16)
	go s.readDeviceCommands(ctx, cancel, connection, principal.ID, direct)
	pingTicker := time.NewTicker(20 * time.Second)
	defer pingTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case response := <-direct:
			if writeWebSocketJSON(ctx, connection, response) != nil {
				return
			}
		case event, ok := <-eventsChannel:
			if !ok {
				return
			}
			translated := s.androidEvent(ctx, principal.ID, event)
			if translated != nil && writeWebSocketJSON(ctx, connection, translated) != nil {
				return
			}
		case <-pingTicker.C:
			pingContext, pingCancel := context.WithTimeout(ctx, 5*time.Second)
			if status, reason := s.deviceSocketCredentialCloseStatus(pingContext, principal.ID, token); status != 0 {
				_ = connection.Close(status, reason)
				pingCancel()
				return
			}
			err := connection.Ping(pingContext)
			pingCancel()
			if err != nil {
				return
			}
		}
	}
}

func (s *Server) deviceSocketCredentialCloseStatus(ctx context.Context, deviceID, token string) (websocket.StatusCode, string) {
	principal, err := s.authenticator.Authenticate(ctx, token)
	if err == nil && principal.Kind == "device" && principal.ID == deviceID {
		return 0, ""
	}
	active, activeErr := s.store.DeviceIsActive(ctx, deviceID)
	if activeErr != nil && !errors.Is(activeErr, persistence.ErrNotFound) {
		return websocket.StatusInternalError, "could not revalidate device credential"
	}
	if active {
		// The identity still exists, so the exact bearer was superseded by a
		// committed rotation. Code 4004 lets Android promote its durable slot.
		return websocket.StatusCode(4004), "device credential rotated"
	}
	return websocket.StatusCode(4003), "device revoked"
}

const (
	androidSnapshotMessageLimit         = 128
	androidSnapshotTaskLimit            = 128
	androidSnapshotActiveTaskLimit      = 64
	androidSnapshotApprovalLimit        = 128
	androidSnapshotPendingApprovalLimit = 64
	androidSnapshotTextBytes            = 512
	androidSnapshotMaxBytes             = 120 << 10
	androidLiveTTSMaxBytes              = 12 << 10
)

var androidActiveTaskStatuses = []tasks.Status{
	tasks.StatusCreated, tasks.StatusQueued, tasks.StatusAssigned, tasks.StatusRunning,
	tasks.StatusWaitingForChildren, tasks.StatusWaitingForApproval, tasks.StatusBlocked,
	tasks.StatusCancelRequested,
}

var androidTerminalTaskStatuses = []tasks.Status{
	tasks.StatusCompleted, tasks.StatusPartiallyCompleted, tasks.StatusFailed,
	tasks.StatusCancelled, tasks.StatusTimedOut,
}

// androidCatchup returns one bounded, authoritative replacement event. Sending
// a single queue item lets Android prune records that disappeared while it was
// offline without replaying hundreds of upserts on every reconnect.
func (s *Server) androidCatchup(ctx context.Context, deviceID string) (any, error) {
	transcriptRetention, err := s.deviceTranscriptRetention(ctx, deviceID, nil)
	if err != nil {
		return nil, err
	}
	conversationIDs, err := s.store.ListDeviceConversationIDs(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	var turns []conversation.Turn
	for _, conversationID := range conversationIDs {
		conversationTurns, err := s.store.ListTurns(ctx, conversationID, androidSnapshotMessageLimit)
		if err != nil {
			return nil, err
		}
		turns = append(turns, conversationTurns...)
	}
	turns = androidRecentTurns(turns)
	messages := make([]map[string]any, 0, len(turns))
	for _, turn := range turns {
		messages = append(messages, androidSnapshotMessagePayload(turn))
	}

	activeTasks, err := s.store.ListTasks(ctx, androidActiveTaskStatuses, androidSnapshotActiveTaskLimit)
	if err != nil {
		return nil, err
	}
	terminalTasks, err := s.store.ListTasksByRecency(ctx, androidTerminalTaskStatuses, androidSnapshotTaskLimit)
	if err != nil {
		return nil, err
	}
	taskList, activeTaskCount := androidSelectSnapshotTasks(activeTasks, terminalTasks)
	taskPayloads := make([]map[string]any, 0, len(taskList))
	for _, task := range taskList {
		taskPayloads = append(taskPayloads, s.androidSnapshotTaskPayload(ctx, task))
	}

	pendingApprovals, err := s.store.ListApprovals(ctx, true, androidSnapshotPendingApprovalLimit)
	if err != nil {
		return nil, err
	}
	recentApprovals, err := s.store.ListApprovals(ctx, false, androidSnapshotApprovalLimit)
	if err != nil {
		return nil, err
	}
	approvalList, pendingApprovalCount := androidSelectSnapshotApprovals(pendingApprovals, recentApprovals)
	approvalPayloads := make([]map[string]any, 0, len(approvalList))
	for _, approval := range approvalList {
		approvalPayloads = append(approvalPayloads, androidSnapshotApprovalPayload(approval))
	}
	voiceSessions, err := s.store.ListVoiceSessions(ctx, true, 20)
	if err != nil {
		return nil, err
	}
	var voicePayload map[string]any
	conversationID := ""
	for _, session := range voiceSessions {
		if session.DeviceID != deviceID {
			continue
		}
		voicePayload = s.androidVoicePayload(session, session.Direction == "INCOMING")
		conversationID = session.ConversationID
		break
	}
	if conversationID == "" && len(turns) > 0 {
		conversationID = turns[len(turns)-1].ConversationID
	}
	snapshotID := ids.New()
	return androidBoundedSnapshotEvent(snapshotID, conversationID, transcriptRetention, messages, taskPayloads,
		activeTaskCount, approvalPayloads, pendingApprovalCount, voicePayload)
}

func androidRecentTurns(turns []conversation.Turn) []conversation.Turn {
	sort.SliceStable(turns, func(i, j int) bool { return turns[i].CreatedAt.Before(turns[j].CreatedAt) })
	if len(turns) > androidSnapshotMessageLimit {
		return turns[len(turns)-androidSnapshotMessageLimit:]
	}
	return turns
}

func androidSelectSnapshotTasks(active, terminal []tasks.Task) ([]tasks.Task, int) {
	if len(active) > androidSnapshotActiveTaskLimit {
		active = active[:androidSnapshotActiveTaskLimit]
	}
	result := append(make([]tasks.Task, 0, androidSnapshotTaskLimit), active...)
	activeIDs := make(map[string]struct{}, len(active))
	for _, task := range active {
		activeIDs[task.ID] = struct{}{}
	}
	for _, task := range terminal {
		if len(result) == androidSnapshotTaskLimit {
			break
		}
		if _, duplicate := activeIDs[task.ID]; duplicate {
			continue
		}
		result = append(result, task)
	}
	return result, len(active)
}

func androidSelectSnapshotApprovals(pending, recent []approvals.Approval) ([]approvals.Approval, int) {
	if len(pending) > androidSnapshotPendingApprovalLimit {
		pending = pending[:androidSnapshotPendingApprovalLimit]
	}
	result := append(make([]approvals.Approval, 0, androidSnapshotApprovalLimit), pending...)
	pendingIDs := make(map[string]struct{}, len(pending))
	for _, approval := range pending {
		pendingIDs[approval.ID] = struct{}{}
	}
	for _, approval := range recent {
		if len(result) == androidSnapshotApprovalLimit {
			break
		}
		if _, duplicate := pendingIDs[approval.ID]; duplicate || approval.Status == approvals.StatusPending {
			continue
		}
		result = append(result, approval)
	}
	return result, len(pending)
}

func (s *Server) registerDeviceSocket(deviceID string, connection *websocket.Conn) {
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	if s.deviceSockets[deviceID] == nil {
		s.deviceSockets[deviceID] = make(map[*websocket.Conn]struct{})
	}
	s.deviceSockets[deviceID][connection] = struct{}{}
}

func (s *Server) unregisterDeviceSocket(deviceID string, connection *websocket.Conn) {
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	delete(s.deviceSockets[deviceID], connection)
	if len(s.deviceSockets[deviceID]) == 0 {
		delete(s.deviceSockets, deviceID)
	}
}

func (s *Server) closeDeviceSockets(deviceID string) {
	s.deviceMu.Lock()
	connections := make([]*websocket.Conn, 0, len(s.deviceSockets[deviceID]))
	for connection := range s.deviceSockets[deviceID] {
		connections = append(connections, connection)
	}
	s.deviceMu.Unlock()
	for _, connection := range connections {
		_ = connection.Close(websocket.StatusCode(4003), "device revoked")
	}
}

func (s *Server) readDeviceCommands(ctx context.Context, cancel context.CancelFunc,
	connection *websocket.Conn, deviceID string, direct chan<- any) {
	defer cancel()
	commandLimiter := rate.NewLimiter(rate.Limit(10), 20)
	for {
		messageType, raw, err := connection.Read(ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		if !commandLimiter.Allow() {
			select {
			case direct <- androidSystemMessage("Device command rate exceeded; wait before retrying.", ids.New()):
			case <-ctx.Done():
				return
			}
			continue
		}
		commandID, commandType := androidCommandIdentity(raw)
		commandErr := s.handleDeviceCommand(ctx, deviceID, raw)
		// Successful retention changes include their audit entry in the same
		// transaction as the privacy setting and scrub. Task controls likewise
		// commit their audit with the task mutation. Rejected outcomes retain the
		// generic command audit path.
		if !deviceCommandAuditedAtomically(commandType) || commandErr != nil {
			s.auditDeviceCommand(context.WithoutCancel(ctx), deviceID, raw, commandErr)
		}
		if commandID != "" {
			select {
			case direct <- androidCommandResult(commandID, commandType, commandErr):
			case <-ctx.Done():
				return
			}
		}
		if commandErr != nil {
			select {
			case direct <- androidSystemMessage("Device command failed: "+commandErr.Error(), ids.New()):
			case <-ctx.Done():
				return
			}
		}
	}
}

func (s *Server) handleDeviceCommand(ctx context.Context, deviceID string, raw []byte) error {
	var command struct {
		CommandID        string `json:"command_id"`
		ProtocolVersion  int    `json:"protocol_version"`
		Type             string `json:"type"`
		ConversationID   string `json:"conversation_id"`
		Text             string `json:"text"`
		SessionID        string `json:"session_id"`
		ApprovalID       string `json:"approval_id"`
		TaskID           string `json:"task_id"`
		IsMuted          bool   `json:"is_muted"`
		Enabled          bool   `json:"enabled"`
		Route            string `json:"route"`
		RetainTranscript *bool  `json:"retain_transcript"`
	}
	if err := json.Unmarshal(raw, &command); err != nil {
		return errors.New("invalid command JSON")
	}
	if command.CommandID == "" || command.ProtocolVersion != 1 {
		return errors.New("command_id and protocol_version 1 are required")
	}
	switch command.Type {
	case "conversation.send_text":
		retainTranscript, err := s.deviceTranscriptRetention(ctx, deviceID, command.RetainTranscript)
		if err != nil {
			return err
		}
		conversationKey := "android:" + deviceID + ":default"
		if command.ConversationID != "" && command.ConversationID != "null" {
			owned, err := s.store.DeviceOwnsConversation(ctx, deviceID, command.ConversationID)
			if err != nil {
				return err
			}
			if !owned {
				return errors.New("conversation does not belong to this device")
			}
			conversationRecord, err := s.store.GetConversation(ctx, command.ConversationID)
			if err != nil {
				return err
			}
			conversationKey = conversationRecord.ExternalKey
			// A content command may reduce privacy but never elevate a
			// conversation that Core already records as non-retained. Re-enabling
			// requires the explicit commit-acknowledged policy command.
			retainTranscript = retainTranscript && conversationRecord.TranscriptRetention
		}
		_, _, err = s.submitAsk(ctx, askContext{
			Request: AskRequest{Text: command.Text, ConversationKey: conversationKey,
				IdempotencyKey: command.CommandID, RetainTranscript: &retainTranscript},
			Source: events.Source{Kind: "android", ConnectorID: "android-device", InstanceID: deviceID},
			Actor:  events.Actor{ID: deviceID}, Trust: events.TrustTrusted, OccurredAt: time.Now().UTC(),
		})
		return err
	case "conversation.set_transcript_retention":
		conversationID := command.ConversationID
		if conversationID == "null" {
			conversationID = ""
		}
		resourceKind, resourceID := "device", deviceID
		if conversationID != "" {
			resourceKind, resourceID = "conversation", conversationID
		}
		err := s.store.SetDeviceTranscriptRetention(ctx, deviceID, conversationID, command.Enabled,
			observability.AuditEntry{
				ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: "device", ActorID: deviceID,
				Action:       "device.command.conversation.set_transcript_retention",
				ResourceKind: resourceKind, ResourceID: resourceID, Decision: "ALLOW",
				Details: mustJSON(map[string]any{"type": command.Type, "result": "committed",
					"enabled": command.Enabled}),
				CorrelationID: command.CommandID, ConversationID: conversationID,
			})
		if err == nil {
			s.hub.Publish(stream.Event{Type: "conversation.retention_changed", ConversationID: conversationID,
				CorrelationID: command.CommandID, Payload: map[string]any{"enabled": command.Enabled}})
		}
		return err
	case "voice.start":
		_, err := s.startDeviceVoice(ctx, deviceID, false, command.RetainTranscript)
		return err
	case "debug.voice.incoming":
		_, err := s.startDeviceVoice(ctx, deviceID, true, command.RetainTranscript)
		return err
	case "voice.answer":
		return s.answerDeviceVoice(ctx, deviceID, command.SessionID)
	case "voice.decline", "voice.end":
		return s.endDeviceVoice(ctx, deviceID, command.SessionID)
	case "voice.set_muted":
		return s.updateDeviceVoicePreferences(ctx, deviceID, command.SessionID, func(preferences *deviceVoicePreferences) { preferences.Muted = command.IsMuted })
	case "voice.set_push_to_talk":
		return s.updateDeviceVoicePreferences(ctx, deviceID, command.SessionID, func(preferences *deviceVoicePreferences) { preferences.PushToTalk = command.Enabled })
	case "voice.select_audio_route":
		return s.updateDeviceVoicePreferences(ctx, deviceID, command.SessionID, func(preferences *deviceVoicePreferences) { preferences.AudioRoute = command.Route })
	case "voice.interrupt_tts":
		session, err := s.store.GetVoiceSession(ctx, command.SessionID)
		if err != nil {
			return err
		}
		if session.DeviceID != deviceID || session.State != conversation.StateSpeaking {
			return errors.New("voice session is not speaking for this device")
		}
		s.cancelTTS(session.ID)
		_, err = s.store.TransitionVoiceSession(ctx, session.ID, conversation.StateInterrupted, true)
		if err == nil {
			s.publishDeviceVoice(session.ID, "voice.interrupted")
		}
		return err
	case "approval.approve", "approval.deny":
		approved := command.Type == "approval.approve"
		approval, task, err := s.store.DecideApproval(ctx, command.ApprovalID, "device:"+deviceID, approved)
		if err != nil {
			return err
		}
		if approved {
			s.runtime.Wake()
		}
		s.hub.Publish(stream.Event{Type: "approval.changed", TaskID: task.ID,
			ConversationID: task.ConversationID, CorrelationID: approval.CorrelationID,
			Payload: map[string]any{"approval": approval, "task": task}})
		s.hub.Publish(stream.Event{Type: "task.changed", TaskID: task.ID,
			ConversationID: task.ConversationID, CorrelationID: task.CorrelationID, Payload: task})
		return nil
	case "task.cancel":
		_, err := s.runtime.CancelWithAudit(ctx, command.TaskID,
			newTaskControlAudit("device", deviceID, "device.command."+command.Type,
				command.CommandID, map[string]any{"type": command.Type, "result": "completed"}))
		return err
	case "task.retry":
		_, err := s.store.RetryTaskWithAudit(ctx, command.TaskID,
			newTaskControlAudit("device", deviceID, "device.command."+command.Type,
				command.CommandID, map[string]any{"type": command.Type, "result": "completed"}))
		if err == nil {
			s.runtime.Wake()
		}
		return err
	default:
		return fmt.Errorf("unsupported command type %q", command.Type)
	}
}

func deviceCommandAuditedAtomically(commandType string) bool {
	switch commandType {
	case "conversation.set_transcript_retention", "task.cancel", "task.retry":
		return true
	default:
		return false
	}
}

func androidCommandIdentity(raw []byte) (string, string) {
	var command struct {
		CommandID string `json:"command_id"`
		Type      string `json:"type"`
	}
	if json.Unmarshal(raw, &command) != nil {
		return "", ""
	}
	return command.CommandID, command.Type
}

func androidCommandResult(commandID, commandType string, commandErr error) map[string]any {
	status := "COMMITTED"
	payload := map[string]any{
		"command_id": commandID, "command_type": commandType, "status": status,
	}
	if commandErr != nil {
		payload["status"] = "REJECTED"
		payload["safe_message"] = androidSafeCommandError(commandErr)
	}
	return map[string]any{
		"id": "command-result:" + commandID, "type": "command.result",
		"correlation_id": commandID, "payload": payload,
	}
}

func androidSafeCommandError(commandErr error) string {
	message := strings.TrimSpace(commandErr.Error())
	if message == "" {
		return "Core rejected the command."
	}
	return androidTruncateUTF8(message, 240)
}

func (s *Server) auditDeviceCommand(ctx context.Context, deviceID string, raw []byte, commandErr error) {
	var command struct {
		CommandID      string `json:"command_id"`
		Type           string `json:"type"`
		ConversationID string `json:"conversation_id"`
		SessionID      string `json:"session_id"`
		ApprovalID     string `json:"approval_id"`
		TaskID         string `json:"task_id"`
	}
	_ = json.Unmarshal(raw, &command)
	if len(command.Type) > 80 {
		command.Type = command.Type[:80]
	}
	decision := "ALLOW"
	result := "completed"
	if commandErr != nil {
		decision = "ERROR"
		result = "rejected"
	}
	correlationID := command.CommandID
	if correlationID == "" {
		correlationID = ids.New()
	}
	resourceKind, resourceID := "device_command", correlationID
	switch {
	case command.ApprovalID != "":
		resourceKind, resourceID = "approval", command.ApprovalID
	case command.TaskID != "":
		resourceKind, resourceID = "task", command.TaskID
	case command.SessionID != "":
		resourceKind, resourceID = "voice_session", command.SessionID
	case command.ConversationID != "":
		resourceKind, resourceID = "conversation", command.ConversationID
	}
	_ = s.store.AddAuditEntry(ctx, observability.AuditEntry{
		ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: "device", ActorID: deviceID,
		Action: "device.command." + command.Type, ResourceKind: resourceKind,
		ResourceID: resourceID, Decision: decision,
		Details:       mustJSON(map[string]any{"type": command.Type, "result": result}),
		CorrelationID: correlationID, TaskID: command.TaskID, ConversationID: command.ConversationID,
	})
}

func (s *Server) startDeviceVoice(ctx context.Context, deviceID string, incoming bool, retainOverride *bool) (conversation.VoiceSession, error) {
	if active, err := s.store.GetActiveVoiceSessionForDevice(ctx, deviceID); err == nil {
		return conversation.VoiceSession{}, fmt.Errorf("%w: device already has active voice session %s", persistence.ErrConflict, active.ID)
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return conversation.VoiceSession{}, fmt.Errorf("verify active voice session: %w", err)
	}
	sessionID := ids.New()
	retain, err := s.deviceTranscriptRetention(ctx, deviceID, retainOverride)
	if err != nil {
		return conversation.VoiceSession{}, err
	}
	conversationRecord, err := s.store.GetOrCreateConversation(ctx, "voice:"+deviceID+":"+sessionID,
		"Veqri call", retain, ids.New())
	if err != nil {
		return conversation.VoiceSession{}, err
	}
	state := conversation.StateConnecting
	if incoming {
		state = conversation.StateRinging
	}
	session := conversation.VoiceSession{
		ID: sessionID, ConversationID: conversationRecord.ID, DeviceID: deviceID,
		State: state, Transport: s.media.Name(), StartedAt: time.Now().UTC(), CorrelationID: ids.New(),
		Direction:  fallback(map[bool]string{true: "INCOMING", false: "OUTGOING"}[incoming], "OUTGOING"),
		AudioRoute: "EARPIECE",
	}
	if err := s.store.CreateVoiceSession(ctx, session); err != nil {
		return conversation.VoiceSession{}, err
	}
	s.voiceMu.Lock()
	s.voiceByConv[session.ConversationID] = session.ID
	s.voiceMu.Unlock()
	if incoming {
		s.hub.Publish(stream.Event{Type: "voice.incoming_call", ConversationID: session.ConversationID,
			CorrelationID: session.CorrelationID, Payload: session})
		return session, nil
	}
	mediaSession, err := s.media.Start(ctx, session.ID, deviceID)
	if err != nil {
		s.failVoiceSession(ctx, session.ID)
		return conversation.VoiceSession{}, err
	}
	if err = s.trackMediaSession(session.ID, mediaSession); err != nil {
		s.failVoiceSession(ctx, session.ID)
		return conversation.VoiceSession{}, err
	}
	session, err = s.store.TransitionVoiceSession(ctx, session.ID, conversation.StateListening, false)
	if err != nil {
		s.failVoiceSession(ctx, session.ID)
	}
	if err == nil {
		s.hub.Publish(stream.Event{Type: "voice.state", ConversationID: session.ConversationID,
			CorrelationID: session.CorrelationID, Payload: session})
	}
	return session, err
}

type devicePrivacySettings struct {
	TranscriptRetention bool `json:"transcript_retention"`
}

func (s *Server) deviceTranscriptRetention(ctx context.Context, deviceID string, override *bool) (bool, error) {
	settings := devicePrivacySettings{TranscriptRetention: s.config.TranscriptRetention}
	key := "device:" + deviceID + ":privacy"
	if err := s.store.GetSetting(ctx, key, &settings); err != nil && !errors.Is(err, persistence.ErrNotFound) {
		return false, err
	}
	// Content commands may opt down but cannot opt up from Core's durable
	// setting. Re-enabling requires the explicit acknowledged policy command.
	if override != nil && !*override {
		return false, nil
	}
	return settings.TranscriptRetention, nil
}

func (s *Server) answerDeviceVoice(ctx context.Context, deviceID, sessionID string) error {
	session, err := s.store.GetVoiceSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if session.DeviceID != deviceID || session.State != conversation.StateRinging {
		return errors.New("voice session is not ringing for this device")
	}
	session, err = s.store.TransitionVoiceSession(ctx, session.ID, conversation.StateConnecting, false)
	if err != nil {
		return err
	}
	mediaSession, err := s.media.Start(ctx, session.ID, deviceID)
	if err != nil {
		s.failVoiceSession(ctx, session.ID)
		return err
	}
	if err = s.trackMediaSession(session.ID, mediaSession); err != nil {
		s.failVoiceSession(ctx, session.ID)
		return err
	}
	session, err = s.store.TransitionVoiceSession(ctx, session.ID, conversation.StateListening, false)
	if err != nil {
		s.failVoiceSession(ctx, session.ID)
	}
	if err == nil {
		s.hub.Publish(stream.Event{Type: "voice.state", ConversationID: session.ConversationID,
			CorrelationID: session.CorrelationID, Payload: session})
	}
	return err
}

func (s *Server) endDeviceVoice(ctx context.Context, deviceID, sessionID string) error {
	session, err := s.store.GetVoiceSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if session.DeviceID != deviceID {
		return errors.New("voice session belongs to another device")
	}
	_, err = s.endVoiceSession(ctx, session)
	return err
}

func (s *Server) updateDeviceVoicePreferences(ctx context.Context, deviceID, sessionID string, update func(*deviceVoicePreferences)) error {
	session, err := s.store.GetVoiceSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if session.DeviceID != deviceID {
		return errors.New("voice session belongs to another device")
	}
	preferences := deviceVoicePreferences{Muted: session.Muted, PushToTalk: session.PushToTalk,
		AudioRoute: fallback(session.AudioRoute, "EARPIECE")}
	update(&preferences)
	session, err = s.store.UpdateVoicePreferences(ctx, sessionID, preferences.Muted,
		preferences.PushToTalk, preferences.AudioRoute)
	if err != nil {
		return err
	}
	s.hub.Publish(stream.Event{Type: "voice.state", ConversationID: session.ConversationID,
		CorrelationID: session.CorrelationID, Payload: session})
	return nil
}

func (s *Server) publishDeviceVoice(sessionID, eventType string) {
	session, err := s.store.GetVoiceSession(context.Background(), sessionID)
	if err == nil {
		s.hub.Publish(stream.Event{Type: eventType, ConversationID: session.ConversationID,
			CorrelationID: session.CorrelationID, Payload: session})
	}
}

func (s *Server) androidEvent(ctx context.Context, deviceID string, event stream.Event) any {
	payload := map[string]any{}
	eventType := ""
	switch {
	case event.Type == "conversation.turn.final":
		owned, err := s.store.DeviceOwnsConversation(ctx, deviceID, event.ConversationID)
		if err != nil || !owned {
			return nil
		}
		var turn conversation.Turn
		if remarshal(event.Payload, &turn) != nil {
			return nil
		}
		eventType = "conversation.message_added"
		payload = androidMessagePayload(turn)
	case strings.HasPrefix(event.Type, "task."):
		var task tasks.Task
		if remarshal(event.Payload, &task) != nil || task.ID == "" {
			return nil
		}
		eventType = "task.changed"
		payload = s.androidTaskPayload(ctx, task)
	case strings.HasPrefix(event.Type, "approval."):
		var approval approvals.Approval
		if remarshal(event.Payload, &approval) != nil || approval.ID == "" {
			var nested struct {
				Approval approvals.Approval `json:"approval"`
			}
			if remarshal(event.Payload, &nested) != nil {
				return nil
			}
			approval = nested.Approval
		}
		if approval.ID == "" {
			return nil
		}
		eventType = "approval.changed"
		payload = androidApprovalPayload(approval)
	case event.Type == "voice.incoming_call" || event.Type == "voice.state" || event.Type == "voice.interrupted" || event.Type == "voice.ended":
		var session conversation.VoiceSession
		if remarshal(event.Payload, &session) != nil || session.ID == "" {
			var nested struct {
				Session conversation.VoiceSession `json:"session"`
			}
			if remarshal(event.Payload, &nested) != nil {
				return nil
			}
			session = nested.Session
		}
		if session.ID == "" || session.DeviceID != deviceID {
			return nil
		}
		if event.Type == "voice.incoming_call" {
			eventType = "voice.incoming"
		} else {
			eventType = "voice.changed"
		}
		payload = s.androidVoicePayload(session, event.Type == "voice.incoming_call")
	case event.Type == "voice.transcript":
		owned, err := s.store.DeviceOwnsConversation(ctx, deviceID, event.ConversationID)
		if err != nil || !owned {
			return nil
		}
		var transcript struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
			Final     bool   `json:"final"`
		}
		if remarshal(event.Payload, &transcript) != nil {
			return nil
		}
		eventType = "transcript.partial"
		if transcript.Final {
			eventType = "transcript.final"
		}
		payload = map[string]any{"conversation_id": event.ConversationID, "text": transcript.Text}
	case event.Type == "voice.tts.playback":
		var spoken struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
		}
		if remarshal(event.Payload, &spoken) != nil || spoken.SessionID == "" || strings.TrimSpace(spoken.Text) == "" {
			return nil
		}
		session, err := s.store.GetVoiceSession(ctx, spoken.SessionID)
		if err != nil || session.DeviceID != deviceID || session.ConversationID != event.ConversationID ||
			session.State != conversation.StateSpeaking {
			return nil
		}
		eventType = "tts.speak"
		payload = map[string]any{
			"session_id":      session.ID,
			"conversation_id": event.ConversationID,
			"status":          "BUFFERING",
			"text":            androidTruncateUTF8(spoken.Text, androidLiveTTSMaxBytes),
		}
	case event.Type == "voice.tts.chunk":
		owned, err := s.store.DeviceOwnsConversation(ctx, deviceID, event.ConversationID)
		if err != nil || !owned {
			return nil
		}
		// Synthesizer chunks prove server-side streaming progress. Android
		// speaks only the single full tts.speak event, so chunks can never
		// replay or duplicate audible text.
		eventType = "tts.changed"
		payload = map[string]any{"status": "SPEAKING"}
	default:
		return nil
	}
	return map[string]any{"id": event.ID, "type": eventType,
		"correlation_id": event.CorrelationID, "payload": payload}
}

func androidMessagePayload(turn conversation.Turn) map[string]any {
	return map[string]any{
		"message_id": turn.ID, "conversation_id": turn.ConversationID,
		"author": strings.ToUpper(string(turn.Role)), "text": turn.Text,
		"created_at_epoch_millis": turn.CreatedAt.UnixMilli(), "correlation_id": turn.CorrelationID,
	}
}

func androidSnapshotMessagePayload(turn conversation.Turn) map[string]any {
	payload := androidMessagePayload(turn)
	payload["text"] = androidTruncateUTF8(turn.Text, androidSnapshotTextBytes)
	return payload
}

func (s *Server) androidTaskPayload(ctx context.Context, task tasks.Task) map[string]any {
	updated := task.CreatedAt
	if task.FinishedAt != nil {
		updated = *task.FinishedAt
	} else if task.StartedAt != nil {
		updated = *task.StartedAt
	}
	return map[string]any{
		"task_id": task.ID, "root_task_id": task.RootTaskID,
		"conversation_id": task.ConversationID, "goal": task.Goal,
		"assigned_agent": task.AssignedAgentID, "status": task.Status,
		"progress_percent": task.Progress, "summary": task.UserFacingSummary,
		"created_at_epoch_millis": task.CreatedAt.UnixMilli(), "updated_at_epoch_millis": updated.UnixMilli(),
		"correlation_id": task.CorrelationID, "can_retry": s.androidTaskCanRetry(ctx, task),
		"priority": task.Priority, "dismissed": task.Dismissed,
	}
}

func (s *Server) androidSnapshotTaskPayload(ctx context.Context, task tasks.Task) map[string]any {
	payload := s.androidTaskPayload(ctx, task)
	payload["goal"] = androidTruncateUTF8(task.Goal, androidSnapshotTextBytes)
	payload["summary"] = androidTruncateUTF8(task.UserFacingSummary, androidSnapshotTextBytes)
	payload["assigned_agent"] = androidTruncateUTF8(task.AssignedAgentID, 128)
	return payload
}

func (s *Server) androidTaskCanRetry(ctx context.Context, task tasks.Task) bool {
	if !androidTaskRetryCandidate(task) {
		return false
	}
	if task.ConversationID == "" {
		return true
	}
	conversationRecord, err := s.store.GetConversation(ctx, task.ConversationID)
	return err == nil && conversationRecord.TranscriptRetention
}

func androidTaskRetryCandidate(task tasks.Task) bool {
	if task.Dismissed || task.TaskType == "shell" || task.RetryCount >= task.MaxRetries {
		return false
	}
	switch task.Status {
	case tasks.StatusFailed, tasks.StatusTimedOut, tasks.StatusCancelled, tasks.StatusPartiallyCompleted:
		return true
	default:
		return false
	}
}

func androidApprovalPayload(approval approvals.Approval) map[string]any {
	status := string(approval.Status)
	if approval.Status == approvals.StatusConsumed {
		status = "APPROVED"
	}
	return map[string]any{
		"approval_id": approval.ID, "task_id": approval.TaskID,
		"title": "Approve " + approval.ToolName, "redacted_arguments": string(approval.ToolArguments),
		"requested_scopes": append([]string(nil), approval.RequestedScopes...), "reason": approval.Reason,
		"risk": approval.Risk, "expires_at_epoch_millis": approval.ExpiresAt.UnixMilli(), "status": status,
	}
}

func androidSnapshotApprovalPayload(approval approvals.Approval) map[string]any {
	return androidApprovalPayload(approval)
}

func androidBoundedSnapshotEvent(snapshotID, conversationID string, transcriptRetention bool,
	messages, taskPayloads []map[string]any, activeTaskCount int,
	approvalPayloads []map[string]any, pendingApprovalCount int,
	voicePayload map[string]any) (any, error) {
	if activeTaskCount > len(taskPayloads) {
		activeTaskCount = len(taskPayloads)
	}
	if pendingApprovalCount > len(approvalPayloads) {
		pendingApprovalCount = len(approvalPayloads)
	}
	for {
		event := map[string]any{
			"id": "snapshot:" + snapshotID, "type": "sync.snapshot", "correlation_id": snapshotID,
			"payload": map[string]any{
				"snapshot_id": snapshotID, "conversation_id": androidNullableString(conversationID),
				"transcript_retention": transcriptRetention,
				"messages":             messages, "tasks": taskPayloads, "approvals": approvalPayloads,
				"voice_session": voicePayload,
			},
		}
		raw, err := json.Marshal(event)
		if err != nil {
			return nil, fmt.Errorf("marshal Android snapshot: %w", err)
		}
		if len(raw) <= androidSnapshotMaxBytes {
			return event, nil
		}
		switch {
		case len(taskPayloads) > activeTaskCount:
			taskPayloads = taskPayloads[:len(taskPayloads)-1]
		case len(approvalPayloads) > pendingApprovalCount:
			approvalPayloads = approvalPayloads[:len(approvalPayloads)-1]
		case len(messages) > 0:
			messages = messages[1:]
		case len(approvalPayloads) > 0:
			approvalPayloads = approvalPayloads[:len(approvalPayloads)-1]
			pendingApprovalCount = len(approvalPayloads)
		case len(taskPayloads) > 0:
			taskPayloads = taskPayloads[:len(taskPayloads)-1]
			activeTaskCount = len(taskPayloads)
		default:
			return nil, fmt.Errorf("Android snapshot metadata exceeds %d bytes", androidSnapshotMaxBytes)
		}
	}
}

func androidTruncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	const suffix = "…"
	if maxBytes <= len(suffix) {
		return ""
	}
	end := maxBytes - len(suffix)
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end] + suffix
}

func androidNullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Server) androidVoicePayload(session conversation.VoiceSession, incoming bool) map[string]any {
	direction := fallback(session.Direction, "OUTGOING")
	if incoming {
		direction = "INCOMING"
	}
	ttsStatus := "IDLE"
	if session.State == conversation.StateSpeaking {
		ttsStatus = "SPEAKING"
	} else if session.State == conversation.StateInterrupted {
		ttsStatus = "INTERRUPTED"
	}
	return map[string]any{
		"session_id": session.ID, "conversation_id": session.ConversationID,
		"direction": direction, "phase": session.State,
		"started_at_epoch_millis": session.StartedAt.UnixMilli(), "is_muted": session.Muted,
		"is_push_to_talk": session.PushToTalk, "audio_route": fallback(session.AudioRoute, "EARPIECE"),
		"tts_status": ttsStatus, "is_simulated_media": strings.Contains(session.Transport, "simulated"),
		"media_notice": "Control-plane voice simulator; no acoustic media is transported.",
	}
}

func remarshal(value any, target any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func androidSystemMessage(text, correlationID string) map[string]any {
	return map[string]any{"id": ids.New(), "type": "conversation.message_added", "correlation_id": correlationID,
		"payload": map[string]any{"message_id": ids.New(), "conversation_id": "system",
			"author": "SYSTEM", "text": text, "created_at_epoch_millis": time.Now().UTC().UnixMilli()}}
}

func fallback(value, fallbackValue string) string {
	if value == "" {
		return fallbackValue
	}
	return value
}
