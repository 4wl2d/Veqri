package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/internal/auth"
)

func TestDeviceSocketCredentialRevalidationDistinguishesRotationAndRevocation(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, filepath.Join(t.TempDir(), "socket-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const (
		deviceID      = "socket-device"
		oldCredential = "socket-old-credential"
		newCredential = "socket-new-credential"
	)
	codeHash := auth.HashPairingCode("admin-token", "12345678")
	if err := store.CreatePairingSession(ctx, "socket-pairing", codeHash, time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimPairingSession(ctx, codeHash, persistence.Device{
		ID: deviceID, Name: "Socket phone", Platform: "android", Capabilities: `{}`,
	}, oldCredential); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, authenticator: auth.New("admin-token", store)}
	if status, _ := server.deviceSocketCredentialCloseStatus(ctx, deviceID, oldCredential); status != 0 {
		t.Fatalf("active credential close status = %d", status)
	}
	prepared, err := store.PrepareDeviceCredentialRotation(ctx, deviceID, oldCredential, newCredential,
		"socket-prepare-audit", "socket-rotation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmDeviceCredentialRotation(ctx, deviceID, newCredential, prepared.KeyVersion,
		"socket-confirm-audit", "socket-rotation"); err != nil {
		t.Fatal(err)
	}
	if status, _ := server.deviceSocketCredentialCloseStatus(ctx, deviceID, oldCredential); status != websocket.StatusCode(4004) {
		t.Fatalf("superseded credential close status = %d, want 4004", status)
	}
	if status, _ := server.deviceSocketCredentialCloseStatus(ctx, deviceID, newCredential); status != 0 {
		t.Fatalf("promoted credential close status = %d", status)
	}
	if err := store.RevokeDevice(ctx, deviceID); err != nil {
		t.Fatal(err)
	}
	if status, _ := server.deviceSocketCredentialCloseStatus(ctx, deviceID, newCredential); status != websocket.StatusCode(4003) {
		t.Fatalf("revoked credential close status = %d, want 4003", status)
	}
}
