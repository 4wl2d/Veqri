package git

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	coretools "github.com/veqri/veqri/core/tools"
)

func TestStatusCommitAndLog(t *testing.T) {
	workspace := t.TempDir()
	repository := filepath.Join(workspace, "repo")
	initRepository(t, repository)
	runGit(t, repository, "config", "user.name", "Veqri Test")
	runGit(t, repository, "config", "user.email", "veqri@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, "note.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "--", "note.txt")

	executor, err := New([]string{workspace})
	if err != nil {
		t.Fatal(err)
	}
	status := executeGitOK(t, executor, Input{Operation: OperationStatus, Repository: repository})
	if status.Risk != coretools.RiskReadOnly || status.ApprovalRequired || !strings.Contains(status.Stdout, "note.txt") {
		t.Fatalf("unexpected status: %+v", status)
	}

	commit := executeGitOK(t, executor, Input{Operation: OperationCommit, Repository: repository, Message: "initial commit"})
	if commit.Risk != coretools.RiskStateChanging || !commit.ApprovalRequired || commit.ExitCode != 0 {
		t.Fatalf("unexpected commit: %+v", commit)
	}
	log := executeGitOK(t, executor, Input{Operation: OperationLog, Repository: repository, Limit: 1})
	if !strings.Contains(log.Stdout, "initial commit") {
		t.Fatalf("log did not contain commit: %+v", log)
	}
}

func TestTraversalSymlinkAndExternalGitDirectoryAreDenied(t *testing.T) {
	workspace := t.TempDir()
	repository := filepath.Join(workspace, "repo")
	initRepository(t, repository)
	outside := t.TempDir()
	outsideRepository := filepath.Join(outside, "outside-repo")
	initRepository(t, outsideRepository)
	executor, err := New([]string{workspace})
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"../outside", outsideRepository} {
		raw, _ := json.Marshal(Input{Operation: OperationStatus, Repository: path})
		if _, _, err := executor.ParseAndValidate(raw); !errors.Is(err, ErrOutsideWorkspace) {
			t.Fatalf("expected workspace denial for %q, got %v", path, err)
		}
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(workspace, "outside-link")
		if err := os.Symlink(outsideRepository, link); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(Input{Operation: OperationStatus, Repository: link})
		if _, _, err := executor.ParseAndValidate(raw); !errors.Is(err, ErrOutsideWorkspace) {
			t.Fatalf("expected symlink denial, got %v", err)
		}
	}

	linkedWorktree := filepath.Join(workspace, "linked")
	if err := os.Mkdir(linkedWorktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linkedWorktree, ".git"), []byte("gitdir: "+filepath.Join(outsideRepository, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = executeGit(t, executor, Input{Operation: OperationStatus, Repository: linkedWorktree})
	if !errors.Is(err, ErrUnsafeRepository) {
		t.Fatalf("expected external gitdir denial, got %v", err)
	}
}

func TestForcePushAndArgumentInjectionAreDenied(t *testing.T) {
	workspace := t.TempDir()
	repository := filepath.Join(workspace, "repo")
	initRepository(t, repository)
	executor, err := New([]string{workspace})
	if err != nil {
		t.Fatal(err)
	}

	force, _ := json.Marshal(Input{Operation: OperationPush, Repository: repository, Remote: "origin", Refspecs: []string{"+main:main"}})
	if _, _, err := executor.ParseAndValidate(force); !errors.Is(err, ErrForcePush) {
		t.Fatalf("force push was not specifically denied: %v", err)
	}
	for _, input := range []Input{
		{Operation: OperationBranchCreate, Repository: repository, Branch: "--help"},
		{Operation: OperationDiff, Repository: repository, Base: "--output=/tmp/owned"},
		{Operation: OperationCommit, Repository: repository, Message: "message", Paths: []string{"../outside"}},
		{Operation: OperationPush, Repository: repository, Remote: "https://example.test/repo", Refspecs: []string{"main:main"}},
	} {
		raw, _ := json.Marshal(input)
		if _, _, err := executor.ParseAndValidate(raw); err == nil {
			t.Fatalf("unsafe input was accepted: %+v", input)
		}
	}
}

func TestCheckoutOperationsDenyExecutableFilterConfiguration(t *testing.T) {
	workspace := t.TempDir()
	repository := filepath.Join(workspace, "repo")
	initRepository(t, repository)
	runGit(t, repository, "config", "filter.evil.smudge", "touch /tmp/veqri-should-not-run")
	executor, err := New([]string{workspace})
	if err != nil {
		t.Fatal(err)
	}
	_, err = executeGit(t, executor, Input{Operation: OperationBranchSwitch, Repository: repository, Branch: "main"})
	if !errors.Is(err, ErrUnsafeRepository) {
		t.Fatalf("expected unsafe filter denial, got %v", err)
	}
}

func TestPushRequiresExplicitSafeRefspecAndAllowedRemoteScheme(t *testing.T) {
	workspace := t.TempDir()
	repository := filepath.Join(workspace, "repo")
	remote := filepath.Join(workspace, "remote.git")
	initRepository(t, repository)
	runCommand(t, "git", "init", "--bare", remote)
	runGit(t, repository, "remote", "add", "origin", remote)

	defaultExecutor, err := New([]string{workspace})
	if err != nil {
		t.Fatal(err)
	}
	_, err = executeGit(t, defaultExecutor, Input{Operation: OperationPush, Repository: repository, Remote: "origin", Refspecs: []string{"HEAD:refs/heads/main"}, DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "local filesystem push") {
		t.Fatalf("default executor allowed local push URL: %v", err)
	}

	executor, err := NewWithConfig(Config{Workspaces: []string{workspace}, AllowedPushSchemes: []string{"file"}})
	if err != nil {
		t.Fatal(err)
	}
	output := executeGitOK(t, executor, Input{Operation: OperationPush, Repository: repository, Remote: "origin", Refspecs: []string{"HEAD:refs/heads/main"}, DryRun: true})
	if output.Risk != coretools.RiskExternalCommunication || !output.ApprovalRequired || !output.DryRun {
		t.Fatalf("unexpected push plan: %+v", output)
	}
	joined := strings.Join(output.Args, " ")
	if !strings.Contains(joined, "--no-force") || strings.Contains(joined, "+HEAD") {
		t.Fatalf("push argv was not force-safe: %q", joined)
	}
}

func TestOutputIsBounded(t *testing.T) {
	workspace := t.TempDir()
	repository := filepath.Join(workspace, "repo")
	initRepository(t, repository)
	if err := os.WriteFile(filepath.Join(repository, "large.txt"), []byte(strings.Repeat("line\n", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "--", "large.txt")
	executor, err := NewWithConfig(Config{Workspaces: []string{workspace}, MaxOutputBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	output := executeGitOK(t, executor, Input{Operation: OperationDiff, Repository: repository, Staged: true})
	if !output.Truncated || len(output.Stdout) > 64 {
		t.Fatalf("expected bounded output, got %d bytes, truncated=%v", len(output.Stdout), output.Truncated)
	}
}

func executeGitOK(t *testing.T, executor *Executor, input Input) Output {
	t.Helper()
	output, err := executeGit(t, executor, input)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func executeGit(t *testing.T, executor *Executor, input Input) (Output, error) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := executor.Execute(context.Background(), raw, nil)
	if err != nil {
		return Output{}, err
	}
	var output Output
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	return output, nil
}

func initRepository(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	runCommand(t, "git", "init", "-b", "main", path)
}

func runGit(t *testing.T, repository string, args ...string) {
	t.Helper()
	all := append([]string{"-C", repository}, args...)
	runCommand(t, "git", all...)
}

func runCommand(t *testing.T, name string, args ...string) {
	t.Helper()
	command := exec.Command(name, args...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, output)
	}
}
