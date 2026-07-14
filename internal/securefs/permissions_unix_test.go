//go:build !windows

package securefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivatePermissionsAreExactAndRepairLoosePaths(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "private")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(directory); err != nil {
		t.Fatalf("EnsurePrivateDir(): %v", err)
	}
	assertMode(t, directory, PrivateDirMode)

	file := filepath.Join(directory, "secret")
	if err := os.WriteFile(file, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateFile(file); err != nil {
		t.Fatalf("EnsurePrivateFile(): %v", err)
	}
	assertMode(t, file, PrivateFileMode)

	written := filepath.Join(directory, "written")
	if err := WriteNewPrivateFile(written, []byte("secret")); err != nil {
		t.Fatalf("WriteNewPrivateFile(): %v", err)
	}
	assertMode(t, written, PrivateFileMode)
}

func TestPrivatePathsRejectSymlinksWithoutChangingTargets(t *testing.T) {
	root := t.TempDir()
	targetDirectory := filepath.Join(root, "target-directory")
	if err := os.Mkdir(targetDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	directoryLink := filepath.Join(root, "directory-link")
	if err := os.Symlink(targetDirectory, directoryLink); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(directoryLink); err == nil {
		t.Fatal("EnsurePrivateDir accepted a symlink")
	}
	if err := SyncDir(directoryLink); err == nil {
		t.Fatal("SyncDir accepted a symlink")
	}
	assertMode(t, targetDirectory, 0o755)

	targetFile := filepath.Join(root, "target-file")
	if err := os.WriteFile(targetFile, []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(root, "file-link")
	if err := os.Symlink(targetFile, fileLink); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateFile(fileLink); err == nil {
		t.Fatal("EnsurePrivateFile accepted a symlink")
	}
	if err := WritePrivateFile(fileLink, []byte("changed")); err == nil {
		t.Fatal("WritePrivateFile accepted a symlink")
	}
	contents, err := os.ReadFile(targetFile)
	if err != nil || string(contents) != "unchanged" {
		t.Fatalf("symlink target contents = %q, error=%v", contents, err)
	}
	assertMode(t, targetFile, 0o644)
}

func TestPrivateTreeRepairsExistingArtifactModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(nested, "legacy.db")
	if err := os.WriteFile(artifact, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateTreeIfExists(root); err != nil {
		t.Fatalf("EnsurePrivateTreeIfExists(): %v", err)
	}
	assertMode(t, root, PrivateDirMode)
	assertMode(t, nested, PrivateDirMode)
	assertMode(t, artifact, PrivateFileMode)
}

func TestPrivateTreeRejectsNestedSymlink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "redirect")); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateTreeIfExists(root); err == nil {
		t.Fatal("EnsurePrivateTreeIfExists accepted a nested symlink")
	}
	assertMode(t, target, 0o644)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want.Perm() {
		t.Fatalf("%s permissions = %#o, want %#o", path, got, want.Perm())
	}
}
