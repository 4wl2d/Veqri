package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/core/voice"
	"github.com/veqri/veqri/internal/ids"
	"github.com/veqri/veqri/internal/stream"
)

func (s *Server) handleStartCall(writer http.ResponseWriter, request *http.Request) {
	if principalFromContext(request.Context()).Kind != "admin" {
		writeError(writer, http.StatusForbidden, "admin_required", "only the local PC administrator can initiate a call")
		return
	}
	var body struct {
		DeviceID       string `json:"device_id"`
		ConversationID string `json:"conversation_id,omitempty"`
	}
	if !decodeJSON(writer, request, &body) {
		return
	}
	if body.DeviceID == "" {
		writeError(writer, http.StatusBadRequest, "device_required", "device_id is required")
		return
	}
	devices, err := s.store.ListDevices(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "devices", "could not validate device")
		return
	}
	found := false
	for _, device := range devices {
		if device.ID == body.DeviceID && device.RevokedAt == nil {
			found = true
			break
		}
	}
	if !found {
		writeError(writer, http.StatusNotFound, "device_not_found", "paired active device was not found")
		return
	}
	if active, activeErr := s.store.GetActiveVoiceSessionForDevice(request.Context(), body.DeviceID); activeErr == nil {
		writeError(writer, http.StatusConflict, "voice_session_active", "device already has active voice session "+active.ID)
		return
	} else if !errors.Is(activeErr, persistence.ErrNotFound) {
		writeError(writer, http.StatusInternalServerError, "voice_session", "could not verify active voice sessions")
		return
	}
	sessionID := ids.New()
	correlationID := ids.New()
	conversationID := body.ConversationID
	if conversationID == "" {
		retain, retentionErr := s.deviceTranscriptRetention(request.Context(), body.DeviceID, nil)
		if retentionErr != nil {
			writeError(writer, http.StatusInternalServerError, "device_privacy", "could not load device privacy settings")
			return
		}
		conversationRecord, createErr := s.store.GetOrCreateConversation(request.Context(),
			"voice:"+body.DeviceID+":"+sessionID, "Veqri call", retain, ids.New())
		if createErr != nil {
			writeError(writer, http.StatusInternalServerError, "conversation", "could not create call conversation")
			return
		}
		conversationID = conversationRecord.ID
	} else if _, err := s.store.GetConversation(request.Context(), conversationID); err != nil {
		writeError(writer, persistenceStatus(err), "conversation_not_found", "conversation was not found")
		return
	}
	session := conversation.VoiceSession{
		ID: sessionID, ConversationID: conversationID, DeviceID: body.DeviceID,
		State: conversation.StateRinging, Transport: s.media.Name(), StartedAt: time.Now().UTC(),
		CorrelationID: correlationID,
		Direction:     "INCOMING", AudioRoute: "EARPIECE",
	}
	if err := s.store.CreateVoiceSession(request.Context(), session); err != nil {
		writeError(writer, http.StatusInternalServerError, "voice_session", "could not create voice session")
		return
	}
	s.voiceMu.Lock()
	s.voiceByConv[conversationID] = sessionID
	s.voiceMu.Unlock()
	s.hub.Publish(stream.Event{Type: "voice.incoming_call", ConversationID: conversationID,
		CorrelationID: correlationID, Payload: map[string]any{
			"session": session, "simulated_media": s.media.Name() == "simulated-no-audio",
			"lan_wake_limitation": "A stopped or sleeping app requires an optional external push adapter.",
		}})
	writeJSON(writer, http.StatusAccepted, map[string]any{"voice_session": session})
}

func (s *Server) handleVoiceSession(writer http.ResponseWriter, request *http.Request) {
	session, err := s.store.GetVoiceSession(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, persistenceStatus(err), "voice_session", "voice session was not found")
		return
	}
	if err := s.verifyVoiceOwner(request.Context(), session); err != nil {
		writeError(writer, http.StatusForbidden, "voice_owner", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"voice_session": session})
}

func (s *Server) handleAnswerCall(writer http.ResponseWriter, request *http.Request) {
	session, err := s.store.GetVoiceSession(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, persistenceStatus(err), "voice_session", "voice session was not found")
		return
	}
	if err := s.verifyVoiceOwner(request.Context(), session); err != nil {
		writeError(writer, http.StatusForbidden, "voice_owner", err.Error())
		return
	}
	if session.State != conversation.StateRinging {
		writeError(writer, http.StatusConflict, "voice_state", "call is not ringing")
		return
	}
	session, err = s.store.TransitionVoiceSession(request.Context(), session.ID, conversation.StateConnecting, false)
	if err != nil {
		writeError(writer, persistenceStatus(err), "voice_connect", err.Error())
		return
	}
	mediaSession, err := s.media.Start(request.Context(), session.ID, session.DeviceID)
	if err != nil {
		s.failVoiceSession(request.Context(), session.ID)
		writeError(writer, http.StatusServiceUnavailable, "media_transport", err.Error())
		return
	}
	if err = s.trackMediaSession(session.ID, mediaSession); err != nil {
		s.failVoiceSession(request.Context(), session.ID)
		writeError(writer, http.StatusServiceUnavailable, "media_transport", err.Error())
		return
	}
	session, err = s.store.TransitionVoiceSession(request.Context(), session.ID, conversation.StateListening, false)
	if err != nil {
		s.failVoiceSession(request.Context(), session.ID)
		writeError(writer, persistenceStatus(err), "voice_listen", err.Error())
		return
	}
	s.hub.Publish(stream.Event{Type: "voice.state", ConversationID: session.ConversationID,
		CorrelationID: session.CorrelationID, Payload: session})
	writeJSON(writer, http.StatusOK, map[string]any{"voice_session": session})
}

func (s *Server) handleVoiceTranscript(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Text           string `json:"text"`
		Final          bool   `json:"final"`
		Sequence       uint64 `json:"sequence"`
		IdempotencyKey string `json:"idempotency_key,omitempty"`
	}
	if !decodeJSON(writer, request, &body) {
		return
	}
	session, err := s.store.GetVoiceSession(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, persistenceStatus(err), "voice_session", "voice session was not found")
		return
	}
	if err := s.verifyVoiceOwner(request.Context(), session); err != nil {
		writeError(writer, http.StatusForbidden, "voice_owner", err.Error())
		return
	}
	if session.State == conversation.StateSpeaking {
		s.cancelTTS(session.ID)
		session, err = s.store.TransitionVoiceSession(request.Context(), session.ID, conversation.StateInterrupted, true)
		if err != nil {
			writeError(writer, http.StatusConflict, "barge_in", err.Error())
			return
		}
	}
	if session.State == conversation.StateInterrupted || session.State == conversation.StateReconnecting {
		session, err = s.store.TransitionVoiceSession(request.Context(), session.ID, conversation.StateListening, false)
		if err != nil {
			writeError(writer, http.StatusConflict, "voice_resume", err.Error())
			return
		}
	}
	if session.State == conversation.StateListening {
		session, err = s.store.TransitionVoiceSession(request.Context(), session.ID, conversation.StateTranscribing, false)
		if err != nil {
			writeError(writer, http.StatusConflict, "voice_transcribe", err.Error())
			return
		}
	} else if session.State != conversation.StateTranscribing {
		writeError(writer, http.StatusConflict, "voice_state", "session is not accepting transcript input")
		return
	}
	s.hub.Publish(stream.Event{Type: "voice.transcript", ConversationID: session.ConversationID,
		CorrelationID: session.CorrelationID, Payload: map[string]any{
			"session_id": session.ID, "text": body.Text, "final": body.Final, "sequence": body.Sequence,
		}})
	if !body.Final {
		writeJSON(writer, http.StatusAccepted, map[string]any{"accepted": true, "final": false})
		return
	}
	conversationRecord, err := s.store.GetConversation(request.Context(), session.ConversationID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "conversation", "could not load voice conversation")
		return
	}
	session, err = s.store.TransitionVoiceSession(request.Context(), session.ID, conversation.StateThinking, false)
	if err != nil {
		writeError(writer, http.StatusConflict, "voice_thinking", err.Error())
		return
	}
	session, err = s.store.TransitionVoiceSession(request.Context(), session.ID, conversation.StateDelegating, false)
	if err != nil {
		writeError(writer, http.StatusConflict, "voice_delegating", err.Error())
		return
	}
	principal := principalFromContext(request.Context())
	task, duplicate, err := s.submitAsk(request.Context(), askContext{
		Request: AskRequest{Text: body.Text, ConversationKey: conversationRecord.ExternalKey,
			IdempotencyKey: body.IdempotencyKey},
		Source: events.Source{Kind: "android_voice"}, Actor: events.Actor{ID: principal.ID},
		Trust: events.TrustTrusted, OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		s.failVoiceSession(request.Context(), session.ID)
		writeError(writer, http.StatusBadRequest, "voice_task", err.Error())
		return
	}
	session, err = s.store.TransitionVoiceSession(request.Context(), session.ID, conversation.StateWaitingForResult, false)
	if err != nil {
		writeError(writer, http.StatusConflict, "voice_wait", err.Error())
		return
	}
	s.hub.Publish(stream.Event{Type: "voice.state", TaskID: task.ID,
		ConversationID: session.ConversationID, CorrelationID: session.CorrelationID, Payload: session})
	writeJSON(writer, http.StatusAccepted, map[string]any{"voice_session": session, "task": task, "duplicate": duplicate})
}

func (s *Server) handleInterruptVoice(writer http.ResponseWriter, request *http.Request) {
	session, err := s.store.GetVoiceSession(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, persistenceStatus(err), "voice_session", "voice session was not found")
		return
	}
	if err := s.verifyVoiceOwner(request.Context(), session); err != nil {
		writeError(writer, http.StatusForbidden, "voice_owner", err.Error())
		return
	}
	if session.State != conversation.StateSpeaking {
		writeError(writer, http.StatusConflict, "voice_state", "TTS is not currently speaking")
		return
	}
	s.cancelTTS(session.ID)
	session, err = s.store.TransitionVoiceSession(request.Context(), session.ID, conversation.StateInterrupted, true)
	if err != nil {
		writeError(writer, persistenceStatus(err), "voice_interrupt", err.Error())
		return
	}
	s.hub.Publish(stream.Event{Type: "voice.interrupted", ConversationID: session.ConversationID,
		CorrelationID: session.CorrelationID, Payload: session})
	writeJSON(writer, http.StatusOK, map[string]any{"voice_session": session, "delegated_tasks_cancelled": false})
}

func (s *Server) handleReconnectVoice(writer http.ResponseWriter, request *http.Request) {
	session, err := s.store.GetVoiceSession(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, persistenceStatus(err), "voice_session", "voice session was not found")
		return
	}
	if err := s.verifyVoiceOwner(request.Context(), session); err != nil {
		writeError(writer, http.StatusForbidden, "voice_owner", err.Error())
		return
	}
	if session.State != conversation.StateReconnecting {
		if !conversation.CanTransition(session.State, conversation.StateReconnecting) {
			writeError(writer, http.StatusConflict, "voice_state", "voice state cannot reconnect")
			return
		}
		session, err = s.store.TransitionVoiceSession(request.Context(), session.ID, conversation.StateReconnecting, session.Interrupted)
		if err != nil {
			writeError(writer, persistenceStatus(err), "voice_reconnect", err.Error())
			return
		}
	}
	_ = s.closeMediaSession(session.ID)
	mediaSession, err := s.media.Recover(request.Context(), session.ID, session.DeviceID)
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "media_recover", err.Error())
		return
	}
	if err = s.trackMediaSession(session.ID, mediaSession); err != nil {
		writeError(writer, http.StatusServiceUnavailable, "media_recover", err.Error())
		return
	}
	session, err = s.store.TransitionVoiceSession(request.Context(), session.ID, conversation.StateListening, false)
	if err != nil {
		s.failVoiceSession(request.Context(), session.ID)
		writeError(writer, persistenceStatus(err), "voice_listen", err.Error())
		return
	}
	s.hub.Publish(stream.Event{Type: "voice.state", ConversationID: session.ConversationID,
		CorrelationID: session.CorrelationID, Payload: session})
	writeJSON(writer, http.StatusOK, map[string]any{"voice_session": session})
}

func (s *Server) handleEndVoice(writer http.ResponseWriter, request *http.Request) {
	session, err := s.store.GetVoiceSession(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(writer, persistenceStatus(err), "voice_session", "voice session was not found")
		return
	}
	if err := s.verifyVoiceOwner(request.Context(), session); err != nil {
		writeError(writer, http.StatusForbidden, "voice_owner", err.Error())
		return
	}
	session, err = s.endVoiceSession(request.Context(), session)
	if err != nil {
		writeError(writer, persistenceStatus(err), "voice_end", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"voice_session": session})
}

func (s *Server) endVoiceSession(ctx context.Context, session conversation.VoiceSession) (conversation.VoiceSession, error) {
	if session.State == conversation.StateEnded {
		return session, s.closeMediaSession(session.ID)
	}
	if !conversation.CanTransition(session.State, conversation.StateEnded) {
		return conversation.VoiceSession{}, errors.New("voice session cannot end from its current state")
	}
	s.cancelTTS(session.ID)
	closeErr := s.closeMediaSession(session.ID)
	updated, err := s.store.TransitionVoiceSession(ctx, session.ID, conversation.StateEnded, session.Interrupted)
	if err != nil {
		return conversation.VoiceSession{}, err
	}
	s.voiceMu.Lock()
	delete(s.voiceByConv, updated.ConversationID)
	delete(s.voiceQueues, updated.ID)
	delete(s.voiceSpeaking, updated.ID)
	s.voiceMu.Unlock()
	s.hub.Publish(stream.Event{Type: "voice.ended", ConversationID: updated.ConversationID,
		CorrelationID: updated.CorrelationID, Payload: updated})
	if closeErr != nil {
		return updated, fmt.Errorf("close voice media: %w", closeErr)
	}
	return updated, nil
}

func (s *Server) trackMediaSession(sessionID string, mediaSession voice.MediaSession) error {
	if sessionID == "" || mediaSession == nil {
		return errors.New("media transport returned an invalid session")
	}
	s.voiceMu.Lock()
	if _, exists := s.mediaSessions[sessionID]; exists {
		s.voiceMu.Unlock()
		_ = mediaSession.Close()
		return errors.New("media session is already tracked")
	}
	s.mediaSessions[sessionID] = mediaSession
	s.voiceMu.Unlock()
	return nil
}

func (s *Server) closeMediaSession(sessionID string) error {
	s.voiceMu.Lock()
	mediaSession := s.mediaSessions[sessionID]
	delete(s.mediaSessions, sessionID)
	s.voiceMu.Unlock()
	if mediaSession == nil {
		return nil
	}
	return mediaSession.Close()
}

func (s *Server) closeAllMediaSessions() error {
	s.voiceMu.Lock()
	sessions := make([]voice.MediaSession, 0, len(s.mediaSessions))
	for sessionID, mediaSession := range s.mediaSessions {
		sessions = append(sessions, mediaSession)
		delete(s.mediaSessions, sessionID)
	}
	s.voiceMu.Unlock()
	var closeErrors []error
	for _, mediaSession := range sessions {
		if err := mediaSession.Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	return errors.Join(closeErrors...)
}

func (s *Server) failVoiceSession(ctx context.Context, sessionID string) {
	_ = s.closeMediaSession(sessionID)
	session, err := s.store.GetVoiceSession(context.WithoutCancel(ctx), sessionID)
	if err != nil || !conversation.CanTransition(session.State, conversation.StateFailed) {
		return
	}
	failed, err := s.store.TransitionVoiceSession(context.WithoutCancel(ctx), sessionID, conversation.StateFailed, session.Interrupted)
	if err == nil {
		s.hub.Publish(stream.Event{Type: "voice.state", ConversationID: failed.ConversationID,
			CorrelationID: failed.CorrelationID, Payload: failed})
	}
}

func (s *Server) voiceDeliveryLoop(ctx context.Context) {
	eventsChannel := s.hub.Subscribe(ctx, 256)
	for event := range eventsChannel {
		if event.Type != "voice.tts.text" || event.ConversationID == "" || event.TaskID == "" {
			continue
		}
		var payload struct {
			Text string `json:"text"`
		}
		if remarshal(event.Payload, &payload) != nil || payload.Text == "" {
			continue
		}
		s.voiceMu.Lock()
		sessionID := s.voiceByConv[event.ConversationID]
		if sessionID != "" {
			s.voiceQueues[sessionID] = append(s.voiceQueues[sessionID],
				voiceDeliveryJob{TaskID: event.TaskID, Text: payload.Text})
			if !s.voiceSpeaking[sessionID] {
				s.voiceSpeaking[sessionID] = true
				go s.drainVoiceQueue(ctx, sessionID)
			}
		}
		s.voiceMu.Unlock()
	}
}

func (s *Server) drainVoiceQueue(parent context.Context, sessionID string) {
	for {
		s.voiceMu.Lock()
		queue := s.voiceQueues[sessionID]
		if len(queue) == 0 {
			s.voiceSpeaking[sessionID] = false
			s.voiceMu.Unlock()
			return
		}
		job := queue[0]
		s.voiceQueues[sessionID] = queue[1:]
		s.voiceMu.Unlock()
		s.speakText(parent, sessionID, job)
	}
}

func (s *Server) speakText(parent context.Context, sessionID string, job voiceDeliveryJob) {
	session, err := s.store.GetVoiceSession(parent, sessionID)
	if err != nil || session.State == conversation.StateEnded || session.State == conversation.StateFailed {
		return
	}
	if session.State == conversation.StateInterrupted {
		// A later user turn can resume delivery. Do not speak over the user.
		return
	}
	if !conversation.CanTransition(session.State, conversation.StateSpeaking) {
		return
	}
	session, err = s.store.TransitionVoiceSession(parent, sessionID, conversation.StateSpeaking, false)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.voiceMu.Lock()
	s.voiceCancels[sessionID] = cancel
	s.voiceMu.Unlock()
	defer func() {
		cancel()
		s.voiceMu.Lock()
		delete(s.voiceCancels, sessionID)
		s.voiceMu.Unlock()
	}()
	s.hub.Publish(stream.Event{Type: "voice.state", TaskID: job.TaskID,
		ConversationID: session.ConversationID, CorrelationID: session.CorrelationID, Payload: session})
	s.hub.Publish(stream.Event{Type: "voice.tts.playback", TaskID: job.TaskID,
		ConversationID: session.ConversationID, CorrelationID: session.CorrelationID,
		Payload: map[string]any{"session_id": sessionID, "text": job.Text}})
	chunks, errorsChannel := s.tts.Synthesize(ctx, job.Text, "default")
	for chunks != nil || errorsChannel != nil {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			s.hub.Publish(stream.Event{Type: "voice.tts.chunk", TaskID: job.TaskID,
				ConversationID: session.ConversationID, CorrelationID: session.CorrelationID,
				Payload: map[string]any{"session_id": sessionID, "chunk": chunk, "simulated": true}})
		case ttsErr, ok := <-errorsChannel:
			if !ok {
				errorsChannel = nil
				continue
			}
			if ttsErr != nil && !errors.Is(ttsErr, context.Canceled) {
				s.logger.Warn("simulated TTS", "session_id", sessionID, "error", ttsErr)
			}
		}
	}
	current, err := s.store.GetVoiceSession(context.WithoutCancel(parent), sessionID)
	if err == nil && current.State == conversation.StateSpeaking {
		current, err = s.store.TransitionVoiceSession(context.WithoutCancel(parent), sessionID, conversation.StateListening, false)
		if err == nil {
			s.hub.Publish(stream.Event{Type: "voice.state", ConversationID: current.ConversationID,
				CorrelationID: current.CorrelationID, Payload: current})
		}
	}
}

func (s *Server) cancelTTS(sessionID string) {
	s.voiceMu.Lock()
	cancel := s.voiceCancels[sessionID]
	s.voiceMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

var _ = persistence.ErrNotFound
