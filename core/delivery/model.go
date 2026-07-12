package delivery

import "time"

type Status string

const (
	StatusPending    Status = "PENDING"
	StatusDelivering Status = "DELIVERING"
	StatusDelivered  Status = "DELIVERED"
	StatusFailed     Status = "FAILED"
)

type Target struct {
	Kind         string `json:"kind"`
	ConnectorID  string `json:"connector_id,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	ChannelID    string `json:"channel_id,omitempty"`
	ThreadID     string `json:"thread_id,omitempty"`
	CallbackURL  string `json:"callback_url,omitempty"`
	VoiceSession string `json:"voice_session_id,omitempty"`
}

type Delivery struct {
	ID             string     `json:"id"`
	TaskID         string     `json:"task_id"`
	Target         Target     `json:"target"`
	Priority       int        `json:"priority"`
	Status         Status     `json:"status"`
	AttemptCount   int        `json:"attempt_count"`
	LastError      string     `json:"last_error,omitempty"`
	IdempotencyKey string     `json:"idempotency_key"`
	CreatedAt      time.Time  `json:"created_at"`
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
	CorrelationID  string     `json:"correlation_id"`
}
