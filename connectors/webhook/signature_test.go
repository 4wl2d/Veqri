package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var webhookNow = time.Date(2026, time.February, 3, 4, 5, 6, 0, time.UTC)

func webhookSignature(secret, timestamp, nonce string, body []byte) string {
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(digest, "%s.%s.", timestamp, nonce)
	_, _ = digest.Write(body)
	return hex.EncodeToString(digest.Sum(nil))
}

func TestVerifierAcceptsValidHMACAndReturnsNonce(t *testing.T) {
	const (
		secret = "generic-webhook-secret"
		nonce  = "0123456789abcdef"
	)
	body := []byte(`{"event":"created"}`)
	timestamp := strconv.FormatInt(webhookNow.Add(-5*time.Minute).Unix(), 10)
	request := httptest.NewRequest("POST", "/webhook", strings.NewReader(string(body)))
	request.Header.Set("X-Veqri-Timestamp", timestamp)
	request.Header.Set("X-Veqri-Nonce", nonce)
	request.Header.Set("X-Veqri-Signature", webhookSignature(secret, timestamp, nonce, body))

	got, err := (Verifier{Secret: secret, Now: func() time.Time { return webhookNow }}).Verify(request, body)
	if err != nil {
		t.Fatalf("Verify(valid): %v", err)
	}
	if got != nonce {
		t.Fatalf("nonce = %q, want %q", got, nonce)
	}
}

func TestVerifierRejectsInvalidHMACReplayMetadata(t *testing.T) {
	const (
		secret = "generic-webhook-secret"
		nonce  = "0123456789abcdef"
	)
	body := []byte(`{"event":"created"}`)
	validTimestamp := strconv.FormatInt(webhookNow.Unix(), 10)
	validSignature := webhookSignature(secret, validTimestamp, nonce, body)

	tests := []struct {
		name      string
		secret    string
		timestamp string
		nonce     string
		signature string
		body      []byte
	}{
		{name: "missing secret", timestamp: validTimestamp, nonce: nonce, signature: validSignature, body: body},
		{name: "invalid timestamp", secret: secret, timestamp: "invalid", nonce: nonce, signature: validSignature, body: body},
		{name: "outside past window", secret: secret, timestamp: strconv.FormatInt(webhookNow.Add(-5*time.Minute-time.Second).Unix(), 10), nonce: nonce, signature: validSignature, body: body},
		{name: "outside future window", secret: secret, timestamp: strconv.FormatInt(webhookNow.Add(5*time.Minute+time.Second).Unix(), 10), nonce: nonce, signature: validSignature, body: body},
		{name: "nonce too short", secret: secret, timestamp: validTimestamp, nonce: "short", signature: validSignature, body: body},
		{name: "nonce too long", secret: secret, timestamp: validTimestamp, nonce: strings.Repeat("n", 129), signature: validSignature, body: body},
		{name: "body tampering", secret: secret, timestamp: validTimestamp, nonce: nonce, signature: validSignature, body: []byte(`{"event":"deleted"}`)},
		{name: "signature tampering", secret: secret, timestamp: validTimestamp, nonce: nonce, signature: strings.Repeat("0", 64), body: body},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/webhook", strings.NewReader(string(tt.body)))
			request.Header.Set("X-Veqri-Timestamp", tt.timestamp)
			request.Header.Set("X-Veqri-Nonce", tt.nonce)
			request.Header.Set("X-Veqri-Signature", tt.signature)
			got, err := (Verifier{Secret: tt.secret, Now: func() time.Time { return webhookNow }}).Verify(request, tt.body)
			if err == nil || got != "" {
				t.Fatalf("Verify(invalid) = (%q, %v), want empty nonce and error", got, err)
			}
		})
	}
}
