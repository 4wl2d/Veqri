package connectors

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/veqri/veqri/core/events"
)

type Health struct {
	Status    string    `json:"status"`
	Message   string    `json:"message,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

type Message struct {
	ChannelID string          `json:"channel_id"`
	ThreadID  string          `json:"thread_id,omitempty"`
	Text      string          `json:"text"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

type Artifact struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	URI       string `json:"uri"`
}

type Connector interface {
	ID() string
	Kind() string
	Start(context.Context) error
	Stop(context.Context) error
	Health(context.Context) Health
	VerifyIncoming(*http.Request, []byte) error
	Normalize(context.Context, []byte) (events.Envelope, error)
	SendMessage(context.Context, Message) (string, error)
	UpdateMessage(context.Context, string, Message) error
	ReplyInThread(context.Context, Message) (string, error)
	SendProgress(context.Context, Message) (string, error)
	UploadArtifact(context.Context, string, Artifact) (string, error)
	ResolveIdentity(context.Context, string, string) (events.Actor, error)
}
