package tasks

import "testing"

func TestTaskTransitions(t *testing.T) {
	tests := []struct {
		name    string
		from    Status
		to      Status
		allowed bool
	}{
		{name: "created queues", from: StatusCreated, to: StatusQueued, allowed: true},
		{name: "created can await approval", from: StatusCreated, to: StatusWaitingForApproval, allowed: true},
		{name: "queued assigns", from: StatusQueued, to: StatusAssigned, allowed: true},
		{name: "assigned runs", from: StatusAssigned, to: StatusRunning, allowed: true},
		{name: "running completes", from: StatusRunning, to: StatusCompleted, allowed: true},
		{name: "running requests cancellation", from: StatusRunning, to: StatusCancelRequested, allowed: true},
		{name: "approval queues", from: StatusWaitingForApproval, to: StatusQueued, allowed: true},
		{name: "same state is idempotent", from: StatusRunning, to: StatusRunning, allowed: true},
		{name: "cannot skip assignment", from: StatusQueued, to: StatusRunning, allowed: false},
		{name: "completed is final", from: StatusCompleted, to: StatusQueued, allowed: false},
		{name: "cancelled is final", from: StatusCancelled, to: StatusRunning, allowed: false},
		{name: "unknown source", from: Status("UNKNOWN"), to: StatusQueued, allowed: false},
		{name: "unknown target", from: StatusRunning, to: Status("UNKNOWN"), allowed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanTransition(tt.from, tt.to); got != tt.allowed {
				t.Fatalf("CanTransition(%s, %s) = %v, want %v", tt.from, tt.to, got, tt.allowed)
			}
			err := ValidateTransition(tt.from, tt.to)
			if tt.allowed && err != nil {
				t.Fatalf("allowed transition rejected: %v", err)
			}
			if !tt.allowed && err == nil {
				t.Fatal("invalid transition accepted")
			}
		})
	}
}

func TestTerminalStatuses(t *testing.T) {
	terminal := map[Status]bool{
		StatusCompleted: true, StatusPartiallyCompleted: true, StatusFailed: true,
		StatusCancelled: true, StatusTimedOut: true,
	}
	all := []Status{
		StatusCreated, StatusQueued, StatusAssigned, StatusRunning, StatusWaitingForChildren,
		StatusWaitingForApproval, StatusBlocked, StatusCompleted, StatusPartiallyCompleted,
		StatusFailed, StatusCancelRequested, StatusCancelled, StatusTimedOut,
	}
	for _, status := range all {
		if got := status.Terminal(); got != terminal[status] {
			t.Errorf("%s.Terminal() = %v, want %v", status, got, terminal[status])
		}
	}
}
