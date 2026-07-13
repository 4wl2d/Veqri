package coreapp

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunReservesListenerBeforeCreatingDatabase(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	dataDirectory := t.TempDir()
	databasePath := filepath.Join(dataDirectory, "must-not-exist.db")
	workspace := filepath.Join(dataDirectory, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VEQRI_ADDR", occupied.Addr().String())
	t.Setenv("VEQRI_AUTH_TOKEN", "core-listener-test-token-0123456789abcdef")
	t.Setenv("VEQRI_DATA_DIR", dataDirectory)
	t.Setenv("VEQRI_DATABASE", databasePath)
	t.Setenv("VEQRI_KEYCHAIN_DISABLED", "true")
	t.Setenv("VEQRI_WORKSPACES", workspace)

	err = Run(context.Background(), "test")
	if err == nil || !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("Run() error = %v, want listener collision", err)
	}
	if _, statErr := os.Stat(databasePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("database was touched before listener reservation: %v", statErr)
	}
}
