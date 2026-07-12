package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maximumTimestampSkew = 5 * time.Minute

type SignatureVerifier struct {
	SigningSecret string
	Now           func() time.Time
}

func (v SignatureVerifier) Verify(request *http.Request, rawBody []byte) error {
	if v.SigningSecret == "" {
		return errors.New("Slack signing secret is not configured")
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	timestampText := request.Header.Get("X-Slack-Request-Timestamp")
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil {
		return errors.New("invalid Slack request timestamp")
	}
	requestTime := time.Unix(timestamp, 0)
	skew := now().Sub(requestTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > maximumTimestampSkew {
		return errors.New("Slack request timestamp is outside the five-minute replay window")
	}
	supplied := request.Header.Get("X-Slack-Signature")
	if !strings.HasPrefix(supplied, "v0=") {
		return errors.New("invalid Slack signature version")
	}
	digest := hmac.New(sha256.New, []byte(v.SigningSecret))
	_, _ = fmt.Fprintf(digest, "v0:%s:", timestampText)
	_, _ = digest.Write(rawBody)
	expected := "v0=" + hex.EncodeToString(digest.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(supplied)) {
		return errors.New("Slack signature mismatch")
	}
	return nil
}
