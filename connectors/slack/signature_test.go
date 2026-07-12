package slack

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

var slackNow = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

func slackSignature(secret, timestamp string, body []byte) string {
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(digest, "v0:%s:", timestamp)
	_, _ = digest.Write(body)
	return "v0=" + hex.EncodeToString(digest.Sum(nil))
}

func TestSignatureVerifierAcceptsValidHMACWithinReplayWindow(t *testing.T) {
	const secret = "slack-signing-secret"
	body := []byte(`token=x&team_id=T1&text=hello`)
	timestamp := strconv.FormatInt(slackNow.Add(-maximumTimestampSkew).Unix(), 10)
	request := httptest.NewRequest("POST", "/slack", strings.NewReader(string(body)))
	request.Header.Set("X-Slack-Request-Timestamp", timestamp)
	request.Header.Set("X-Slack-Signature", slackSignature(secret, timestamp, body))

	verifier := SignatureVerifier{SigningSecret: secret, Now: func() time.Time { return slackNow }}
	if err := verifier.Verify(request, body); err != nil {
		t.Fatalf("Verify(valid): %v", err)
	}
}

func TestSignatureVerifierRejectsTamperingAndInvalidMetadata(t *testing.T) {
	const secret = "slack-signing-secret"
	body := []byte(`{"text":"hello"}`)
	timestamp := strconv.FormatInt(slackNow.Unix(), 10)
	validSignature := slackSignature(secret, timestamp, body)

	tests := []struct {
		name      string
		secret    string
		timestamp string
		signature string
		body      []byte
	}{
		{name: "missing secret", timestamp: timestamp, signature: validSignature, body: body},
		{name: "invalid timestamp", secret: secret, timestamp: "not-a-time", signature: validSignature, body: body},
		{name: "stale request", secret: secret, timestamp: strconv.FormatInt(slackNow.Add(-maximumTimestampSkew-time.Second).Unix(), 10), signature: validSignature, body: body},
		{name: "future request", secret: secret, timestamp: strconv.FormatInt(slackNow.Add(maximumTimestampSkew+time.Second).Unix(), 10), signature: validSignature, body: body},
		{name: "wrong version", secret: secret, timestamp: timestamp, signature: strings.TrimPrefix(validSignature, "v0="), body: body},
		{name: "body tampering", secret: secret, timestamp: timestamp, signature: validSignature, body: []byte(`{"text":"admin"}`)},
		{name: "signature tampering", secret: secret, timestamp: timestamp, signature: "v0=" + strings.Repeat("0", 64), body: body},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/slack", strings.NewReader(string(tt.body)))
			request.Header.Set("X-Slack-Request-Timestamp", tt.timestamp)
			request.Header.Set("X-Slack-Signature", tt.signature)
			verifier := SignatureVerifier{SigningSecret: tt.secret, Now: func() time.Time { return slackNow }}
			if err := verifier.Verify(request, tt.body); err == nil {
				t.Fatal("Verify() accepted an invalid request")
			}
		})
	}
}
