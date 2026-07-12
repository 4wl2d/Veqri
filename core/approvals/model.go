package approvals

import (
	"encoding/json"
	"time"

	"github.com/veqri/veqri/core/tools"
)

type Status string

const (
	StatusPending  Status = "PENDING"
	StatusApproved Status = "APPROVED"
	StatusDenied   Status = "DENIED"
	StatusExpired  Status = "EXPIRED"
	StatusConsumed Status = "CONSUMED"
)

type Approval struct {
	ID              string          `json:"id"`
	TaskID          string          `json:"task_id"`
	ToolName        string          `json:"tool_name"`
	ToolArguments   json.RawMessage `json:"tool_arguments"`
	RequestedScopes []string        `json:"requested_scopes"`
	Risk            tools.Risk      `json:"risk"`
	Reason          string          `json:"reason"`
	Status          Status          `json:"status"`
	RequestedAt     time.Time       `json:"requested_at"`
	ExpiresAt       time.Time       `json:"expires_at"`
	DecidedAt       *time.Time      `json:"decided_at,omitempty"`
	DecidedBy       string          `json:"decided_by,omitempty"`
	ConsumedAt      *time.Time      `json:"consumed_at,omitempty"`
	CorrelationID   string          `json:"correlation_id"`
}
