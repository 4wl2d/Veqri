package integration_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/veqri/veqri/tests/integration/testfixture"
)

type preparedCredentialRotation struct {
	DeviceID      string    `json:"device_id"`
	Credential    string    `json:"credential"`
	KeyVersion    int       `json:"key_version"`
	PreparedAt    time.Time `json:"prepared_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	CorrelationID string    `json:"correlation_id"`
}

type confirmedCredentialRotation struct {
	DeviceID         string `json:"device_id"`
	KeyVersion       int    `json:"key_version"`
	Confirmed        bool   `json:"confirmed"`
	AlreadyConfirmed bool   `json:"already_confirmed"`
}

func TestDeviceCredentialRotationHTTPContractAndSocketHandoff(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{WorkerCount: 1})
	device := fixture.PairAndroid(t, "Rotation integration phone")
	const preparePath = "/v1/devices/self/credential-rotation/prepare"
	const confirmPath = "/v1/devices/self/credential-rotation/confirm"

	unauthenticated := fixture.JSON(t, http.MethodPost, preparePath, "", nil, nil)
	testfixture.RequireStatus(t, unauthenticated, http.StatusUnauthorized)
	admin := fixture.JSON(t, http.MethodPost, preparePath, fixture.AdminToken, nil, nil)
	testfixture.RequireStatus(t, admin, http.StatusForbidden)

	streamContext, cancelStream := context.WithTimeout(context.Background(), testfixture.DefaultTimeout)
	defer cancelStream()
	deviceStream, response, err := websocket.Dial(streamContext, fixture.WebSocketURL("/v1/device/events"), &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization":            []string{"Bearer " + device.Credential},
			"X-Veqri-Device-Id":        []string{device.ID},
			"X-Veqri-Protocol-Version": []string{"1"},
		},
		Subprotocols: []string{"veqri.v1"},
	})
	if err != nil {
		if response != nil {
			t.Fatalf("dial device stream: %v (HTTP %s)", err, response.Status)
		}
		t.Fatalf("dial device stream: %v", err)
	}
	defer deviceStream.CloseNow()
	closed := make(chan error, 1)
	go func() {
		for {
			if _, _, readErr := deviceStream.Read(streamContext); readErr != nil {
				closed <- readErr
				return
			}
		}
	}()

	prepareResponse := fixture.JSON(t, http.MethodPost, preparePath, device.Credential, nil, nil)
	testfixture.RequireStatus(t, prepareResponse, http.StatusCreated)
	prepared := testfixture.Decode[preparedCredentialRotation](t, prepareResponse)
	if prepared.DeviceID != device.ID || prepared.Credential == "" || prepared.KeyVersion != 2 ||
		!prepared.ExpiresAt.After(prepared.PreparedAt) || prepared.CorrelationID == "" {
		t.Fatalf("prepare response = %+v", prepared)
	}
	if bytes.Count(prepareResponse.Body, []byte(prepared.Credential)) != 1 ||
		bytes.Contains(prepareResponse.Body, []byte("access_token")) {
		t.Fatalf("replacement credential was not returned exactly once: %s", prepareResponse.Body)
	}
	if prepareResponse.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("prepare Cache-Control = %q, want no-store", prepareResponse.Header.Get("Cache-Control"))
	}

	// Preparation cannot strand the device: old auth still works, while the
	// pending credential has only enough authority to call confirm.
	oldStillActive := fixture.JSON(t, http.MethodGet, "/v1/tasks", device.Credential, nil, nil)
	testfixture.RequireStatus(t, oldStillActive, http.StatusOK)
	pendingGeneralAccess := fixture.JSON(t, http.MethodGet, "/v1/tasks", prepared.Credential, nil, nil)
	testfixture.RequireStatus(t, pendingGeneralAccess, http.StatusUnauthorized)
	duplicatePrepare := fixture.JSON(t, http.MethodPost, preparePath, device.Credential, nil, nil)
	testfixture.RequireStatus(t, duplicatePrepare, http.StatusConflict)
	missingProtocol := fixture.JSON(t, http.MethodPost, confirmPath, "",
		map[string]any{"key_version": prepared.KeyVersion}, map[string]string{
			"Authorization": "Bearer " + prepared.Credential,
		})
	testfixture.RequireStatus(t, missingProtocol, http.StatusUpgradeRequired)

	confirmResponse := fixture.JSON(t, http.MethodPost, confirmPath, prepared.Credential,
		map[string]any{"key_version": prepared.KeyVersion}, nil)
	testfixture.RequireStatus(t, confirmResponse, http.StatusOK)
	confirmed := testfixture.Decode[confirmedCredentialRotation](t, confirmResponse)
	if confirmed.DeviceID != device.ID || !confirmed.Confirmed || confirmed.AlreadyConfirmed || confirmed.KeyVersion != 2 {
		t.Fatalf("confirm response = %+v", confirmed)
	}
	select {
	case closeErr := <-closed:
		if status := websocket.CloseStatus(closeErr); status != 4004 {
			t.Fatalf("device stream close status = %d (%v), want 4004", status, closeErr)
		}
	case <-streamContext.Done():
		t.Fatal("old authenticated device stream was not closed after rotation")
	}

	oldRejected := fixture.JSON(t, http.MethodGet, "/v1/tasks", device.Credential, nil, nil)
	testfixture.RequireStatus(t, oldRejected, http.StatusUnauthorized)
	newAccepted := fixture.JSON(t, http.MethodGet, "/v1/tasks", prepared.Credential, nil, nil)
	testfixture.RequireStatus(t, newAccepted, http.StatusOK)

	// A lost confirmation response is safe because the client persisted the new
	// token first; retrying confirm with that promoted token is idempotent.
	retryResponse := fixture.JSON(t, http.MethodPost, confirmPath, prepared.Credential,
		map[string]any{"key_version": prepared.KeyVersion}, nil)
	testfixture.RequireStatus(t, retryResponse, http.StatusOK)
	retried := testfixture.Decode[confirmedCredentialRotation](t, retryResponse)
	if !retried.Confirmed || !retried.AlreadyConfirmed || retried.KeyVersion != 2 {
		t.Fatalf("confirmation retry = %+v", retried)
	}

	var prepareAudits, confirmAudits int
	if err := fixture.Store.DB().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM audit_entries WHERE actor_id = ? AND action = 'device.credential_rotation_prepared'", device.ID).
		Scan(&prepareAudits); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Store.DB().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM audit_entries WHERE actor_id = ? AND action = 'device.credential_rotated'", device.ID).
		Scan(&confirmAudits); err != nil {
		t.Fatal(err)
	}
	if prepareAudits != 1 || confirmAudits != 1 {
		t.Fatalf("rotation audits = prepared:%d confirmed:%d", prepareAudits, confirmAudits)
	}
	rows, err := fixture.Store.DB().QueryContext(context.Background(),
		"SELECT details_json FROM audit_entries WHERE actor_id = ? AND action LIKE 'device.credential_%'", device.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var details string
		if err := rows.Scan(&details); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(details, device.Credential) || strings.Contains(details, prepared.Credential) {
			t.Fatalf("rotation audit leaked a credential: %s", details)
		}
	}
}

func TestDeviceCredentialRotationCancelAndExpiryRecovery(t *testing.T) {
	fixture := testfixture.New(t, testfixture.Options{WorkerCount: 1})
	device := fixture.PairAndroid(t, "Rotation recovery phone")
	const preparePath = "/v1/devices/self/credential-rotation/prepare"
	const confirmPath = "/v1/devices/self/credential-rotation/confirm"
	const cancelPath = "/v1/devices/self/credential-rotation/cancel"

	lostPrepareResponse := fixture.JSON(t, http.MethodPost, preparePath, device.Credential, nil, nil)
	testfixture.RequireStatus(t, lostPrepareResponse, http.StatusCreated)
	lost := testfixture.Decode[preparedCredentialRotation](t, lostPrepareResponse)
	cancelResponse := fixture.JSON(t, http.MethodPost, cancelPath, device.Credential, nil, nil)
	testfixture.RequireStatus(t, cancelResponse, http.StatusOK)
	cancelled := testfixture.Decode[struct {
		Cancelled bool `json:"cancelled"`
	}](t, cancelResponse)
	if !cancelled.Cancelled {
		t.Fatal("lost prepare response did not leave a cancellable pending rotation")
	}
	cancelRetry := fixture.JSON(t, http.MethodPost, cancelPath, device.Credential, nil, nil)
	testfixture.RequireStatus(t, cancelRetry, http.StatusOK)
	if testfixture.Decode[struct {
		Cancelled bool `json:"cancelled"`
	}](t, cancelRetry).Cancelled {
		t.Fatal("idempotent cancellation reported a second state change")
	}
	cancelledConfirm := fixture.JSON(t, http.MethodPost, confirmPath, lost.Credential,
		map[string]any{"key_version": lost.KeyVersion}, nil)
	testfixture.RequireStatus(t, cancelledConfirm, http.StatusUnauthorized)

	expiringResponse := fixture.JSON(t, http.MethodPost, preparePath, device.Credential, nil, nil)
	testfixture.RequireStatus(t, expiringResponse, http.StatusCreated)
	expiring := testfixture.Decode[preparedCredentialRotation](t, expiringResponse)
	if _, err := fixture.Store.DB().ExecContext(context.Background(),
		`UPDATE devices SET pending_credential_expires_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), device.ID); err != nil {
		t.Fatal(err)
	}
	expiredConfirm := fixture.JSON(t, http.MethodPost, confirmPath, expiring.Credential,
		map[string]any{"key_version": expiring.KeyVersion}, nil)
	testfixture.RequireStatus(t, expiredConfirm, http.StatusGone)
	activeAfterExpiry := fixture.JSON(t, http.MethodGet, "/v1/tasks", device.Credential, nil, nil)
	testfixture.RequireStatus(t, activeAfterExpiry, http.StatusOK)
}
