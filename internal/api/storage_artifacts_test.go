package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/internal/config"
)

func TestDiagnosticsExportsAreRapidlyUniquePrivateFiles(t *testing.T) {
	ctx := context.Background()
	dataDirectory := filepath.Join(t.TempDir(), "state")
	diagnosticsDirectory := filepath.Join(dataDirectory, "diagnostics")
	if err := os.MkdirAll(dataDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(dataDirectory, "veqri.db")
	store, err := persistence.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := &Server{config: config.Config{
		DataDir: dataDirectory, DatabasePath: databasePath, Address: "127.0.0.1:7342",
		MediaTransport: "simulated", STTProvider: "mock", TTSProvider: "mock",
	}, store: store}

	namePattern := regexp.MustCompile(`^veqri-diagnostics-[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[0-9a-f-]{36}\.json$`)
	seen := make(map[string]struct{})
	for index := 0; index < 12; index++ {
		path, err := server.createDiagnosticsExport(ctx, true)
		if err != nil {
			t.Fatalf("createDiagnosticsExport(%d): %v", index, err)
		}
		if filepath.Dir(path) != diagnosticsDirectory || !namePattern.MatchString(filepath.Base(path)) {
			t.Fatalf("diagnostics path = %q", path)
		}
		if _, exists := seen[path]; exists {
			t.Fatalf("diagnostics path reused: %q", path)
		}
		seen[path] = struct{}{}
		assertAPIPrivateMode(t, path, 0o600)

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Redacted    bool                    `json:"redacted"`
			Diagnostics persistence.Diagnostics `json:"diagnostics"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode diagnostics %q: %v", path, err)
		}
		if !payload.Redacted || !payload.Diagnostics.DatabaseOK {
			t.Fatalf("unexpected diagnostics payload: %+v", payload)
		}
	}
	assertAPIPrivateMode(t, diagnosticsDirectory, 0o700)
}

func assertAPIPrivateMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != want {
		t.Fatalf("mode for %q = %04o, want %04o", path, info.Mode().Perm(), want)
	}
}
