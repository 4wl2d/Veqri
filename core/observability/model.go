package observability

import (
	"encoding/json"
	"time"
)

type AuditEntry struct {
	ID             string          `json:"id"`
	OccurredAt     time.Time       `json:"occurred_at"`
	ActorKind      string          `json:"actor_kind"`
	ActorID        string          `json:"actor_id"`
	Action         string          `json:"action"`
	ResourceKind   string          `json:"resource_kind"`
	ResourceID     string          `json:"resource_id"`
	Decision       string          `json:"decision,omitempty"`
	Details        json.RawMessage `json:"details"`
	CorrelationID  string          `json:"correlation_id"`
	TaskID         string          `json:"task_id,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	ConnectorID    string          `json:"connector_id,omitempty"`
}
