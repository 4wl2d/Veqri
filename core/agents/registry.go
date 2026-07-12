package agents

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/veqri/veqri/core/tasks"
)

type registeredRunner struct {
	runner    Runner
	semaphore chan struct{}
}

type Registry struct {
	mu      sync.RWMutex
	runners map[string]*registeredRunner
}

type retryClassifier interface {
	Retryable(error) bool
}

func NewRegistry() *Registry { return &Registry{runners: make(map[string]*registeredRunner)} }

func (r *Registry) Register(runner Runner) error {
	definition := runner.Definition()
	if definition.ID == "" {
		return errors.New("agent id is required")
	}
	if definition.ConcurrencyLimit < 1 {
		return errors.New("agent concurrency limit must be positive")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.runners[definition.ID]; exists {
		return fmt.Errorf("agent %q is already registered", definition.ID)
	}
	r.runners[definition.ID] = &registeredRunner{
		runner: runner, semaphore: make(chan struct{}, definition.ConcurrencyLimit),
	}
	return nil
}

func (r *Registry) Definitions() []Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Definition, 0, len(r.runners))
	for _, registered := range r.runners {
		result = append(result, registered.runner.Definition())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (r *Registry) Run(ctx context.Context, task tasks.Task, progress func(Progress)) (Result, error) {
	r.mu.RLock()
	registered, exists := r.runners[task.AssignedAgentID]
	r.mu.RUnlock()
	if !exists {
		return Result{}, fmt.Errorf("agent %q is not registered", task.AssignedAgentID)
	}
	select {
	case registered.semaphore <- struct{}{}:
		defer func() { <-registered.semaphore }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	return registered.runner.Run(ctx, task, progress)
}

func (r *Registry) Retryable(agentID string, runErr error) bool {
	if runErr == nil || errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		return false
	}
	r.mu.RLock()
	registered := r.runners[agentID]
	r.mu.RUnlock()
	if registered == nil {
		return false
	}
	classifier, ok := registered.runner.(retryClassifier)
	return ok && classifier.Retryable(runErr)
}
