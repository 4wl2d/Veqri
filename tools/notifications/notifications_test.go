package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/veqri/veqri/internal/stream"
)

func TestRouterPublishesRequestedAndDeliveredEvents(t *testing.T) {
	hub := stream.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := hub.Subscribe(ctx, 4)
	fixed := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	router, err := New(hub,
		WithClock(func() time.Time { return fixed }),
		WithIDGenerator(func() string { return "delivery-1" }),
		WithHandler(KindAndroidNotification, HandlerFunc(func(_ context.Context, request Request) (HandlerReceipt, error) {
			if request.DeviceID != "phone" || request.Text != "Build finished" {
				t.Fatalf("request = %#v", request)
			}
			return HandlerReceipt{Provider: "android-local", Receipt: json.RawMessage(`{"notification_id":"n1"}`)}, nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := router.Execute(context.Background(), json.RawMessage(`{
		"kind":"android_notification","device_id":"phone","title":"Veqri","text":"Build finished",
		"task_id":"task-1","conversation_id":"conversation-1","correlation_id":"correlation-1","idempotency_key":"idem-1"
	}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.DeliveryID != "delivery-1" || receipt.Status != "delivered" || receipt.Provider != "android-local" {
		t.Fatalf("receipt = %#v", receipt)
	}
	requested := <-events
	delivered := <-events
	if requested.Type != "notification.requested" || requested.ID != "delivery-1" || requested.CorrelationID != "correlation-1" || requested.TaskID != "task-1" {
		t.Fatalf("requested event = %#v", requested)
	}
	if delivered.Type != "notification.delivered" || delivered.CorrelationID != "correlation-1" {
		t.Fatalf("delivered event = %#v", delivered)
	}
}

func TestRouterQueuesWhenNoDeliveryHandlerIsRegistered(t *testing.T) {
	hub := stream.New()
	router, err := New(hub, WithIDGenerator(func() string { return "queued-1" }))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := router.Execute(context.Background(), json.RawMessage(`{
		"kind":"desktop_notification","text":"Ready","correlation_id":"c","idempotency_key":"i"
	}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "queued" {
		t.Fatalf("receipt = %#v", receipt)
	}
}

func TestRouterPublishesDeliveryFailure(t *testing.T) {
	hub := stream.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := hub.Subscribe(ctx, 4)
	router, err := New(hub, WithHandler(KindConnectorReply, HandlerFunc(func(context.Context, Request) (HandlerReceipt, error) {
		return HandlerReceipt{}, errors.New("connector offline")
	})))
	if err != nil {
		t.Fatal(err)
	}
	_, err = router.Execute(context.Background(), json.RawMessage(`{
		"kind":"connector_reply","connector_id":"slack","channel_id":"C1","thread_id":"T1","text":"Done",
		"correlation_id":"c","idempotency_key":"i"
	}`), nil)
	if err == nil {
		t.Fatal("delivery failure was not returned")
	}
	if first, second := <-events, <-events; first.Type != "notification.requested" || second.Type != "notification.failed" {
		t.Fatalf("events = %q, %q", first.Type, second.Type)
	}
}

func TestRouterRejectsMismatchedTargetsAndMissingCorrelation(t *testing.T) {
	hub := stream.New()
	router, err := New(hub)
	if err != nil {
		t.Fatal(err)
	}
	cases := []json.RawMessage{
		json.RawMessage(`{"kind":"android_notification","text":"x","correlation_id":"c","idempotency_key":"i"}`),
		json.RawMessage(`{"kind":"desktop_notification","device_id":"phone","text":"x","correlation_id":"c","idempotency_key":"i"}`),
		json.RawMessage(`{"kind":"spoken_result","conversation_id":"conversation","text":"x","idempotency_key":"i"}`),
	}
	for _, input := range cases {
		if _, err := router.Execute(context.Background(), input, nil); err == nil {
			t.Fatalf("invalid input accepted: %s", input)
		}
	}
}
