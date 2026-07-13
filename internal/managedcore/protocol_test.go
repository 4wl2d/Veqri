package managedcore

import (
	"strings"
	"testing"
)

func TestOwnerProofIsDeterministicAndDoesNotExposeToken(t *testing.T) {
	const token = "managed-owner-token-0123456789abcdef"
	first := OwnerProof(token)
	if first == "" || first != OwnerProof(token) {
		t.Fatalf("OwnerProof() is not deterministic: %q", first)
	}
	if strings.Contains(first, token) || first == OwnerProof(token+"-other") {
		t.Fatal("OwnerProof() exposed or ignored the owner token")
	}
}
