package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/coder/websocket"

	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/tests/integration/testfixture"
)

func TestPairedAndroidDeviceSendsAuthenticatedTextOverWebSocket(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{WorkerCount: 3})
	device := fixture.PairAndroid(t, "E2E Android")

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
	readInitialDeviceSnapshot(t, ctx, connection)

	command := map[string]any{
		"command_id": "android-text-command-001", "protocol_version": 1,
		"type": "conversation.send_text", "text": "Ask the coding agent to inspect the repository",
	}
	writeDeviceCommand(t, ctx, connection, command)

	var taskID, conversationID, correlationID, assistantText string
	seenUser := false
	seenProgress := false
	seenCompleted := false
	seenCommandCommit := false
	for !seenUser || !seenProgress || !seenCompleted || assistantText == "" || !seenCommandCommit {
		event := readDeviceEvent(t, ctx, connection)
		if event.Type != "command.result" && event.CorrelationID != "" {
			if correlationID == "" {
				correlationID = event.CorrelationID
			} else if event.Type != "conversation.message_added" || event.PayloadAuthor(t) != "SYSTEM" {
				if event.CorrelationID != correlationID {
					t.Fatalf("device event lost request correlation: got %s, want %s (%s)", event.CorrelationID, correlationID, event.Type)
				}
			}
		}
		switch event.Type {
		case "command.result":
			var result struct {
				CommandID string `json:"command_id"`
				Status    string `json:"status"`
			}
			event.decodePayload(t, &result)
			if result.CommandID == command["command_id"] && result.Status == "COMMITTED" &&
				event.CorrelationID == result.CommandID {
				seenCommandCommit = true
			}
		case "conversation.message_added":
			var message struct {
				ConversationID string `json:"conversation_id"`
				Author         string `json:"author"`
				Text           string `json:"text"`
			}
			event.decodePayload(t, &message)
			conversationID = firstNonEmpty(conversationID, message.ConversationID)
			if message.Author == "USER" && message.Text == command["text"] {
				seenUser = true
			}
			if message.Author == "ASSISTANT" {
				assistantText = message.Text
			}
		case "task.changed":
			var changed struct {
				TaskID          string       `json:"task_id"`
				ConversationID  string       `json:"conversation_id"`
				AssignedAgent   string       `json:"assigned_agent"`
				Status          tasks.Status `json:"status"`
				ProgressPercent int          `json:"progress_percent"`
				Summary         string       `json:"summary"`
			}
			event.decodePayload(t, &changed)
			taskID = firstNonEmpty(taskID, changed.TaskID)
			conversationID = firstNonEmpty(conversationID, changed.ConversationID)
			if changed.AssignedAgent != "builtin.general" {
				t.Fatalf("Android text task routed to unexpected agent: %+v", changed)
			}
			if changed.ProgressPercent > 0 && changed.ProgressPercent < 100 {
				seenProgress = true
			}
			if changed.Status == tasks.StatusCompleted {
				seenCompleted = true
				if changed.Summary == "" {
					t.Fatal("completed Android task event omitted its summary")
				}
			}
		}
	}
	if taskID == "" || conversationID == "" || correlationID == "" {
		t.Fatalf("device stream omitted stable IDs: task=%q conversation=%q correlation=%q", taskID, conversationID, correlationID)
	}
	completed := fixture.WaitTask(t, taskID, tasks.StatusCompleted)
	if completed.ConversationID != conversationID || completed.CorrelationID != correlationID || completed.UserFacingSummary != assistantText {
		t.Fatalf("device events diverged from durable task: task=%+v assistant=%q", completed, assistantText)
	}
	conversationRecord, err := fixture.Store.GetConversationByExternalKey(context.Background(), "android:"+device.ID+":default")
	if err != nil {
		t.Fatalf("load Android conversation: %v", err)
	}
	turns, err := fixture.Store.ListTurns(context.Background(), conversationRecord.ID, 20)
	if err != nil {
		t.Fatalf("load Android turns: %v", err)
	}
	if len(turns) != 2 || turns[0].Role != conversation.RoleUser || turns[1].Role != conversation.RoleAssistant || turns[1].Text != assistantText {
		t.Fatalf("unexpected durable Android transcript: %+v", turns)
	}

	writeDeviceCommand(t, ctx, connection, command)
	writeDeviceCommand(t, ctx, connection, map[string]any{
		"command_id": "android-barrier-001", "protocol_version": 1, "type": "unsupported.integration.barrier",
	})
	for {
		event := readDeviceEvent(t, ctx, connection)
		if event.Type != "conversation.message_added" {
			continue
		}
		var message struct {
			Author string `json:"author"`
			Text   string `json:"text"`
		}
		event.decodePayload(t, &message)
		if message.Author == "SYSTEM" {
			break
		}
	}
	assertDatabaseCount(t, fixture, "SELECT COUNT(*) FROM events WHERE source_kind = 'android' AND idempotency_key = ?", "android-text-command-001", 1)
	assertDatabaseCount(t, fixture, "SELECT COUNT(*) FROM tasks WHERE root_task_id = ?", completed.RootTaskID, 1)

	var eventID string
	if err := fixture.Store.DB().QueryRowContext(context.Background(),
		"SELECT id FROM events WHERE source_kind = 'android' AND idempotency_key = ?", "android-text-command-001").Scan(&eventID); err != nil {
		t.Fatalf("load normalized Android event ID: %v", err)
	}
	envelope, err := fixture.Store.GetEvent(context.Background(), eventID)
	if err != nil {
		t.Fatalf("load normalized Android event: %v", err)
	}
	if envelope.Actor.ID != device.ID || envelope.Source.ConnectorID != "android-device" || envelope.CorrelationID != correlationID {
		t.Fatalf("normalized Android event lost identity/correlation: %+v", envelope)
	}
}

type deviceWireEvent struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

func (event deviceWireEvent) decodePayload(t testing.TB, target any) {
	t.Helper()
	if err := json.Unmarshal(event.Payload, target); err != nil {
		t.Fatalf("decode %s device payload %s: %v", event.Type, event.Payload, err)
	}
}

func (event deviceWireEvent) PayloadAuthor(t testing.TB) string {
	t.Helper()
	var message struct {
		Author string `json:"author"`
	}
	event.decodePayload(t, &message)
	return message.Author
}

func writeDeviceCommand(t testing.TB, ctx context.Context, connection *websocket.Conn, command any) {
	t.Helper()
	raw, err := json.Marshal(command)
	if err != nil {
		t.Fatalf("marshal device command: %v", err)
	}
	if err := connection.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("write device command: %v", err)
	}
}

func readDeviceEvent(t testing.TB, ctx context.Context, connection *websocket.Conn) deviceWireEvent {
	t.Helper()
	_, raw, err := connection.Read(ctx)
	if err != nil {
		t.Fatalf("read device event: %v", err)
	}
	var event deviceWireEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("decode device event %s: %v", raw, err)
	}
	return event
}

func readInitialDeviceSnapshot(t testing.TB, ctx context.Context, connection *websocket.Conn) deviceWireEvent {
	t.Helper()
	event := readDeviceEvent(t, ctx, connection)
	if event.Type != "sync.snapshot" || event.ID == "" || event.CorrelationID == "" {
		t.Fatalf("first device event = %+v, want identified sync.snapshot", event)
	}
	var payload struct {
		SnapshotID          string            `json:"snapshot_id"`
		TranscriptRetention *bool             `json:"transcript_retention"`
		Messages            []json.RawMessage `json:"messages"`
		Tasks               []json.RawMessage `json:"tasks"`
		Approvals           []json.RawMessage `json:"approvals"`
	}
	event.decodePayload(t, &payload)
	if payload.SnapshotID == "" || event.ID != "snapshot:"+payload.SnapshotID ||
		event.CorrelationID != payload.SnapshotID || payload.Messages == nil ||
		payload.Tasks == nil || payload.Approvals == nil || payload.TranscriptRetention == nil {
		t.Fatalf("invalid authoritative device snapshot: event=%+v payload=%+v", event, payload)
	}
	return event
}

func assertDatabaseCount(t testing.TB, fixture *testfixture.Fixture, query string, argument any, expected int) {
	t.Helper()
	var count int
	if err := fixture.Store.DB().QueryRowContext(context.Background(), query, argument).Scan(&count); err != nil {
		t.Fatalf("run count query %q: %v", query, err)
	}
	if count != expected {
		t.Fatalf("count query %q returned %d, want %d", query, count, expected)
	}
}

func firstNonEmpty(current, candidate string) string {
	if current != "" {
		return current
	}
	return candidate
}
