package teams

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/internal/ids"
)

// TokenVerifier must implement the complete Microsoft Bot Connector JWT
// specification. Production ingress fails closed when no verifier is present.
type TokenVerifier interface {
	Verify(ctx context.Context, authorizationHeader, expectedServiceURL string) error
}

type Activity struct {
	Type       string    `json:"type"`
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	ServiceURL string    `json:"serviceUrl"`
	Text       string    `json:"text"`
	From       struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"from"`
	Conversation struct {
		ID       string `json:"id"`
		TenantID string `json:"tenantId"`
	} `json:"conversation"`
	ChannelID string `json:"channelId"`
	ReplyToID string `json:"replyToId"`
}

func VerifyAndNormalize(ctx context.Context, verifier TokenVerifier, authHeader, connectorID string, raw []byte, receivedAt time.Time) (events.Envelope, error) {
	if verifier == nil {
		return events.Envelope{}, errors.New("Teams JWT verifier is not configured")
	}
	var activity Activity
	if err := json.Unmarshal(raw, &activity); err != nil {
		return events.Envelope{}, err
	}
	if !strings.EqualFold(activity.Type, "message") || activity.ID == "" || activity.Conversation.ID == "" {
		return events.Envelope{}, errors.New("unsupported Teams activity")
	}
	if err := verifier.Verify(ctx, authHeader, activity.ServiceURL); err != nil {
		return events.Envelope{}, err
	}
	tenantID := activity.Conversation.TenantID
	if tenantID == "" {
		tenantID = "unknown-tenant"
	}
	idempotency := tenantID + ":" + activity.Conversation.ID + ":" + activity.ID
	payload, _ := json.Marshal(map[string]any{"text": activity.Text, "platform_message_id": activity.ID, "service_url": activity.ServiceURL})
	return events.Envelope{
		ID: ids.New(), Type: "message.received", Version: 1,
		Source:     events.Source{Kind: "teams", ConnectorID: connectorID, InstanceID: tenantID},
		Actor:      events.Actor{ID: activity.From.ID, DisplayName: activity.From.Name},
		OccurredAt: activity.Timestamp, ReceivedAt: receivedAt,
		ConversationKey: "teams:" + tenantID + ":" + activity.Conversation.ID,
		CorrelationID:   ids.New(), IdempotencyKey: idempotency, TrustLevel: events.TrustUntrusted,
		ReplyTarget: events.ReplyTarget{ConnectorID: connectorID, ChannelID: activity.Conversation.ID, ThreadID: activity.ID},
		Payload:     payload,
	}, nil
}
