package mattermost

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"time"

	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/internal/ids"
)

type OutgoingWebhook struct {
	Token       string `json:"token"`
	TeamID      string `json:"team_id"`
	ChannelID   string `json:"channel_id"`
	UserID      string `json:"user_id"`
	UserName    string `json:"user_name"`
	PostID      string `json:"post_id"`
	RootID      string `json:"root_id"`
	Text        string `json:"text"`
	TriggerWord string `json:"trigger_word"`
}

// VerifyOutgoingToken implements Mattermost's documented compatibility-mode
// shared token check. Mattermost outgoing webhooks do not define HMAC signing.
func VerifyOutgoingToken(expected, supplied string) error {
	if expected == "" || supplied == "" || len(expected) != len(supplied) || subtle.ConstantTimeCompare([]byte(expected), []byte(supplied)) != 1 {
		return errors.New("Mattermost outgoing webhook token mismatch")
	}
	return nil
}

func NormalizeOutgoing(connectorID string, raw []byte, receivedAt time.Time) (events.Envelope, error) {
	var webhook OutgoingWebhook
	if err := json.Unmarshal(raw, &webhook); err != nil {
		return events.Envelope{}, err
	}
	if webhook.PostID == "" || webhook.ChannelID == "" || webhook.UserID == "" {
		return events.Envelope{}, errors.New("Mattermost post_id, channel_id, and user_id are required")
	}
	rootID := webhook.RootID
	if rootID == "" {
		rootID = webhook.PostID
	}
	payload, _ := json.Marshal(map[string]any{"text": webhook.Text, "trigger_word": webhook.TriggerWord, "platform_message_id": webhook.PostID})
	return events.Envelope{
		ID: ids.New(), Type: "message.received", Version: 1,
		Source:     events.Source{Kind: "mattermost", ConnectorID: connectorID, InstanceID: webhook.TeamID},
		Actor:      events.Actor{ID: webhook.UserID, DisplayName: webhook.UserName},
		OccurredAt: receivedAt, ReceivedAt: receivedAt,
		ConversationKey: "mattermost:" + webhook.TeamID + ":" + webhook.ChannelID + ":" + rootID,
		CorrelationID:   ids.New(), IdempotencyKey: webhook.PostID, TrustLevel: events.TrustUntrusted,
		ReplyTarget: events.ReplyTarget{ConnectorID: connectorID, ChannelID: webhook.ChannelID, ThreadID: rootID},
		Payload:     payload,
	}, nil
}
