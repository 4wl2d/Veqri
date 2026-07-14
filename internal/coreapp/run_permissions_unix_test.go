//go:build !windows

package coreapp

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veqri/veqri/internal/buildinfo"
)

func TestRunDoesNotTouchDataDirectoryBeforeListenerReservation(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	dataDirectory := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VEQRI_ADDR", occupied.Addr().String())
	t.Setenv("VEQRI_AUTH_TOKEN", "core-listener-order-token-0123456789abcdef")
	t.Setenv("VEQRI_DATA_DIR", dataDirectory)
	t.Setenv("VEQRI_DATABASE", ":memory:")
	t.Setenv("VEQRI_REMOTE_AGENT_ENDPOINT", "")
	t.Setenv("VEQRI_REMOTE_AGENT_TOKEN_REF", "")

	err = Run(context.Background(), buildinfo.Development())
	if err == nil || !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("Run() error = %v, want listener collision", err)
	}
	info, statErr := os.Stat(dataDirectory)
	if statErr != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("DataDir changed before listener reservation: info=%v error=%v", info, statErr)
	}
}

func TestRunSecuresDataDirectoryWithConfiguredTokenAndMemoryDatabase(t *testing.T) {
	root := t.TempDir()
	dataDirectory := filepath.Join(root, "data")
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(dataDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyArtifacts := []string{
		filepath.Join(dataDirectory, "backups", "legacy.db"),
		filepath.Join(dataDirectory, "diagnostics", "legacy.json"),
	}
	for _, path := range legacyArtifacts {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("legacy"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VEQRI_ADDR", address)
	t.Setenv("VEQRI_AUTH_TOKEN", "core-data-permissions-token-0123456789abcdef")
	t.Setenv("VEQRI_DATA_DIR", dataDirectory)
	t.Setenv("VEQRI_DATABASE", ":memory:")
	t.Setenv("VEQRI_KEYCHAIN_DISABLED", "true")
	t.Setenv("VEQRI_WORKSPACES", workspace)
	t.Setenv("VEQRI_REMOTE_AGENT_ENDPOINT", "")
	t.Setenv("VEQRI_REMOTE_AGENT_TOKEN_REF", "")
	t.Setenv("VEQRI_STDIO_AGENT_COMMAND", "")
	t.Setenv("VEQRI_STDIO_AGENT_ARGS_JSON", "")

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- Run(ctx, buildinfo.Development()) }()

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	for {
		response, requestErr := client.Get("http://" + address + "/healthz")
		if requestErr == nil {
			_ = response.Body.Close()
		}
		if requestErr == nil && response.StatusCode == http.StatusOK {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("Run exited before becoming healthy: %v", err)
		case <-deadline.C:
			t.Fatalf("Core did not become healthy: %v", requestErr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	info, err := os.Stat(dataDirectory)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("DataDir permissions were not repaired to 0700: info=%v error=%v", info, err)
	}
	for _, path := range legacyArtifacts {
		artifactInfo, err := os.Stat(path)
		if err != nil || artifactInfo.Mode().Perm() != 0o600 {
			t.Fatalf("legacy artifact permissions were not repaired to 0600: path=%s info=%v error=%v",
				path, artifactInfo, err)
		}
		directoryInfo, err := os.Stat(filepath.Dir(path))
		if err != nil || directoryInfo.Mode().Perm() != 0o700 {
			t.Fatalf("legacy artifact directory permissions were not repaired to 0700: path=%s info=%v error=%v",
				filepath.Dir(path), directoryInfo, err)
		}
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}
