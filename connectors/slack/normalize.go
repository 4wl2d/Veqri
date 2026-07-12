package slack

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/internal/ids"
)

type EventCallback struct {
	Type      string `json:"type"`
	EventID   string `json:"event_id"`
	TeamID    string `json:"team_id"`
	Challenge string `json:"challenge,omitempty"`
	Event     struct {
		Type      string `json:"type"`
		User      string `json:"user"`
		Text      string `json:"text"`
		Channel   string `json:"channel"`
		Timestamp string `json:"ts"`
		ThreadTS  string `json:"thread_ts"`
		BotID     string `json:"bot_id"`
		Subtype   string `json:"subtype"`
	} `json:"event"`
}

func Normalize(connectorID string, raw []byte, receivedAt time.Time) (events.Envelope, error) {
	var callback EventCallback
	if err := json.Unmarshal(raw, &callback); err != nil {
		return events.Envelope{}, err
	}
	if callback.Type != "event_callback" || callback.EventID == "" {
		return events.Envelope{}, errors.New("unsupported Slack envelope")
	}
	if callback.Event.BotID != "" || callback.Event.Subtype == "bot_message" {
		return events.Envelope{}, errors.New("Slack bot-originated event is ignored")
	}
	isMention := callback.Event.Type == "app_mention"
	isDirectMessage := callback.Event.Type == "message" && strings.HasPrefix(callback.Event.Channel, "D")
	if !isMention && !isDirectMessage {
		return events.Envelope{}, errors.New("Slack event did not explicitly mention Veqri or originate in a direct message")
	}
	rootThread := callback.Event.ThreadTS
	if rootThread == "" {
		rootThread = callback.Event.Timestamp
	}
	payload, _ := json.Marshal(map[string]any{
		"text": callback.Event.Text, "mention": isMention,
		"platform_message_id": callback.Event.Timestamp,
	})
	correlationID := ids.New()
	return events.Envelope{
		ID: ids.New(), Type: "message.received", Version: 1,
		Source: events.Source{Kind: "slack", ConnectorID: connectorID, InstanceID: callback.TeamID},
		Actor:  events.Actor{ID: callback.Event.User}, OccurredAt: receivedAt, ReceivedAt: receivedAt,
		ConversationKey: "slack:" + callback.TeamID + ":" + callback.Event.Channel + ":" + rootThread,
		CorrelationID:   correlationID, IdempotencyKey: callback.EventID,
		TrustLevel:  events.TrustUntrusted,
		ReplyTarget: events.ReplyTarget{ConnectorID: connectorID, ChannelID: callback.Event.Channel, ThreadID: rootThread},
		Payload:     payload,
	}, nil
}
