package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/internal/ids"
	"github.com/veqri/veqri/internal/stream"
)

func (s *Server) handleLocalEvent(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Type            string          `json:"type"`
		Data            json.RawMessage `json:"data"`
		ConversationKey string          `json:"conversation_key,omitempty"`
		IdempotencyKey  string          `json:"idempotency_key,omitempty"`
		CreateTask      bool            `json:"create_task,omitempty"`
	}
	if !decodeJSON(writer, request, &body) {
		return
	}
	if body.Type == "" {
		writeError(writer, http.StatusBadRequest, "event_type", "event type is required")
		return
	}
	principal := principalFromContext(request.Context())
	if body.IdempotencyKey == "" {
		body.IdempotencyKey = request.Header.Get("Idempotency-Key")
	}
	if body.IdempotencyKey == "" {
		body.IdempotencyKey = ids.New()
	}
	if body.ConversationKey == "" {
		body.ConversationKey = "local-events:" + body.Type
	}
	var data struct {
		Text string `json:"text"`
		Goal string `json:"goal"`
	}
	_ = json.Unmarshal(body.Data, &data)
	goal := data.Text
	if goal == "" {
		goal = data.Goal
	}
	if body.CreateTask || goal != "" {
		task, duplicate, err := s.submitAsk(request.Context(), askContext{
			Request: AskRequest{Text: goal, ConversationKey: body.ConversationKey, IdempotencyKey: body.IdempotencyKey},
			Source:  events.Source{Kind: "local_event", ConnectorID: "veqri-cli", InstanceID: "localhost"},
			Actor:   events.Actor{ID: principal.ID}, Trust: events.TrustLocal, OccurredAt: time.Now().UTC(),
		})
		if err != nil {
			writeError(writer, http.StatusBadRequest, "local_event_task", err.Error())
			return
		}
		writeJSON(writer, http.StatusAccepted, map[string]any{"accepted": true, "task": task, "duplicate": duplicate})
		return
	}
	now := time.Now().UTC()
	event := events.Envelope{
		ID: ids.New(), Type: body.Type, Version: 1,
		Source: events.Source{Kind: "local_event", ConnectorID: "veqri-cli", InstanceID: "localhost"},
		Actor:  events.Actor{ID: principal.ID}, OccurredAt: now, ReceivedAt: now,
		ConversationKey: body.ConversationKey, CorrelationID: ids.New(),
		IdempotencyKey: body.IdempotencyKey, TrustLevel: events.TrustLocal, Payload: body.Data,
	}
	eventID, duplicate, err := s.store.IngestEvent(request.Context(), event)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "local_event", err.Error())
		return
	}
	if !duplicate {
		_ = s.store.MarkEventProcessed(request.Context(), eventID, nil)
		s.hub.Publish(stream.Event{Type: "event.received", CorrelationID: event.CorrelationID, Payload: event})
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"accepted": true, "event_id": eventID, "duplicate": duplicate})
}

func (s *Server) handleReplayEvent(writer http.ResponseWriter, request *http.Request) {
	original, err := s.store.GetEvent(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, persistenceStatus(err), "event_not_found", "event was not found")
		return
	}
	var data struct {
		Text string `json:"text"`
		Goal string `json:"goal"`
	}
	_ = json.Unmarshal(original.Payload, &data)
	goal := data.Text
	if goal == "" {
		goal = data.Goal
	}
	replayID := ids.New()
	if goal != "" {
		task, _, err := s.submitAsk(request.Context(), askContext{
			Request: AskRequest{Text: goal, ConversationKey: original.ConversationKey,
				IdempotencyKey: "replay:" + original.ID + ":" + replayID},
			Source: events.Source{Kind: "development_replay", ConnectorID: "local-admin", InstanceID: "localhost"},
			Actor:  events.Actor{ID: principalFromContext(request.Context()).ID},
			Trust:  events.TrustLocal, OccurredAt: time.Now().UTC(),
		})
		if err != nil {
			writeError(writer, http.StatusBadRequest, "event_replay", err.Error())
			return
		}
		writeJSON(writer, http.StatusAccepted, map[string]any{"replayed_event_id": original.ID, "task": task})
		return
	}
	causationID := original.ID
	replay := original
	replay.ID = replayID
	replay.Source = events.Source{Kind: "development_replay", ConnectorID: "local-admin", InstanceID: "localhost"}
	replay.Actor = events.Actor{ID: principalFromContext(request.Context()).ID}
	replay.OccurredAt = time.Now().UTC()
	replay.ReceivedAt = replay.OccurredAt
	replay.CorrelationID = ids.New()
	replay.CausationID = &causationID
	replay.IdempotencyKey = "replay:" + original.ID + ":" + replayID
	replay.TrustLevel = events.TrustLocal
	replay.ReplyTarget = events.ReplyTarget{}
	eventID, _, err := s.store.IngestEvent(request.Context(), replay)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "event_replay", err.Error())
		return
	}
	_ = s.store.MarkEventProcessed(request.Context(), eventID, nil)
	s.hub.Publish(stream.Event{Type: "event.replayed", CorrelationID: replay.CorrelationID, Payload: replay})
	writeJSON(writer, http.StatusAccepted, map[string]any{"replayed_event_id": original.ID, "event_id": eventID})
}
