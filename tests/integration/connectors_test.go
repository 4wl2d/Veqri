package integration_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/veqri/veqri/connectors/slack"
	"github.com/veqri/veqri/core/delivery"
	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/internal/stream"
	"github.com/veqri/veqri/tests/integration/testfixture"
)

func TestSlackSignatureNormalizationDeduplicationAndSimulatorThreadDelivery(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{WorkerCount: 4, AgentDelay: 2 * time.Millisecond})

	rawSlack := json.RawMessage(`{"type":"event_callback","event_id":"Ev-integration-001","team_id":"T-integration","event":{"type":"app_mention","user":"U-test","text":"summarize the deterministic run","channel":"C-thread","ts":"1720000000.001","thread_ts":"1720000000.000"}}`)
	fixedReceivedAt := time.Date(2026, time.July, 12, 8, 0, 0, 0, time.UTC)
	normalized, err := slack.Normalize("slack-default", rawSlack, fixedReceivedAt)
	if err != nil {
		t.Fatalf("normalize Slack fixture: %v", err)
	}
	if normalized.Type != "message.received" || normalized.IdempotencyKey != "Ev-integration-001" || normalized.TrustLevel != events.TrustUntrusted {
		t.Fatalf("unexpected normalized envelope: %+v", normalized)
	}
	if normalized.ConversationKey != "slack:T-integration:C-thread:1720000000.000" {
		t.Fatalf("normalized conversation key = %q", normalized.ConversationKey)
	}
	if normalized.ReplyTarget.ConnectorID != "slack-default" || normalized.ReplyTarget.ChannelID != "C-thread" || normalized.ReplyTarget.ThreadID != "1720000000.000" {
		t.Fatalf("normalized reply target lost thread identity: %+v", normalized.ReplyTarget)
	}

	timestamp := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	signature := slackSignature(testfixture.SlackSecret, timestamp, rawSlack)
	headers := map[string]string{
		"X-Slack-Request-Timestamp": timestamp,
		"X-Slack-Signature":         signature,
	}
	first := fixture.JSON(t, http.MethodPost, "/v1/connectors/slack/events", "", rawSlack, headers)
	testfixture.RequireStatus(t, first, http.StatusAccepted)
	firstResult := testfixture.Decode[struct {
		TaskID    string `json:"task_id"`
		Duplicate bool   `json:"duplicate"`
	}](t, first)
	if firstResult.TaskID == "" || firstResult.Duplicate {
		t.Fatalf("unexpected first Slack result: %s", first.Body)
	}
	fixture.WaitTask(t, firstResult.TaskID, tasks.StatusCompleted)

	second := fixture.JSON(t, http.MethodPost, "/v1/connectors/slack/events", "", rawSlack, headers)
	testfixture.RequireStatus(t, second, http.StatusAccepted)
	secondResult := testfixture.Decode[struct {
		TaskID    string `json:"task_id"`
		Duplicate bool   `json:"duplicate"`
	}](t, second)
	if !secondResult.Duplicate || secondResult.TaskID != firstResult.TaskID {
		t.Fatalf("Slack retry was not deduplicated to %s: %s", firstResult.TaskID, second.Body)
	}
	assertCounts(t, fixture, map[string]int{
		"SELECT COUNT(*) FROM events WHERE source_kind = 'slack' AND idempotency_key = 'Ev-integration-001'": 1,
		"SELECT COUNT(*) FROM tasks WHERE id = '" + firstResult.TaskID + "'":                                 1,
		"SELECT COUNT(*) FROM deliveries WHERE task_id = '" + firstResult.TaskID + "'":                       1,
	})
	assertDelivery(t, fixture, firstResult.TaskID, delivery.StatusPending, delivery.Target{
		Kind: "slack", ConnectorID: "slack-default", ChannelID: "C-thread", ThreadID: "1720000000.000",
	})

	streamContext, cancelStream := context.WithTimeout(context.Background(), testfixture.DefaultTimeout)
	defer cancelStream()
	eventStream := fixture.Hub.Subscribe(streamContext, 64)
	simulatorBody := map[string]any{
		"text": "run connector simulator", "actor_id": "U-simulated",
		"workspace_id": "T-simulated", "channel_id": "C-simulated",
		"thread_id": "thread-root-42", "message_id": "simulated-message-42",
	}
	simulated := fixture.JSON(t, http.MethodPost, "/v1/connectors/simulate/slack", fixture.AdminToken, simulatorBody, nil)
	testfixture.RequireStatus(t, simulated, http.StatusAccepted)
	simulatedResult := testfixture.Decode[struct {
		Task      tasks.Task `json:"task"`
		Duplicate bool       `json:"duplicate"`
	}](t, simulated)
	if simulatedResult.Duplicate || simulatedResult.Task.ID == "" {
		t.Fatalf("unexpected simulator response: %s", simulated.Body)
	}
	fixture.WaitTask(t, simulatedResult.Task.ID, tasks.StatusCompleted)

	var replyEvent stream.Event
	for replyEvent.Type != "connector.reply" || replyEvent.TaskID != simulatedResult.Task.ID {
		select {
		case replyEvent = <-eventStream:
		case <-streamContext.Done():
			t.Fatalf("timed out waiting for connector.reply for %s", simulatedResult.Task.ID)
		}
	}
	var replyPayload struct {
		Delivery  delivery.Delivery `json:"delivery"`
		Text      string            `json:"text"`
		Simulated bool              `json:"simulated"`
	}
	remarshal(t, replyEvent.Payload, &replyPayload)
	if !replyPayload.Simulated || replyPayload.Text == "" || replyPayload.Delivery.Status != delivery.StatusDelivered {
		t.Fatalf("simulator did not emit a delivered final reply: %+v", replyPayload)
	}
	if replyPayload.Delivery.Target.ChannelID != "C-simulated" || replyPayload.Delivery.Target.ThreadID != "thread-root-42" {
		t.Fatalf("simulator reply changed its originating thread: %+v", replyPayload.Delivery.Target)
	}
	assertDelivery(t, fixture, simulatedResult.Task.ID, delivery.StatusDelivered, delivery.Target{
		Kind: "slack", ConnectorID: "slack-simulator", ChannelID: "C-simulated", ThreadID: "thread-root-42",
	})

	duplicateSimulation := fixture.JSON(t, http.MethodPost, "/v1/connectors/simulate/slack", fixture.AdminToken, simulatorBody, nil)
	testfixture.RequireStatus(t, duplicateSimulation, http.StatusAccepted)
	duplicateResult := testfixture.Decode[struct {
		Task      tasks.Task `json:"task"`
		Duplicate bool       `json:"duplicate"`
	}](t, duplicateSimulation)
	if !duplicateResult.Duplicate || duplicateResult.Task.ID != simulatedResult.Task.ID {
		t.Fatalf("simulator retry was not deduplicated: %s", duplicateSimulation.Body)
	}
	assertCounts(t, fixture, map[string]int{
		"SELECT COUNT(*) FROM tasks WHERE id = '" + simulatedResult.Task.ID + "'":           1,
		"SELECT COUNT(*) FROM deliveries WHERE task_id = '" + simulatedResult.Task.ID + "'": 1,
	})
}

func slackSignature(secret, timestamp string, raw []byte) string {
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(digest, "v0:%s:", timestamp)
	_, _ = digest.Write(raw)
	return "v0=" + hex.EncodeToString(digest.Sum(nil))
}

func assertCounts(t testing.TB, fixture *testfixture.Fixture, queries map[string]int) {
	t.Helper()
	for query, expected := range queries {
		var actual int
		if err := fixture.Store.DB().QueryRowContext(context.Background(), query).Scan(&actual); err != nil {
			t.Fatalf("run count query %q: %v", query, err)
		}
		if actual != expected {
			t.Fatalf("query %q returned %d, want %d", query, actual, expected)
		}
	}
}

func assertDelivery(t testing.TB, fixture *testfixture.Fixture, taskID string, expectedStatus delivery.Status, expectedTarget delivery.Target) {
	t.Helper()
	var status string
	var targetJSON string
	testfixture.Eventually(t, testfixture.DefaultTimeout, "delivery for task "+taskID, func() (bool, error) {
		err := fixture.Store.DB().QueryRowContext(context.Background(),
			"SELECT status, target_json FROM deliveries WHERE task_id = ?", taskID).Scan(&status, &targetJSON)
		return err == nil, err
	})
	var target delivery.Target
	if err := json.Unmarshal([]byte(targetJSON), &target); err != nil {
		t.Fatalf("decode delivery target: %v", err)
	}
	if delivery.Status(status) != expectedStatus {
		t.Fatalf("delivery status = %s, want %s", status, expectedStatus)
	}
	if target.Kind != expectedTarget.Kind || target.ConnectorID != expectedTarget.ConnectorID ||
		target.ChannelID != expectedTarget.ChannelID || target.ThreadID != expectedTarget.ThreadID {
		t.Fatalf("delivery target = %+v, want %+v", target, expectedTarget)
	}
}

func remarshal(t testing.TB, input, output any) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal fixture value: %v", err)
	}
	if err := json.Unmarshal(raw, output); err != nil {
		t.Fatalf("unmarshal fixture value: %v", err)
	}
}
