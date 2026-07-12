package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/veqri/veqri/connectors/mattermost"
	"github.com/veqri/veqri/connectors/slack"
	"github.com/veqri/veqri/connectors/webhook"
	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/internal/ids"
)

func (s *Server) handleSlackEvent(writer http.ResponseWriter, request *http.Request) {
	raw, ok := readRawBody(writer, request)
	if !ok {
		return
	}
	if err := (slack.SignatureVerifier{SigningSecret: s.config.SlackSigningSecret}).Verify(request, raw); err != nil {
		writeError(writer, http.StatusUnauthorized, "slack_signature", err.Error())
		return
	}
	var envelopeProbe struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(raw, &envelopeProbe); err != nil {
		writeError(writer, http.StatusBadRequest, "slack_event", "invalid Slack JSON")
		return
	}
	if envelopeProbe.Type == "url_verification" {
		writeJSON(writer, http.StatusOK, map[string]any{"challenge": envelopeProbe.Challenge})
		return
	}
	normalized, err := slack.Normalize("slack-default", raw, time.Now().UTC())
	if err != nil {
		writeError(writer, http.StatusBadRequest, "slack_event", err.Error())
		return
	}
	text := payloadText(normalized.Payload)
	task, duplicate, err := s.submitAsk(request.Context(), askContext{
		Request: AskRequest{Text: text, ConversationKey: normalized.ConversationKey, IdempotencyKey: normalized.IdempotencyKey},
		Source:  normalized.Source, Actor: normalized.Actor, Trust: normalized.TrustLevel,
		ReplyTarget: normalized.ReplyTarget, OccurredAt: normalized.OccurredAt,
	})
	if err != nil {
		writeError(writer, http.StatusBadRequest, "slack_task", err.Error())
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"accepted": true, "task_id": task.ID, "duplicate": duplicate})
}

func (s *Server) handleMattermostEvent(writer http.ResponseWriter, request *http.Request) {
	raw, ok := readRawBody(writer, request)
	if !ok {
		return
	}
	var webhookBody mattermost.OutgoingWebhook
	if err := json.Unmarshal(raw, &webhookBody); err != nil {
		writeError(writer, http.StatusBadRequest, "mattermost_event", "the deterministic endpoint accepts JSON; production uses bot WebSocket ingress")
		return
	}
	if err := mattermost.VerifyOutgoingToken(s.config.MattermostToken, webhookBody.Token); err != nil {
		writeError(writer, http.StatusUnauthorized, "mattermost_token", err.Error())
		return
	}
	normalized, err := mattermost.NormalizeOutgoing("mattermost-default", raw, time.Now().UTC())
	if err != nil {
		writeError(writer, http.StatusBadRequest, "mattermost_event", err.Error())
		return
	}
	task, duplicate, err := s.submitAsk(request.Context(), askContext{
		Request: AskRequest{Text: payloadText(normalized.Payload), ConversationKey: normalized.ConversationKey, IdempotencyKey: normalized.IdempotencyKey},
		Source:  normalized.Source, Actor: normalized.Actor, Trust: normalized.TrustLevel,
		ReplyTarget: normalized.ReplyTarget, OccurredAt: normalized.OccurredAt,
	})
	if err != nil {
		writeError(writer, http.StatusBadRequest, "mattermost_task", err.Error())
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{
		"accepted": true, "task_id": task.ID, "duplicate": duplicate,
		"response_type": "comment", "text": "Veqri accepted the task.",
	})
}

func (s *Server) handleGenericWebhook(writer http.ResponseWriter, request *http.Request) {
	raw, ok := readRawBody(writer, request)
	if !ok {
		return
	}
	connectorID := request.PathValue("connectorID")
	if connectorID != "webhook-default" {
		writeError(writer, http.StatusNotFound, "connector_not_found", "generic webhook connector is not configured")
		return
	}
	nonce, err := (webhook.Verifier{Secret: s.config.WebhookSecret}).Verify(request, raw)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "webhook_signature", err.Error())
		return
	}
	duplicateNonce, err := s.store.UseWebhookNonce(request.Context(), connectorID, nonce, time.Now().UTC())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "webhook_replay", "could not record replay nonce")
		return
	}
	if duplicateNonce {
		writeError(writer, http.StatusConflict, "webhook_replay", "webhook nonce was already used")
		return
	}
	var body struct {
		Type            string             `json:"type"`
		ConversationKey string             `json:"conversation_key"`
		IdempotencyKey  string             `json:"idempotency_key"`
		Actor           events.Actor       `json:"actor"`
		ReplyTarget     events.ReplyTarget `json:"reply_target"`
		Data            json.RawMessage    `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(writer, http.StatusBadRequest, "webhook_body", "invalid webhook JSON")
		return
	}
	if body.Type == "" || body.ConversationKey == "" || body.IdempotencyKey == "" {
		writeError(writer, http.StatusBadRequest, "webhook_body", "type, conversation_key, and idempotency_key are required")
		return
	}
	if strings.HasSuffix(strings.ToLower(connectorID), "-simulator") {
		writeError(writer, http.StatusBadRequest, "reply_target", "simulator connector IDs are reserved for the admin simulator endpoint")
		return
	}
	if body.ReplyTarget.ConnectorID != "" && body.ReplyTarget.ConnectorID != connectorID {
		writeError(writer, http.StatusBadRequest, "reply_target", "reply target must match the authenticated connector")
		return
	}
	body.ReplyTarget.ConnectorID = connectorID
	text := payloadText(body.Data)
	task, duplicate, err := s.submitAsk(request.Context(), askContext{
		Request: AskRequest{Text: text, ConversationKey: body.ConversationKey, IdempotencyKey: body.IdempotencyKey},
		Source:  events.Source{Kind: "webhook", ConnectorID: connectorID, InstanceID: connectorID},
		Actor:   body.Actor, Trust: events.TrustUntrusted, ReplyTarget: body.ReplyTarget, OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		writeError(writer, http.StatusBadRequest, "webhook_task", err.Error())
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"accepted": true, "task_id": task.ID, "duplicate": duplicate})
}

func (s *Server) handleConnectorSimulator(writer http.ResponseWriter, request *http.Request) {
	kind := strings.ToLower(request.PathValue("kind"))
	if kind != "slack" && kind != "mattermost" && kind != "teams" {
		writeError(writer, http.StatusBadRequest, "simulator_kind", "supported simulators: slack, mattermost, teams")
		return
	}
	var body struct {
		Text            string `json:"text"`
		ActorID         string `json:"actor_id"`
		ActorName       string `json:"actor_name,omitempty"`
		WorkspaceID     string `json:"workspace_id,omitempty"`
		ChannelID       string `json:"channel_id"`
		ThreadID        string `json:"thread_id,omitempty"`
		MessageID       string `json:"message_id,omitempty"`
		ConversationKey string `json:"conversation_key,omitempty"`
	}
	if !decodeJSON(writer, request, &body) {
		return
	}
	if body.ActorID == "" {
		body.ActorID = "simulated-user"
	}
	if body.WorkspaceID == "" {
		body.WorkspaceID = "simulated-workspace"
	}
	if body.ChannelID == "" {
		body.ChannelID = "simulated-channel"
	}
	if body.ThreadID == "" {
		body.ThreadID = "simulated-thread"
	}
	if body.MessageID == "" {
		body.MessageID = ids.New()
	}
	if body.ConversationKey == "" {
		body.ConversationKey = kind + ":" + body.WorkspaceID + ":" + body.ChannelID + ":" + body.ThreadID
	}
	connectorID := kind + "-simulator"
	task, duplicate, err := s.submitAsk(request.Context(), askContext{
		Request: AskRequest{Text: body.Text, ConversationKey: body.ConversationKey, IdempotencyKey: body.MessageID},
		Source:  events.Source{Kind: kind, ConnectorID: connectorID, InstanceID: body.WorkspaceID},
		Actor:   events.Actor{ID: body.ActorID, DisplayName: body.ActorName}, Trust: events.TrustUntrusted,
		ReplyTarget: events.ReplyTarget{ConnectorID: connectorID, ChannelID: body.ChannelID, ThreadID: body.ThreadID},
		OccurredAt:  time.Now().UTC(),
	})
	if err != nil {
		writeError(writer, http.StatusBadRequest, "simulator_task", err.Error())
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{
		"accepted": true, "simulated": true, "task": task, "duplicate": duplicate,
		"reply_target": map[string]string{"channel_id": body.ChannelID, "thread_id": body.ThreadID},
	})
}

func readRawBody(writer http.ResponseWriter, request *http.Request) ([]byte, bool) {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "request_body", "could not read request body")
		return nil, false
	}
	return raw, true
}

func payloadText(raw json.RawMessage) string {
	var payload struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &payload)
	return payload.Text
}
