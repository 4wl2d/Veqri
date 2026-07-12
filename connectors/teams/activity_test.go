package teams

import (
	"context"
	"errors"
	"testing"
	"time"
)

type recordingVerifier struct {
	err        error
	calls      int
	authHeader string
	serviceURL string
}

func (v *recordingVerifier) Verify(_ context.Context, authorizationHeader, expectedServiceURL string) error {
	v.calls++
	v.authHeader = authorizationHeader
	v.serviceURL = expectedServiceURL
	return v.err
}

func teamsActivityJSON() []byte {
	return []byte(`{
		"type":"message",
		"id":"activity-1",
		"timestamp":"2026-03-04T05:06:07Z",
		"serviceUrl":"https://smba.trafficmanager.net/example/",
		"text":"hello",
		"from":{"id":"user-1","name":"Ada"},
		"conversation":{"id":"conversation-1","tenantId":"tenant-1"},
		"channelId":"msteams"
	}`)
}

func TestVerifyAndNormalizeFailsClosedWithoutJWTVerifier(t *testing.T) {
	_, err := VerifyAndNormalize(context.Background(), nil, "Bearer token", "teams-1", teamsActivityJSON(), time.Date(2026, 3, 4, 5, 6, 8, 0, time.UTC))
	if err == nil {
		t.Fatal("Teams activity was accepted without a JWT verifier")
	}
}

func TestVerifyAndNormalizePropagatesVerifierFailure(t *testing.T) {
	want := errors.New("invalid JWT audience")
	verifier := &recordingVerifier{err: want}
	_, err := VerifyAndNormalize(context.Background(), verifier, "Bearer bad", "teams-1", teamsActivityJSON(), time.Date(2026, 3, 4, 5, 6, 8, 0, time.UTC))
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want verifier error", err)
	}
	if verifier.calls != 1 || verifier.serviceURL != "https://smba.trafficmanager.net/example/" {
		t.Fatalf("verifier call = %+v", verifier)
	}
}

func TestVerifyAndNormalizePassesExpectedJWTContext(t *testing.T) {
	verifier := &recordingVerifier{}
	receivedAt := time.Date(2026, 3, 4, 5, 6, 8, 0, time.UTC)
	event, err := VerifyAndNormalize(context.Background(), verifier, "Bearer good", "teams-1", teamsActivityJSON(), receivedAt)
	if err != nil {
		t.Fatalf("VerifyAndNormalize(): %v", err)
	}
	if verifier.calls != 1 || verifier.authHeader != "Bearer good" || verifier.serviceURL != "https://smba.trafficmanager.net/example/" {
		t.Fatalf("verifier received %+v", verifier)
	}
	if event.Source.InstanceID != "tenant-1" || event.IdempotencyKey != "tenant-1:conversation-1:activity-1" {
		t.Fatalf("normalized event identity = %+v", event)
	}
	if event.ReceivedAt != receivedAt || event.ConversationKey != "teams:tenant-1:conversation-1" {
		t.Fatalf("normalized event routing = %+v", event)
	}
}

func TestVerifyAndNormalizeRejectsUnsupportedActivityBeforeVerification(t *testing.T) {
	verifier := &recordingVerifier{}
	raw := []byte(`{"type":"typing","id":"activity-1","conversation":{"id":"conversation-1"}}`)
	if _, err := VerifyAndNormalize(context.Background(), verifier, "Bearer token", "teams-1", raw, time.Date(2026, 3, 4, 5, 6, 8, 0, time.UTC)); err == nil {
		t.Fatal("unsupported activity was accepted")
	}
	if verifier.calls != 0 {
		t.Fatal("JWT verifier called for structurally unsupported activity")
	}
}
