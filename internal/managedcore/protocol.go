// Package managedcore defines the private ownership handshake between the
// desktop supervisor and the Core child started from the same executable.
package managedcore

import (
	"crypto/sha256"
	"encoding/base64"
)

const (
	OwnerTokenEnvironment = "VEQRI_MANAGED_CORE_OWNER_TOKEN"
	OwnerTokenHeader      = "X-Veqri-Managed-Core-Owner"
)

func OwnerProof(token string) string {
	digest := sha256.Sum256([]byte("veqri-managed-core-owner-v1\x00" + token))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
