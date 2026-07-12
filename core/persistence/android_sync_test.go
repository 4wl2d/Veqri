package persistence

import (
	"context"
	"testing"
	"time"

	"github.com/veqri/veqri/core/tasks"
)

func TestListTasksByRecencyIgnoresTerminalTaskPriority(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

	olderHighPriority := testRootTask("older-high-priority", "", "older-event", now.Add(-2*time.Hour))
	olderHighPriority.Status = tasks.StatusCompleted
	olderHighPriority.Priority = 100
	olderFinished := now.Add(-time.Hour)
	olderHighPriority.FinishedAt = &olderFinished
	newerLowPriority := testRootTask("newer-low-priority", "", "newer-event", now.Add(-30*time.Minute))
	newerLowPriority.Status = tasks.StatusCompleted
	newerLowPriority.Priority = -100
	newerFinished := now
	newerLowPriority.FinishedAt = &newerFinished
	for _, task := range []tasks.Task{olderHighPriority, newerLowPriority} {
		if _, _, err := store.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}

	priorityOrdered, err := store.ListTasks(ctx, []tasks.Status{tasks.StatusCompleted}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if priorityOrdered[0].ID != olderHighPriority.ID {
		t.Fatalf("ListTasks first = %q, want high-priority task", priorityOrdered[0].ID)
	}
	recent, err := store.ListTasksByRecency(ctx, []tasks.Status{tasks.StatusCompleted}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if recent[0].ID != newerLowPriority.ID {
		t.Fatalf("ListTasksByRecency first = %q, want newest terminal task", recent[0].ID)
	}
}
