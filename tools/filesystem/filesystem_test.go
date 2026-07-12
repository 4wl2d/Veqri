package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	coretools "github.com/veqri/veqri/core/tools"
)

func TestReadWriteAndExactPatchReturnHashes(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "note.txt")
	if err := os.WriteFile(path, []byte("alpha beta\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	executor, err := New([]string{workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()

	read := executeOK(t, executor, Input{Operation: OperationRead, Path: "note.txt"})
	if read.Content != "alpha beta\n" || read.SHA256 == "" || read.Risk != coretools.RiskReadOnly || read.ApprovalRequired {
		t.Fatalf("unexpected read output: %+v", read)
	}

	write := executeOK(t, executor, Input{
		Operation: OperationWrite, Path: path, Content: "alpha gamma\n",
		ExpectedSHA256: read.SHA256,
	})
	if write.PreviousSHA256 != read.SHA256 || write.SHA256 == read.SHA256 || !write.ApprovalRequired || write.Risk != coretools.RiskStateChanging {
		t.Fatalf("unexpected write output: %+v", write)
	}

	badPatch := Input{Operation: OperationPatch, Path: "note.txt", Patches: []ExactPatch{{OldText: "alpha", NewText: "omega", ExpectedOccurrences: 2}}}
	_, err = execute(t, executor, badPatch)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected exact-patch conflict, got %v", err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != "alpha gamma\n" {
		t.Fatalf("failed patch changed the file: %q, %v", unchanged, err)
	}

	patch := executeOK(t, executor, Input{
		Operation: OperationPatch, Path: "note.txt", ExpectedSHA256: write.SHA256,
		Patches: []ExactPatch{{OldText: "gamma", NewText: "delta"}},
	})
	if patch.PreviousSHA256 != write.SHA256 || patch.SHA256 == write.SHA256 {
		t.Fatalf("unexpected patch hashes: %+v", patch)
	}
	updated, err := os.ReadFile(path)
	if err != nil || string(updated) != "alpha delta\n" {
		t.Fatalf("unexpected patched file: %q, %v", updated, err)
	}
}

func TestTraversalAndSymlinkEscapesAreDenied(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor, err := New([]string{workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()

	for _, path := range []string{"../secret.txt", secret} {
		raw, _ := json.Marshal(Input{Operation: OperationRead, Path: path})
		if _, _, err := executor.ParseAndValidate(raw); !errors.Is(err, ErrOutsideWorkspace) {
			t.Fatalf("expected traversal denial for %q, got %v", path, err)
		}
	}

	if runtime.GOOS == "windows" {
		t.Skip("symlink creation can require privileges on Windows")
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	_, err = execute(t, executor, Input{Operation: OperationRead, Path: "escape/secret.txt"})
	if err == nil {
		t.Fatal("read through an escaping symlink was allowed")
	}
	_, err = execute(t, executor, Input{Operation: OperationWrite, Path: "escape/created.txt", Content: "owned"})
	if err == nil {
		t.Fatal("write through an escaping symlink was allowed")
	}
	if _, err := os.Stat(filepath.Join(outside, "created.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escape write affected the outside directory: %v", err)
	}
}

func TestSizeLimitsSearchAndBoundedListing(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("needle one\nsecond needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "large.txt"), []byte(strings.Repeat("x", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "nested", "b.txt"), []byte("Needle nested\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor, err := NewWithConfig(Config{
		Workspaces: workspaceSlice(workspace), MaxReadBytes: 16, MaxWriteBytes: 16,
		MaxSearchFileBytes: 30, MaxSearchResults: 10, MaxSearchFiles: 10, MaxListEntries: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()

	_, err = execute(t, executor, Input{Operation: OperationRead, Path: "large.txt"})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected read size error, got %v", err)
	}
	_, err = execute(t, executor, Input{Operation: OperationWrite, Path: "new.txt", Content: strings.Repeat("y", 17)})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected write size error, got %v", err)
	}

	search := executeOK(t, executor, Input{Operation: OperationSearch, Path: ".", Query: "needle", Recursive: true, MaxResults: 10})
	if len(search.Matches) != 3 || search.FilesSkipped != 1 {
		t.Fatalf("unexpected search output: %+v", search)
	}
	listing := executeOK(t, executor, Input{Operation: OperationList, Path: ".", Recursive: true, MaxResults: 2})
	if len(listing.Entries) != 2 || !listing.Truncated {
		t.Fatalf("expected bounded list, got %+v", listing)
	}
}

func TestMoveDeleteDryRunAndClassification(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "source.txt"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor, err := New([]string{workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()

	dryMove := executeOK(t, executor, Input{Operation: OperationMove, Path: "source.txt", Destination: "target.txt", DryRun: true})
	if dryMove.SHA256 == "" || !dryMove.DryRun || dryMove.Risk != coretools.RiskStateChanging {
		t.Fatalf("unexpected dry move: %+v", dryMove)
	}
	if _, err := os.Stat(filepath.Join(workspace, "source.txt")); err != nil {
		t.Fatalf("dry move changed source: %v", err)
	}

	executeOK(t, executor, Input{Operation: OperationMove, Path: "source.txt", Destination: "target.txt"})
	dryDelete := executeOK(t, executor, Input{Operation: OperationDelete, Path: "target.txt", DryRun: true})
	if dryDelete.Risk != coretools.RiskDestructive || !dryDelete.ApprovalRequired || dryDelete.PreviousSHA256 == "" {
		t.Fatalf("unexpected dry delete: %+v", dryDelete)
	}
	executeOK(t, executor, Input{Operation: OperationDelete, Path: "target.txt"})
	if _, err := os.Stat(filepath.Join(workspace, "target.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete did not remove target: %v", err)
	}
}

func TestUnknownJSONFieldsAreRejected(t *testing.T) {
	executor, err := New([]string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	if _, _, err := executor.ParseAndValidate(json.RawMessage(`{"operation":"read","path":"x","argv":["../escape"]}`)); err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func executeOK(t *testing.T, executor *Executor, input Input) Output {
	t.Helper()
	output, err := execute(t, executor, input)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func execute(t *testing.T, executor *Executor, input Input) (Output, error) {
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

func workspaceSlice(path string) []string { return []string{path} }
