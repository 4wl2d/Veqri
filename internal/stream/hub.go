package stream

import (
	"context"
	"sync"
	"time"

	"github.com/veqri/veqri/internal/ids"
)

type Event struct {
	ID             string    `json:"id"`
	Type           string    `json:"type"`
	OccurredAt     time.Time `json:"occurred_at"`
	CorrelationID  string    `json:"correlation_id,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	TaskID         string    `json:"task_id,omitempty"`
	Payload        any       `json:"payload,omitempty"`
}

type Hub struct {
	mu          sync.RWMutex
	subscribers map[uint64]chan Event
	nextID      uint64
	lastEventID string
}

func New() *Hub {
	return &Hub{subscribers: make(map[uint64]chan Event)}
}

func (h *Hub) Publish(event Event) {
	if event.ID == "" {
		event.ID = ids.New()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastEventID = event.ID
	for id, subscriber := range h.subscribers {
		select {
		case subscriber <- event:
		default:
			// Never leave a client connected after silently dropping state. A
			// closed stream forces Android through durable catch-up and desktop
			// through a fresh snapshot.
			delete(h.subscribers, id)
			close(subscriber)
		}
	}
}

type Stats struct {
	Subscribers int
	Queued      int
	LastEventID string
}

func (h *Hub) Stats() Stats {
	h.mu.RLock()
	defer h.mu.RUnlock()
	stats := Stats{Subscribers: len(h.subscribers), LastEventID: h.lastEventID}
	for _, subscriber := range h.subscribers {
		stats.Queued += len(subscriber)
	}
	return stats
}

func (h *Hub) Subscribe(ctx context.Context, buffer int) <-chan Event {
	if buffer < 1 {
		buffer = 32
	}
	channel := make(chan Event, buffer)
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	h.subscribers[id] = channel
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		if current, ok := h.subscribers[id]; ok {
			delete(h.subscribers, id)
			close(current)
		}
		h.mu.Unlock()
	}()
	return channel
}
