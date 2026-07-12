package e2e_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/core/voice"
	"github.com/veqri/veqri/tests/integration/testfixture"
)

func TestSimulatedVoiceDelegatesSpeaksInterruptsWithoutCancellingAndReconnects(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{
		WorkerCount: 3,
		TTS:         voice.MockTTS{ChunkDelay: 100 * time.Millisecond},
	})
	device := fixture.PairAndroid(t, "Voice Android")

	ctx, cancel := context.WithTimeout(context.Background(), testfixture.DefaultTimeout)
	defer cancel()
	hubEvents := fixture.Hub.Subscribe(ctx, 128)
	connection, response, err := websocket.Dial(ctx, fixture.WebSocketURL("/v1/device/events"), &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization":            []string{"Bearer " + device.Credential},
			"X-Veqri-Device-Id":        []string{device.ID},
			"X-Veqri-Protocol-Version": []string{"1"},
		},
		Subprotocols: []string{"veqri.v1"},
	})
	if err != nil {
		if response != nil {
			t.Fatalf("dial voice device stream: %v (HTTP %s)", err, response.Status)
		}
		t.Fatalf("dial voice device stream: %v", err)
	}
	defer connection.CloseNow()
	readInitialDeviceSnapshot(t, ctx, connection)

	startedResponse := fixture.JSON(t, http.MethodPost, "/v1/voice/calls", fixture.AdminToken, map[string]any{
		"device_id": device.ID,
	}, nil)
	testfixture.RequireStatus(t, startedResponse, http.StatusAccepted)
	started := testfixture.Decode[struct {
		VoiceSession conversation.VoiceSession `json:"voice_session"`
	}](t, startedResponse).VoiceSession
	if started.ID == "" || started.ConversationID == "" || started.State != conversation.StateRinging || started.DeviceID != device.ID {
		t.Fatalf("unexpected simulated incoming session: %+v", started)
	}
	secondCall := fixture.JSON(t, http.MethodPost, "/v1/voice/calls", fixture.AdminToken, map[string]any{
		"device_id": device.ID,
	}, nil)
	testfixture.RequireStatus(t, secondCall, http.StatusConflict)
	incoming := readVoicePhase(t, ctx, connection, started.ID, conversation.StateRinging)
	if incoming.Direction != "INCOMING" || !incoming.SimulatedMedia {
		t.Fatalf("Android incoming-call event did not disclose simulator mode: %+v", incoming)
	}

	writeDeviceCommand(t, ctx, connection, map[string]any{
		"command_id": "voice-answer-001", "protocol_version": 1,
		"type": "voice.answer", "session_id": started.ID,
	})
	listening := readVoicePhase(t, ctx, connection, started.ID, conversation.StateListening)
	if listening.ConversationID != started.ConversationID {
		t.Fatalf("answer created a different conversation: %+v", listening)
	}

	partial := fixture.JSON(t, http.MethodPost, "/v1/voice/sessions/"+started.ID+"/transcript", device.Credential, map[string]any{
		"text": "Ask the coding agent", "final": false, "sequence": 1,
	}, nil)
	testfixture.RequireStatus(t, partial, http.StatusAccepted)
	partialEvent := readDeviceEventType(t, ctx, connection, "transcript.partial")
	var partialPayload struct {
		ConversationID string `json:"conversation_id"`
		Text           string `json:"text"`
	}
	partialEvent.decodePayload(t, &partialPayload)
	if partialPayload.ConversationID != started.ConversationID || partialPayload.Text != "Ask the coding agent" {
		t.Fatalf("unexpected partial transcript event: %+v", partialPayload)
	}

	finalText := "Ask the coding agent to inspect the repository"
	final := fixture.JSON(t, http.MethodPost, "/v1/voice/sessions/"+started.ID+"/transcript", device.Credential, map[string]any{
		"text": finalText, "final": true, "sequence": 2, "idempotency_key": "voice-final-001",
	}, nil)
	testfixture.RequireStatus(t, final, http.StatusAccepted)
	delegated := testfixture.Decode[struct {
		VoiceSession conversation.VoiceSession `json:"voice_session"`
		Task         tasks.Task                `json:"task"`
		Duplicate    bool                      `json:"duplicate"`
	}](t, final)
	if delegated.Duplicate || delegated.Task.ID == "" || delegated.Task.ConversationID != started.ConversationID || delegated.Task.AssignedAgentID != "builtin.general" {
		t.Fatalf("final transcript did not delegate into the existing call: %+v", delegated)
	}
	if delegated.VoiceSession.State != conversation.StateWaitingForResult {
		t.Fatalf("voice session did not wait for delegated result: %+v", delegated.VoiceSession)
	}
	finalTranscript := readDeviceEventType(t, ctx, connection, "transcript.final")
	var finalPayload struct {
		ConversationID string `json:"conversation_id"`
		Text           string `json:"text"`
	}
	finalTranscript.decodePayload(t, &finalPayload)
	if finalPayload.ConversationID != started.ConversationID || finalPayload.Text != finalText {
		t.Fatalf("unexpected final transcript event: %+v", finalPayload)
	}

	completed := fixture.WaitTask(t, delegated.Task.ID, tasks.StatusCompleted)
	var speaking androidVoicePayload
	var spokenText string
	for speaking.SessionID == "" || spokenText == "" {
		event := readDeviceEvent(t, ctx, connection)
		switch event.Type {
		case "voice.changed":
			var payload androidVoicePayload
			event.decodePayload(t, &payload)
			if payload.SessionID == started.ID && payload.Phase == conversation.StateSpeaking {
				speaking = payload
			}
		case "tts.speak":
			var payload struct {
				SessionID      string `json:"session_id"`
				ConversationID string `json:"conversation_id"`
				Status         string `json:"status"`
				Text           string `json:"text"`
			}
			event.decodePayload(t, &payload)
			if payload.SessionID == started.ID && payload.ConversationID == started.ConversationID && payload.Status == "BUFFERING" {
				spokenText = payload.Text
			}
		}
	}
	if speaking.TTSStatus != "SPEAKING" || speaking.ConversationID != started.ConversationID {
		t.Fatalf("Android did not receive speaking state: %+v", speaking)
	}
	expectedSpoken := "General dialog agent completed the local simulated task: " + finalText + "."
	if spokenText != expectedSpoken {
		t.Fatalf("Android full TTS text = %q, want exact spoken summary %q", spokenText, expectedSpoken)
	}
	seenRootTTSChunk := false
	for !seenRootTTSChunk {
		select {
		case event := <-hubEvents:
			seenRootTTSChunk = event.Type == "voice.tts.chunk" && event.TaskID == completed.ID && event.ConversationID == started.ConversationID
		case <-ctx.Done():
			t.Fatalf("no simulated TTS chunk was emitted for root task %s", completed.ID)
		}
	}
	ttsEvent := readDeviceEventType(t, ctx, connection, "tts.changed")
	var ttsPayload struct {
		Status string `json:"status"`
	}
	ttsEvent.decodePayload(t, &ttsPayload)
	if ttsPayload.Status != "SPEAKING" {
		t.Fatalf("Android TTS event status = %q, want SPEAKING", ttsPayload.Status)
	}

	interruptedResponse := fixture.JSON(t, http.MethodPost, "/v1/voice/sessions/"+started.ID+"/interrupt", device.Credential, map[string]any{}, nil)
	testfixture.RequireStatus(t, interruptedResponse, http.StatusOK)
	interrupted := testfixture.Decode[struct {
		VoiceSession            conversation.VoiceSession `json:"voice_session"`
		DelegatedTasksCancelled bool                      `json:"delegated_tasks_cancelled"`
	}](t, interruptedResponse)
	if interrupted.DelegatedTasksCancelled || interrupted.VoiceSession.State != conversation.StateInterrupted || !interrupted.VoiceSession.Interrupted {
		t.Fatalf("barge-in did not interrupt only TTS: %+v", interrupted)
	}
	interruptedEvent := readVoicePhase(t, ctx, connection, started.ID, conversation.StateInterrupted)
	if interruptedEvent.TTSStatus != "INTERRUPTED" {
		t.Fatalf("Android interrupted event did not expose TTS state: %+v", interruptedEvent)
	}
	stillCompleted := fixture.WaitTask(t, completed.ID, tasks.StatusCompleted)
	if stillCompleted.Status != tasks.StatusCompleted || stillCompleted.Error != "" {
		t.Fatalf("TTS interruption cancelled or failed the delegated task: %+v", stillCompleted)
	}

	reconnectedResponse := fixture.JSON(t, http.MethodPost, "/v1/voice/sessions/"+started.ID+"/reconnect", device.Credential, map[string]any{}, nil)
	testfixture.RequireStatus(t, reconnectedResponse, http.StatusOK)
	reconnected := testfixture.Decode[struct {
		VoiceSession conversation.VoiceSession `json:"voice_session"`
	}](t, reconnectedResponse).VoiceSession
	if reconnected.ID != started.ID || reconnected.ConversationID != started.ConversationID || reconnected.State != conversation.StateListening {
		t.Fatalf("voice reconnect changed session identity or failed: %+v", reconnected)
	}
	readVoicePhase(t, ctx, connection, started.ID, conversation.StateListening)

	turns, err := fixture.Store.ListTurns(context.Background(), started.ConversationID, 20)
	if err != nil {
		t.Fatalf("load voice transcript turns: %v", err)
	}
	if len(turns) != 2 || turns[0].Role != conversation.RoleUser || turns[0].Text != finalText ||
		turns[1].Role != conversation.RoleAssistant || turns[1].Text != completed.UserFacingSummary {
		t.Fatalf("voice transcript/delegation was not durably preserved: %+v", turns)
	}
	assertDatabaseCount(t, fixture, "SELECT COUNT(*) FROM events WHERE source_kind = 'android_voice' AND idempotency_key = ?", "voice-final-001", 1)
}

type androidVoicePayload struct {
	SessionID      string                   `json:"session_id"`
	ConversationID string                   `json:"conversation_id"`
	Direction      string                   `json:"direction"`
	Phase          conversation.DialogState `json:"phase"`
	TTSStatus      string                   `json:"tts_status"`
	SimulatedMedia bool                     `json:"is_simulated_media"`
}

func readVoicePhase(t testing.TB, ctx context.Context, connection *websocket.Conn, sessionID string, phase conversation.DialogState) androidVoicePayload {
	t.Helper()
	for {
		event := readDeviceEvent(t, ctx, connection)
		if event.Type != "voice.incoming" && event.Type != "voice.changed" {
			continue
		}
		var payload androidVoicePayload
		event.decodePayload(t, &payload)
		if payload.SessionID == sessionID && payload.Phase == phase {
			return payload
		}
	}
}

func readDeviceEventType(t testing.TB, ctx context.Context, connection *websocket.Conn, eventType string) deviceWireEvent {
	t.Helper()
	for {
		event := readDeviceEvent(t, ctx, connection)
		if event.Type == eventType {
			return event
		}
	}
}
