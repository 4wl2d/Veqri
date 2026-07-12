package policy

import (
	"encoding/json"
	"time"

	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/core/tools"
)

type Decision string

const (
	DecisionAllow              Decision = "ALLOW"
	DecisionAllowWithRedaction Decision = "ALLOW_WITH_REDACTION"
	DecisionRequireApproval    Decision = "REQUIRE_APPROVAL"
	DecisionDeny               Decision = "DENY"
)

type Request struct {
	Source          events.Source     `json:"source"`
	TrustLevel      events.TrustLevel `json:"trust_level"`
	ActorID         string            `json:"actor_id"`
	ConnectorID     string            `json:"connector_id,omitempty"`
	ChannelID       string            `json:"channel_id,omitempty"`
	DeviceID        string            `json:"device_id,omitempty"`
	AgentID         string            `json:"agent_id"`
	ToolName        string            `json:"tool_name"`
	ToolArguments   json.RawMessage   `json:"tool_arguments"`
	Workspace       string            `json:"workspace,omitempty"`
	At              time.Time         `json:"at"`
	ConversationID  string            `json:"conversation_id,omitempty"`
	ApprovalID      string            `json:"approval_id,omitempty"`
	Risk            tools.Risk        `json:"risk"`
	RequestedScopes []string          `json:"requested_scopes"`
	SideEffects     []string          `json:"side_effects"`
}

type Result struct {
	Decision       Decision `json:"decision"`
	Reason         string   `json:"reason"`
	RedactedFields []string `json:"redacted_fields,omitempty"`
}
