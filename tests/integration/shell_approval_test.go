package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/veqri/veqri/core/approvals"
	"github.com/veqri/veqri/core/policy"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/tests/integration/testfixture"
	"github.com/veqri/veqri/tools/shell"
)

func TestShellPolicyApprovalSingleUseExecutionAuditAndDenial(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{WorkerCount: 2})
	device := fixture.PairAndroid(t, "Approval phone")

	readOnly := fixture.JSON(t, http.MethodPost, "/v1/tools/shell", fixture.AdminToken, map[string]any{
		"input": map[string]any{
			"command": "pwd", "args": []string{}, "working_directory": fixture.Workspace,
			"timeout_seconds": 2,
		},
		"idempotency_key": "read-only-pwd",
	}, nil)
	testfixture.RequireStatus(t, readOnly, http.StatusAccepted)
	readResult := testfixture.Decode[shellRequestResult](t, readOnly)
	if readResult.Duplicate || readResult.Approval != nil || readResult.Policy.Decision != policy.DecisionAllow {
		t.Fatalf("trusted read-only command was not directly allowed: %+v", readResult)
	}
	completedRead := fixture.WaitTask(t, readResult.Task.ID, tasks.StatusCompleted)
	invocation, err := fixture.Store.GetToolInvocationByIdempotencyKey(context.Background(), "shell:"+completedRead.ID)
	if err != nil {
		t.Fatalf("load read-only tool invocation: %v", err)
	}
	var readOutput shell.Output
	if err := json.Unmarshal(invocation.Output, &readOutput); err != nil {
		t.Fatalf("decode read-only shell output: %v", err)
	}
	if readOutput.ExitCode != 0 || strings.TrimSpace(readOutput.Stdout) != readOutput.WorkingDir || readOutput.DryRun {
		t.Fatalf("unexpected read-only shell output: %+v", readOutput)
	}

	approvedDirectory := filepath.Join(fixture.Workspace, "approved-exactly-once")
	stateChanging := fixture.JSON(t, http.MethodPost, "/v1/tools/shell", fixture.AdminToken, map[string]any{
		"input": map[string]any{
			"command": "mkdir", "args": []string{"approved-exactly-once"},
			"working_directory": fixture.Workspace, "timeout_seconds": 2,
		},
		"idempotency_key": "mkdir-approved-once",
	}, nil)
	testfixture.RequireStatus(t, stateChanging, http.StatusAccepted)
	stateResult := testfixture.Decode[shellRequestResult](t, stateChanging)
	if stateResult.Approval == nil || stateResult.Policy.Decision != policy.DecisionRequireApproval || stateResult.Task.Status != tasks.StatusWaitingForApproval {
		t.Fatalf("state-changing command did not wait for approval: %+v", stateResult)
	}
	var approvedInput shell.Input
	if err := json.Unmarshal(stateResult.Approval.ToolArguments, &approvedInput); err != nil {
		t.Fatalf("decode canonical approval arguments: %v", err)
	}
	if !filepath.IsAbs(approvedInput.Command) || filepath.Base(approvedInput.Command) != "mkdir" ||
		len(approvedInput.ExecutableSHA256) != 64 || len(approvedInput.Args) != 1 ||
		approvedInput.Args[0] != "approved-exactly-once" || approvedInput.WorkingDir != fixture.Shell.Workspaces()[0] ||
		approvedInput.TimeoutSeconds != 2 || string(stateResult.Task.Input) != string(stateResult.Approval.ToolArguments) {
		t.Fatalf("approval did not expose the exact canonical invocation: %+v", approvedInput)
	}
	if _, err := os.Stat(approvedDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state-changing command ran before approval: %v", err)
	}

	approved := fixture.JSON(t, http.MethodPost, "/v1/approvals/"+stateResult.Approval.ID+"/approve", device.Credential, map[string]any{}, nil)
	testfixture.RequireStatus(t, approved, http.StatusOK)
	approvedResult := testfixture.Decode[struct {
		Approval approvals.Approval `json:"approval"`
		Task     tasks.Task         `json:"task"`
	}](t, approved)
	if approvedResult.Approval.Status != approvals.StatusConsumed || approvedResult.Approval.ConsumedAt == nil || approvedResult.Task.Status != tasks.StatusQueued {
		t.Fatalf("approval was not atomically consumed: %+v", approvedResult)
	}
	replayedApproval := fixture.JSON(t, http.MethodPost, "/v1/approvals/"+stateResult.Approval.ID+"/approve", device.Credential, map[string]any{}, nil)
	testfixture.RequireStatus(t, replayedApproval, http.StatusConflict)
	completedStateChange := fixture.WaitTask(t, stateResult.Task.ID, tasks.StatusCompleted)
	if info, err := os.Stat(approvedDirectory); err != nil || !info.IsDir() {
		t.Fatalf("approved mkdir did not execute: info=%v err=%v", info, err)
	}
	assertTaskInvocationCount(t, fixture, completedStateChange.ID, 1)
	assertAuditActions(t, fixture, completedStateChange.ID, map[string]int{
		"approval.decide": 1,
		"tool.started":    1,
		"tool.finished":   1,
	})

	deniedDirectory := filepath.Join(fixture.Workspace, "must-not-exist")
	deniedRequest := fixture.JSON(t, http.MethodPost, "/v1/tools/shell", fixture.AdminToken, map[string]any{
		"input": map[string]any{
			"command": "mkdir", "args": []string{"must-not-exist"},
			"working_directory": fixture.Workspace, "timeout_seconds": 2,
		},
		"idempotency_key": "mkdir-denied",
	}, nil)
	testfixture.RequireStatus(t, deniedRequest, http.StatusAccepted)
	deniedResult := testfixture.Decode[shellRequestResult](t, deniedRequest)
	if deniedResult.Approval == nil || deniedResult.Task.Status != tasks.StatusWaitingForApproval {
		t.Fatalf("denial fixture did not create a pending approval: %+v", deniedResult)
	}
	denied := fixture.JSON(t, http.MethodPost, "/v1/approvals/"+deniedResult.Approval.ID+"/deny", device.Credential, map[string]any{}, nil)
	testfixture.RequireStatus(t, denied, http.StatusOK)
	deniedDecision := testfixture.Decode[struct {
		Approval approvals.Approval `json:"approval"`
		Task     tasks.Task         `json:"task"`
	}](t, denied)
	if deniedDecision.Approval.Status != approvals.StatusDenied || deniedDecision.Task.Status != tasks.StatusCancelled {
		t.Fatalf("denied approval did not cancel its task: %+v", deniedDecision)
	}
	replayedDenial := fixture.JSON(t, http.MethodPost, "/v1/approvals/"+deniedResult.Approval.ID+"/deny", device.Credential, map[string]any{}, nil)
	testfixture.RequireStatus(t, replayedDenial, http.StatusConflict)
	if _, err := os.Stat(deniedDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied command changed the workspace: %v", err)
	}
	assertTaskInvocationCount(t, fixture, deniedResult.Task.ID, 0)
	assertAuditActions(t, fixture, deniedResult.Task.ID, map[string]int{"approval.decide": 1})

	privileged := fixture.JSON(t, http.MethodPost, "/v1/tools/shell", fixture.AdminToken, map[string]any{
		"input": map[string]any{
			"command": "sudo", "args": []string{"whoami"}, "working_directory": fixture.Workspace,
		},
		"idempotency_key": "privileged-denied",
	}, nil)
	testfixture.RequireStatus(t, privileged, http.StatusForbidden)
	var privilegedTasks int
	if err := fixture.Store.DB().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM tasks WHERE idempotency_key = ?", "shell-request:privileged-denied").Scan(&privilegedTasks); err != nil {
		t.Fatalf("count privileged denied tasks: %v", err)
	}
	if privilegedTasks != 0 {
		t.Fatalf("policy-denied privileged command created %d executable task(s)", privilegedTasks)
	}
}

type shellRequestResult struct {
	Task      tasks.Task          `json:"task"`
	Approval  *approvals.Approval `json:"approval"`
	Policy    policy.Result       `json:"policy"`
	Duplicate bool                `json:"duplicate"`
}

func assertTaskInvocationCount(t testing.TB, fixture *testfixture.Fixture, taskID string, expected int) {
	t.Helper()
	var count int
	if err := fixture.Store.DB().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM tool_invocations WHERE task_id = ?", taskID).Scan(&count); err != nil {
		t.Fatalf("count tool invocations for %s: %v", taskID, err)
	}
	if count != expected {
		t.Fatalf("tool invocation count for %s = %d, want %d", taskID, count, expected)
	}
}

func assertAuditActions(t testing.TB, fixture *testfixture.Fixture, taskID string, expected map[string]int) {
	t.Helper()
	for action, wanted := range expected {
		var count int
		if err := fixture.Store.DB().QueryRowContext(context.Background(),
			"SELECT COUNT(*) FROM audit_entries WHERE task_id = ? AND action = ?", taskID, action).Scan(&count); err != nil {
			t.Fatalf("count %s audit entries for %s: %v", action, taskID, err)
		}
		if count != wanted {
			t.Fatalf("%s audit entry count for %s = %d, want %d", action, taskID, count, wanted)
		}
	}
}
