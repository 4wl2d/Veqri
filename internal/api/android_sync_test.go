package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/veqri/veqri/core/approvals"
	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/tasks"
)

func TestAndroidRecentTurnsIsGloballyBoundedAndChronological(t *testing.T) {
	base := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	turns := make([]conversation.Turn, androidSnapshotMessageLimit+40)
	for index := range turns {
		reverse := len(turns) - index - 1
		turns[index] = conversation.Turn{
			ID: fmt.Sprintf("turn-%03d", reverse), CreatedAt: base.Add(time.Duration(reverse) * time.Second),
		}
	}

	bounded := androidRecentTurns(turns)
	if len(bounded) != androidSnapshotMessageLimit {
		t.Fatalf("len(androidRecentTurns) = %d, want %d", len(bounded), androidSnapshotMessageLimit)
	}
	wantLast := fmt.Sprintf("turn-%03d", androidSnapshotMessageLimit+39)
	if bounded[0].ID != "turn-040" || bounded[len(bounded)-1].ID != wantLast {
		t.Fatalf("bounded window = %s..%s, want turn-040..%s", bounded[0].ID, bounded[len(bounded)-1].ID, wantLast)
	}
	for index := 1; index < len(bounded); index++ {
		if bounded[index].CreatedAt.Before(bounded[index-1].CreatedAt) {
			t.Fatalf("turn %d is out of chronological order", index)
		}
	}
}

func TestAndroidSnapshotTasksPreferActiveBeforeTerminalHistory(t *testing.T) {
	active := make([]tasks.Task, androidSnapshotActiveTaskLimit+10)
	for index := range active {
		active[index] = tasks.Task{ID: fmt.Sprintf("active-%03d", index), Status: tasks.StatusRunning}
	}
	terminal := make([]tasks.Task, androidSnapshotTaskLimit)
	for index := range terminal {
		terminal[index] = tasks.Task{ID: fmt.Sprintf("terminal-%03d", index), Status: tasks.StatusCompleted}
	}

	selected, activeCount := androidSelectSnapshotTasks(active, terminal)
	if activeCount != androidSnapshotActiveTaskLimit || len(selected) != androidSnapshotTaskLimit {
		t.Fatalf("selected %d tasks (%d active), want %d (%d active)",
			len(selected), activeCount, androidSnapshotTaskLimit, androidSnapshotActiveTaskLimit)
	}
	for index := 0; index < activeCount; index++ {
		if !strings.HasPrefix(selected[index].ID, "active-") {
			t.Fatalf("task %d = %q, active tasks must be first", index, selected[index].ID)
		}
	}
	for index := activeCount; index < len(selected); index++ {
		if !strings.HasPrefix(selected[index].ID, "terminal-") {
			t.Fatalf("task %d = %q, terminal history must fill the remainder", index, selected[index].ID)
		}
	}
}

func TestAndroidSnapshotFitsBelowReadLimitWithoutDroppingActiveTasks(t *testing.T) {
	text := strings.Repeat("x", androidSnapshotTextBytes)
	messages := make([]map[string]any, androidSnapshotMessageLimit)
	for index := range messages {
		messages[index] = map[string]any{"message_id": fmt.Sprint(index), "conversation_id": "conversation",
			"author": "ASSISTANT", "text": text, "created_at_epoch_millis": index}
	}
	taskPayloads := make([]map[string]any, androidSnapshotTaskLimit)
	for index := range taskPayloads {
		prefix := "terminal"
		if index < androidSnapshotActiveTaskLimit {
			prefix = "active"
		}
		taskPayloads[index] = map[string]any{"task_id": fmt.Sprintf("%s-%03d", prefix, index),
			"root_task_id": "root", "conversation_id": "conversation", "goal": text,
			"assigned_agent": "agent", "status": "RUNNING", "summary": text,
			"created_at_epoch_millis": index, "updated_at_epoch_millis": index, "can_retry": false}
	}
	approvals := make([]map[string]any, androidSnapshotApprovalLimit)
	for index := range approvals {
		approvals[index] = map[string]any{"approval_id": fmt.Sprint(index), "task_id": "task",
			"title": text, "redacted_arguments": text, "status": "PENDING"}
	}

	event, err := androidBoundedSnapshotEvent("snapshot", "conversation", false, messages, taskPayloads,
		androidSnapshotActiveTaskLimit, approvals, androidSnapshotPendingApprovalLimit, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > androidSnapshotMaxBytes || len(raw) >= 128<<10 {
		t.Fatalf("snapshot size = %d bytes", len(raw))
	}
	payload := event.(map[string]any)["payload"].(map[string]any)
	if retained, ok := payload["transcript_retention"].(bool); !ok || retained {
		t.Fatalf("snapshot retention = %#v, want authoritative false", payload["transcript_retention"])
	}
	boundedTasks := payload["tasks"].([]map[string]any)
	if len(boundedTasks) < androidSnapshotActiveTaskLimit {
		t.Fatalf("snapshot retained %d tasks, want all %d active tasks", len(boundedTasks), androidSnapshotActiveTaskLimit)
	}
	for index := 0; index < androidSnapshotActiveTaskLimit; index++ {
		if !strings.HasPrefix(boundedTasks[index]["task_id"].(string), "active-") {
			t.Fatalf("active task %d was displaced: %v", index, boundedTasks[index]["task_id"])
		}
	}
}

func TestAndroidSnapshotTextTruncationPreservesUTF8(t *testing.T) {
	value := strings.Repeat("🙂", 200)
	truncated := androidTruncateUTF8(value, 127)
	if len(truncated) > 127 || !utf8.ValidString(truncated) || !strings.HasSuffix(truncated, "…") {
		t.Fatalf("invalid truncated value: bytes=%d valid=%v", len(truncated), utf8.ValidString(truncated))
	}
}

func TestAndroidLiveTTSBoundPreservesUTF8AndFrameHeadroom(t *testing.T) {
	value := strings.Repeat("🚀", androidLiveTTSMaxBytes)
	truncated := androidTruncateUTF8(value, androidLiveTTSMaxBytes)
	if !utf8.ValidString(truncated) {
		t.Fatal("bounded TTS text is not valid UTF-8")
	}
	if len(truncated) > androidLiveTTSMaxBytes || len(truncated) >= 128<<10 {
		t.Fatalf("bounded TTS text length = %d", len(truncated))
	}
}

func TestAndroidLiveApprovalPreservesExactArguments(t *testing.T) {
	argumentsJSON := json.RawMessage(`{"command":"git","args":["status","--short"]}`)
	approval := approvals.Approval{
		ID: "approval", TaskID: "task", ToolName: "shell",
		ToolArguments: argumentsJSON, RequestedScopes: []string{"tool.shell.execute"},
		Reason: "state-changing operation requires explicit approval", Status: approvals.StatusPending,
	}
	for name, payload := range map[string]map[string]any{
		"live": androidApprovalPayload(approval), "snapshot": androidSnapshotApprovalPayload(approval),
	} {
		if payload["title"] != "Approve shell" || payload["redacted_arguments"] != string(argumentsJSON) {
			t.Fatalf("%s approval display lost exact arguments: %+v", name, payload)
		}
		scopes, ok := payload["requested_scopes"].([]string)
		if !ok || len(scopes) != 1 || scopes[0] != "tool.shell.execute" ||
			payload["reason"] != approval.Reason {
			t.Fatalf("%s approval display lost scope/reason: %+v", name, payload)
		}
	}
}

func TestAndroidTaskRetryCandidateMatchesCoreRetryGuard(t *testing.T) {
	base := tasks.Task{TaskType: "dialog", Status: tasks.StatusFailed, RetryCount: 0, MaxRetries: 2}
	tests := []struct {
		name string
		task tasks.Task
		want bool
	}{
		{name: "failed task with budget", task: base, want: true},
		{name: "partial task with budget", task: withTaskStatus(base, tasks.StatusPartiallyCompleted), want: true},
		{name: "shell never retries", task: withTaskType(base, "shell"), want: false},
		{name: "exhausted budget", task: withTaskRetryCount(base, 2), want: false},
		{name: "dismissed task", task: withTaskDismissed(base), want: false},
		{name: "completed task", task: withTaskStatus(base, tasks.StatusCompleted), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := androidTaskRetryCandidate(test.task); got != test.want {
				t.Fatalf("androidTaskRetryCandidate() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestAndroidTaskPayloadCarriesPriorityAndDismissal(t *testing.T) {
	task := tasks.Task{ID: "dismissed-task", RootTaskID: "dismissed-task", Status: tasks.StatusCompleted,
		Priority: 42, Dismissed: true, CreatedAt: time.Now().UTC()}
	payload := (&Server{}).androidTaskPayload(context.Background(), task)
	if payload["priority"] != 42 || payload["dismissed"] != true || payload["can_retry"] != false {
		t.Fatalf("task payload lost dismissal controls: %+v", payload)
	}
}

func TestAndroidRetentionCommandResultIsCorrelatedAndUnambiguous(t *testing.T) {
	raw := []byte(`{"command_id":"privacy-1","protocol_version":1,"type":"conversation.set_transcript_retention"}`)
	commandID, commandType := androidCommandIdentity(raw)
	if commandID != "privacy-1" || commandType != "conversation.set_transcript_retention" {
		t.Fatalf("command identity = (%q, %q)", commandID, commandType)
	}
	success := androidCommandResult(commandID, commandType, nil)
	if success["correlation_id"] != commandID {
		t.Fatalf("success correlation = %#v", success)
	}
	payload := success["payload"].(map[string]any)
	if payload["status"] != "COMMITTED" || payload["command_id"] != commandID {
		t.Fatalf("success result = %#v", success)
	}
	rejected := androidCommandResult(commandID, commandType, fmt.Errorf("rejected"))
	rejectedPayload := rejected["payload"].(map[string]any)
	if rejectedPayload["status"] != "REJECTED" || rejectedPayload["safe_message"] != "rejected" {
		t.Fatalf("rejected result = %#v", rejected)
	}
}

func withTaskStatus(task tasks.Task, status tasks.Status) tasks.Task {
	task.Status = status
	return task
}

func withTaskType(task tasks.Task, taskType string) tasks.Task {
	task.TaskType = taskType
	return task
}

func withTaskRetryCount(task tasks.Task, count int) tasks.Task {
	task.RetryCount = count
	return task
}

func withTaskDismissed(task tasks.Task) tasks.Task {
	task.Dismissed = true
	return task
}
