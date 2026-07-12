package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/internal/auth"
	"github.com/veqri/veqri/internal/ids"
	"github.com/veqri/veqri/internal/stream"
)

const deviceCredentialRotatedCloseCode websocket.StatusCode = 4004

func (s *Server) handlePrepareDeviceCredentialRotation(writer http.ResponseWriter, request *http.Request) {
	principal := principalFromContext(request.Context())
	if principal.Kind != "device" {
		writeError(writer, http.StatusForbidden, "device_required", "paired device authentication required")
		return
	}
	currentCredential := auth.BearerToken(request.Header.Get("Authorization"))
	newCredential, err := auth.RandomToken(32)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "credential_rotation", "could not generate replacement credential")
		return
	}
	rotation, err := s.store.PrepareDeviceCredentialRotation(request.Context(), principal.ID,
		currentCredential, newCredential, ids.New(), ids.New())
	if err != nil {
		switch {
		case errors.Is(err, persistence.ErrInvalidDeviceCredential):
			writeError(writer, http.StatusUnauthorized, "credential_stale", "the active device credential changed")
		case errors.Is(err, persistence.ErrConflict):
			writeError(writer, http.StatusConflict, "credential_rotation_pending", "a credential rotation is already pending; confirm or cancel it before preparing another")
		default:
			writeError(writer, http.StatusInternalServerError, "credential_rotation", "could not prepare device credential rotation")
		}
		return
	}
	s.hub.Publish(stream.Event{
		Type: "device.credential_rotation_prepared", CorrelationID: rotation.CorrelationID,
		Payload: map[string]any{
			"device_id": rotation.DeviceID, "key_version": rotation.KeyVersion,
			"expires_at": rotation.ExpiresAt,
		},
	})
	writeJSON(writer, http.StatusCreated, map[string]any{
		"device_id": rotation.DeviceID, "credential": newCredential,
		"key_version": rotation.KeyVersion, "prepared_at": rotation.PreparedAt,
		"expires_at": rotation.ExpiresAt, "correlation_id": rotation.CorrelationID,
		"protocol_version": 1,
		"warning":          "Persist this credential in Android Keystore before confirming. The active credential remains valid until confirmation.",
	})
}

func (s *Server) handleConfirmDeviceCredentialRotation(writer http.ResponseWriter, request *http.Request) {
	credential := auth.BearerToken(request.Header.Get("Authorization"))
	if credential == "" {
		writeError(writer, http.StatusUnauthorized, "device_auth", "prepared device credential authentication required")
		return
	}
	if !supportedProtocol(request.Header.Get("X-Veqri-Protocol-Version")) {
		writeError(writer, http.StatusUpgradeRequired, "protocol_version", "protocol version 1 is required")
		return
	}

	deviceID, pendingErr := s.store.VerifyPendingDeviceCredential(request.Context(), credential)
	if pendingErr != nil {
		if errors.Is(pendingErr, persistence.ErrExpired) {
			writeError(writer, http.StatusGone, "credential_rotation_expired", "the prepared credential expired; keep using the active credential and prepare again")
			return
		}
		principal, activeErr := s.authenticator.Authenticate(request.Context(), credential)
		if activeErr != nil {
			writeError(writer, http.StatusUnauthorized, "device_auth", "prepared device credential authentication required")
			return
		}
		if principal.Kind != "device" {
			writeError(writer, http.StatusForbidden, "device_required", "paired device authentication required")
			return
		}
		deviceID = principal.ID
	}
	var body struct {
		KeyVersion int `json:"key_version"`
	}
	if !decodeJSON(writer, request, &body) {
		return
	}
	if body.KeyVersion < 2 {
		writeError(writer, http.StatusBadRequest, "key_version", "the prepared key_version is required")
		return
	}
	rotation, err := s.store.ConfirmDeviceCredentialRotation(request.Context(), deviceID,
		credential, body.KeyVersion, ids.New(), ids.New())
	if err != nil {
		switch {
		case errors.Is(err, persistence.ErrInvalidDeviceCredential):
			writeError(writer, http.StatusUnauthorized, "credential_stale", "the prepared credential or key version is not valid")
		case errors.Is(err, persistence.ErrExpired):
			writeError(writer, http.StatusGone, "credential_rotation_expired", "the prepared credential expired; keep using the active credential and prepare again")
		default:
			writeError(writer, http.StatusInternalServerError, "credential_rotation", "could not confirm device credential rotation")
		}
		return
	}
	if !rotation.AlreadyConfirmed {
		s.closeDeviceSocketsAfterCredentialRotation(rotation.DeviceID)
		s.hub.Publish(stream.Event{
			Type: "device.credential_rotated", CorrelationID: rotation.CorrelationID,
			Payload: map[string]any{
				"device_id": rotation.DeviceID, "key_version": rotation.KeyVersion,
				"confirmed_at": time.Now().UTC(),
			},
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"device_id": rotation.DeviceID, "key_version": rotation.KeyVersion,
		"confirmed": true, "already_confirmed": rotation.AlreadyConfirmed,
		"correlation_id": rotation.CorrelationID, "protocol_version": 1,
	})
}

func (s *Server) handleCancelDeviceCredentialRotation(writer http.ResponseWriter, request *http.Request) {
	principal := principalFromContext(request.Context())
	if principal.Kind != "device" {
		writeError(writer, http.StatusForbidden, "device_required", "paired device authentication required")
		return
	}
	cancelled, err := s.store.CancelDeviceCredentialRotation(request.Context(), principal.ID,
		auth.BearerToken(request.Header.Get("Authorization")), ids.New(), ids.New())
	if err != nil {
		if errors.Is(err, persistence.ErrInvalidDeviceCredential) {
			writeError(writer, http.StatusUnauthorized, "credential_stale", "the active device credential changed")
			return
		}
		writeError(writer, http.StatusInternalServerError, "credential_rotation", "could not cancel device credential rotation")
		return
	}
	if cancelled {
		s.hub.Publish(stream.Event{
			Type:    "device.credential_rotation_cancelled",
			Payload: map[string]any{"device_id": principal.ID},
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"device_id": principal.ID, "cancelled": cancelled, "protocol_version": 1,
	})
}

// Rotation uses a distinct close code from revocation so clients reconnect
// with the already-persisted replacement rather than discarding device state.
func (s *Server) closeDeviceSocketsAfterCredentialRotation(deviceID string) {
	s.deviceMu.Lock()
	connections := make([]*websocket.Conn, 0, len(s.deviceSockets[deviceID]))
	for connection := range s.deviceSockets[deviceID] {
		connections = append(connections, connection)
	}
	s.deviceMu.Unlock()
	for _, connection := range connections {
		_ = connection.Close(deviceCredentialRotatedCloseCode, "device credential rotated")
	}
}
