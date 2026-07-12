package persistence

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/internal/auth"
)

var ErrInvalidDeviceCredential = errors.New("invalid device credential")

const DeviceCredentialRotationTTL = 5 * time.Minute

type Device struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Platform     string     `json:"platform"`
	Capabilities string     `json:"capabilities_json"`
	CreatedAt    time.Time  `json:"created_at"`
	LastSeenAt   *time.Time `json:"last_seen_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	KeyVersion   int        `json:"key_version"`
}

type PairingSession struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

type PendingDeviceCredentialRotation struct {
	DeviceID      string    `json:"device_id"`
	KeyVersion    int       `json:"key_version"`
	PreparedAt    time.Time `json:"prepared_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	CorrelationID string    `json:"correlation_id"`
}

type ConfirmedDeviceCredentialRotation struct {
	DeviceID         string `json:"device_id"`
	KeyVersion       int    `json:"key_version"`
	AlreadyConfirmed bool   `json:"already_confirmed"`
	CorrelationID    string `json:"correlation_id"`
}

func (s *Store) CreatePairingSession(ctx context.Context, id string, codeHash []byte, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pairing_sessions(id, code_hash,
expires_at, created_at) VALUES(?, ?, ?, ?)`, id, codeHash, formatTime(expiresAt), formatTime(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("create pairing session: %w", err)
	}
	return nil
}

func (s *Store) ClaimPairingSession(ctx context.Context, codeHash []byte, device Device, credential string,
	transcriptRetention ...bool) error {
	var retention *bool
	if len(transcriptRetention) > 0 {
		retention = &transcriptRetention[0]
	}
	return s.claimPairingSession(ctx, codeHash, device, credential, retention, nil)
}

// ClaimPairingSessionWithAudit atomically consumes the one-time code, creates
// the credential hash/privacy setting, and records the mandatory security fact.
func (s *Store) ClaimPairingSessionWithAudit(ctx context.Context, codeHash []byte, device Device,
	credential string, transcriptRetention bool, audit observability.AuditEntry) error {
	return s.claimPairingSession(ctx, codeHash, device, credential, &transcriptRetention, &audit)
}

func (s *Store) claimPairingSession(ctx context.Context, codeHash []byte, device Device, credential string,
	transcriptRetention *bool, audit *observability.AuditEntry) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var pairingID, expiresAt string
	var consumed sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id, expires_at, consumed_at FROM pairing_sessions
WHERE code_hash = ?`, codeHash).Scan(&pairingID, &expiresAt, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("find pairing session: %w", err)
	}
	expires, err := parseTime(expiresAt)
	if err != nil {
		return err
	}
	if consumed.Valid {
		return ErrConflict
	}
	if !expires.After(now) {
		return ErrExpired
	}
	result, err := tx.ExecContext(ctx, `UPDATE pairing_sessions SET consumed_at = ?
WHERE id = ? AND consumed_at IS NULL`, formatTime(now), pairingID)
	if err != nil {
		return fmt.Errorf("consume pairing session: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO devices(id, name, platform,
credential_hash, capabilities_json, created_at, last_seen_at, key_version)
VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, device.ID, device.Name, device.Platform,
		auth.HashCredential(credential), device.Capabilities, formatTime(now), formatTime(now), 1)
	if err != nil {
		return fmt.Errorf("create paired device: %w", err)
	}
	if transcriptRetention != nil {
		privacy, err := json.Marshal(map[string]bool{"transcript_retention": *transcriptRetention})
		if err != nil {
			return fmt.Errorf("encode paired device privacy: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value_json, updated_at)
VALUES(?, ?, ?)`, "device:"+device.ID+":privacy", string(privacy), formatTime(now)); err != nil {
			return fmt.Errorf("create paired device privacy: %w", err)
		}
	}
	if audit != nil {
		if err := insertAuditEntry(ctx, tx, *audit); err != nil {
			return fmt.Errorf("audit paired device: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) VerifyDeviceCredential(ctx context.Context, credential string) (string, error) {
	wanted := auth.HashCredential(credential)
	rows, err := s.db.QueryContext(ctx, `SELECT id, credential_hash FROM devices WHERE revoked_at IS NULL`)
	if err != nil {
		return "", fmt.Errorf("query device credentials: %w", err)
	}
	matchedID := ""
	for rows.Next() {
		var id string
		var stored []byte
		if err := rows.Scan(&id, &stored); err != nil {
			return "", err
		}
		if len(stored) == len(wanted) && subtle.ConstantTimeCompare(stored, wanted) == 1 {
			matchedID = id
			break
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", err
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	if matchedID != "" {
		_, _ = s.db.ExecContext(ctx, "UPDATE devices SET last_seen_at = ? WHERE id = ?", formatTime(time.Now().UTC()), matchedID)
		return matchedID, nil
	}
	return "", ErrInvalidDeviceCredential
}

// VerifyPendingDeviceCredential authenticates a prepared credential only for
// the rotation-confirm endpoint. Pending credentials deliberately remain
// invalid on every general authenticated API surface until promotion.
func (s *Store) VerifyPendingDeviceCredential(ctx context.Context, credential string) (string, error) {
	wanted := auth.HashCredential(credential)
	rows, err := s.db.QueryContext(ctx, `SELECT id, pending_credential_hash,
pending_credential_expires_at FROM devices
WHERE revoked_at IS NULL AND pending_credential_hash IS NOT NULL`)
	if err != nil {
		return "", fmt.Errorf("query pending device credentials: %w", err)
	}
	matchedID := ""
	expiredMatch := false
	now := time.Now().UTC()
	for rows.Next() {
		var id string
		var stored []byte
		var expiresRaw string
		if err := rows.Scan(&id, &stored, &expiresRaw); err != nil {
			_ = rows.Close()
			return "", err
		}
		expiresAt, err := parseTime(expiresRaw)
		if err != nil {
			_ = rows.Close()
			return "", err
		}
		if len(stored) == len(wanted) && subtle.ConstantTimeCompare(stored, wanted) == 1 {
			if expiresAt.After(now) {
				matchedID = id
				break
			}
			expiredMatch = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", err
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	if matchedID == "" {
		if expiredMatch {
			return "", ErrExpired
		}
		return "", ErrInvalidDeviceCredential
	}
	return matchedID, nil
}

// PrepareDeviceCredentialRotation stores only the replacement hash. The
// current credential remains authoritative until ConfirmDeviceCredentialRotation
// promotes the pending hash. The pending hash and sanitized audit fact commit
// together, so an audit failure cannot create an unaudited credential.
func (s *Store) PrepareDeviceCredentialRotation(ctx context.Context, deviceID, currentCredential,
	newCredential, auditID, correlationID string) (PendingDeviceCredentialRotation, error) {
	if deviceID == "" || currentCredential == "" || newCredential == "" || auditID == "" || correlationID == "" {
		return PendingDeviceCredentialRotation{}, errors.New("device credential rotation requires device, credentials, audit, and correlation identifiers")
	}
	currentHash := auth.HashCredential(currentCredential)
	newHash := auth.HashCredential(newCredential)
	if subtle.ConstantTimeCompare(currentHash, newHash) == 1 {
		return PendingDeviceCredentialRotation{}, errors.New("replacement device credential must differ from the current credential")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PendingDeviceCredentialRotation{}, fmt.Errorf("begin device credential rotation preparation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var storedHash []byte
	var keyVersion int
	var pendingHash []byte
	var pendingVersion sql.NullInt64
	var pendingExpires sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT credential_hash, key_version, pending_credential_hash,
pending_key_version, pending_credential_expires_at FROM devices
WHERE id = ? AND revoked_at IS NULL`, deviceID).
		Scan(&storedHash, &keyVersion, &pendingHash, &pendingVersion, &pendingExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingDeviceCredentialRotation{}, ErrInvalidDeviceCredential
	}
	if err != nil {
		return PendingDeviceCredentialRotation{}, fmt.Errorf("load device credential for rotation preparation: %w", err)
	}
	if len(storedHash) != len(currentHash) || subtle.ConstantTimeCompare(storedHash, currentHash) != 1 {
		return PendingDeviceCredentialRotation{}, ErrInvalidDeviceCredential
	}
	if keyVersion < 1 || keyVersion == int(^uint(0)>>1) {
		return PendingDeviceCredentialRotation{}, fmt.Errorf("invalid device key version %d", keyVersion)
	}

	now := time.Now().UTC()
	if len(pendingHash) > 0 || pendingVersion.Valid || pendingExpires.Valid {
		if len(pendingHash) == 0 || !pendingVersion.Valid || !pendingExpires.Valid {
			return PendingDeviceCredentialRotation{}, errors.New("inconsistent pending device credential state")
		}
		expiresAt, err := parseTime(pendingExpires.String)
		if err != nil {
			return PendingDeviceCredentialRotation{}, err
		}
		if expiresAt.After(now) {
			return PendingDeviceCredentialRotation{}, ErrConflict
		}
	}
	nextVersion := keyVersion + 1
	expiresAt := now.Add(DeviceCredentialRotationTTL)
	result, err := tx.ExecContext(ctx, `UPDATE devices SET pending_credential_hash = ?,
pending_key_version = ?, pending_credential_expires_at = ?, last_seen_at = ?
WHERE id = ? AND revoked_at IS NULL AND key_version = ? AND credential_hash = ?`,
		newHash, nextVersion, formatTime(expiresAt), formatTime(now), deviceID, keyVersion, storedHash)
	if err != nil {
		return PendingDeviceCredentialRotation{}, fmt.Errorf("prepare replacement device credential: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return PendingDeviceCredentialRotation{}, fmt.Errorf("inspect device credential rotation preparation: %w", err)
	}
	if changed != 1 {
		return PendingDeviceCredentialRotation{}, ErrInvalidDeviceCredential
	}
	details, err := json.Marshal(map[string]any{
		"credential_stored":   "hash-only",
		"current_key_version": keyVersion,
		"pending_key_version": nextVersion,
		"expires_at":          expiresAt,
	})
	if err != nil {
		return PendingDeviceCredentialRotation{}, fmt.Errorf("encode device credential rotation preparation audit: %w", err)
	}
	if err := insertAuditEntry(ctx, tx, observability.AuditEntry{
		ID: auditID, OccurredAt: now, ActorKind: "device", ActorID: deviceID,
		Action: "device.credential_rotation_prepared", ResourceKind: "device", ResourceID: deviceID,
		Decision: "ALLOW", Details: details, CorrelationID: correlationID,
	}); err != nil {
		return PendingDeviceCredentialRotation{}, fmt.Errorf("record device credential rotation preparation audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PendingDeviceCredentialRotation{}, fmt.Errorf("commit device credential rotation preparation: %w", err)
	}
	return PendingDeviceCredentialRotation{
		DeviceID: deviceID, KeyVersion: nextVersion, PreparedAt: now,
		ExpiresAt: expiresAt, CorrelationID: correlationID,
	}, nil
}

// ConfirmDeviceCredentialRotation promotes an unexpired pending credential and
// removes the old hash in one transaction with its audit entry. Retrying after
// a committed-but-unobserved response is safe: the promoted credential and
// expected key version return an idempotent success without another mutation.
func (s *Store) ConfirmDeviceCredentialRotation(ctx context.Context, deviceID, credential string,
	expectedVersion int, auditID, correlationID string) (ConfirmedDeviceCredentialRotation, error) {
	if deviceID == "" || credential == "" || expectedVersion < 2 || auditID == "" || correlationID == "" {
		return ConfirmedDeviceCredentialRotation{}, errors.New("device credential rotation confirmation is incomplete")
	}
	wanted := auth.HashCredential(credential)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConfirmedDeviceCredentialRotation{}, fmt.Errorf("begin device credential rotation confirmation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var activeHash, pendingHash []byte
	var keyVersion int
	var pendingVersion sql.NullInt64
	var pendingExpires sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT credential_hash, key_version, pending_credential_hash,
pending_key_version, pending_credential_expires_at FROM devices
WHERE id = ? AND revoked_at IS NULL`, deviceID).
		Scan(&activeHash, &keyVersion, &pendingHash, &pendingVersion, &pendingExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return ConfirmedDeviceCredentialRotation{}, ErrInvalidDeviceCredential
	}
	if err != nil {
		return ConfirmedDeviceCredentialRotation{}, fmt.Errorf("load device credential rotation confirmation: %w", err)
	}
	if len(activeHash) == len(wanted) && subtle.ConstantTimeCompare(activeHash, wanted) == 1 {
		if keyVersion != expectedVersion {
			return ConfirmedDeviceCredentialRotation{}, ErrInvalidDeviceCredential
		}
		return ConfirmedDeviceCredentialRotation{
			DeviceID: deviceID, KeyVersion: keyVersion, AlreadyConfirmed: true,
			CorrelationID: correlationID,
		}, nil
	}
	if len(pendingHash) == 0 || !pendingVersion.Valid || !pendingExpires.Valid {
		return ConfirmedDeviceCredentialRotation{}, ErrInvalidDeviceCredential
	}
	expiresAt, err := parseTime(pendingExpires.String)
	if err != nil {
		return ConfirmedDeviceCredentialRotation{}, err
	}
	if !expiresAt.After(time.Now().UTC()) {
		return ConfirmedDeviceCredentialRotation{}, ErrExpired
	}
	if int(pendingVersion.Int64) != expectedVersion || len(pendingHash) != len(wanted) ||
		subtle.ConstantTimeCompare(pendingHash, wanted) != 1 {
		return ConfirmedDeviceCredentialRotation{}, ErrInvalidDeviceCredential
	}

	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE devices SET credential_hash = ?, key_version = ?,
pending_credential_hash = NULL, pending_key_version = NULL,
pending_credential_expires_at = NULL, last_seen_at = ?
WHERE id = ? AND revoked_at IS NULL AND key_version = ? AND credential_hash = ?
AND pending_key_version = ? AND pending_credential_hash = ?`,
		pendingHash, expectedVersion, formatTime(now), deviceID, keyVersion, activeHash,
		expectedVersion, pendingHash)
	if err != nil {
		return ConfirmedDeviceCredentialRotation{}, fmt.Errorf("promote replacement device credential: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ConfirmedDeviceCredentialRotation{}, fmt.Errorf("inspect device credential rotation confirmation: %w", err)
	}
	if changed != 1 {
		return ConfirmedDeviceCredentialRotation{}, ErrInvalidDeviceCredential
	}
	details, err := json.Marshal(map[string]any{
		"credential_stored":    "hash-only",
		"previous_key_version": keyVersion,
		"new_key_version":      expectedVersion,
	})
	if err != nil {
		return ConfirmedDeviceCredentialRotation{}, fmt.Errorf("encode device credential rotation confirmation audit: %w", err)
	}
	if err := insertAuditEntry(ctx, tx, observability.AuditEntry{
		ID: auditID, OccurredAt: now, ActorKind: "device", ActorID: deviceID,
		Action: "device.credential_rotated", ResourceKind: "device", ResourceID: deviceID,
		Decision: "ALLOW", Details: details, CorrelationID: correlationID,
	}); err != nil {
		return ConfirmedDeviceCredentialRotation{}, fmt.Errorf("record device credential rotation confirmation audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ConfirmedDeviceCredentialRotation{}, fmt.Errorf("commit device credential rotation confirmation: %w", err)
	}
	return ConfirmedDeviceCredentialRotation{
		DeviceID: deviceID, KeyVersion: expectedVersion, CorrelationID: correlationID,
	}, nil
}

// CancelDeviceCredentialRotation clears an outstanding pending hash while the
// original credential is still active. This lets a client recover immediately
// when a prepare response was lost before the new credential could be stored.
func (s *Store) CancelDeviceCredentialRotation(ctx context.Context, deviceID, currentCredential,
	auditID, correlationID string) (bool, error) {
	if deviceID == "" || currentCredential == "" || auditID == "" || correlationID == "" {
		return false, errors.New("device credential rotation cancellation is incomplete")
	}
	wanted := auth.HashCredential(currentCredential)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin device credential rotation cancellation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var activeHash, pendingHash []byte
	var keyVersion int
	var pendingVersion sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT credential_hash, key_version,
pending_credential_hash, pending_key_version FROM devices
WHERE id = ? AND revoked_at IS NULL`, deviceID).
		Scan(&activeHash, &keyVersion, &pendingHash, &pendingVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrInvalidDeviceCredential
	}
	if err != nil {
		return false, fmt.Errorf("load device credential rotation cancellation: %w", err)
	}
	if len(activeHash) != len(wanted) || subtle.ConstantTimeCompare(activeHash, wanted) != 1 {
		return false, ErrInvalidDeviceCredential
	}
	if len(pendingHash) == 0 && !pendingVersion.Valid {
		return false, nil
	}
	if len(pendingHash) == 0 || !pendingVersion.Valid {
		return false, errors.New("inconsistent pending device credential state")
	}

	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE devices SET pending_credential_hash = NULL,
pending_key_version = NULL, pending_credential_expires_at = NULL, last_seen_at = ?
WHERE id = ? AND revoked_at IS NULL AND key_version = ? AND credential_hash = ?
AND pending_key_version = ? AND pending_credential_hash = ?`,
		formatTime(now), deviceID, keyVersion, activeHash, pendingVersion.Int64, pendingHash)
	if err != nil {
		return false, fmt.Errorf("cancel pending device credential rotation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect device credential rotation cancellation: %w", err)
	}
	if changed != 1 {
		return false, ErrConflict
	}
	details, err := json.Marshal(map[string]any{
		"current_key_version":   keyVersion,
		"cancelled_key_version": pendingVersion.Int64,
	})
	if err != nil {
		return false, fmt.Errorf("encode device credential rotation cancellation audit: %w", err)
	}
	if err := insertAuditEntry(ctx, tx, observability.AuditEntry{
		ID: auditID, OccurredAt: now, ActorKind: "device", ActorID: deviceID,
		Action: "device.credential_rotation_cancelled", ResourceKind: "device", ResourceID: deviceID,
		Decision: "ALLOW", Details: details, CorrelationID: correlationID,
	}); err != nil {
		return false, fmt.Errorf("record device credential rotation cancellation audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit device credential rotation cancellation: %w", err)
	}
	return true, nil
}

func (s *Store) RevokeDevice(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "UPDATE devices SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", formatTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("revoke device: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeDeviceWithAudit makes credential invalidation and its mandatory audit
// entry one commit. Socket closure is performed only after this succeeds.
func (s *Store) RevokeDeviceWithAudit(ctx context.Context, id string, audit observability.AuditEntry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, "UPDATE devices SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL",
		formatTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("revoke device: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrNotFound
	}
	if err := insertAuditEntry(ctx, tx, audit); err != nil {
		return fmt.Errorf("audit device revocation: %w", err)
	}
	return tx.Commit()
}

func (s *Store) TouchDevice(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE devices SET last_seen_at = ?
WHERE id = ? AND revoked_at IS NULL`, formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeviceIsActive(ctx context.Context, id string) (bool, error) {
	var active bool
	err := s.db.QueryRowContext(ctx, "SELECT revoked_at IS NULL FROM devices WHERE id = ?", id).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return active, nil
}

func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, platform, capabilities_json,
created_at, last_seen_at, revoked_at, key_version FROM devices ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Device
	for rows.Next() {
		var item Device
		var created string
		var lastSeen, revoked sql.NullString
		if err := rows.Scan(&item.ID, &item.Name, &item.Platform, &item.Capabilities,
			&created, &lastSeen, &revoked, &item.KeyVersion); err != nil {
			return nil, err
		}
		item.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		item.LastSeenAt, err = parseOptionalTime(lastSeen)
		if err != nil {
			return nil, err
		}
		item.RevokedAt, err = parseOptionalTime(revoked)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
