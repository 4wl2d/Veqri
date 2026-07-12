// Package notifications implements the typed delivery edge for notifications,
// calls, spoken results, and connector replies. Every accepted request is
// published to the existing stream hub; optional handlers can perform the
// actual platform delivery and produce delivered/failed events.
package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	coretools "github.com/veqri/veqri/core/tools"
	"github.com/veqri/veqri/internal/ids"
	"github.com/veqri/veqri/internal/stream"
)

const (
	maxTextBytes     = 64 << 10
	maxMetadataBytes = 256 << 10
)

type Kind string

const (
	KindAndroidNotification Kind = "android_notification"
	KindDesktopNotification Kind = "desktop_notification"
	KindAndroidCall         Kind = "android_call"
	KindSpokenResult        Kind = "spoken_result"
	KindConnectorReply      Kind = "connector_reply"
)

type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
	PriorityUrgent Priority = "urgent"
)

// Request is both the tool input and the event payload. Fields not applicable
// to Kind are rejected rather than silently ignored.
type Request struct {
	Kind           Kind            `json:"kind"`
	Title          string          `json:"title,omitempty"`
	Text           string          `json:"text,omitempty"`
	DeviceID       string          `json:"device_id,omitempty"`
	ConnectorID    string          `json:"connector_id,omitempty"`
	ChannelID      string          `json:"channel_id,omitempty"`
	ThreadID       string          `json:"thread_id,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	TaskID         string          `json:"task_id,omitempty"`
	CorrelationID  string          `json:"correlation_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Priority       Priority        `json:"priority,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
}

type Receipt struct {
	DeliveryID      string          `json:"delivery_id"`
	Kind            Kind            `json:"kind"`
	Status          string          `json:"status"`
	QueuedAt        time.Time       `json:"queued_at"`
	DeliveredAt     *time.Time      `json:"delivered_at,omitempty"`
	Provider        string          `json:"provider,omitempty"`
	ProviderReceipt json.RawMessage `json:"provider_receipt,omitempty"`
}

// Handler is implemented by Android, desktop, voice, or connector delivery
// adapters. Provider receipts must be JSON when present and must not contain
// credentials.
type Handler interface {
	Deliver(context.Context, Request) (HandlerReceipt, error)
}

type HandlerFunc func(context.Context, Request) (HandlerReceipt, error)

func (f HandlerFunc) Deliver(ctx context.Context, request Request) (HandlerReceipt, error) {
	if f == nil {
		return HandlerReceipt{}, errors.New("notification handler is nil")
	}
	return f(ctx, request)
}

type HandlerReceipt struct {
	Provider string          `json:"provider,omitempty"`
	Receipt  json.RawMessage `json:"receipt,omitempty"`
}

type Router struct {
	hub      *stream.Hub
	mu       sync.RWMutex
	handlers map[Kind]Handler
	now      func() time.Time
	newID    func() string
}

var _ coretools.Executor = (*Router)(nil)

type Option func(*Router) error

func WithHandler(kind Kind, handler Handler) Option {
	return func(router *Router) error {
		if !validKind(kind) || handler == nil {
			return errors.New("notification handler requires a supported kind and non-nil handler")
		}
		router.handlers[kind] = handler
		return nil
	}
}

func WithClock(now func() time.Time) Option {
	return func(router *Router) error {
		if now == nil {
			return errors.New("notification clock cannot be nil")
		}
		router.now = now
		return nil
	}
}

func WithIDGenerator(generator func() string) Option {
	return func(router *Router) error {
		if generator == nil {
			return errors.New("notification ID generator cannot be nil")
		}
		router.newID = generator
		return nil
	}
}

func New(hub *stream.Hub, options ...Option) (*Router, error) {
	if hub == nil {
		return nil, errors.New("notification router requires a stream hub")
	}
	router := &Router{
		hub: hub, handlers: make(map[Kind]Handler), now: time.Now, newID: ids.New,
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("notification router option cannot be nil")
		}
		if err := option(router); err != nil {
			return nil, err
		}
	}
	return router, nil
}

// RegisterHandler updates one delivery backend safely at runtime. Registering
// nil explicitly disables that backend while preserving hub-only queuing.
func (r *Router) RegisterHandler(kind Kind, handler Handler) error {
	if !validKind(kind) {
		return fmt.Errorf("unsupported notification kind %q", kind)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if handler == nil {
		delete(r.handlers, kind)
	} else {
		r.handlers[kind] = handler
	}
	return nil
}

func (r *Router) Definition() coretools.Definition {
	return coretools.Definition{
		Name: "notifications", Description: "Queues typed Android, desktop, voice, and connector deliveries",
		InputSchema:    json.RawMessage(`{"type":"object","additionalProperties":false,"required":["kind","correlation_id","idempotency_key"],"properties":{"kind":{"enum":["android_notification","desktop_notification","android_call","spoken_result","connector_reply"]},"title":{"type":"string","maxLength":65536},"text":{"type":"string","maxLength":65536},"device_id":{"type":"string"},"connector_id":{"type":"string"},"channel_id":{"type":"string"},"thread_id":{"type":"string"},"conversation_id":{"type":"string"},"task_id":{"type":"string"},"correlation_id":{"type":"string"},"idempotency_key":{"type":"string"},"priority":{"enum":["low","normal","high","urgent"]},"metadata":{}}}`),
		OutputSchema:   json.RawMessage(`{"type":"object","required":["delivery_id","kind","status","queued_at"]}`),
		RequiredScopes: []string{"tool.notifications.deliver"}, Risk: coretools.RiskExternalCommunication,
		ApprovalRequired: true, DefaultTimeout: 30 * time.Second,
		SupportsCancellation: true, SupportsStreaming: false,
		SupportedOS: []string{"darwin", "linux", "windows"},
	}
}

func (r *Router) ParseAndValidate(raw json.RawMessage) (Request, error) {
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return Request{}, fmt.Errorf("decode notification input: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Request{}, errors.New("notification input must contain exactly one JSON object")
	}
	if !validKind(request.Kind) {
		return Request{}, fmt.Errorf("unsupported notification kind %q", request.Kind)
	}
	if request.Priority == "" {
		request.Priority = PriorityNormal
	}
	if !validPriority(request.Priority) {
		return Request{}, fmt.Errorf("unsupported notification priority %q", request.Priority)
	}
	for name, value := range map[string]string{
		"title": request.Title, "text": request.Text, "device_id": request.DeviceID,
		"connector_id": request.ConnectorID, "channel_id": request.ChannelID,
		"thread_id": request.ThreadID, "conversation_id": request.ConversationID,
		"task_id": request.TaskID, "correlation_id": request.CorrelationID,
		"idempotency_key": request.IdempotencyKey,
	} {
		if len(value) > maxTextBytes || strings.ContainsRune(value, 0) {
			return Request{}, fmt.Errorf("notification %s is too long or contains NUL", name)
		}
	}
	if strings.TrimSpace(request.CorrelationID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return Request{}, errors.New("notification correlation_id and idempotency_key are required")
	}
	if len(request.Metadata) > maxMetadataBytes || (len(request.Metadata) > 0 && !json.Valid(request.Metadata)) {
		return Request{}, errors.New("notification metadata must be valid JSON no larger than 256 KiB")
	}
	if err := validateTargetFields(request); err != nil {
		return Request{}, err
	}
	return request, nil
}

func (r *Router) Execute(ctx context.Context, raw json.RawMessage, _ func(coretools.Progress)) (json.RawMessage, error) {
	request, err := r.ParseAndValidate(raw)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deliveryID := r.newID()
	if strings.TrimSpace(deliveryID) == "" {
		return nil, errors.New("notification ID generator returned an empty ID")
	}
	queuedAt := r.now().UTC()
	receipt := Receipt{DeliveryID: deliveryID, Kind: request.Kind, Status: "queued", QueuedAt: queuedAt}
	r.hub.Publish(stream.Event{
		ID: deliveryID, Type: "notification.requested", OccurredAt: queuedAt,
		CorrelationID: request.CorrelationID, ConversationID: request.ConversationID,
		TaskID: request.TaskID, Payload: request,
	})

	r.mu.RLock()
	handler := r.handlers[request.Kind]
	r.mu.RUnlock()
	if handler == nil {
		return json.Marshal(receipt)
	}
	handlerReceipt, deliveryErr := handler.Deliver(ctx, request)
	finishedAt := r.now().UTC()
	if deliveryErr != nil {
		r.publishFailure(request, deliveryID, finishedAt, deliveryErr)
		return nil, fmt.Errorf("deliver %s notification: %w", request.Kind, deliveryErr)
	}
	if len(handlerReceipt.Receipt) > maxMetadataBytes || (len(handlerReceipt.Receipt) > 0 && !json.Valid(handlerReceipt.Receipt)) {
		err := errors.New("notification handler returned an invalid or oversized receipt")
		r.publishFailure(request, deliveryID, finishedAt, err)
		return nil, err
	}
	receipt.Status = "delivered"
	receipt.DeliveredAt = &finishedAt
	receipt.Provider = handlerReceipt.Provider
	receipt.ProviderReceipt = handlerReceipt.Receipt
	r.hub.Publish(stream.Event{
		Type: "notification.delivered", OccurredAt: finishedAt,
		CorrelationID: request.CorrelationID, ConversationID: request.ConversationID,
		TaskID: request.TaskID, Payload: receipt,
	})
	return json.Marshal(receipt)
}

func (r *Router) publishFailure(request Request, deliveryID string, occurredAt time.Time, deliveryErr error) {
	r.hub.Publish(stream.Event{
		Type: "notification.failed", OccurredAt: occurredAt,
		CorrelationID: request.CorrelationID, ConversationID: request.ConversationID,
		TaskID: request.TaskID, Payload: map[string]any{
			"delivery_id": deliveryID, "kind": request.Kind, "error": deliveryErr.Error(),
		},
	})
}

func validateTargetFields(request Request) error {
	requireText := func() error {
		if strings.TrimSpace(request.Text) == "" {
			return errors.New("notification text is required")
		}
		return nil
	}
	connectorFields := request.ConnectorID != "" || request.ChannelID != "" || request.ThreadID != ""
	switch request.Kind {
	case KindAndroidNotification:
		if request.DeviceID == "" {
			return errors.New("device_id is required for an Android notification")
		}
		if connectorFields {
			return errors.New("connector fields are valid only for connector_reply")
		}
		return requireText()
	case KindDesktopNotification:
		if request.DeviceID != "" || connectorFields {
			return errors.New("device and connector fields are not valid for a desktop notification")
		}
		return requireText()
	case KindAndroidCall:
		if request.DeviceID == "" {
			return errors.New("device_id is required for an Android call")
		}
		if connectorFields {
			return errors.New("connector fields are valid only for connector_reply")
		}
		return nil
	case KindSpokenResult:
		if request.ConversationID == "" {
			return errors.New("conversation_id is required for a spoken result")
		}
		if request.DeviceID != "" || connectorFields {
			return errors.New("device and connector fields are not valid for a spoken result")
		}
		return requireText()
	case KindConnectorReply:
		if request.ConnectorID == "" || request.ChannelID == "" {
			return errors.New("connector_id and channel_id are required for a connector reply")
		}
		if request.DeviceID != "" {
			return errors.New("device_id is not valid for a connector reply")
		}
		return requireText()
	default:
		return fmt.Errorf("unsupported notification kind %q", request.Kind)
	}
}

func validKind(kind Kind) bool {
	switch kind {
	case KindAndroidNotification, KindDesktopNotification, KindAndroidCall, KindSpokenResult, KindConnectorReply:
		return true
	default:
		return false
	}
}

func validPriority(priority Priority) bool {
	switch priority {
	case PriorityLow, PriorityNormal, PriorityHigh, PriorityUrgent:
		return true
	default:
		return false
	}
}
