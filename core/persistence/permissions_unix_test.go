//go:build !windows

package persistence

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCreatesPrivateDatabaseAndRepairsExistingArtifacts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "veqri.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open(first): %v", err)
	}
	defer first.Close()

	artifacts := []string{path, path + "-wal", path + "-shm"}
	for _, artifact := range artifacts {
		if _, err := os.Stat(artifact); err != nil {
			t.Fatalf("expected database artifact %q: %v", artifact, err)
		}
		assertDatabaseMode(t, artifact, 0o600)
		if err := os.Chmod(artifact, 0o644); err != nil {
			t.Fatalf("loosen %q: %v", artifact, err)
		}
	}

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open(second): %v", err)
	}
	defer second.Close()
	for _, artifact := range artifacts {
		assertDatabaseMode(t, artifact, 0o600)
	}
}

func assertDatabaseMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want.Perm() {
		t.Fatalf("%s permissions = %#o, want %#o", path, got, want.Perm())
	}
}
