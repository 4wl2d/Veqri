package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/veqri/veqri/core/agents"
	"github.com/veqri/veqri/core/tasks"
)

type Runner struct {
	id          string
	displayName string
	capability  string
	stepDelay   time.Duration
}

func New(id, displayName, capability string, stepDelay time.Duration) *Runner {
	return &Runner{id: id, displayName: displayName, capability: capability, stepDelay: stepDelay}
}

func (r *Runner) Definition() agents.Definition {
	return agents.Definition{
		ID: r.id, DisplayName: r.displayName,
		Description:          "Deterministic local agent used for development and tests",
		Capabilities:         []string{r.capability, "deterministic", "offline"},
		AcceptedTaskTypes:    []string{"dialog", "coding", "research", "automation", "synthesis"},
		InputSchema:          json.RawMessage(`{"type":"object"}`),
		OutputSchema:         json.RawMessage(`{"type":"object","required":["answer","simulated"]}`),
		ToolScopes:           []string{},
		TrustLevel:           agents.TrustTrusted,
		CostMetadata:         json.RawMessage(`{"currency":"USD","fixed":0}`),
		LatencyMetadata:      json.RawMessage(`{"class":"deterministic-local"}`),
		ConcurrencyLimit:     8,
		Health:               agents.HealthHealthy,
		ExecutionMode:        agents.ModeBuiltin,
		SupportsCancellation: true,
		SupportsStreaming:    true,
		UpdatedAt:            time.Now().UTC(),
	}
}

func (r *Runner) Run(ctx context.Context, task tasks.Task, progress func(agents.Progress)) (agents.Result, error) {
	steps := []agents.Progress{
		{Percent: 15, Message: r.displayName + " accepted the task"},
		{Percent: 50, Message: "Working deterministically offline"},
		{Percent: 85, Message: "Preparing the result"},
	}
	for _, step := range steps {
		if r.stepDelay > 0 {
			timer := time.NewTimer(r.stepDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return agents.Result{}, ctx.Err()
			case <-timer.C:
			}
		}
		if progress != nil {
			progress(step)
		}
	}
	goal := strings.TrimSpace(task.Goal)
	answer := fmt.Sprintf("%s completed the local simulated task: %s", r.displayName, goal)
	structured, err := json.Marshal(map[string]any{
		"answer": answer, "agent_id": r.id, "capability": r.capability,
		"simulated": true, "task_id": task.ID,
	})
	if err != nil {
		return agents.Result{}, err
	}
	return agents.Result{
		Structured:     structured,
		WrittenSummary: answer + ". This deterministic result validates orchestration; configure a local model, subprocess, or remote agent for real domain work.",
		SpokenSummary:  answer + ".",
	}, nil
}
