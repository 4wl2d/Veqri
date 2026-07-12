package policy

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"

	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/core/tools"
)

type Engine struct {
	mu                 sync.RWMutex
	emergencyStop      bool
	disabledAgents     map[string]bool
	disabledConnectors map[string]bool
}

type KillSwitches struct {
	Agents     map[string]bool `json:"agents"`
	Connectors map[string]bool `json:"connectors"`
}

func NewEngine() *Engine {
	return &Engine{
		disabledAgents:     make(map[string]bool),
		disabledConnectors: make(map[string]bool),
	}
}

func (e *Engine) SetEmergencyStop(enabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emergencyStop = enabled
}

func (e *Engine) EmergencyStop() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.emergencyStop
}

func (e *Engine) SetAgentDisabled(id string, disabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.disabledAgents[id] = disabled
}

func (e *Engine) AgentDisabled(id string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.disabledAgents[id]
}

func (e *Engine) SetConnectorDisabled(id string, disabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.disabledConnectors[id] = disabled
}

func (e *Engine) ConnectorDisabled(id string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.disabledConnectors[id]
}

func (e *Engine) LoadKillSwitches(state KillSwitches) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, disabled := range state.Agents {
		e.disabledAgents[id] = disabled
	}
	for id, disabled := range state.Connectors {
		e.disabledConnectors[id] = disabled
	}
}

func (e *Engine) KillSwitches() KillSwitches {
	e.mu.RLock()
	defer e.mu.RUnlock()
	state := KillSwitches{Agents: make(map[string]bool), Connectors: make(map[string]bool)}
	for id, disabled := range e.disabledAgents {
		state.Agents[id] = disabled
	}
	for id, disabled := range e.disabledConnectors {
		state.Connectors[id] = disabled
	}
	return state
}

func (e *Engine) Evaluate(request Request) Result {
	e.mu.RLock()
	stopped := e.emergencyStop
	agentDisabled := e.disabledAgents[request.AgentID]
	connectorDisabled := request.ConnectorID != "" && e.disabledConnectors[request.ConnectorID]
	e.mu.RUnlock()
	if stopped {
		return Result{Decision: DecisionDeny, Reason: "emergency stop prevents new tool execution"}
	}
	if agentDisabled {
		return Result{Decision: DecisionDeny, Reason: "agent kill switch is active"}
	}
	if connectorDisabled {
		return Result{Decision: DecisionDeny, Reason: "connector kill switch is active"}
	}
	if request.Risk == tools.RiskPrivileged {
		return Result{Decision: DecisionDeny, Reason: "privilege escalation is denied by default"}
	}
	if request.ToolName == "shell" && containsShellInterpreter(request.ToolArguments) {
		return Result{Decision: DecisionDeny, Reason: "shell interpreter string execution is denied; use a structured binary and arguments"}
	}
	if request.TrustLevel == events.TrustUntrusted {
		return Result{Decision: DecisionRequireApproval, Reason: "untrusted content cannot authorize tool execution"}
	}
	switch request.Risk {
	case tools.RiskReadOnly:
		if request.TrustLevel == events.TrustLocal || request.TrustLevel == events.TrustTrusted {
			return Result{Decision: DecisionAllow, Reason: "read-only tool is allowed for trusted local context"}
		}
		return Result{Decision: DecisionRequireApproval, Reason: "non-local read-only request requires confirmation"}
	case tools.RiskLow:
		if request.TrustLevel == events.TrustLocal {
			return Result{Decision: DecisionAllow, Reason: "low-risk local operation is allowed"}
		}
		return Result{Decision: DecisionRequireApproval, Reason: "remote low-risk operation requires confirmation"}
	case tools.RiskStateChanging:
		return Result{Decision: DecisionRequireApproval, Reason: "state-changing operation requires explicit approval"}
	case tools.RiskDestructive:
		return Result{Decision: DecisionRequireApproval, Reason: "destructive operation requires explicit approval"}
	case tools.RiskExternalCommunication:
		return Result{Decision: DecisionRequireApproval, Reason: "external communication requires policy approval"}
	case tools.RiskSecretAccess:
		return Result{Decision: DecisionRequireApproval, Reason: "secret access requires a declared reference and approval"}
	default:
		return Result{Decision: DecisionDeny, Reason: "unknown risk classification"}
	}
}

func containsShellInterpreter(arguments []byte) bool {
	var input struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(arguments, &input) != nil {
		// Invalid tool arguments are rejected by the typed executor. They are
		// not interpreted as evidence that an operation is safe.
		return true
	}
	command := strings.ToLower(filepath.Base(strings.TrimSpace(input.Command)))
	return map[string]bool{
		"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true,
		"cmd": true, "cmd.exe": true, "powershell": true,
		"powershell.exe": true, "pwsh": true,
	}[command]
}
