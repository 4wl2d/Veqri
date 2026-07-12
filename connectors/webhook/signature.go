package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

type Verifier struct {
	Secret string
	Now    func() time.Time
}

func (v Verifier) Verify(request *http.Request, rawBody []byte) (nonce string, err error) {
	if v.Secret == "" {
		return "", errors.New("webhook secret is not configured")
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	timestampText := request.Header.Get("X-Veqri-Timestamp")
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil {
		return "", errors.New("invalid webhook timestamp")
	}
	skew := now().Sub(time.Unix(timestamp, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > 5*time.Minute {
		return "", errors.New("webhook timestamp is outside replay window")
	}
	nonce = request.Header.Get("X-Veqri-Nonce")
	if len(nonce) < 16 || len(nonce) > 128 {
		return "", errors.New("webhook nonce must contain 16 to 128 characters")
	}
	digest := hmac.New(sha256.New, []byte(v.Secret))
	_, _ = fmt.Fprintf(digest, "%s.%s.", timestampText, nonce)
	_, _ = digest.Write(rawBody)
	expected := hex.EncodeToString(digest.Sum(nil))
	supplied := request.Header.Get("X-Veqri-Signature")
	if !hmac.Equal([]byte(expected), []byte(supplied)) {
		return "", errors.New("webhook signature mismatch")
	}
	return nonce, nil
}
