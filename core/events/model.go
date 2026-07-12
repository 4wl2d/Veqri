package events

import (
	"encoding/json"
	"errors"
	"time"
)

type TrustLevel string

const (
	TrustUntrusted TrustLevel = "untrusted"
	TrustKnown     TrustLevel = "known"
	TrustTrusted   TrustLevel = "trusted"
	TrustLocal     TrustLevel = "local"
)

type Source struct {
	Kind        string `json:"kind"`
	ConnectorID string `json:"connector_id,omitempty"`
	InstanceID  string `json:"instance_id,omitempty"`
}

type Actor struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
}

type ReplyTarget struct {
	ConnectorID string `json:"connector_id,omitempty"`
	ChannelID   string `json:"channel_id,omitempty"`
	ThreadID    string `json:"thread_id,omitempty"`
}

// Envelope is the only event shape accepted by orchestration. Connector data
// is normalized into Payload before it crosses this boundary.
type Envelope struct {
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	Version         int             `json:"version"`
	Source          Source          `json:"source"`
	Actor           Actor           `json:"actor"`
	OccurredAt      time.Time       `json:"occurred_at"`
	ReceivedAt      time.Time       `json:"received_at"`
	ConversationKey string          `json:"conversation_key"`
	CorrelationID   string          `json:"correlation_id"`
	CausationID     *string         `json:"causation_id,omitempty"`
	IdempotencyKey  string          `json:"idempotency_key"`
	TrustLevel      TrustLevel      `json:"trust_level"`
	ReplyTarget     ReplyTarget     `json:"reply_target"`
	Payload         json.RawMessage `json:"payload"`
}

func (e Envelope) Validate() error {
	if e.ID == "" || e.Type == "" || e.Version < 1 || e.CorrelationID == "" || e.IdempotencyKey == "" {
		return errors.New("event id, type, version, correlation_id, and idempotency_key are required")
	}
	if e.OccurredAt.IsZero() || e.ReceivedAt.IsZero() {
		return errors.New("occurred_at and received_at are required")
	}
	switch e.TrustLevel {
	case TrustUntrusted, TrustKnown, TrustTrusted, TrustLocal:
		return nil
	default:
		return errors.New("invalid trust_level")
	}
}
