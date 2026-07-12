package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/veqri/veqri/core/tasks"
)

func TestExplicitRetryOfFailedChildRearmsTerminalSynthesisRoot(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now().UTC()
	root, child, completed := retryGraphTasks(now, "child-retry")
	if _, _, err := store.CreateTaskGraph(ctx, []tasks.Task{root, child, completed}, []tasks.Dependency{
		{TaskID: root.ID, DependsOnTaskID: child.ID},
		{TaskID: root.ID, DependsOnTaskID: completed.ID},
	}); err != nil {
		t.Fatal(err)
	}

	retried, err := store.RetryTask(ctx, child.ID)
	if err != nil {
		t.Fatalf("RetryTask(child): %v", err)
	}
	if retried.Status != tasks.StatusQueued || retried.RetryCount != 1 || len(retried.Result) != 0 ||
		retried.Error != "" || retried.FinishedAt != nil {
		t.Fatalf("retried child retained terminal attempt state: %+v", retried)
	}
	updatedRoot, err := store.GetTask(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedRoot.Status != tasks.StatusQueued || updatedRoot.RetryCount != 1 ||
		len(updatedRoot.Result) != 0 || updatedRoot.UserFacingSummary != "" || updatedRoot.FinishedAt != nil {
		t.Fatalf("terminal synthesis root was not re-armed: %+v", updatedRoot)
	}
	unchanged, err := store.GetTask(ctx, completed.ID)
	if err != nil || unchanged.Status != tasks.StatusCompleted || unchanged.RetryCount != 0 {
		t.Fatalf("successful sibling changed during targeted retry: %+v, %v", unchanged, err)
	}

	claimed, err := store.ClaimNextTask(ctx)
	if err != nil || claimed.ID != child.ID {
		t.Fatalf("first claim = %+v, %v; synthesis root ran before retried dependency", claimed, err)
	}
	running, err := store.StartTask(ctx, claimed.ID, claimed.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteTask(ctx, running.ID, json.RawMessage(`{"attempt":2}`), "recovered child", false); err != nil {
		t.Fatal(err)
	}
	claimedRoot, err := store.ClaimNextTask(ctx)
	if err != nil || claimedRoot.ID != root.ID {
		t.Fatalf("claim after dependency settled = %+v, %v; want synthesis root", claimedRoot, err)
	}
}

func TestExplicitRetryOfPartialSynthesisRootRetriesFailedDependencies(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root, child, completed := retryGraphTasks(time.Now().UTC(), "root-retry")
	if _, _, err := store.CreateTaskGraph(ctx, []tasks.Task{root, child, completed}, []tasks.Dependency{
		{TaskID: root.ID, DependsOnTaskID: child.ID},
		{TaskID: root.ID, DependsOnTaskID: completed.ID},
	}); err != nil {
		t.Fatal(err)
	}

	retriedRoot, err := store.RetryTask(ctx, root.ID)
	if err != nil {
		t.Fatalf("RetryTask(root): %v", err)
	}
	if retriedRoot.Status != tasks.StatusQueued || retriedRoot.RetryCount != 1 ||
		len(retriedRoot.Result) != 0 || retriedRoot.UserFacingSummary != "" {
		t.Fatalf("retried synthesis root = %+v", retriedRoot)
	}
	retriedChild, err := store.GetTask(ctx, child.ID)
	if err != nil || retriedChild.Status != tasks.StatusQueued || retriedChild.RetryCount != 1 {
		t.Fatalf("failed dependency was not queued with root retry: %+v, %v", retriedChild, err)
	}
	claimed, err := store.ClaimNextTask(ctx)
	if err != nil || claimed.ID != child.ID {
		t.Fatalf("first claim = %+v, %v; partial root merely re-synthesized unchanged failures", claimed, err)
	}
}

func TestExplicitRetryOfFailedSynthesizerDoesNotReplaySuccessfulDependencies(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root, _, completed := retryGraphTasks(time.Now().UTC(), "failed-synthesizer")
	root.Status = tasks.StatusFailed
	root.Error = "synthesizer failed"
	root.Result = nil
	root.UserFacingSummary = ""
	if _, _, err := store.CreateTaskGraph(ctx, []tasks.Task{root, completed}, []tasks.Dependency{
		{TaskID: root.ID, DependsOnTaskID: completed.ID},
	}); err != nil {
		t.Fatal(err)
	}

	retriedRoot, err := store.RetryTask(ctx, root.ID)
	if err != nil || retriedRoot.Status != tasks.StatusQueued || retriedRoot.RetryCount != 1 {
		t.Fatalf("RetryTask(failed synthesizer) = %+v, %v", retriedRoot, err)
	}
	unchanged, err := store.GetTask(ctx, completed.ID)
	if err != nil || unchanged.Status != tasks.StatusCompleted || unchanged.RetryCount != 0 {
		t.Fatalf("successful dependency was replayed: %+v, %v", unchanged, err)
	}
}

func TestExplicitChildRetryRollsBackWhenSynthesisRootCannotBeRearmed(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root, child, _ := retryGraphTasks(time.Now().UTC(), "exhausted-root")
	root.RetryCount = root.MaxRetries
	if _, _, err := store.CreateTaskGraph(ctx, []tasks.Task{root, child}, []tasks.Dependency{
		{TaskID: root.ID, DependsOnTaskID: child.ID},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.RetryTask(ctx, child.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("RetryTask(child) error = %v, want conflict", err)
	}
	unchanged, err := store.GetTask(ctx, child.ID)
	if err != nil || unchanged.Status != tasks.StatusFailed || unchanged.RetryCount != 0 {
		t.Fatalf("child changed despite root guard failure: %+v, %v", unchanged, err)
	}
}

func retryGraphTasks(now time.Time, suffix string) (tasks.Task, tasks.Task, tasks.Task) {
	rootID := "retry-root-" + suffix
	eventID := "retry-event-" + suffix
	root := testRootTask(rootID, "", eventID, now)
	root.TaskType = "synthesis"
	root.AssignedAgentID = "builtin.synthesizer"
	root.Status = tasks.StatusPartiallyCompleted
	root.Progress = 100
	root.ProgressMessage = "Complete"
	root.Result = json.RawMessage(`{"failed_subtasks":["child failed"]}`)
	root.UserFacingSummary = "partial answer"
	root.FinishedAt = &now

	parentID := root.ID
	child := testRootTask("retry-child-"+suffix, "", eventID+"-child", now.Add(time.Millisecond))
	child.RootTaskID = root.ID
	child.ParentTaskID = &parentID
	child.TaskType = "coding"
	child.AssignedAgentID = "builtin.coding"
	child.Status = tasks.StatusFailed
	child.Error = "first attempt failed"
	child.Result = json.RawMessage(`{"failed":true}`)
	child.FinishedAt = &now

	completed := testRootTask("retry-completed-"+suffix, "", eventID+"-completed", now.Add(2*time.Millisecond))
	completed.RootTaskID = root.ID
	completed.ParentTaskID = &parentID
	completed.TaskType = "research"
	completed.AssignedAgentID = "builtin.research"
	completed.Status = tasks.StatusCompleted
	completed.Progress = 100
	completed.Result = json.RawMessage(`{"ok":true}`)
	completed.UserFacingSummary = "successful sibling"
	completed.FinishedAt = &now
	return root, child, completed
}
