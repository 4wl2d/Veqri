package integration_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/veqri/veqri/core/events"
	"github.com/veqri/veqri/tests/integration/testfixture"
)

func TestAdminOnlyHTTPRoutesRejectPairedDevicesAndReachHandlersForAdmin(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{NoWorkers: true})
	device := fixture.PairAndroid(t, "Security boundary phone")

	tests := []struct {
		name        string
		method      string
		path        string
		body        any
		adminStatus int
	}{
		{name: "metrics", method: http.MethodGet, path: "/metrics", adminStatus: http.StatusOK},
		{name: "devices", method: http.MethodGet, path: "/v1/devices", adminStatus: http.StatusOK},
		{
			name: "events", method: http.MethodPost, path: "/v1/events",
			body: map[string]any{
				"type": "security.boundary.test", "data": map[string]any{"ok": true},
				"idempotency_key": "security-boundary-event",
			},
			adminStatus: http.StatusAccepted,
		},
		{
			name: "shell", method: http.MethodPost, path: "/v1/tools/shell",
			body: map[string]any{
				"input": map[string]any{
					"command": "mkdir", "args": []string{"security-boundary-never-executed"},
					"working_directory": fixture.Workspace, "timeout_seconds": 2,
				},
				"idempotency_key": "security-boundary-shell",
			},
			adminStatus: http.StatusAccepted,
		},
		{
			name: "outbound call", method: http.MethodPost, path: "/v1/voice/calls",
			body: map[string]any{"device_id": "missing-device"}, adminStatus: http.StatusNotFound,
		},
		{name: "audit", method: http.MethodGet, path: "/v1/audit", adminStatus: http.StatusOK},
		{name: "diagnostics", method: http.MethodGet, path: "/v1/diagnostics", adminStatus: http.StatusOK},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deviceResponse := fixture.JSON(t, test.method, test.path, device.Credential, test.body, nil)
			testfixture.RequireStatus(t, deviceResponse, http.StatusForbidden)

			adminResponse := fixture.JSON(t, test.method, test.path, fixture.AdminToken, test.body, nil)
			testfixture.RequireStatus(t, adminResponse, test.adminStatus)
		})
	}

	for _, path := range []string{"/v1/tasks", "/v1/approvals"} {
		response := fixture.JSON(t, http.MethodGet, path, device.Credential, nil, nil)
		testfixture.RequireStatus(t, response, http.StatusOK)
	}
}

func TestDeviceCannotIssueLocalTrustEventsOrLowLevelToolWork(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{NoWorkers: true})
	device := fixture.PairAndroid(t, "Trust boundary phone")
	before := securityBoundaryCounts(t, fixture)

	eventResponse := fixture.JSON(t, http.MethodPost, "/v1/events", device.Credential, map[string]any{
		"type": "security.device.event", "data": map[string]any{"goal": "must not persist"},
		"idempotency_key": "security-device-event", "create_task": true,
	}, nil)
	testfixture.RequireStatus(t, eventResponse, http.StatusForbidden)

	toolResponse := fixture.JSON(t, http.MethodPost, "/v1/tools/shell", device.Credential, map[string]any{
		"input": map[string]any{
			"command": "pwd", "args": []string{}, "working_directory": fixture.Workspace,
			"dry_run": true,
		},
		"idempotency_key": "security-device-tool",
	}, nil)
	testfixture.RequireStatus(t, toolResponse, http.StatusForbidden)

	if after := securityBoundaryCounts(t, fixture); after != before {
		t.Fatalf("denied device requests changed durable state: before=%+v after=%+v", before, after)
	}

	adminResponse := fixture.JSON(t, http.MethodPost, "/v1/events", fixture.AdminToken, map[string]any{
		"type": "security.admin.event", "data": map[string]any{"ok": true},
		"idempotency_key": "security-admin-event",
	}, nil)
	testfixture.RequireStatus(t, adminResponse, http.StatusAccepted)
	receipt := testfixture.Decode[struct {
		EventID string `json:"event_id"`
	}](t, adminResponse)
	stored, err := fixture.Store.GetEvent(context.Background(), receipt.EventID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TrustLevel != events.TrustLocal || stored.Actor.ID != "local-admin" {
		t.Fatalf("admin local event trust/actor = %q/%q", stored.TrustLevel, stored.Actor.ID)
	}

	askResponse := fixture.JSON(t, http.MethodPost, "/v1/ask", device.Credential, map[string]any{
		"text": "persist a trusted owner request", "idempotency_key": "security-device-ask",
	}, nil)
	testfixture.RequireStatus(t, askResponse, http.StatusAccepted)
	askReceipt := testfixture.Decode[struct {
		Task struct {
			CausationID *string `json:"causation_id"`
		} `json:"task"`
	}](t, askResponse)
	if askReceipt.Task.CausationID == nil {
		t.Fatalf("device ask omitted causation event: %s", askResponse.Body)
	}
	trustedEvent, err := fixture.Store.GetEvent(context.Background(), *askReceipt.Task.CausationID)
	if err != nil {
		t.Fatal(err)
	}
	if trustedEvent.TrustLevel != events.TrustTrusted || trustedEvent.Actor.ID != device.ID {
		t.Fatalf("device ask trust/actor = %q/%q", trustedEvent.TrustLevel, trustedEvent.Actor.ID)
	}
}

type boundaryCounts struct {
	Events    int
	Tasks     int
	Approvals int
	Audits    int
}

func securityBoundaryCounts(t *testing.T, fixture *testfixture.Fixture) boundaryCounts {
	t.Helper()
	ctx := context.Background()
	var result boundaryCounts
	for table, target := range map[string]*int{
		"events": &result.Events, "tasks": &result.Tasks,
		"approvals": &result.Approvals, "audit_entries": &result.Audits,
	} {
		if err := fixture.Store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(target); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
	}
	return result
}

func TestAuthenticatedHTTPRoutesRequireExplicitProtocolHeader(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{NoWorkers: true})
	device := fixture.PairAndroid(t, "Protocol header phone")

	tests := []struct {
		name  string
		path  string
		token string
	}{
		{name: "device accessible route", path: "/v1/tasks", token: device.Credential},
		{name: "admin only route", path: "/metrics", token: fixture.AdminToken},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Supplying Authorization through custom headers avoids the fixture's
			// normal behavior of adding the protocol header alongside a token.
			response := fixture.JSON(t, http.MethodGet, test.path, "", nil, map[string]string{
				"Authorization": "Bearer " + test.token,
			})
			testfixture.RequireStatus(t, response, http.StatusUpgradeRequired)
			errorResponse := testfixture.Decode[struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}](t, response)
			if errorResponse.Error.Code != "protocol_version" {
				t.Fatalf("error code = %q, want protocol_version", errorResponse.Error.Code)
			}
		})
	}
}
