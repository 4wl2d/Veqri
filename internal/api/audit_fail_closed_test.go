package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/core/policy"
	"github.com/veqri/veqri/internal/auth"
	"github.com/veqri/veqri/tools/shell"
)

func TestShellRequestFailsClosedWhenPolicyAuditIsUnavailable(t *testing.T) {
	store := openAuditAPITestStore(t)
	workspace := t.TempDir()
	executor, err := shell.New([]string{workspace}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(context.Background(), `CREATE TRIGGER reject_policy_audit
BEFORE INSERT ON audit_entries WHEN NEW.action = 'policy.evaluate'
BEGIN SELECT RAISE(FAIL, 'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, shell: executor, policy: policy.NewEngine()}
	body := `{"input":{"command":"pwd","args":[],"working_directory":` + quotedJSON(workspace) + `,"dry_run":true}}`
	request := httptest.NewRequest(http.MethodPost, "/v1/tools/shell", strings.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{},
		auth.Principal{Kind: "admin", ID: "test-admin"}))
	response := httptest.NewRecorder()

	server.handleShell(response, request)

	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "audit_unavailable") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	var tasks int
	if err := store.DB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM tasks").Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if tasks != 0 {
		t.Fatalf("created %d tool task(s) despite unavailable policy audit", tasks)
	}
}

func TestEmergencyStopDoesNotChangeMemoryOrDiskWhenAuditFails(t *testing.T) {
	store := openAuditAPITestStore(t)
	ctx := context.Background()
	if err := store.SetSetting(ctx, "emergency_stop", false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `CREATE TRIGGER reject_emergency_audit
BEFORE INSERT ON audit_entries WHEN NEW.action = 'core.emergency_stop.set'
BEGIN SELECT RAISE(FAIL, 'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	engine := policy.NewEngine()
	server := &Server{store: store, policy: engine}
	request := httptest.NewRequest(http.MethodPost, "/v1/emergency-stop", strings.NewReader(`{"enabled":true}`))
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{},
		auth.Principal{Kind: "admin", ID: "test-admin"}))
	response := httptest.NewRecorder()

	server.handleEmergencyStop(response, request)

	if response.Code != http.StatusServiceUnavailable || engine.EmergencyStop() {
		t.Fatalf("response=%d memory_stop=%v body=%s", response.Code, engine.EmergencyStop(), response.Body.String())
	}
	var persisted bool
	if err := store.GetSetting(ctx, "emergency_stop", &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted {
		t.Fatal("emergency stop persisted despite audit failure")
	}
}

func TestDesktopDeviceRevokeFailsClosedWhenAuditIsUnavailable(t *testing.T) {
	store := openAuditAPITestStore(t)
	ctx := context.Background()
	const credential = "desktop-revoke-audit-credential"
	codeHash := auth.HashPairingCode("admin", "12345678")
	if err := store.CreatePairingSession(ctx, "desktop-revoke-pairing", codeHash, time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimPairingSession(ctx, codeHash, persistence.Device{
		ID: "desktop-revoke-device", Name: "Desktop revoke phone", Platform: "android", Capabilities: `{}`,
	}, credential); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `CREATE TRIGGER reject_desktop_revoke_audit
BEFORE INSERT ON audit_entries WHEN NEW.action = 'device.revoked'
BEGIN SELECT RAISE(FAIL, 'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store}
	_, _, actionErr := server.performDesktopAction(ctx, "desktop-revoke-request",
		json.RawMessage(`{"type":"device.revoke","device_id":"desktop-revoke-device"}`))
	if actionErr == nil {
		t.Fatal("desktop revocation committed without mandatory audit")
	}
	if deviceID, err := store.VerifyDeviceCredential(ctx, credential); err != nil || deviceID != "desktop-revoke-device" {
		t.Fatalf("desktop audit failure revoked credential: device=%q error=%v", deviceID, err)
	}
}

func openAuditAPITestStore(t *testing.T) *persistence.Store {
	t.Helper()
	store, err := persistence.Open(context.Background(), filepath.Join(t.TempDir(), "state", "veqri.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func quotedJSON(value string) string {
	result := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
	return `"` + result + `"`
}
