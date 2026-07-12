package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var fixedEventTime = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

func validEnvelope() Envelope {
	return Envelope{
		ID:              "event-1",
		Type:            "message.received",
		Version:         1,
		Source:          Source{Kind: "test", ConnectorID: "connector-1", InstanceID: "instance-1"},
		Actor:           Actor{ID: "actor-1"},
		OccurredAt:      fixedEventTime,
		ReceivedAt:      fixedEventTime.Add(time.Second),
		ConversationKey: "test:conversation-1",
		CorrelationID:   "correlation-1",
		IdempotencyKey:  "idempotency-1",
		TrustLevel:      TrustTrusted,
		Payload:         json.RawMessage(`{"text":"hello"}`),
	}
}

func TestEnvelopeValidate(t *testing.T) {
	if err := validEnvelope().Validate(); err != nil {
		t.Fatalf("valid envelope rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*Envelope)
		want string
	}{
		{name: "missing id", edit: func(e *Envelope) { e.ID = "" }, want: "required"},
		{name: "missing type", edit: func(e *Envelope) { e.Type = "" }, want: "required"},
		{name: "nonpositive version", edit: func(e *Envelope) { e.Version = 0 }, want: "required"},
		{name: "missing correlation", edit: func(e *Envelope) { e.CorrelationID = "" }, want: "required"},
		{name: "missing idempotency", edit: func(e *Envelope) { e.IdempotencyKey = "" }, want: "required"},
		{name: "missing occurred time", edit: func(e *Envelope) { e.OccurredAt = time.Time{} }, want: "occurred_at"},
		{name: "missing received time", edit: func(e *Envelope) { e.ReceivedAt = time.Time{} }, want: "received_at"},
		{name: "unknown trust", edit: func(e *Envelope) { e.TrustLevel = TrustLevel("root") }, want: "trust_level"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := validEnvelope()
			tt.edit(&event)
			err := event.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want text %q", err, tt.want)
			}
		})
	}
}

func TestEnvelopeValidateAcceptsEveryDocumentedTrustLevel(t *testing.T) {
	for _, level := range []TrustLevel{TrustUntrusted, TrustKnown, TrustTrusted, TrustLocal} {
		t.Run(string(level), func(t *testing.T) {
			event := validEnvelope()
			event.TrustLevel = level
			if err := event.Validate(); err != nil {
				t.Fatalf("Validate() rejected %q: %v", level, err)
			}
		})
	}
}
