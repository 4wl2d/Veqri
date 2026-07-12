package agents

import (
	"context"
	"encoding/json"
	"time"

	"github.com/veqri/veqri/core/tasks"
)

type TrustLevel string
type ExecutionMode string
type Health string

const (
	TrustUntrusted TrustLevel = "untrusted"
	TrustKnown     TrustLevel = "known"
	TrustTrusted   TrustLevel = "trusted"

	ModeBuiltin    ExecutionMode = "builtin"
	ModeSubprocess ExecutionMode = "subprocess"
	ModeLocalModel ExecutionMode = "local_model"
	ModeHTTP       ExecutionMode = "http"
	ModeGRPC       ExecutionMode = "grpc"
	ModeStdio      ExecutionMode = "stdio"
	ModeMCP        ExecutionMode = "mcp"

	HealthUnknown   Health = "unknown"
	HealthHealthy   Health = "healthy"
	HealthDegraded  Health = "degraded"
	HealthUnhealthy Health = "unhealthy"
)

type Definition struct {
	ID                   string          `json:"id"`
	DisplayName          string          `json:"display_name"`
	Description          string          `json:"description"`
	Capabilities         []string        `json:"capabilities"`
	AcceptedTaskTypes    []string        `json:"accepted_task_types"`
	InputSchema          json.RawMessage `json:"input_schema"`
	OutputSchema         json.RawMessage `json:"output_schema"`
	ToolScopes           []string        `json:"tool_scopes"`
	TrustLevel           TrustLevel      `json:"trust_level"`
	CostMetadata         json.RawMessage `json:"cost_metadata"`
	LatencyMetadata      json.RawMessage `json:"latency_metadata"`
	ConcurrencyLimit     int             `json:"concurrency_limit"`
	Health               Health          `json:"health"`
	ExecutionMode        ExecutionMode   `json:"execution_mode"`
	SupportsCancellation bool            `json:"supports_cancellation"`
	SupportsStreaming    bool            `json:"supports_streaming"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

type Progress struct {
	Percent int    `json:"percent"`
	Message string `json:"message"`
}

type Result struct {
	Structured     json.RawMessage  `json:"structured"`
	WrittenSummary string           `json:"written_summary"`
	SpokenSummary  string           `json:"spoken_summary"`
	Artifacts      []tasks.Artifact `json:"artifacts"`
	Partial        bool             `json:"partial"`
}

type Runner interface {
	Definition() Definition
	Run(ctx context.Context, task tasks.Task, progress func(Progress)) (Result, error)
}
