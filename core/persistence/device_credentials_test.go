package persistence

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/veqri/veqri/internal/auth"
)

func pairCredentialRotationDevice(t testing.TB, store *Store, deviceID, credential string) {
	t.Helper()
	ctx := context.Background()
	codeHash := auth.HashPairingCode("rotation-test-secret", "rotation-"+deviceID)
	if err := store.CreatePairingSession(ctx, "pairing-"+deviceID, codeHash, persistenceFuture); err != nil {
		t.Fatalf("CreatePairingSession(): %v", err)
	}
	if err := store.ClaimPairingSession(ctx, codeHash, Device{
		ID: deviceID, Name: "Rotation phone", Platform: "android", Capabilities: `{}`,
	}, credential); err != nil {
		t.Fatalf("ClaimPairingSession(): %v", err)
	}
}

func TestDeviceCredentialRotationIsTwoPhaseRecoverableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const deviceID = "device-rotation"
	const activeCredential = "active-device-credential-with-enough-entropy"
	const pendingCredential = "pending-device-credential-with-enough-entropy"
	pairCredentialRotationDevice(t, store, deviceID, activeCredential)

	prepared, err := store.PrepareDeviceCredentialRotation(ctx, deviceID, activeCredential,
		pendingCredential, "audit-prepare", "correlation-prepare")
	if err != nil {
		t.Fatalf("PrepareDeviceCredentialRotation(): %v", err)
	}
	if prepared.DeviceID != deviceID || prepared.KeyVersion != 2 ||
		prepared.ExpiresAt.Sub(prepared.PreparedAt) != DeviceCredentialRotationTTL {
		t.Fatalf("prepared rotation = %+v", prepared)
	}
	if id, err := store.VerifyDeviceCredential(ctx, activeCredential); err != nil || id != deviceID {
		t.Fatalf("active credential stopped working before confirmation: (%q, %v)", id, err)
	}
	if _, err := store.VerifyDeviceCredential(ctx, pendingCredential); !errors.Is(err, ErrInvalidDeviceCredential) {
		t.Fatalf("pending credential reached general APIs before confirmation: %v", err)
	}
	if id, err := store.VerifyPendingDeviceCredential(ctx, pendingCredential); err != nil || id != deviceID {
		t.Fatalf("pending credential could not confirm: (%q, %v)", id, err)
	}
	if _, err := store.PrepareDeviceCredentialRotation(ctx, deviceID, activeCredential,
		"second-pending-credential", "audit-conflict", "correlation-conflict"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second pending rotation error = %v, want ErrConflict", err)
	}

	confirmed, err := store.ConfirmDeviceCredentialRotation(ctx, deviceID, pendingCredential,
		prepared.KeyVersion, "audit-confirm", "correlation-confirm")
	if err != nil {
		t.Fatalf("ConfirmDeviceCredentialRotation(): %v", err)
	}
	if confirmed.KeyVersion != 2 || confirmed.AlreadyConfirmed {
		t.Fatalf("confirmed rotation = %+v", confirmed)
	}
	if _, err := store.VerifyDeviceCredential(ctx, activeCredential); !errors.Is(err, ErrInvalidDeviceCredential) {
		t.Fatalf("old credential remained active after confirmation: %v", err)
	}
	if id, err := store.VerifyDeviceCredential(ctx, pendingCredential); err != nil || id != deviceID {
		t.Fatalf("replacement credential was not promoted: (%q, %v)", id, err)
	}

	// Simulate a successful confirmation whose HTTP response was lost. A retry
	// with the already-persisted replacement credential must be harmless.
	retried, err := store.ConfirmDeviceCredentialRotation(ctx, deviceID, pendingCredential,
		prepared.KeyVersion, "unused-audit-retry", "correlation-confirm-retry")
	if err != nil || !retried.AlreadyConfirmed || retried.KeyVersion != 2 {
		t.Fatalf("idempotent confirmation retry = %+v, %v", retried, err)
	}

	var storedHash []byte
	var keyVersion int
	if err := store.db.QueryRowContext(ctx, `SELECT credential_hash, key_version FROM devices WHERE id = ?`, deviceID).
		Scan(&storedHash, &keyVersion); err != nil {
		t.Fatal(err)
	}
	if keyVersion != 2 || !bytes.Equal(storedHash, auth.HashCredential(pendingCredential)) ||
		bytes.Equal(storedHash, []byte(pendingCredential)) {
		t.Fatalf("persisted credential state = version %d, hash %x", keyVersion, storedHash)
	}
	entries, err := store.ListAuditEntries(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	actions := make(map[string]int)
	for _, entry := range entries {
		actions[entry.Action]++
		if strings.Contains(string(entry.Details), activeCredential) || strings.Contains(string(entry.Details), pendingCredential) {
			t.Fatalf("audit entry contains a raw credential: %s", entry.Details)
		}
	}
	if actions["device.credential_rotation_prepared"] != 1 || actions["device.credential_rotated"] != 1 {
		t.Fatalf("rotation audit actions = %v", actions)
	}
}

func TestDeviceCredentialRotationCancelRecoversLostPrepareResponse(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const deviceID = "device-rotation-cancel"
	const activeCredential = "cancel-active-device-credential"
	const lostCredential = "lost-prepared-device-credential"
	pairCredentialRotationDevice(t, store, deviceID, activeCredential)

	if _, err := store.PrepareDeviceCredentialRotation(ctx, deviceID, activeCredential,
		lostCredential, "audit-lost-prepare", "correlation-lost-prepare"); err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.CancelDeviceCredentialRotation(ctx, deviceID, activeCredential,
		"audit-cancel", "correlation-cancel")
	if err != nil || !cancelled {
		t.Fatalf("CancelDeviceCredentialRotation() = (%v, %v)", cancelled, err)
	}
	if _, err := store.VerifyPendingDeviceCredential(ctx, lostCredential); !errors.Is(err, ErrInvalidDeviceCredential) {
		t.Fatalf("cancelled pending credential still verifies: %v", err)
	}
	if id, err := store.VerifyDeviceCredential(ctx, activeCredential); err != nil || id != deviceID {
		t.Fatalf("cancel changed active credential: (%q, %v)", id, err)
	}
	cancelled, err = store.CancelDeviceCredentialRotation(ctx, deviceID, activeCredential,
		"unused-audit-cancel-retry", "correlation-cancel-retry")
	if err != nil || cancelled {
		t.Fatalf("idempotent cancel retry = (%v, %v)", cancelled, err)
	}
}

func TestDeviceCredentialRotationAuditFailureRollsBackCredentialState(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const deviceID = "device-rotation-audit"
	const activeCredential = "audit-active-device-credential"
	const pendingCredential = "audit-pending-device-credential"
	pairCredentialRotationDevice(t, store, deviceID, activeCredential)

	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_rotation_prepare_audit
BEFORE INSERT ON audit_entries WHEN NEW.action = 'device.credential_rotation_prepared'
BEGIN SELECT RAISE(ABORT, 'rotation audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareDeviceCredentialRotation(ctx, deviceID, activeCredential,
		pendingCredential, "audit-rejected-prepare", "correlation-rejected-prepare"); err == nil {
		t.Fatal("prepare committed while its audit insert failed")
	}
	if _, err := store.VerifyPendingDeviceCredential(ctx, pendingCredential); !errors.Is(err, ErrInvalidDeviceCredential) {
		t.Fatalf("failed prepare retained pending credential: %v", err)
	}
	if id, err := store.VerifyDeviceCredential(ctx, activeCredential); err != nil || id != deviceID {
		t.Fatalf("failed prepare changed active credential: (%q, %v)", id, err)
	}
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER reject_rotation_prepare_audit"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareDeviceCredentialRotation(ctx, deviceID, activeCredential,
		pendingCredential, "audit-accepted-prepare", "correlation-accepted-prepare"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER reject_rotation_confirm_audit
BEFORE INSERT ON audit_entries WHEN NEW.action = 'device.credential_rotated'
BEGIN SELECT RAISE(ABORT, 'rotation audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmDeviceCredentialRotation(ctx, deviceID, pendingCredential,
		2, "audit-rejected-confirm", "correlation-rejected-confirm"); err == nil {
		t.Fatal("confirmation committed while its audit insert failed")
	}
	if id, err := store.VerifyDeviceCredential(ctx, activeCredential); err != nil || id != deviceID {
		t.Fatalf("failed confirmation removed active credential: (%q, %v)", id, err)
	}
	if _, err := store.VerifyDeviceCredential(ctx, pendingCredential); !errors.Is(err, ErrInvalidDeviceCredential) {
		t.Fatalf("failed confirmation promoted pending credential: %v", err)
	}
	if id, err := store.VerifyPendingDeviceCredential(ctx, pendingCredential); err != nil || id != deviceID {
		t.Fatalf("failed confirmation removed recoverable pending credential: (%q, %v)", id, err)
	}
}

func TestExpiredPendingDeviceCredentialNeverAuthenticates(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const deviceID = "device-rotation-expired"
	const activeCredential = "expired-active-device-credential"
	const pendingCredential = "expired-pending-device-credential"
	pairCredentialRotationDevice(t, store, deviceID, activeCredential)
	if _, err := store.PrepareDeviceCredentialRotation(ctx, deviceID, activeCredential,
		pendingCredential, "audit-expiring-prepare", "correlation-expiring-prepare"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE devices SET pending_credential_expires_at = ? WHERE id = ?`,
		formatTime(time.Now().UTC().Add(-time.Second)), deviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VerifyPendingDeviceCredential(ctx, pendingCredential); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired pending verification error = %v, want ErrExpired", err)
	}
	if _, err := store.ConfirmDeviceCredentialRotation(ctx, deviceID, pendingCredential,
		2, "audit-expired-confirm", "correlation-expired-confirm"); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired confirmation error = %v, want ErrExpired", err)
	}
	if id, err := store.VerifyDeviceCredential(ctx, activeCredential); err != nil || id != deviceID {
		t.Fatalf("expiration changed active credential: (%q, %v)", id, err)
	}
}
