package stream

import (
	"context"
	"testing"
	"time"
)

func TestSlowSubscriberIsDisconnectedInsteadOfSilentlyLosingEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := New()
	events := hub.Subscribe(ctx, 1)
	hub.Publish(Event{Type: "first"})
	hub.Publish(Event{Type: "overflow"})
	first, ok := <-events
	if !ok || first.Type != "first" {
		t.Fatalf("first event = %#v, open=%v", first, ok)
	}
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("overflowed subscriber remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("overflowed subscriber was not disconnected")
	}
}
