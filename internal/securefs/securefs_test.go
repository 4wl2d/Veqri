package securefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsurePrivateDirCreatesNestedDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "private")
	if err := EnsurePrivateDir(path); err != nil {
		t.Fatalf("EnsurePrivateDir(): %v", err)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("private directory info=%v error=%v", info, err)
	}
}

func TestEnsurePrivateDirDurableCreatesNestedDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "durable")
	if err := EnsurePrivateDirDurable(path); err != nil {
		t.Fatalf("EnsurePrivateDirDurable(): %v", err)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("durable private directory info=%v error=%v", info, err)
	}
}

func TestEnsurePrivateDirRejectsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(path); err == nil {
		t.Fatal("EnsurePrivateDir accepted a regular file")
	}
}

func TestEnsurePrivateFileRejectsMissingAndNonRegularPaths(t *testing.T) {
	root := t.TempDir()
	if err := EnsurePrivateFile(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error = %v, want os.ErrNotExist", err)
	}
	if err := EnsurePrivateFile(root); err == nil {
		t.Fatal("EnsurePrivateFile accepted a directory")
	}
	if err := EnsurePrivateFileIfExists(filepath.Join(root, "missing")); err != nil {
		t.Fatalf("EnsurePrivateFileIfExists(missing): %v", err)
	}
}

func TestEnsurePrivateTreeIfExistsAllowsMissingTree(t *testing.T) {
	if err := EnsurePrivateTreeIfExists(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("EnsurePrivateTreeIfExists(missing): %v", err)
	}
}

func TestPrivateFileWritersCreateReplaceAndExclude(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private.json")
	if err := WritePrivateFile(path, []byte("one")); err != nil {
		t.Fatalf("WritePrivateFile(create): %v", err)
	}
	if err := WritePrivateFile(path, []byte("two")); err != nil {
		t.Fatalf("WritePrivateFile(replace): %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "two" {
		t.Fatalf("private contents = %q, error=%v", got, err)
	}

	exclusive := filepath.Join(root, "new.json")
	if err := WriteNewPrivateFile(exclusive, []byte("durable")); err != nil {
		t.Fatalf("WriteNewPrivateFile(): %v", err)
	}
	if err := WriteNewPrivateFile(exclusive, []byte("replacement")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second WriteNewPrivateFile() error = %v, want os.ErrExist", err)
	}
	if got, err := os.ReadFile(exclusive); err != nil || string(got) != "durable" {
		t.Fatalf("exclusive contents = %q, error=%v", got, err)
	}
}

func TestSyncDirValidatesPath(t *testing.T) {
	root := t.TempDir()
	if err := SyncDir(root); err != nil {
		t.Fatalf("SyncDir(directory): %v", err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(file); err == nil {
		t.Fatal("SyncDir accepted a regular file")
	}
}
