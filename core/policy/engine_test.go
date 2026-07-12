package policy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/core/tools"
)

func policyRequest() Request {
	return Request{
		Source:        events.Source{Kind: "local"},
		TrustLevel:    events.TrustLocal,
		ActorID:       "actor-1",
		ConnectorID:   "connector-1",
		AgentID:       "agent-1",
		ToolName:      "filesystem",
		ToolArguments: json.RawMessage(`{}`),
		At:            time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC),
		Risk:          tools.RiskReadOnly,
	}
}

func TestEnginePolicyDecisions(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Request)
		want Decision
	}{
		{name: "trusted read only", edit: func(r *Request) { r.TrustLevel = events.TrustTrusted }, want: DecisionAllow},
		{name: "local read only", edit: func(r *Request) {}, want: DecisionAllow},
		{name: "known read only", edit: func(r *Request) { r.TrustLevel = events.TrustKnown }, want: DecisionRequireApproval},
		{name: "untrusted read only", edit: func(r *Request) { r.TrustLevel = events.TrustUntrusted }, want: DecisionRequireApproval},
		{name: "local low risk", edit: func(r *Request) { r.Risk = tools.RiskLow }, want: DecisionAllow},
		{name: "remote low risk", edit: func(r *Request) { r.Risk = tools.RiskLow; r.TrustLevel = events.TrustTrusted }, want: DecisionRequireApproval},
		{name: "state changing", edit: func(r *Request) { r.Risk = tools.RiskStateChanging }, want: DecisionRequireApproval},
		{name: "destructive", edit: func(r *Request) { r.Risk = tools.RiskDestructive }, want: DecisionRequireApproval},
		{name: "external communication", edit: func(r *Request) { r.Risk = tools.RiskExternalCommunication }, want: DecisionRequireApproval},
		{name: "secret access", edit: func(r *Request) { r.Risk = tools.RiskSecretAccess }, want: DecisionRequireApproval},
		{name: "privileged is denied", edit: func(r *Request) { r.Risk = tools.RiskPrivileged }, want: DecisionDeny},
		{name: "unknown risk is denied", edit: func(r *Request) { r.Risk = tools.Risk("SURPRISE") }, want: DecisionDeny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := policyRequest()
			tt.edit(&request)
			result := NewEngine().Evaluate(request)
			if result.Decision != tt.want {
				t.Fatalf("decision = %s (%s), want %s", result.Decision, result.Reason, tt.want)
			}
			if strings.TrimSpace(result.Reason) == "" {
				t.Fatal("decision reason is empty")
			}
		})
	}
}

func TestEngineEmergencyAndKillSwitchesFailClosedAndAreReversible(t *testing.T) {
	engine := NewEngine()
	request := policyRequest()

	engine.SetEmergencyStop(true)
	if !engine.EmergencyStop() || engine.Evaluate(request).Decision != DecisionDeny {
		t.Fatal("emergency stop did not deny execution")
	}
	engine.SetEmergencyStop(false)
	if engine.EmergencyStop() || engine.Evaluate(request).Decision != DecisionAllow {
		t.Fatal("disabling emergency stop did not restore normal policy")
	}

	engine.SetAgentDisabled(request.AgentID, true)
	if result := engine.Evaluate(request); result.Decision != DecisionDeny || !strings.Contains(result.Reason, "agent kill switch") {
		t.Fatalf("agent kill switch result = %+v", result)
	}
	engine.SetAgentDisabled(request.AgentID, false)
	if engine.Evaluate(request).Decision != DecisionAllow {
		t.Fatal("agent remained disabled after switch was cleared")
	}

	engine.SetConnectorDisabled(request.ConnectorID, true)
	if result := engine.Evaluate(request); result.Decision != DecisionDeny || !strings.Contains(result.Reason, "connector kill switch") {
		t.Fatalf("connector kill switch result = %+v", result)
	}
	engine.SetConnectorDisabled(request.ConnectorID, false)
	if engine.Evaluate(request).Decision != DecisionAllow {
		t.Fatal("connector remained disabled after switch was cleared")
	}
}

func TestEngineDeniesStructuredShellInterpreters(t *testing.T) {
	for _, arguments := range []json.RawMessage{
		json.RawMessage(`{"command":"bash","args":["-c","id"]}`),
		json.RawMessage(`{"command": "bash", "args": ["-c", "id"]}`),
		json.RawMessage(`{"COMMAND":"PoWeRsHeLl","args":[]}`),
	} {
		request := policyRequest()
		request.ToolName = "shell"
		request.ToolArguments = arguments
		result := NewEngine().Evaluate(request)
		if result.Decision != DecisionDeny {
			t.Errorf("shell interpreter payload %s received %s, want DENY", arguments, result.Decision)
		}
	}
}
