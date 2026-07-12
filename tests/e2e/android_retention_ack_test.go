package e2e_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/coder/websocket"

	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/tests/integration/testfixture"
)

func TestAndroidPairingCommitsRetentionBeforePCIncomingCall(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{NoWorkers: true})
	device := fixture.PairAndroidWithRetention(t, "Private Android", false)

	ctx, cancel := context.WithTimeout(context.Background(), testfixture.DefaultTimeout)
	defer cancel()
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
			t.Fatalf("dial paired device stream: %v (HTTP %s)", err, response.Status)
		}
		t.Fatalf("dial paired device stream: %v", err)
	}
	defer connection.CloseNow()
	snapshot := readInitialDeviceSnapshot(t, ctx, connection)
	var snapshotPayload struct {
		TranscriptRetention bool `json:"transcript_retention"`
	}
	snapshot.decodePayload(t, &snapshotPayload)
	if snapshotPayload.TranscriptRetention {
		t.Fatal("pairing response committed disabled retention but reconnect snapshot reported enabled")
	}

	startedResponse := fixture.JSON(t, http.MethodPost, "/v1/voice/calls", fixture.AdminToken,
		map[string]any{"device_id": device.ID}, nil)
	testfixture.RequireStatus(t, startedResponse, http.StatusAccepted)
	started := testfixture.Decode[struct {
		VoiceSession conversation.VoiceSession `json:"voice_session"`
	}](t, startedResponse).VoiceSession
	conversationRecord, err := fixture.Store.GetConversation(context.Background(), started.ConversationID)
	if err != nil {
		t.Fatalf("load incoming-call conversation: %v", err)
	}
	if conversationRecord.TranscriptRetention {
		t.Fatal("PC incoming call retained a transcript after pairing acknowledged retention=false")
	}
}

func TestStaleAndroidTrueOverrideCannotReenableDisabledConversation(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{NoWorkers: true})
	device := fixture.PairAndroid(t, "Android stale policy")
	ctx, cancel := context.WithTimeout(context.Background(), testfixture.DefaultTimeout)
	defer cancel()
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
			t.Fatalf("dial paired device stream: %v (HTTP %s)", err, response.Status)
		}
		t.Fatal(err)
	}
	defer connection.CloseNow()
	readInitialDeviceSnapshot(t, ctx, connection)

	writeDeviceCommand(t, ctx, connection, map[string]any{
		"command_id": "initial-retained", "protocol_version": 1, "type": "conversation.send_text",
		"text": "initial retained text", "retain_transcript": true,
	})
	readCommittedCommand(t, ctx, connection, "initial-retained")
	conversationRecord, err := fixture.Store.GetConversationByExternalKey(ctx, "android:"+device.ID+":default")
	if err != nil {
		t.Fatal(err)
	}
	disabled := fixture.JSON(t, http.MethodPut,
		"/v1/conversations/"+conversationRecord.ID+"/transcript-retention", fixture.AdminToken,
		map[string]any{"enabled": false}, nil)
	testfixture.RequireStatus(t, disabled, http.StatusOK)

	writeDeviceCommand(t, ctx, connection, map[string]any{
		"command_id": "stale-retained", "protocol_version": 1, "type": "conversation.send_text",
		"conversation_id": conversationRecord.ID, "text": "must remain non-retained",
		"retain_transcript": true,
	})
	readCommittedCommand(t, ctx, connection, "stale-retained")
	conversationRecord, err = fixture.Store.GetConversation(ctx, conversationRecord.ID)
	if err != nil {
		t.Fatal(err)
	}
	if conversationRecord.TranscriptRetention {
		t.Fatal("stale Android retain_transcript=true re-enabled an explicitly disabled conversation")
	}
	turns, err := fixture.Store.ListTurns(ctx, conversationRecord.ID, 10)
	if err != nil || len(turns) != 1 || turns[0].Text != "[transcript retention disabled]" {
		t.Fatalf("non-retained follow-up turns = %+v, %v", turns, err)
	}
}

func readCommittedCommand(t testing.TB, ctx context.Context, connection *websocket.Conn, commandID string) {
	t.Helper()
	for {
		event := readDeviceEvent(t, ctx, connection)
		if event.Type != "command.result" {
			continue
		}
		var result struct {
			CommandID string `json:"command_id"`
			Status    string `json:"status"`
		}
		event.decodePayload(t, &result)
		if result.CommandID == commandID {
			if result.Status != "COMMITTED" || event.CorrelationID != commandID {
				t.Fatalf("command %s result = %+v", commandID, event)
			}
			return
		}
	}
}
