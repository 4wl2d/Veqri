package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/internal/buildinfo"
	"github.com/veqri/veqri/internal/config"
	"github.com/veqri/veqri/internal/managedcore"
)

func TestHealthReturnsManagedCoreOwnershipProof(t *testing.T) {
	info, err := buildinfo.Parse("1.2.3-rc.1", "0123456789abcdef0123456789abcdef01234567", "2026-07-14T12:34:56Z")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		config:    config.Config{ManagedCoreOwnerToken: "managed-owner-token-0123456789abcdef"},
		buildInfo: info,
		startedAt: time.Now(),
	}
	response := httptest.NewRecorder()
	server.handleHealth(response, httptest.NewRequest("GET", "/healthz", nil))
	want := managedcore.OwnerProof(server.config.ManagedCoreOwnerToken)
	if got := response.Header().Get(managedcore.OwnerTokenHeader); got != want {
		t.Fatalf("managed owner header = %q, want %q", got, want)
	}
	assertBuildInfoResponse(t, response.Body.Bytes(), "ok", info)
}

func TestReadyReturnsBuildInfo(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	info := buildinfo.Development()
	server := &Server{store: store, buildInfo: info}
	response := httptest.NewRecorder()
	server.handleReady(response, httptest.NewRequest("GET", "/readyz", nil))
	assertBuildInfoResponse(t, response.Body.Bytes(), "ready", info)
}

func assertBuildInfoResponse(t *testing.T, raw []byte, status string, want buildinfo.Info) {
	t.Helper()
	var response struct {
		Status string `json:"status"`
		buildinfo.Info
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Status != status || response.Info != want {
		t.Fatalf("response = %+v, want status %q and build info %+v", response, status, want)
	}
}

func TestDecodeJSONRequiresExactlyOneValue(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		valid bool
	}{
		{name: "single value", body: `{"name":"veqri"}`, valid: true},
		{name: "single value with whitespace", body: "{\"name\":\"veqri\"}\n\t", valid: true},
		{name: "second object", body: `{"name":"veqri"} {}`, valid: false},
		{name: "trailing garbage", body: `{"name":"veqri"} trailing`, valid: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/", strings.NewReader(tt.body))
			response := httptest.NewRecorder()
			var decoded struct {
				Name string `json:"name"`
			}
			valid := decodeJSON(response, request, &decoded)
			if valid != tt.valid {
				t.Fatalf("decodeJSON() = %v, status=%d body=%s", valid, response.Code, response.Body.String())
			}
			if tt.valid && decoded.Name != "veqri" {
				t.Fatalf("decoded name = %q", decoded.Name)
			}
			if !tt.valid && response.Code != 400 {
				t.Fatalf("status = %d, want 400", response.Code)
			}
		})
	}
}

func TestLocalOriginIncludesOnlyLoopbackAndPackagedWailsOrigins(t *testing.T) {
	allowed := []string{
		"http://localhost:5173", "https://127.0.0.1:8443", "wails://wails",
		"http://wails.localhost",
	}
	for _, origin := range allowed {
		if !localOrigin(origin) {
			t.Errorf("localOrigin(%q) = false, want true", origin)
		}
	}
	denied := []string{
		"https://example.com", "http://wails.localhost.evil.example", "wails://evil",
		"http://localhost.evil.example", "http://localhost:5173/path",
	}
	for _, origin := range denied {
		if localOrigin(origin) {
			t.Errorf("localOrigin(%q) = true, want false", origin)
		}
	}
}

func TestSupportedProtocolRequiresExplicitV1(t *testing.T) {
	tests := map[string]bool{
		"": false, "0": false, "2": false, "1.": false, "1.foo": false,
		"1.2garbage": false, "1.2.3": false, "1.-1": false, " 1": false,
		"1": true, "1.0": true, "1.7": true, "1.000": true,
	}
	for value, wanted := range tests {
		if got := supportedProtocol(value); got != wanted {
			t.Errorf("supportedProtocol(%q) = %v, want %v", value, got, wanted)
		}
	}
}

func TestHasWebSocketProtocolChecksEveryHeaderValue(t *testing.T) {
	if !hasWebSocketProtocol([]string{"veqri.auth.token", "veqri.v1"}, "veqri.v1") {
		t.Fatal("protocol in a repeated header value was not found")
	}
	if !hasWebSocketProtocol([]string{"veqri.auth.token, veqri.v1"}, "veqri.v1") {
		t.Fatal("protocol in a comma-separated header value was not found")
	}
	if hasWebSocketProtocol([]string{"veqri.auth.token", "veqri.v2"}, "veqri.v1") {
		t.Fatal("missing protocol was reported as present")
	}
}

func TestRetentionCutoffUsesUTCCalendarDaysAndZeroDisablesSweeps(t *testing.T) {
	now := time.Date(2026, time.March, 31, 23, 45, 0, 0, time.FixedZone("test", 2*60*60))
	if cutoff, enabled := retentionCutoff(now, 0); enabled || !cutoff.IsZero() {
		t.Fatalf("retentionCutoff(days=0) = (%v, %v), want disabled", cutoff, enabled)
	}
	cutoff, enabled := retentionCutoff(now, 30)
	if !enabled {
		t.Fatal("positive retention unexpectedly disabled")
	}
	want := now.UTC().AddDate(0, 0, -30)
	if !cutoff.Equal(want) || cutoff.Location() != time.UTC {
		t.Fatalf("retentionCutoff() = %v, want %v UTC", cutoff, want)
	}
}
