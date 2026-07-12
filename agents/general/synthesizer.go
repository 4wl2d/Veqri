package general

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/veqri/veqri/core/agents"
	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/core/tasks"
)

type Synthesizer struct {
	store *persistence.Store
}

func NewSynthesizer(store *persistence.Store) *Synthesizer { return &Synthesizer{store: store} }

func (s *Synthesizer) Definition() agents.Definition {
	return agents.Definition{
		ID: "builtin.synthesizer", DisplayName: "Result synthesizer",
		Description:          "Aggregates persisted child results while preserving failures and uncertainty",
		Capabilities:         []string{"result-synthesis", "conflict-disclosure", "offline"},
		AcceptedTaskTypes:    []string{"synthesis"},
		InputSchema:          json.RawMessage(`{"type":"object"}`),
		OutputSchema:         json.RawMessage(`{"type":"object","required":["agreements","conflicts","failed_subtasks","written","spoken"]}`),
		TrustLevel:           agents.TrustTrusted,
		CostMetadata:         json.RawMessage(`{"currency":"USD","fixed":0}`),
		LatencyMetadata:      json.RawMessage(`{"class":"local"}`),
		ConcurrencyLimit:     4,
		Health:               agents.HealthHealthy,
		ExecutionMode:        agents.ModeBuiltin,
		SupportsCancellation: true,
		SupportsStreaming:    true,
		UpdatedAt:            time.Now().UTC(),
	}
}

func (s *Synthesizer) Run(ctx context.Context, task tasks.Task, progress func(agents.Progress)) (agents.Result, error) {
	if progress != nil {
		progress(agents.Progress{Percent: 25, Message: "Loading child results"})
	}
	graph, _, err := s.store.GetTaskGraph(ctx, task.RootTaskID)
	if err != nil {
		return agents.Result{}, err
	}
	successes := []string{}
	failures := []string{}
	artifacts := []tasks.Artifact{}
	for _, child := range graph {
		if child.ID == task.ID {
			continue
		}
		switch child.Status {
		case tasks.StatusCompleted, tasks.StatusPartiallyCompleted:
			summary := strings.TrimSpace(child.UserFacingSummary)
			if summary == "" {
				summary = string(child.Result)
			}
			successes = append(successes, fmt.Sprintf("%s: %s", child.AssignedAgentID, summary))
			artifacts = append(artifacts, child.Artifacts...)
		default:
			failures = append(failures, fmt.Sprintf("%s (%s): %s", child.AssignedAgentID, child.Status, child.Error))
		}
	}
	if progress != nil {
		progress(agents.Progress{Percent: 75, Message: "Reconciling results and failures"})
	}
	agreements := append([]string(nil), successes...)
	conflicts := []string{}
	written := "No child task produced a usable result."
	if len(successes) > 0 {
		written = "Delegated results:\n- " + strings.Join(successes, "\n- ")
	}
	if len(failures) > 0 {
		written += "\n\nFailed or incomplete subtasks:\n- " + strings.Join(failures, "\n- ")
	}
	spoken := fmt.Sprintf("%d delegated task results are ready", len(successes))
	if len(failures) > 0 {
		spoken += fmt.Sprintf(", with %d failure(s)", len(failures))
	}
	spoken += "."
	structured, err := json.Marshal(map[string]any{
		"agreements": agreements, "conflicts": conflicts, "failed_subtasks": failures,
		"written": written, "spoken": spoken,
	})
	if err != nil {
		return agents.Result{}, err
	}
	return agents.Result{Structured: structured, WrittenSummary: written,
		SpokenSummary: spoken, Artifacts: artifacts, Partial: len(failures) > 0}, nil
}
