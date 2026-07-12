// Package remote implements an authenticated HTTP agent runtime.
//
// The wire protocol is deliberately small. A run is a POST containing a
// Request. Servers may respond with one JSON Result or a stream of Frame values
// using NDJSON, JSON-seq, or server-sent events. Cancelling the Run context
// cancels the in-flight request and, when CancelURL is configured, sends a
// bounded best-effort cancellation request containing the task ID.
package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	coreagents "github.com/veqri/veqri/core/agents"
	"github.com/veqri/veqri/core/tasks"
)

const (
	defaultMaxResponseBytes int64 = 4 << 20
	defaultMaxFrameBytes          = 256 << 10
	defaultCancelTimeout          = 3 * time.Second
)

var (
	ErrAuthenticationRequired = errors.New("remote agent authentication is required")
	ErrInsecureEndpoint       = errors.New("remote agent endpoint must use HTTPS (HTTP is allowed only for loopback when explicitly enabled)")
	ErrResponseTooLarge       = errors.New("remote agent response exceeds the configured limit")
	ErrProtocol               = errors.New("remote agent protocol error")
)

type HTTPStatusError struct {
	StatusCode int
}

func (e HTTPStatusError) Error() string {
	return fmt.Sprintf("remote agent returned HTTP %d", e.StatusCode)
}

// TokenSource obtains a bearer token at request time. Implementations may use
// an OS keychain and must honor cancellation.
type TokenSource interface {
	Token(context.Context) (string, error)
}

type TokenSourceFunc func(context.Context) (string, error)

func (f TokenSourceFunc) Token(ctx context.Context) (string, error) {
	if f == nil {
		return "", ErrAuthenticationRequired
	}
	return f(ctx)
}

type Config struct {
	Endpoint              string
	CancelURL             string
	Definition            coreagents.Definition
	Client                *http.Client
	TokenSource           TokenSource
	StaticBearerToken     string
	AllowInsecureLoopback bool
	MaxResponseBytes      int64
	MaxFrameBytes         int
	CancelTimeout         time.Duration
	Headers               http.Header
}

type Runner struct {
	endpoint      *url.URL
	cancelURL     *url.URL
	definition    coreagents.Definition
	client        *http.Client
	tokenSource   TokenSource
	staticToken   string
	maxResponse   int64
	maxFrame      int
	cancelTimeout time.Duration
	headers       http.Header
}

var _ coreagents.Runner = (*Runner)(nil)

// Request is the stable request envelope accepted by remote agent servers.
type Request struct {
	Version int        `json:"version"`
	Task    tasks.Task `json:"task"`
}

type CancelRequest struct {
	Version int    `json:"version"`
	TaskID  string `json:"task_id"`
}

// Frame is used for NDJSON, JSON-seq, and SSE streaming responses.
type Frame struct {
	Type     string               `json:"type"`
	Progress *coreagents.Progress `json:"progress,omitempty"`
	Result   *coreagents.Result   `json:"result,omitempty"`
	Error    string               `json:"error,omitempty"`
}

func New(config Config) (*Runner, error) {
	endpoint, err := parseEndpoint(config.Endpoint, config.AllowInsecureLoopback)
	if err != nil {
		return nil, err
	}
	var cancelURL *url.URL
	if strings.TrimSpace(config.CancelURL) != "" {
		cancelURL, err = parseEndpoint(config.CancelURL, config.AllowInsecureLoopback)
		if err != nil {
			return nil, fmt.Errorf("invalid remote agent cancel URL: %w", err)
		}
	}
	if config.TokenSource == nil && strings.TrimSpace(config.StaticBearerToken) == "" {
		return nil, ErrAuthenticationRequired
	}
	if config.TokenSource != nil && strings.TrimSpace(config.StaticBearerToken) != "" {
		return nil, errors.New("configure either a token source or a static bearer token, not both")
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = defaultMaxResponseBytes
	}
	if config.MaxResponseBytes < 1 || config.MaxResponseBytes > 1<<30 {
		return nil, errors.New("remote agent response limit must be between 1 byte and 1 GiB")
	}
	if config.MaxFrameBytes == 0 {
		config.MaxFrameBytes = defaultMaxFrameBytes
		if int64(config.MaxFrameBytes) > config.MaxResponseBytes {
			config.MaxFrameBytes = int(config.MaxResponseBytes)
		}
	}
	if config.MaxFrameBytes < 1 || int64(config.MaxFrameBytes) > config.MaxResponseBytes {
		return nil, errors.New("remote agent frame limit must be positive and no larger than the response limit")
	}
	if config.CancelTimeout == 0 {
		config.CancelTimeout = defaultCancelTimeout
	}
	if config.CancelTimeout < time.Millisecond || config.CancelTimeout > time.Minute {
		return nil, errors.New("remote agent cancel timeout must be between 1 millisecond and 1 minute")
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: 0}
	}
	clientCopy := *client
	// Redirects are disabled so bearer credentials never follow an endpoint
	// change or an HTTPS-to-HTTP downgrade. Configure the final run URL.
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	definition := config.Definition
	if definition.ID == "" {
		definition.ID = "remote-http"
	}
	if definition.DisplayName == "" {
		definition.DisplayName = definition.ID
	}
	definition.ExecutionMode = coreagents.ModeHTTP
	// Request-context cancellation is always supported. CancelURL additionally
	// provides cooperative server-side cancellation after transport teardown.
	definition.SupportsCancellation = true
	definition.SupportsStreaming = true
	if definition.Health == "" {
		definition.Health = coreagents.HealthUnknown
	}
	definition.UpdatedAt = time.Now().UTC()
	return &Runner{
		endpoint: endpoint, cancelURL: cancelURL, definition: definition,
		client: &clientCopy, tokenSource: config.TokenSource,
		staticToken: strings.TrimSpace(config.StaticBearerToken),
		maxResponse: config.MaxResponseBytes, maxFrame: config.MaxFrameBytes,
		cancelTimeout: config.CancelTimeout, headers: cloneHeaders(config.Headers),
	}, nil
}

func (r *Runner) Definition() coreagents.Definition { return r.definition }

func (r *Runner) Run(ctx context.Context, task tasks.Task, progress func(coreagents.Progress)) (coreagents.Result, error) {
	if err := ctx.Err(); err != nil {
		return coreagents.Result{}, err
	}
	body, err := json.Marshal(Request{Version: 1, Task: task})
	if err != nil {
		return coreagents.Result{}, fmt.Errorf("encode remote agent request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return coreagents.Result{}, fmt.Errorf("create remote agent request: %w", err)
	}
	if err := r.authorize(ctx, req); err != nil {
		return coreagents.Result{}, err
	}
	copyHeaders(req.Header, r.headers)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, application/x-ndjson, application/json-seq, text/event-stream")
	req.Header.Set("X-Veqri-Agent-Protocol", "1")

	response, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			r.bestEffortCancel(task.ID)
			return coreagents.Result{}, ctx.Err()
		}
		return coreagents.Result{}, fmt.Errorf("call remote agent: %w", err)
	}
	defer response.Body.Close()
	limited := &countingReader{reader: response.Body, remaining: r.maxResponse}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, readErr := io.Copy(io.Discard, limited)
		if errors.Is(readErr, ErrResponseTooLarge) {
			return coreagents.Result{}, readErr
		}
		return coreagents.Result{}, HTTPStatusError{StatusCode: response.StatusCode}
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	var result coreagents.Result
	switch mediaType {
	case "application/x-ndjson", "application/ndjson", "application/json-seq":
		result, err = r.decodeNDJSON(limited, progress)
	case "text/event-stream":
		result, err = r.decodeSSE(limited, progress)
	default:
		decoder := json.NewDecoder(limited)
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&result)
		if err == nil {
			var extra any
			if extraErr := decoder.Decode(&extra); !errors.Is(extraErr, io.EOF) {
				err = errors.New("response must contain exactly one JSON result")
			}
		}
	}
	if ctx.Err() != nil {
		r.bestEffortCancel(task.ID)
		return coreagents.Result{}, ctx.Err()
	}
	if err != nil {
		return coreagents.Result{}, fmt.Errorf("%w: %w", ErrProtocol, err)
	}
	if err := validateResult(result); err != nil {
		return coreagents.Result{}, fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	return result, nil
}

func (r *Runner) Retryable(runErr error) bool {
	if errors.Is(runErr, ErrAuthenticationRequired) || errors.Is(runErr, ErrResponseTooLarge) ||
		errors.Is(runErr, ErrProtocol) || errors.Is(runErr, context.Canceled) ||
		errors.Is(runErr, context.DeadlineExceeded) {
		return false
	}
	var statusErr HTTPStatusError
	if errors.As(runErr, &statusErr) {
		return statusErr.StatusCode == http.StatusRequestTimeout || statusErr.StatusCode == http.StatusTooEarly ||
			statusErr.StatusCode == http.StatusTooManyRequests || statusErr.StatusCode >= 500
	}
	var networkErr net.Error
	return errors.As(runErr, &networkErr)
}

func (r *Runner) decodeNDJSON(reader io.Reader, progress func(coreagents.Progress)) (coreagents.Result, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), r.maxFrame)
	var result *coreagents.Result
	for scanner.Scan() {
		data := bytes.TrimSpace(bytes.TrimPrefix(scanner.Bytes(), []byte{0x1e}))
		if len(data) == 0 {
			continue
		}
		if err := applyFrame(data, progress, &result); err != nil {
			return coreagents.Result{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		if strings.Contains(err.Error(), "token too long") {
			return coreagents.Result{}, ErrResponseTooLarge
		}
		return coreagents.Result{}, err
	}
	if result == nil {
		return coreagents.Result{}, errors.New("stream ended without a result frame")
	}
	return *result, nil
}

func (r *Runner) decodeSSE(reader io.Reader, progress func(coreagents.Progress)) (coreagents.Result, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), r.maxFrame)
	var data bytes.Buffer
	var result *coreagents.Result
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
		data.Reset()
		return applyFrame(payload, progress, &result)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return coreagents.Result{}, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if data.Len()+len(value)+1 > r.maxFrame {
				return coreagents.Result{}, ErrResponseTooLarge
			}
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return coreagents.Result{}, err
	}
	if err := flush(); err != nil {
		return coreagents.Result{}, err
	}
	if result == nil {
		return coreagents.Result{}, errors.New("stream ended without a result frame")
	}
	return *result, nil
}

func applyFrame(data []byte, progress func(coreagents.Progress), result **coreagents.Result) error {
	var frame Frame
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&frame); err != nil {
		return fmt.Errorf("decode stream frame: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("stream frame must contain exactly one JSON object")
	}
	switch frame.Type {
	case "progress":
		if frame.Progress == nil || frame.Result != nil || frame.Error != "" {
			return errors.New("invalid progress frame")
		}
		if frame.Progress.Percent < 0 || frame.Progress.Percent > 100 {
			return errors.New("progress percent must be between 0 and 100")
		}
		if progress != nil {
			progress(*frame.Progress)
		}
	case "result":
		if frame.Result == nil || frame.Progress != nil || frame.Error != "" || *result != nil {
			return errors.New("invalid or duplicate result frame")
		}
		copy := *frame.Result
		*result = &copy
	case "error":
		if strings.TrimSpace(frame.Error) == "" || frame.Progress != nil || frame.Result != nil {
			return errors.New("invalid error frame")
		}
		return errors.New(frame.Error)
	default:
		return fmt.Errorf("unsupported frame type %q", frame.Type)
	}
	return nil
}

func validateResult(result coreagents.Result) error {
	if len(result.Structured) > 0 && !json.Valid(result.Structured) {
		return errors.New("result.structured is not valid JSON")
	}
	if len(result.Structured) == 0 && strings.TrimSpace(result.WrittenSummary) == "" && strings.TrimSpace(result.SpokenSummary) == "" {
		return errors.New("result is empty")
	}
	return nil
}

func (r *Runner) authorize(ctx context.Context, request *http.Request) error {
	token := r.staticToken
	if r.tokenSource != nil {
		var err error
		token, err = r.tokenSource.Token(ctx)
		if err != nil {
			return fmt.Errorf("obtain remote agent token: %w", err)
		}
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return ErrAuthenticationRequired
	}
	request.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (r *Runner) bestEffortCancel(taskID string) {
	if r.cancelURL == nil || taskID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.cancelTimeout)
	defer cancel()
	body, err := json.Marshal(CancelRequest{Version: 1, TaskID: taskID})
	if err != nil {
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cancelURL.String(), bytes.NewReader(body))
	if err != nil || r.authorize(ctx, request) != nil {
		return
	}
	copyHeaders(request.Header, r.headers)
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		_ = response.Body.Close()
	}
}

func parseEndpoint(raw string, allowLoopback bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawFragment != "" {
		return nil, errors.New("remote agent endpoint must be an absolute URL without user info or a fragment")
	}
	if parsed.Scheme != "https" {
		if parsed.Scheme != "http" || !allowLoopback || !loopbackHost(parsed.Hostname()) {
			return nil, ErrInsecureEndpoint
		}
	}
	return parsed, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type countingReader struct {
	reader    io.Reader
	remaining int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	if r.remaining < 0 {
		return 0, ErrResponseTooLarge
	}
	max := int64(len(buffer))
	if max > r.remaining+1 {
		max = r.remaining + 1
	}
	n, err := r.reader.Read(buffer[:max])
	if int64(n) > r.remaining {
		r.remaining = -1
		return 0, ErrResponseTooLarge
	}
	r.remaining -= int64(n)
	return n, err
}

func cloneHeaders(input http.Header) http.Header {
	result := make(http.Header, len(input))
	copyHeaders(result, input)
	return result
}

func copyHeaders(target, source http.Header) {
	for name, values := range source {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Content-Length") || strings.ContainsAny(name, "\r\n") {
			continue
		}
		for _, value := range values {
			if !strings.ContainsAny(value, "\r\n") {
				target.Add(name, value)
			}
		}
	}
}
