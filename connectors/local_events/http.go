// Package local_events contains local-first adapters applications can use to
// submit structured events to Veqri without depending on core internals.
package local_events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMaxHTTPResponseBytes int64 = 1 << 20
	coreProtocolVersion               = "1"
)

var (
	ErrNonLocalEndpoint = errors.New("local event HTTP endpoint must resolve syntactically to localhost or a loopback IP")
	ErrSigningRequired  = errors.New("local event signing secret is required")
	ErrResponseTooLarge = errors.New("local event HTTP response exceeds the configured limit")
)

// Event is accepted by the core's POST /v1/events endpoint and by the stdio
// decoder in this package.
type Event struct {
	Type            string          `json:"type"`
	Data            json.RawMessage `json:"data"`
	ConversationKey string          `json:"conversation_key,omitempty"`
	IdempotencyKey  string          `json:"idempotency_key,omitempty"`
	CreateTask      bool            `json:"create_task,omitempty"`
}

func (e Event) Validate() error {
	if !validEventType(e.Type) {
		return errors.New("event type must contain 1 to 128 ASCII letters, digits, dots, underscores, colons, or hyphens")
	}
	if len(e.Data) == 0 {
		e.Data = json.RawMessage(`{}`)
	}
	if !json.Valid(e.Data) {
		return errors.New("event data must be valid JSON")
	}
	if len(e.ConversationKey) > 512 || strings.ContainsAny(e.ConversationKey, "\r\n\x00") {
		return errors.New("conversation key is invalid")
	}
	if len(e.IdempotencyKey) > 512 || strings.ContainsAny(e.IdempotencyKey, "\r\n\x00") {
		return errors.New("idempotency key is invalid")
	}
	return nil
}

type Receipt struct {
	Accepted  bool            `json:"accepted"`
	EventID   string          `json:"event_id,omitempty"`
	Duplicate bool            `json:"duplicate,omitempty"`
	Task      json.RawMessage `json:"task,omitempty"`
}

type HTTPConfig struct {
	Endpoint         string
	SigningSecret    string
	BearerToken      string
	Client           *http.Client
	Now              func() time.Time
	Nonce            func() (string, error)
	MaxResponseBytes int64
	Headers          http.Header
}

type HTTPClient struct {
	endpoint    *url.URL
	secret      string
	bearerToken string
	client      *http.Client
	now         func() time.Time
	nonce       func() (string, error)
	maxResponse int64
	headers     http.Header
}

func NewHTTPClient(config HTTPConfig) (*HTTPClient, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, errors.New("local event endpoint must be an absolute HTTP(S) URL without user info or a fragment")
	}
	if !isLoopbackHost(endpoint.Hostname()) {
		return nil, ErrNonLocalEndpoint
	}
	if strings.TrimSpace(config.SigningSecret) == "" {
		return nil, ErrSigningRequired
	}
	if strings.ContainsAny(config.BearerToken, "\r\n") {
		return nil, errors.New("local event bearer token is invalid")
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = defaultMaxHTTPResponseBytes
	}
	if config.MaxResponseBytes < 1 || config.MaxResponseBytes > 1<<30 {
		return nil, errors.New("local event response limit must be between 1 byte and 1 GiB")
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	clientCopy := *client
	// Local event credentials and signatures must never follow a redirect.
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	now := config.Now
	if now == nil {
		now = time.Now
	}
	nonce := config.Nonce
	if nonce == nil {
		nonce = secureNonce
	}
	return &HTTPClient{
		endpoint: endpoint, secret: config.SigningSecret,
		bearerToken: strings.TrimSpace(config.BearerToken), client: &clientCopy,
		now: now, nonce: nonce, maxResponse: config.MaxResponseBytes,
		headers: cloneHTTPHeaders(config.Headers),
	}, nil
}

func (c *HTTPClient) Emit(ctx context.Context, event Event) (Receipt, error) {
	if err := event.Validate(); err != nil {
		return Receipt{}, err
	}
	if len(event.Data) == 0 {
		event.Data = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(event)
	if err != nil {
		return Receipt{}, fmt.Errorf("encode local event: %w", err)
	}
	nonce, err := c.nonce()
	if err != nil {
		return Receipt{}, fmt.Errorf("create local event nonce: %w", err)
	}
	if len(nonce) < 16 || len(nonce) > 128 || strings.ContainsAny(nonce, "\r\n") {
		return Receipt{}, errors.New("local event nonce must contain 16 to 128 header-safe characters")
	}
	timestamp := strconv.FormatInt(c.now().UTC().Unix(), 10)
	signature := sign(c.secret, timestamp, nonce, body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Receipt{}, fmt.Errorf("create local event request: %w", err)
	}
	copyHTTPHeaders(request.Header, c.headers)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Veqri-Protocol-Version", coreProtocolVersion)
	request.Header.Set("X-Veqri-Timestamp", timestamp)
	request.Header.Set("X-Veqri-Nonce", nonce)
	request.Header.Set("X-Veqri-Signature", signature)
	request.Header.Set("X-Veqri-Signature-Version", "v1")
	if c.bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}
	if event.IdempotencyKey != "" {
		request.Header.Set("Idempotency-Key", event.IdempotencyKey)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return Receipt{}, fmt.Errorf("submit local event: %w", err)
	}
	defer response.Body.Close()
	bodyReader := io.LimitReader(response.Body, c.maxResponse+1)
	encoded, err := io.ReadAll(bodyReader)
	if err != nil {
		return Receipt{}, fmt.Errorf("read local event response: %w", err)
	}
	if int64(len(encoded)) > c.maxResponse {
		return Receipt{}, ErrResponseTooLarge
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Receipt{}, fmt.Errorf("local event endpoint returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(encoded)))
	}
	var receipt Receipt
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := decoder.Decode(&receipt); err != nil {
		return Receipt{}, fmt.Errorf("decode local event receipt: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Receipt{}, errors.New("local event response must contain exactly one JSON receipt")
	}
	if !receipt.Accepted {
		return Receipt{}, errors.New("local event endpoint did not accept the event")
	}
	return receipt, nil
}

func sign(secret, timestamp, nonce string, body []byte) string {
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(digest, "%s.%s.", timestamp, nonce)
	_, _ = digest.Write(body)
	return hex.EncodeToString(digest.Sum(nil))
}

func secureNonce() (string, error) {
	value := make([]byte, 18)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validEventType(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._:-", character) {
			continue
		}
		return false
	}
	return true
}

func cloneHTTPHeaders(input http.Header) http.Header {
	result := make(http.Header, len(input))
	copyHTTPHeaders(result, input)
	return result
}

func copyHTTPHeaders(target, source http.Header) {
	for name, values := range source {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Content-Length") || strings.HasPrefix(strings.ToLower(name), "x-veqri-") || strings.ContainsAny(name, "\r\n") {
			continue
		}
		for _, value := range values {
			if !strings.ContainsAny(value, "\r\n") {
				target.Add(name, value)
			}
		}
	}
}
