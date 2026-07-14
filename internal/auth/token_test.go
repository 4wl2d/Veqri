package auth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
	assertPrivatePermissions(t, dataDir, 0o700)
	assertPrivatePermissions(t, tokenFile, 0o600)
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
	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatalf("loosen data directory permissions for reload test: %v", err)
	}
	reloaded, reloadedPath, err := LoadOrCreateAdminToken(dataDir, "")
	if err != nil {
		t.Fatalf("LoadOrCreateAdminToken(reload): %v", err)
	}
	if reloaded != token || !strings.Contains(reloadedPath, tokenFile) {
		t.Fatalf("reloaded token/path = (%q, %q), want original", reloaded, reloadedPath)
	}
	assertPrivatePermissions(t, dataDir, 0o700)
	assertPrivatePermissions(t, tokenFile, 0o600)
}

func TestReadAdminTokenRepairsFallbackPermissions(t *testing.T) {
	t.Setenv("VEQRI_AUTH_TOKEN", "")
	t.Setenv("VEQRI_KEYCHAIN_DISABLED", "true")
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, "admin.token")
	const token = "read-admin-token-0123456789abcdef"
	if err := os.WriteFile(path, []byte(token+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, source, err := ReadAdminToken(dataDir)
	if err != nil {
		t.Fatalf("ReadAdminToken(): %v", err)
	}
	if got != token || !strings.Contains(source, path) {
		t.Fatalf("ReadAdminToken() = (%q, %q), want token and fallback path", got, source)
	}
	assertPrivatePermissions(t, dataDir, 0o700)
	assertPrivatePermissions(t, path, 0o600)
}

func TestLoadOrCreateAdminTokenValidatesConfiguredAndExistingTokens(t *testing.T) {
	t.Setenv("VEQRI_KEYCHAIN_DISABLED", "true")
	if _, _, err := LoadOrCreateAdminToken(t.TempDir(), "too-short"); err == nil {
		t.Fatal("short configured admin token was accepted")
	}
	configuredDataDir := t.TempDir()
	if err := os.Chmod(configuredDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fallbackPath := filepath.Join(configuredDataDir, "admin.token")
	if err := os.WriteFile(fallbackPath, []byte("old-fallback-token-0123456789abcdef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configured := "0123456789abcdef0123456789abcdef"
	token, path, err := LoadOrCreateAdminToken(configuredDataDir, configured)
	if err != nil || token != configured || path != "environment" {
		t.Fatalf("configured token result = (%q, %q, %v)", token, path, err)
	}
	assertPrivatePermissions(t, configuredDataDir, 0o700)
	assertPrivatePermissions(t, fallbackPath, 0o600)

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

func assertPrivatePermissions(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want.Perm() {
		t.Fatalf("%s permissions = %#o, want %#o", path, got, want.Perm())
	}
}
