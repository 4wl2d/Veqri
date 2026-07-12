package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	coretools "github.com/veqri/veqri/core/tools"
)

const helperEnvironment = "GO_WANT_VEQRI_SHELL_HELPER"

func TestShellHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvironment) != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		t.Fatal("missing helper mode")
	}
	arguments := os.Args[separator+1:]
	switch arguments[0] {
	case "echo":
		for _, argument := range arguments[1:] {
			fmt.Fprintln(os.Stdout, argument)
		}
	case "streams":
		fmt.Fprint(os.Stdout, arguments[1])
		fmt.Fprint(os.Stderr, arguments[2])
	case "environment":
		fmt.Fprint(os.Stdout, os.Getenv(arguments[1]))
	case "sleep":
		time.Sleep(30 * time.Second)
	case "flood":
		value := strings.Repeat("x", maxOutputBytes+4096)
		_, _ = os.Stdout.WriteString(value)
		_, _ = os.Stderr.WriteString(value)
	default:
		t.Fatalf("unknown helper mode %q", arguments[0])
	}
}

func newTestExecutor(t *testing.T, extraEnv, redactions []string) (*Executor, string) {
	t.Helper()
	workspace := t.TempDir()
	executor, err := New([]string{workspace}, append([]string{helperEnvironment}, extraEnv...), redactions)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return executor, executor.workspaces[0]
}

func helperInput(t *testing.T, workspace string, timeout int, mode string, arguments ...string) json.RawMessage {
	t.Helper()
	binary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	args := []string{"-test.run=^TestShellHelperProcess$", "--", mode}
	args = append(args, arguments...)
	raw, err := json.Marshal(Input{
		Command: binary, Args: args, WorkingDir: workspace,
		Environment: map[string]string{helperEnvironment: "1"}, TimeoutSeconds: timeout,
	})
	if err != nil {
		t.Fatalf("marshal helper input: %v", err)
	}
	return raw
}

func decodeOutput(t *testing.T, raw json.RawMessage) Output {
	t.Helper()
	var output Output
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatalf("decode shell output %q: %v", raw, err)
	}
	return output
}

func TestNewRequiresResolvableWorkspace(t *testing.T) {
	if _, err := New(nil, nil, nil); err == nil {
		t.Fatal("New(nil) accepted no workspaces")
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := New([]string{missing}, nil, nil); err == nil {
		t.Fatal("New() accepted a missing workspace")
	}

	root := t.TempDir()
	realWorkspace := filepath.Join(root, "real")
	if err := os.Mkdir(realWorkspace, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realWorkspace, alias); err != nil {
		t.Fatal(err)
	}
	executor, err := New([]string{alias}, nil, nil)
	if err != nil {
		t.Fatalf("New(workspace symlink): %v", err)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(realWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(executor.workspaces) != 1 || executor.workspaces[0] != resolvedWorkspace {
		t.Fatalf("resolved workspaces = %v, want %q", executor.workspaces, resolvedWorkspace)
	}
}

func TestParseAndValidateRequiresOneStrictStructuredInvocation(t *testing.T) {
	executor, workspace := newTestExecutor(t, nil, nil)
	valid := []byte(`{"command":"git","args":["status"],"dry_run":true}`)
	input, risk, err := executor.ParseAndValidate(valid)
	if err != nil {
		t.Fatalf("ParseAndValidate(valid): %v", err)
	}
	if !filepath.IsAbs(input.Command) || filepath.Base(input.Command) != "git" ||
		len(input.ExecutableSHA256) != 64 || len(input.Args) != 1 || input.Args[0] != "status" ||
		input.WorkingDir != workspace || input.TimeoutSeconds != 120 {
		t.Fatalf("parsed input = %+v", input)
	}
	// The request is rewritten to a symlink-resolved absolute path and content
	// identity before approval. Git remains state-changing in the generic tool.
	if risk != coretools.RiskStateChanging {
		t.Fatalf("risk = %s, want STATE_CHANGING for PATH-resolved binary", risk)
	}

	invalid := []struct {
		name string
		raw  string
	}{
		{name: "empty document", raw: ``},
		{name: "unknown field", raw: `{"command":"git","args":[],"script":"status"}`},
		{name: "missing command", raw: `{"args":[]}`},
		{name: "missing argument array", raw: `{"command":"git"}`},
		{name: "trailing document", raw: `{"command":"git","args":[]} {"command":"bash","args":[]}`},
		{name: "command NUL", raw: `{"command":"git\u0000","args":[]}`},
		{name: "argument NUL", raw: `{"command":"git","args":["bad\u0000arg"]}`},
		{name: "relative command path", raw: `{"command":"../bin/git","args":[]}`},
		{name: "timeout below range", raw: `{"command":"git","args":[],"timeout_seconds":-1}`},
		{name: "timeout above range", raw: `{"command":"git","args":[],"timeout_seconds":3601}`},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := executor.ParseAndValidate([]byte(tt.raw)); err == nil {
				t.Fatalf("ParseAndValidate(%s) accepted invalid input", tt.raw)
			}
		})
	}
}

func TestParseAndValidateBoundsApprovalPayloadInputs(t *testing.T) {
	executor, workspace := newTestExecutor(t, nil, nil)
	tests := []Input{
		{Command: strings.Repeat("x", maxCommandBytes+1), Args: []string{}, WorkingDir: workspace},
		{Command: "git", Args: make([]string, maxArgumentCount+1), WorkingDir: workspace},
		{Command: "git", Args: []string{strings.Repeat("x", maxArgumentBytes+1)}, WorkingDir: workspace},
		{Command: "git", Args: []string{strings.Repeat("x", maxArgumentsBytes/2), strings.Repeat("y", maxArgumentsBytes/2+1)}, WorkingDir: workspace},
		{Command: "git", Args: []string{}, WorkingDir: workspace, Environment: map[string]string{helperEnvironment: strings.Repeat("x", maxEnvironmentValue+1)}},
	}
	for index, input := range tests {
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := executor.ParseAndValidate(raw); err == nil {
			t.Errorf("oversized input %d was accepted", index)
		}
	}
}

func TestReadOnlyAutoAllowRejectsUnconfinedOperandsAndWritingOptions(t *testing.T) {
	tests := []Input{
		{Command: "/bin/cat", Args: []string{"/etc/passwd"}},
		{Command: "/usr/bin/head", Args: []string{"/outside/file"}},
		{Command: "/usr/bin/git", Args: []string{"diff", "--output=/tmp/leak"}},
		{Command: "/usr/bin/git", Args: []string{"branch", "new-branch"}},
	}
	for _, input := range tests {
		if risk := Classify(input); risk != coretools.RiskStateChanging {
			t.Errorf("Classify(%s %v) = %s, want STATE_CHANGING", input.Command, input.Args, risk)
		}
	}
	if risk := Classify(Input{Command: "/usr/bin/git", Args: []string{"status"}}); risk != coretools.RiskStateChanging {
		t.Fatalf("git status = %s, want STATE_CHANGING in generic shell", risk)
	}
	if risk := Classify(Input{Command: "/bin/pwd", Args: []string{}}); risk != coretools.RiskReadOnly {
		t.Fatalf("pwd = %s, want READ_ONLY", risk)
	}
}

func TestParseAndValidateDeniesInterpreters(t *testing.T) {
	executor, _ := newTestExecutor(t, nil, nil)
	for _, command := range []string{
		"sh", "bash", "zsh", "fish", "dash", "cmd", "cmd.exe", "powershell",
		"powershell.exe", "pwsh", "osascript", "/bin/sh", "/usr/bin/ENV/../bash",
	} {
		t.Run(strings.ReplaceAll(command, "/", "_"), func(t *testing.T) {
			raw, err := json.Marshal(Input{Command: command, Args: []string{}})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := executor.ParseAndValidate(raw); err == nil {
				t.Fatalf("interpreter %q was accepted", command)
			}
		})
	}
}

func TestParseAndValidateClassifiesCanonicalExecutableTarget(t *testing.T) {
	executor, workspace := newTestExecutor(t, nil, nil)
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		t.Skip("sudo is unavailable on this test host")
	}
	canonicalSudo, err := filepath.EvalSymlinks(sudo)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(workspace, "safe-tool")
	if err := os.Symlink(canonicalSudo, alias); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(Input{Command: alias, Args: []string{"-n", "true"}, WorkingDir: workspace})
	if err != nil {
		t.Fatal(err)
	}
	input, risk, err := executor.ParseAndValidate(raw)
	if err != nil {
		t.Fatal(err)
	}
	if risk != coretools.RiskPrivileged || input.Command != canonicalSudo || len(input.ExecutableSHA256) != 64 {
		t.Fatalf("canonical alias = command %q risk %s digest %q", input.Command, risk, input.ExecutableSHA256)
	}
}

func TestExecuteDeniesPrivilegedBinaryWithoutRuntimePolicy(t *testing.T) {
	executor, workspace := newTestExecutor(t, nil, nil)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	privilegedAlias := filepath.Join(workspace, "runas.exe")
	copyExecutableForTest(t, testBinary, privilegedAlias)
	raw, err := json.Marshal(Input{
		Command: privilegedAlias, Args: []string{}, WorkingDir: workspace, TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), raw, nil); err == nil ||
		!strings.Contains(err.Error(), "privilege escalation is denied") {
		t.Fatalf("direct privileged execution error = %v", err)
	}
}

func TestParseAndValidateRejectsScriptAliasAndExecutableIdentityDrift(t *testing.T) {
	executor, workspace := newTestExecutor(t, nil, nil)
	script := filepath.Join(workspace, "approved-tool")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho unsafe\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(Input{Command: script, Args: []string{}, WorkingDir: workspace})
	if _, _, err := executor.ParseAndValidate(raw); err == nil {
		t.Fatal("native-binary boundary accepted a shebang script")
	}

	first, err := exec.LookPath("echo")
	if err != nil {
		t.Skip("echo binary is unavailable")
	}
	second, err := exec.LookPath("ls")
	if err != nil {
		t.Skip("ls binary is unavailable")
	}
	copyExecutableForTest(t, first, script)
	raw, _ = json.Marshal(Input{Command: script, Args: []string{}, WorkingDir: workspace})
	approved, _, err := executor.ParseAndValidate(raw)
	if err != nil {
		t.Fatal(err)
	}
	copyExecutableForTest(t, second, script)
	approvedRaw, err := json.Marshal(approved)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := executor.ParseAndValidate(approvedRaw); err == nil || !strings.Contains(err.Error(), "changed after approval") {
		t.Fatalf("replaced executable validation error = %v", err)
	}
}

func TestExecuteUsesStagedApprovedBytesWhenSourcePathIsSwappedBeforeLaunch(t *testing.T) {
	executor, workspace := newTestExecutor(t, nil, nil)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	approvedPath := filepath.Join(workspace, "approved-helper")
	copyExecutableForTest(t, testBinary, approvedPath)
	replacementBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go binary is unavailable for executable-swap fixture")
	}
	replacementPath := filepath.Join(workspace, "replacement-helper")
	copyExecutableForTest(t, replacementBinary, replacementPath)

	raw, err := json.Marshal(Input{
		Command: approvedPath,
		Args: []string{
			"-test.run=^TestShellHelperProcess$", "--", "echo", "approved staged bytes",
		},
		WorkingDir: workspace, TimeoutSeconds: 5,
		Environment: map[string]string{helperEnvironment: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	approved, _, err := executor.ParseAndValidate(raw)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRaw, err := json.Marshal(approved)
	if err != nil {
		t.Fatal(err)
	}

	var stagedPath string
	var swapErr error
	executor.beforeLaunch = func(originalCommand, stagedCommand string) {
		stagedPath = stagedCommand
		if originalCommand != approved.Command || stagedCommand == originalCommand {
			swapErr = fmt.Errorf("launch paths = original %q staged %q", originalCommand, stagedCommand)
			return
		}
		// Rename-over is atomic on Unix. Windows may reject replacement of an
		// existing pathname, so fall back to remove+rename there; either way the
		// swap is forced after the approved descriptor was copied and verified.
		if err := os.Rename(replacementPath, originalCommand); err != nil {
			if removeErr := os.Remove(originalCommand); removeErr != nil {
				swapErr = fmt.Errorf("replace approved source: %v (remove: %v)", err, removeErr)
				return
			}
			swapErr = os.Rename(replacementPath, originalCommand)
		}
	}

	encoded, err := executor.Execute(context.Background(), canonicalRaw, nil)
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	if err != nil {
		t.Fatalf("Execute() after source swap: %v", err)
	}
	output := decodeOutput(t, encoded)
	if output.Command != approved.Command || !strings.Contains(output.Stdout, "approved staged bytes") {
		t.Fatalf("execution did not preserve approved identity and bytes: %+v", output)
	}
	if stagedPath == "" {
		t.Fatal("before-launch swap hook was not invoked")
	}
	if _, statErr := os.Stat(stagedPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("private staged executable was not cleaned up: %v", statErr)
	}
	if _, _, err := executor.ParseAndValidate(canonicalRaw); err == nil || !strings.Contains(err.Error(), "changed after approval") {
		t.Fatalf("source path did not drift after forced swap: %v", err)
	}
}

func copyExecutableForTest(t testing.TB, source, target string) {
	t.Helper()
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyIsConservative(t *testing.T) {
	tests := []struct {
		name    string
		command string
		args    []string
		want    coretools.Risk
	}{
		{name: "read only binary", command: "cat", want: coretools.RiskReadOnly},
		{name: "absolute read only binary", command: "/bin/cat", want: coretools.RiskReadOnly},
		{name: "git status uses typed tool or approval", command: "git", args: []string{"status"}, want: coretools.RiskStateChanging},
		{name: "git diff uses typed tool or approval", command: "git", args: []string{"diff", "--cached"}, want: coretools.RiskStateChanging},
		{name: "git push", command: "git", args: []string{"push"}, want: coretools.RiskStateChanging},
		{name: "unknown command", command: "custom-tool", want: coretools.RiskStateChanging},
		{name: "go command", command: "go", args: []string{"test", "./..."}, want: coretools.RiskStateChanging},
		{name: "destructive", command: "rm", want: coretools.RiskDestructive},
		{name: "destructive absolute uppercase", command: "/bin/RM", want: coretools.RiskDestructive},
		{name: "privileged", command: "sudo", want: coretools.RiskPrivileged},
		{name: "Windows runas", command: "runas.exe", want: coretools.RiskPrivileged},
		{name: "Windows sudo", command: "sudo.exe", want: coretools.RiskPrivileged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(Input{Command: tt.command, Args: tt.args}); got != tt.want {
				t.Fatalf("Classify() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestWorkingDirectoryCannotTraverseOrEscapeThroughSymlink(t *testing.T) {
	executor, workspace := newTestExecutor(t, nil, nil)
	inside := filepath.Join(workspace, "inside")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	escape := filepath.Join(workspace, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		dir  string
	}{
		{name: "outside absolute", dir: outside},
		{name: "parent traversal", dir: filepath.Join(workspace, "..")},
		{name: "symlink escape", dir: escape},
		{name: "symlink escape child", dir: filepath.Join(escape, ".")},
		{name: "nonexistent", dir: filepath.Join(workspace, "missing")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(Input{Command: "pwd", Args: []string{}, WorkingDir: tt.dir})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := executor.ParseAndValidate(raw); err == nil {
				t.Fatalf("working directory %q was accepted", tt.dir)
			}
		})
	}
	raw, _ := json.Marshal(Input{Command: "pwd", Args: []string{}, WorkingDir: inside})
	input, _, err := executor.ParseAndValidate(raw)
	if err != nil || input.WorkingDir != inside {
		t.Fatalf("inside working directory result = (%q, %v)", input.WorkingDir, err)
	}
}

func TestEnvironmentAllowlistAndSecretNames(t *testing.T) {
	executor, _ := newTestExecutor(t, []string{"SAFE_VALUE", "API_TOKEN"}, nil)
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "not allowlisted", env: map[string]string{"LD_PRELOAD": "evil"}},
		{name: "secret like even when allowlisted", env: map[string]string{"API_TOKEN": "secret"}},
		{name: "name contains equals", env: map[string]string{"SAFE_VALUE=BAD": "value"}},
		{name: "name contains NUL", env: map[string]string{"SAFE_VALUE\x00": "value"}},
		{name: "value contains NUL", env: map[string]string{"SAFE_VALUE": "bad\x00value"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(Input{Command: "pwd", Args: []string{}, Environment: tt.env})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := executor.ParseAndValidate(raw); err == nil {
				t.Fatalf("environment %v was accepted", tt.env)
			}
		})
	}
	raw, _ := json.Marshal(Input{Command: "pwd", Args: []string{}, Environment: map[string]string{"SAFE_VALUE": "safe"}})
	if _, _, err := executor.ParseAndValidate(raw); err != nil {
		t.Fatalf("allowlisted safe environment rejected: %v", err)
	}
}

func TestStructuredArgumentsCannotInjectCommands(t *testing.T) {
	executor, workspace := newTestExecutor(t, nil, nil)
	sentinel := filepath.Join(workspace, "injected")
	payloads := []string{
		"; touch " + sentinel,
		"$(touch " + sentinel + ")",
		"`touch " + sentinel + "`",
		"&& touch " + sentinel,
	}
	raw := helperInput(t, workspace, 5, "echo", payloads...)
	encoded, err := executor.Execute(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("Execute(): %v", err)
	}
	output := decodeOutput(t, encoded)
	for _, payload := range payloads {
		if !strings.Contains(output.Stdout, payload) {
			t.Errorf("structured argument %q was not preserved literally in %q", payload, output.Stdout)
		}
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("injection sentinel exists or stat failed unexpectedly: %v", err)
	}
}

func TestExecuteTimesOutAndReportsStructuredResult(t *testing.T) {
	executor, workspace := newTestExecutor(t, nil, nil)
	started := time.Now()
	encoded, err := executor.Execute(context.Background(), helperInput(t, workspace, 1, "sleep"), nil)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("timed out command returned nil error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout took %s, want less than 5s", elapsed)
	}
	output := decodeOutput(t, encoded)
	if !output.TimedOut || output.ExitCode == 0 {
		t.Fatalf("timeout output = %+v", output)
	}
}

func TestExecuteRedactsCapturedProgressAndMetadata(t *testing.T) {
	const secret = "super-secret-value"
	executor, workspace := newTestExecutor(t, nil, []string{secret})
	raw := helperInput(t, workspace, 5, "streams", "stdout "+secret, "stderr "+secret)
	var mu sync.Mutex
	var progress []coretools.Progress
	encoded, err := executor.Execute(context.Background(), raw, func(item coretools.Progress) {
		mu.Lock()
		defer mu.Unlock()
		progress = append(progress, item)
	})
	if err != nil {
		t.Fatalf("Execute(): %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("structured output leaked redacted value: %s", encoded)
	}
	output := decodeOutput(t, encoded)
	if !strings.Contains(output.Stdout, "[REDACTED]") || !strings.Contains(output.Stderr, "[REDACTED]") {
		t.Fatalf("captured output was not redacted: %+v", output)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(progress) == 0 {
		t.Fatal("no progress was emitted")
	}
	for _, item := range progress {
		if strings.Contains(item.Data, secret) {
			t.Fatalf("progress leaked secret in %+v", item)
		}
	}
}

func TestExecuteDoesNotInheritSecretLikeEnvironment(t *testing.T) {
	t.Setenv("BUILD_TOKEN", "inherited-secret")
	executor, workspace := newTestExecutor(t, []string{"BUILD_TOKEN"}, nil)
	raw := helperInput(t, workspace, 5, "environment", "BUILD_TOKEN")
	encoded, err := executor.Execute(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("Execute(): %v", err)
	}
	if output := decodeOutput(t, encoded); strings.Contains(output.Stdout, "inherited-secret") {
		t.Fatalf("secret-like environment was inherited: %q", output.Stdout)
	}
}

func TestExecuteCapsStdoutAndStderr(t *testing.T) {
	executor, workspace := newTestExecutor(t, nil, nil)
	encoded, err := executor.Execute(context.Background(), helperInput(t, workspace, 10, "flood"), nil)
	if err != nil {
		t.Fatalf("Execute(): %v", err)
	}
	output := decodeOutput(t, encoded)
	if !output.Truncated {
		t.Fatal("oversized output was not marked truncated")
	}
	if len(output.Stdout) != maxOutputBytes || len(output.Stderr) != maxOutputBytes {
		t.Fatalf("captured lengths = (%d, %d), want (%d, %d)", len(output.Stdout), len(output.Stderr), maxOutputBytes, maxOutputBytes)
	}
}

func TestDryRunNeverExecutesCommand(t *testing.T) {
	executor, workspace := newTestExecutor(t, nil, nil)
	sentinel := filepath.Join(workspace, "dry-run-injected")
	input := Input{
		Command: "touch", Args: []string{sentinel}, WorkingDir: workspace,
		TimeoutSeconds: 5, DryRun: true,
	}
	raw, _ := json.Marshal(input)
	encoded, err := executor.Execute(context.Background(), raw, nil)
	if err != nil {
		t.Fatalf("Execute(dry run): %v", err)
	}
	output := decodeOutput(t, encoded)
	if !output.DryRun || output.ExitCode != 0 {
		t.Fatalf("dry run output = %+v", output)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created sentinel or stat failed: %v", err)
	}
}
