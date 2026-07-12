package auth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeDeviceVerifier struct {
	wantCredential string
	deviceID       string
	err            error
	calls          int
}

func (v *fakeDeviceVerifier) VerifyDeviceCredential(_ context.Context, credential string) (string, error) {
	v.calls++
	if v.err != nil {
		return "", v.err
	}
	if credential != v.wantCredential {
		return "", errors.New("invalid device credential")
	}
	return v.deviceID, nil
}

func TestLoadOrCreateAdminTokenCreatesPrivateReusableFile(t *testing.T) {
	t.Setenv("VEQRI_KEYCHAIN_DISABLED", "true")
	dataDir := filepath.Join(t.TempDir(), "nested", "data")
	tokenFile := filepath.Join(dataDir, "admin.token")
	token, path, err := LoadOrCreateAdminToken(dataDir, "")
	if err != nil {
		t.Fatalf("LoadOrCreateAdminToken(create): %v", err)
	}
	if len(token) < 32 || !strings.Contains(path, tokenFile) {
		t.Fatalf("token/path = (%q, %q)", token, path)
	}
	info, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("token permissions = %#o, want 0600", got)
	}
	contents, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	if string(contents) != token+"\n" {
		t.Fatal("token file content does not match returned token")
	}

	if err := os.Chmod(tokenFile, 0o644); err != nil {
		t.Fatalf("loosen token permissions for reload test: %v", err)
	}
	reloaded, reloadedPath, err := LoadOrCreateAdminToken(dataDir, "")
	if err != nil {
		t.Fatalf("LoadOrCreateAdminToken(reload): %v", err)
	}
	if reloaded != token || !strings.Contains(reloadedPath, tokenFile) {
		t.Fatalf("reloaded token/path = (%q, %q), want original", reloaded, reloadedPath)
	}
	info, err = os.Stat(tokenFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("reload did not restore 0600 permissions: info=%v err=%v", info, err)
	}
}

func TestLoadOrCreateAdminTokenValidatesConfiguredAndExistingTokens(t *testing.T) {
	t.Setenv("VEQRI_KEYCHAIN_DISABLED", "true")
	if _, _, err := LoadOrCreateAdminToken(t.TempDir(), "too-short"); err == nil {
		t.Fatal("short configured admin token was accepted")
	}
	configured := "0123456789abcdef0123456789abcdef"
	token, path, err := LoadOrCreateAdminToken(t.TempDir(), configured)
	if err != nil || token != configured || path != "environment" {
		t.Fatalf("configured token result = (%q, %q, %v)", token, path, err)
	}

	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "admin.token"), []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreateAdminToken(dataDir, ""); err == nil {
		t.Fatal("short persisted admin token was accepted")
	}
}

func TestCredentialHashingAndPairingCodes(t *testing.T) {
	if EqualToken("token-a", "token-b") {
		t.Fatal("different tokens compared equal")
	}
	if !EqualToken("token-a", "token-a") {
		t.Fatal("identical tokens did not compare equal")
	}
	hash := HashCredential("credential")
	if len(hash) != 32 || string(hash) == "credential" {
		t.Fatalf("unexpected credential hash %x", hash)
	}
	first := HashPairingCode("secret-a", "123456")
	if !bytes.Equal(first, HashPairingCode("secret-a", "123456")) {
		t.Fatal("pairing code hash is not deterministic")
	}
	if bytes.Equal(first, HashPairingCode("secret-b", "123456")) || bytes.Equal(first, HashPairingCode("secret-a", "654321")) {
		t.Fatal("pairing code hash did not bind both secret and code")
	}
}

func TestAuthenticatorDistinguishesAdminDeviceAndInvalidCredentials(t *testing.T) {
	devices := &fakeDeviceVerifier{wantCredential: "device-token", deviceID: "device-1"}
	authenticator := New("admin-token", devices)

	principal, err := authenticator.Authenticate(context.Background(), "admin-token")
	if err != nil || principal.Kind != "admin" || principal.ID != "local-admin" {
		t.Fatalf("admin Authenticate() = (%+v, %v)", principal, err)
	}
	if devices.calls != 0 {
		t.Fatal("device verifier was called for a valid admin token")
	}
	principal, err = authenticator.Authenticate(context.Background(), "device-token")
	if err != nil || principal.Kind != "device" || principal.ID != "device-1" {
		t.Fatalf("device Authenticate() = (%+v, %v)", principal, err)
	}
	if _, err := authenticator.Authenticate(context.Background(), "wrong-token"); err == nil {
		t.Fatal("invalid credential was accepted")
	}
	if _, err := authenticator.Authenticate(context.Background(), ""); err == nil {
		t.Fatal("empty credential was accepted")
	}
}

func TestBearerTokenParsing(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{header: "Bearer abc", want: "abc"},
		{header: "bearer   abc  ", want: "abc"},
		{header: "Basic abc"},
		{header: "Bearer"},
		{header: ""},
	}
	for _, tt := range tests {
		if got := BearerToken(tt.header); got != tt.want {
			t.Errorf("BearerToken(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}
