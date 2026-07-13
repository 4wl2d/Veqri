package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veqri/veqri/internal/managedcore"
)

func TestProbeVeqriCoreAcceptsCompatibleReadyServer(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/healthz":
			_, _ = writer.Write([]byte(`{"status":"ok","protocol_version":1}`))
		case "/readyz":
			_, _ = writer.Write([]byte(`{"status":"ready"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	if err := probeVeqriCore(context.Background(), server.URL, ""); err != nil {
		t.Fatalf("probeVeqriCore() returned error: %v", err)
	}
}

func TestProbeVeqriCoreRejectsForeignLoopbackService(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"ok","protocol_version":99}`))
	}))
	defer server.Close()

	err := probeVeqriCore(context.Background(), server.URL, "")
	if err == nil || !strings.Contains(err.Error(), "compatible Veqri Core") {
		t.Fatalf("probeVeqriCore() error = %v, want compatibility error", err)
	}
}

func TestProbeVeqriCoreDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"ok","protocol_version":1}`))
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	if err := probeVeqriCore(context.Background(), redirector.URL, ""); err == nil {
		t.Fatal("probeVeqriCore() followed a redirect")
	}
}

func TestWaitForVeqriCoreReturnsManagedProcessFailure(t *testing.T) {
	t.Parallel()

	done := make(chan error, 1)
	done <- errors.New("bind failed")
	close(done)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := waitForVeqriCore(ctx, "http://127.0.0.1:1", "owner-token", done)
	if err == nil || !strings.Contains(err.Error(), "bind failed") {
		t.Fatalf("waitForVeqriCore() error = %v, want managed process failure", err)
	}
}

func TestVerifyVeqriCoreCredentialNegotiatesAuthenticatedProtocol(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/protocol/negotiate" {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "Bearer local-token" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"selected":{"major":1,"minor":0}}`))
	}))
	defer server.Close()

	if err := verifyVeqriCoreCredential(context.Background(), server.URL, "local-token"); err != nil {
		t.Fatalf("verifyVeqriCoreCredential() returned error: %v", err)
	}
	if err := verifyVeqriCoreCredential(context.Background(), server.URL, "wrong-token"); err == nil {
		t.Fatal("verifyVeqriCoreCredential() accepted the wrong token")
	}
}

func TestManagedCoreEnvironmentPinsAddressAndWorkspace(t *testing.T) {
	environment, err := managedCoreEnvironment(
		[]string{"PATH=/tools", "VEQRI_ADDR=127.0.0.1:9999", "veqri_workspaces=/unsafe", managedcore.OwnerTokenEnvironment + "=untrusted"},
		"http://localhost:7342",
		"/safe/workspace",
		"owned-token",
	)
	if err != nil {
		t.Fatalf("managedCoreEnvironment() returned error: %v", err)
	}
	joined := strings.Join(environment, "\n")
	for _, want := range []string{
		"PATH=/tools",
		"VEQRI_ADDR=localhost:7342",
		"VEQRI_WORKSPACES=/safe/workspace",
		managedcore.OwnerTokenEnvironment + "=owned-token",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("managedCoreEnvironment() = %q, missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "9999") || strings.Contains(joined, "/unsafe") {
		t.Fatalf("managedCoreEnvironment() retained conflicting values: %q", joined)
	}
}

func TestManagedCoreEnvironmentRejectsSchemeTLSMismatch(t *testing.T) {
	t.Parallel()

	_, err := managedCoreEnvironment(
		[]string{"VEQRI_TLS_CERT_FILE=/cert.pem", "VEQRI_TLS_KEY_FILE=/key.pem"},
		"http://127.0.0.1:7342",
		"/workspace",
		"owner-token",
	)
	if err == nil || !strings.Contains(err.Error(), "managed HTTP Core") {
		t.Fatalf("managedCoreEnvironment(http with TLS) error = %v", err)
	}
	_, err = managedCoreEnvironment(nil, "https://127.0.0.1:7342", "/workspace", "owner-token")
	if err == nil || !strings.Contains(err.Error(), "managed HTTPS Core") {
		t.Fatalf("managedCoreEnvironment(https without TLS) error = %v", err)
	}
}

func TestManagedCoreEnvironmentMakesInheritedPathsAbsolute(t *testing.T) {
	t.Parallel()

	environment, err := managedCoreEnvironment(
		[]string{"VEQRI_DATA_DIR=relative-data", "VEQRI_DATABASE=relative-db/veqri.db"},
		"http://127.0.0.1:7342",
		"/workspace",
		"owner-token",
	)
	if err != nil {
		t.Fatalf("managedCoreEnvironment() returned error: %v", err)
	}
	joined := strings.Join(environment, "\n")
	for _, relative := range []string{"relative-data", filepath.Join("relative-db", "veqri.db")} {
		absolute, err := filepath.Abs(relative)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(joined, absolute) {
			t.Fatalf("managedCoreEnvironment() = %q, missing %q", joined, absolute)
		}
	}
}

func TestProbeVeqriCoreRequiresManagedOwnerToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/healthz" {
			writer.Header().Set(managedcore.OwnerTokenHeader, "different-owner-token")
			_, _ = writer.Write([]byte(`{"status":"ok","protocol_version":1}`))
			return
		}
		_, _ = writer.Write([]byte(`{"status":"ready"}`))
	}))
	defer server.Close()

	err := probeVeqriCore(context.Background(), server.URL, "expected-owner-token")
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("probeVeqriCore() error = %v, want ownership failure", err)
	}
}

func TestManagedModeRefusesUnownedExistingCore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/healthz" {
			_, _ = writer.Write([]byte(`{"status":"ok","protocol_version":1}`))
			return
		}
		_, _ = writer.Write([]byte(`{"status":"ready"}`))
	}))
	defer server.Close()
	t.Setenv("VEQRI_URL", server.URL)
	t.Setenv(desktopCoreModeEnvironment, managedCoreMode)

	bridge := &Bridge{}
	bridge.Startup(context.Background())
	bridge.mu.Lock()
	err := bridge.startupErr
	bridge.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), desktopCoreModeEnvironment+"=external") {
		t.Fatalf("Bridge.Startup() error = %v, want explicit external-mode requirement", err)
	}
}

func TestLoadAdminTokenHonorsKeychainDisabledFallback(t *testing.T) {
	dataDirectory := t.TempDir()
	t.Setenv("VEQRI_KEYCHAIN_DISABLED", "true")
	t.Setenv("VEQRI_DATA_DIR", dataDirectory)
	t.Setenv("VEQRI_AUTH_TOKEN", "")
	const token = "desktop-test-token-0123456789abcdef"
	if err := os.WriteFile(filepath.Join(dataDirectory, "admin.token"), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadAdminToken()
	if err != nil {
		t.Fatalf("loadAdminToken() returned error: %v", err)
	}
	if got != token {
		t.Fatalf("loadAdminToken() = %q, want %q", got, token)
	}
}

func TestManagedWorkspacePreservesConfiguredWorkspaceList(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	t.Setenv("VEQRI_WORKSPACES", strings.Join([]string{first, second}, string(os.PathListSeparator)))

	got, err := managedWorkspace()
	if err != nil {
		t.Fatalf("managedWorkspace() returned error: %v", err)
	}
	if got.directory != first {
		t.Fatalf("managedWorkspace().directory = %q, want %q", got.directory, first)
	}
	wantValue := strings.Join([]string{first, second}, string(os.PathListSeparator))
	if got.environmentValue != wantValue {
		t.Fatalf("managedWorkspace().environmentValue = %q, want %q", got.environmentValue, wantValue)
	}
}

func TestBoundedTextBufferKeepsLatestCoreErrorOutput(t *testing.T) {
	t.Parallel()

	buffer := &boundedTextBuffer{limit: 6}
	_, _ = buffer.Write([]byte("12345"))
	_, _ = buffer.Write([]byte("67890"))
	if got := buffer.String(); got != "567890" {
		t.Fatalf("boundedTextBuffer.String() = %q, want %q", got, "567890")
	}
}

func TestManagedCoreExitIsFailStopOnlyAfterReadiness(t *testing.T) {
	t.Parallel()

	early := &Bridge{ctx: context.Background()}
	_, shouldQuit := early.recordManagedCoreExit(errors.New("startup failed"))
	if shouldQuit {
		t.Fatal("early managed Core failure requested UI shutdown")
	}
	if err := early.markCoreReady(); err == nil || !strings.Contains(err.Error(), "startup failed") {
		t.Fatalf("markCoreReady() error = %v, want captured startup failure", err)
	}

	late := &Bridge{ctx: context.Background(), ready: true}
	_, shouldQuit = late.recordManagedCoreExit(errors.New("late crash"))
	if !shouldQuit {
		t.Fatal("late managed Core crash did not request fail-stop UI shutdown")
	}
}

func TestManagedCoreModeRequiresSupervisorOwnerToken(t *testing.T) {
	t.Setenv(managedcore.OwnerTokenEnvironment, "")
	if err := runManagedCore(); err == nil || !strings.Contains(err.Error(), managedcore.OwnerTokenEnvironment) {
		t.Fatalf("runManagedCore() error = %v, want missing owner token", err)
	}
}
