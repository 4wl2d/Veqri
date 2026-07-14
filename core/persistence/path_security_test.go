package persistence

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenRejectsSQLiteFileURIsBeforeTouchingStorage(t *testing.T) {
	target := filepath.Join(t.TempDir(), "uri-target.db")
	for _, path := range []string{
		"file:" + filepath.ToSlash(target) + "?mode=rwc",
		"file::memory:?cache=shared",
	} {
		store, err := Open(context.Background(), path)
		if store != nil {
			_ = store.Close()
			t.Fatalf("Open(%q) returned a store", path)
		}
		if err == nil || !strings.Contains(err.Error(), "file URI") {
			t.Fatalf("Open(%q) error = %v, want unsupported file URI", path, err)
		}
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("URI target was touched: %v", err)
	}
}

func TestOpenRejectsQueryBearingPathBeforeTouchingDriverTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "query-target.db")
	want := []byte("must remain untouched")
	if err := os.WriteFile(target, want, 0o644); err != nil {
		t.Fatal(err)
	}
	literalDSN := target + "?_pragma=journal_mode(WAL)"
	store, err := Open(context.Background(), literalDSN)
	if store != nil {
		_ = store.Close()
		t.Fatal("query-bearing path returned a store")
	}
	if err == nil || !strings.Contains(err.Error(), "query parameters") {
		t.Fatalf("Open(query-bearing path) error = %v, want unsupported query parameters", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil || string(got) != string(want) {
		t.Fatalf("driver target contents = %q, error=%v", got, readErr)
	}
	if _, statErr := os.Lstat(literalDSN); !os.IsNotExist(statErr) {
		t.Fatalf("literal query-bearing path was touched: %v", statErr)
	}
}
