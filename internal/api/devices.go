package api

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/veqri/veqri/core/observability"
	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/internal/auth"
	"github.com/veqri/veqri/internal/ids"
	"github.com/veqri/veqri/internal/stream"
)

func (s *Server) handleCreatePairing(writer http.ResponseWriter, request *http.Request) {
	code, err := randomPairingCode()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "pairing_code", "could not create pairing code")
		return
	}
	sessionID := ids.New()
	expiresAt := time.Now().UTC().Add(5 * time.Minute)
	if err := s.store.CreatePairingSession(request.Context(), sessionID,
		auth.HashPairingCode(s.adminToken, code), expiresAt); err != nil {
		writeError(writer, http.StatusInternalServerError, "pairing_session", "could not create pairing session")
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{
		"pairing_id": sessionID, "code": code, "expires_at": expiresAt,
		"core_url": advertisedCoreURL(request),
	})
}

func (s *Server) handleClaimPairing(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Code                  string         `json:"code"`
		Name                  string         `json:"name"`
		Platform              string         `json:"platform"`
		OneTimeCode           string         `json:"one_time_code"`
		DeviceName            string         `json:"device_name"`
		ClientProtocolVersion int            `json:"client_protocol_version"`
		RetainTranscript      *bool          `json:"retain_transcript"`
		Capabilities          map[string]any `json:"capabilities,omitempty"`
	}
	if !decodeJSON(writer, request, &body) {
		return
	}
	if body.Code == "" {
		body.Code = body.OneTimeCode
	}
	if body.Name == "" {
		body.Name = body.DeviceName
	}
	if body.Platform == "" {
		body.Platform = "android"
	}
	if body.ClientProtocolVersion != 0 && body.ClientProtocolVersion != 1 {
		writeError(writer, http.StatusUpgradeRequired, "protocol_version", "protocol version 1 is required")
		return
	}
	body.Code = strings.ReplaceAll(strings.TrimSpace(body.Code), "-", "")
	if len(body.Code) != 8 || body.Name == "" || body.Platform == "" {
		writeError(writer, http.StatusBadRequest, "invalid_pairing", "eight-digit code, name, and platform are required")
		return
	}
	credential, err := auth.RandomToken(32)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "credential", "could not create device credential")
		return
	}
	if body.Capabilities == nil {
		body.Capabilities = map[string]any{}
	}
	capabilities, _ := json.Marshal(body.Capabilities)
	device := persistence.Device{ID: ids.New(), Name: truncateTitle(body.Name, 80),
		Platform: truncateTitle(body.Platform, 40), Capabilities: string(capabilities),
		CreatedAt: time.Now().UTC(), KeyVersion: 1}
	retainTranscript := s.config.TranscriptRetention
	if body.RetainTranscript != nil {
		retainTranscript = *body.RetainTranscript
	}
	pairingAudit := observability.AuditEntry{
		ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: "device", ActorID: device.ID,
		Action: "device.paired", ResourceKind: "device", ResourceID: device.ID,
		Decision: "ALLOW", Details: json.RawMessage(`{"credential_stored":"hash-only"}`), CorrelationID: ids.New(),
	}
	err = s.store.ClaimPairingSessionWithAudit(request.Context(), auth.HashPairingCode(s.adminToken, body.Code),
		device, credential, retainTranscript, pairingAudit)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) || errors.Is(err, persistence.ErrConflict) ||
			errors.Is(err, persistence.ErrExpired) {
			writeError(writer, persistenceStatus(err), "pairing_failed", "pairing code is invalid, expired, or already used")
		} else {
			writeError(writer, http.StatusServiceUnavailable, "audit_unavailable", "device was not paired because its audit record could not be persisted")
		}
		return
	}
	s.hub.Publish(stream.Event{Type: "device.paired", Payload: device})
	writeJSON(writer, http.StatusCreated, map[string]any{
		"device": device, "credential": credential, "protocol_version": 1,
		"device_id": device.ID, "access_token": credential,
		"issued_at_epoch_millis": time.Now().UTC().UnixMilli(),
		"warning":                "The credential is returned once and must be stored in Android Keystore.",
	})
}

func (s *Server) handleDevices(writer http.ResponseWriter, request *http.Request) {
	devices, err := s.store.ListDevices(request.Context())
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "devices", "could not list devices")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"devices": devices})
}

func (s *Server) handleRevokeDevice(writer http.ResponseWriter, request *http.Request) {
	id := request.PathValue("id")
	revocationAudit := observability.AuditEntry{
		ID: ids.New(), OccurredAt: time.Now().UTC(), ActorKind: "admin", ActorID: "local-admin",
		Action: "device.revoked", ResourceKind: "device", ResourceID: id, Decision: "ALLOW",
		Details: json.RawMessage(`{"credential":"revoked"}`), CorrelationID: ids.New(),
	}
	if err := s.store.RevokeDeviceWithAudit(request.Context(), id, revocationAudit); err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			writeError(writer, persistenceStatus(err), "revoke_device", "device was not found or is already revoked")
		} else {
			writeError(writer, http.StatusServiceUnavailable, "audit_unavailable", "device was not revoked because its audit record could not be persisted")
		}
		return
	}
	s.closeDeviceSockets(id)
	s.hub.Publish(stream.Event{Type: "device.revoked", Payload: map[string]any{"device_id": id}})
	writeJSON(writer, http.StatusOK, map[string]any{"revoked": true, "device_id": id})
}

func randomPairingCode() (string, error) {
	limit := big.NewInt(100_000_000)
	value, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%08d", value.Int64()), nil
}

func advertisedCoreURL(request *http.Request) string {
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + request.Host
}
