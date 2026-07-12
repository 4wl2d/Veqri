package tools

import (
	"context"
	"encoding/json"
	"time"
)

type Risk string

const (
	RiskReadOnly              Risk = "READ_ONLY"
	RiskLow                   Risk = "LOW"
	RiskStateChanging         Risk = "STATE_CHANGING"
	RiskDestructive           Risk = "DESTRUCTIVE"
	RiskPrivileged            Risk = "PRIVILEGED"
	RiskExternalCommunication Risk = "EXTERNAL_COMMUNICATION"
	RiskSecretAccess          Risk = "SECRET_ACCESS"
)

type Definition struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description"`
	InputSchema          json.RawMessage `json:"input_schema"`
	OutputSchema         json.RawMessage `json:"output_schema"`
	RequiredScopes       []string        `json:"required_scopes"`
	Risk                 Risk            `json:"risk"`
	ApprovalRequired     bool            `json:"approval_required"`
	DefaultTimeout       time.Duration   `json:"default_timeout"`
	SupportsCancellation bool            `json:"supports_cancellation"`
	SupportsStreaming    bool            `json:"supports_streaming"`
	SupportedOS          []string        `json:"supported_os"`
}

type Invocation struct {
	ID             string          `json:"id"`
	TaskID         string          `json:"task_id"`
	ToolName       string          `json:"tool_name"`
	Input          json.RawMessage `json:"input"`
	Risk           Risk            `json:"risk"`
	Status         string          `json:"status"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	FinishedAt     *time.Time      `json:"finished_at,omitempty"`
	ExitCode       *int            `json:"exit_code,omitempty"`
	Output         json.RawMessage `json:"output,omitempty"`
	Error          string          `json:"error,omitempty"`
	CorrelationID  string          `json:"correlation_id"`
	IdempotencyKey string          `json:"idempotency_key"`
}

type Progress struct {
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

type Executor interface {
	Definition() Definition
	Execute(ctx context.Context, input json.RawMessage, progress func(Progress)) (json.RawMessage, error)
}
