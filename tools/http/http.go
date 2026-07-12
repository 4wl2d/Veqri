// Package http implements Veqri's policy-ready outbound HTTP tool.
//
// The executor is deliberately not a general-purpose net/http wrapper. Every
// request is constrained by a hostname and method allowlist, bounded in size,
// and sent through a transport which resolves and validates the destination
// immediately before dialing a concrete IP address. Authentication material is
// accepted only through SecretReference values resolved at execution time.
package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	coretools "github.com/veqri/veqri/core/tools"
)

const (
	DefaultTimeout                = 30 * time.Second
	DefaultMaxRequestBytes  int64 = 1 << 20 // 1 MiB, including headers and URL.
	DefaultMaxResponseBytes int64 = 4 << 20 // 4 MiB after transparent decoding.
	DefaultMaxHeaderBytes   int64 = 64 << 10
	DefaultMaxRedirects           = 5

	maxURLBytes         = 8 << 10
	maxHeaderCount      = 128
	maxSecretReferences = 16
	maxSecretBytes      = 16 << 10
	maxResolvedIPs      = 32
	maxConfiguredBytes  = 1 << 30
)

var (
	ErrDomainNotAllowed       = errors.New("HTTP destination domain is not allowed")
	ErrMethodNotAllowed       = errors.New("HTTP method is not allowed")
	ErrUnsafeAddress          = errors.New("HTTP destination resolves to an unsafe address")
	ErrPlaintextSecret        = errors.New("plaintext authentication material is not allowed")
	ErrRequestTooLarge        = errors.New("HTTP request exceeds the configured size limit")
	ErrSecretResolverRequired = errors.New("secret references require a secret resolver")
	ErrSecretResolution       = errors.New("secret reference could not be resolved")
	ErrTooManyRedirects       = errors.New("HTTP redirect limit exceeded")
	ErrRedirectDowngrade      = errors.New("HTTPS to HTTP redirect is not allowed")
	ErrRequestFailed          = errors.New("HTTP request failed")
)

// Resolver is the narrow subset of net.Resolver used by the executor.
// Implementations must respect context cancellation.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// SecretResolver exchanges an opaque reference for a secret only when an
// already-authorized invocation is executed. Implementations should obtain the
// value from an OS keychain or an equivalently protected store.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, reference string) (string, error)
}

type SecretResolverFunc func(context.Context, string) (string, error)

func (f SecretResolverFunc) ResolveSecret(ctx context.Context, reference string) (string, error) {
	if f == nil {
		return "", errors.New("nil secret resolver")
	}
	return f(ctx, reference)
}

type DialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Config contains trusted administrator configuration, not agent-provided
// input. AllowedCIDRs are explicit exceptions to the special/private address
// denylist and should normally be empty. They exist for intentional local
// integrations and deterministic tests; an allowed CIDR never bypasses the
// domain allowlist.
type Config struct {
	AllowedDomains         []string
	AllowedMethods         []string
	AllowedPorts           []int
	AllowedCIDRs           []string
	Timeout                time.Duration
	MaxRequestBytes        int64
	MaxResponseBytes       int64
	MaxResponseHeaderBytes int64
	MaxRedirects           int
	DisableRedirects       bool
	Resolver               Resolver
	DialContext            DialContextFunc
	UserAgent              string
}

// SecretReference describes one header populated from protected storage.
// Prefix is useful for schemes such as "Bearer "; it is configuration data,
// never the secret itself.
type SecretReference struct {
	Reference string `json:"reference"`
	Header    string `json:"header"`
	Prefix    string `json:"prefix,omitempty"`
}

// Input is the strict JSON contract accepted by Executor.Execute.
// Body and BodyBase64 are mutually exclusive.
type Input struct {
	Method         string            `json:"method"`
	URL            string            `json:"url"`
	Headers        map[string]string `json:"headers,omitempty"`
	Body           string            `json:"body,omitempty"`
	BodyBase64     string            `json:"body_base64,omitempty"`
	SecretHeaders  []SecretReference `json:"secret_headers,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	DryRun         bool              `json:"dry_run,omitempty"`
}

// Assessment can be persisted or passed to the policy engine before Execute.
// It never resolves or contains secret values.
type Assessment struct {
	Method            string         `json:"method"`
	URL               string         `json:"url"`
	Risk              coretools.Risk `json:"risk"`
	ApprovalRequired  bool           `json:"approval_required"`
	RequiredScopes    []string       `json:"required_scopes"`
	SecretReferences  []string       `json:"secret_references,omitempty"`
	RequestBodyBytes  int64          `json:"request_body_bytes"`
	RequestBodySHA256 string         `json:"request_body_sha256"`
}

// Hop is an audit-safe account of one request in a redirect chain. Query
// values are redacted in URL. ResolvedIPs contain only validated addresses.
type Hop struct {
	Method      string   `json:"method"`
	URL         string   `json:"url"`
	ResolvedIPs []string `json:"resolved_ips,omitempty"`
	ConnectedIP string   `json:"connected_ip,omitempty"`
	StatusCode  int      `json:"status_code,omitempty"`
	DurationMS  int64    `json:"duration_ms"`
	ErrorCode   string   `json:"error_code,omitempty"`
}

// Output is structured for both callers and an append-only audit record. It
// does not echo request headers or bodies, and query values are redacted.
type Output struct {
	Method                    string              `json:"method"`
	URL                       string              `json:"url"`
	Risk                      coretools.Risk      `json:"risk"`
	ApprovalRequired          bool                `json:"approval_required"`
	RequiredScopes            []string            `json:"required_scopes"`
	SecretReferences          []string            `json:"secret_references,omitempty"`
	RequestBodyBytes          int64               `json:"request_body_bytes"`
	RequestBodySHA256         string              `json:"request_body_sha256"`
	StatusCode                int                 `json:"status_code,omitempty"`
	Status                    string              `json:"status,omitempty"`
	Headers                   map[string][]string `json:"headers,omitempty"`
	RedactedResponseHeaders   []string            `json:"redacted_response_headers,omitempty"`
	Body                      string              `json:"body,omitempty"`
	BodyBase64                string              `json:"body_base64,omitempty"`
	BodyEncoding              string              `json:"body_encoding,omitempty"`
	BodyBytes                 int64               `json:"body_bytes"`
	BodySHA256                string              `json:"body_sha256,omitempty"`
	DeclaredResponseBodyBytes int64               `json:"declared_response_body_bytes,omitempty"`
	Truncated                 bool                `json:"truncated"`
	TimedOut                  bool                `json:"timed_out"`
	Cancelled                 bool                `json:"cancelled"`
	DryRun                    bool                `json:"dry_run"`
	UntrustedContent          bool                `json:"untrusted_content"`
	StartedAt                 time.Time           `json:"started_at"`
	FinishedAt                time.Time           `json:"finished_at"`
	DurationMS                int64               `json:"duration_ms"`
	Hops                      []Hop               `json:"hops,omitempty"`
	ErrorCode                 string              `json:"error_code,omitempty"`
}

type Executor struct {
	allowedDomains   []domainRule
	allowedMethods   map[string]struct{}
	allowedPorts     map[int]struct{}
	allowedCIDRs     []ipPrefix
	timeout          time.Duration
	maxRequest       int64
	maxResponse      int64
	maxHeaders       int64
	maxRedirects     int
	disableRedirects bool
	resolver         Resolver
	dialContext      DialContextFunc
	secretResolver   SecretResolver
	userAgent        string
}

// New constructs an executor. Passing no SecretResolver is valid until an
// invocation contains secret_headers. At most one resolver may be supplied.
func New(config Config, secretResolvers ...SecretResolver) (*Executor, error) {
	if len(secretResolvers) > 1 {
		return nil, errors.New("at most one HTTP secret resolver may be configured")
	}
	if len(config.AllowedDomains) == 0 {
		return nil, errors.New("at least one HTTP destination domain is required")
	}

	domains := make([]domainRule, 0, len(config.AllowedDomains))
	seenDomains := make(map[string]struct{})
	for _, value := range config.AllowedDomains {
		rule, err := parseDomainRule(value)
		if err != nil {
			return nil, fmt.Errorf("invalid allowed domain %q: %w", value, err)
		}
		key := rule.String()
		if _, exists := seenDomains[key]; exists {
			continue
		}
		seenDomains[key] = struct{}{}
		domains = append(domains, rule)
	}

	methods := config.AllowedMethods
	if len(methods) == 0 {
		methods = []string{stdhttp.MethodGet, stdhttp.MethodHead}
	}
	allowedMethods := make(map[string]struct{}, len(methods))
	for _, method := range methods {
		method = strings.ToUpper(strings.TrimSpace(method))
		if !validToken(method) {
			return nil, fmt.Errorf("invalid allowed HTTP method %q", method)
		}
		allowedMethods[method] = struct{}{}
	}

	ports := make(map[int]struct{}, len(config.AllowedPorts))
	for _, port := range config.AllowedPorts {
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid allowed HTTP port %d", port)
		}
		ports[port] = struct{}{}
	}

	cidrs, err := parseAllowedCIDRs(config.AllowedCIDRs)
	if err != nil {
		return nil, err
	}

	timeout := config.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if timeout < time.Millisecond || timeout > 24*time.Hour {
		return nil, errors.New("HTTP timeout must be between 1 millisecond and 24 hours")
	}
	maxRequest, err := configuredLimit(config.MaxRequestBytes, DefaultMaxRequestBytes, "request")
	if err != nil {
		return nil, err
	}
	maxResponse, err := configuredLimit(config.MaxResponseBytes, DefaultMaxResponseBytes, "response")
	if err != nil {
		return nil, err
	}
	maxHeaders, err := configuredLimit(config.MaxResponseHeaderBytes, DefaultMaxHeaderBytes, "response header")
	if err != nil {
		return nil, err
	}
	maxRedirects := config.MaxRedirects
	if maxRedirects == 0 {
		maxRedirects = DefaultMaxRedirects
	}
	if maxRedirects < 0 || maxRedirects > 20 {
		return nil, errors.New("max redirects must be between 0 and 20")
	}
	if config.UserAgent != "" && (!validHeaderValue(config.UserAgent) || len(config.UserAgent) > 256) {
		return nil, errors.New("invalid HTTP user agent")
	}

	resolver := config.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dialContext := config.DialContext
	if dialContext == nil {
		dialer := &net.Dialer{Timeout: minDuration(10*time.Second, timeout), KeepAlive: 30 * time.Second}
		dialContext = dialer.DialContext
	}
	var secretResolver SecretResolver
	if len(secretResolvers) == 1 {
		secretResolver = secretResolvers[0]
	}
	return &Executor{
		allowedDomains: domains, allowedMethods: allowedMethods, allowedPorts: ports,
		allowedCIDRs: cidrs, timeout: timeout, maxRequest: maxRequest,
		maxResponse: maxResponse, maxHeaders: maxHeaders, maxRedirects: maxRedirects,
		disableRedirects: config.DisableRedirects, resolver: resolver,
		dialContext: dialContext, secretResolver: secretResolver,
		userAgent: config.UserAgent,
	}, nil
}

func configuredLimit(value, defaultValue int64, label string) (int64, error) {
	if value == 0 {
		return defaultValue, nil
	}
	if value < 1 || value > maxConfiguredBytes {
		return 0, fmt.Errorf("HTTP %s size limit must be between 1 byte and 1 GiB", label)
	}
	return value, nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (e *Executor) Definition() coretools.Definition {
	return coretools.Definition{
		Name:                 "http",
		Description:          "Performs a bounded outbound HTTP request to an explicitly allowed destination",
		InputSchema:          json.RawMessage(`{"type":"object","additionalProperties":false,"required":["method","url"],"properties":{"method":{"type":"string"},"url":{"type":"string"},"headers":{"type":"object","additionalProperties":{"type":"string"}},"body":{"type":"string"},"body_base64":{"type":"string"},"secret_headers":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["reference","header"],"properties":{"reference":{"type":"string"},"header":{"type":"string"},"prefix":{"type":"string"}}}},"timeout_seconds":{"type":"integer","minimum":1},"dry_run":{"type":"boolean"}}}`),
		OutputSchema:         json.RawMessage(`{"type":"object","required":["method","url","risk","approval_required","request_body_bytes","truncated","timed_out","cancelled","dry_run","untrusted_content","started_at","finished_at","duration_ms"]}`),
		RequiredScopes:       []string{"tool.http.request"},
		Risk:                 coretools.RiskExternalCommunication,
		ApprovalRequired:     true,
		DefaultTimeout:       e.timeout,
		SupportsCancellation: true,
		SupportsStreaming:    false,
		SupportedOS:          []string{"darwin", "linux", "windows"},
	}
}

// ParseAndValidate strictly decodes input without DNS resolution or secret
// access, making it safe to use before policy evaluation.
func (e *Executor) ParseAndValidate(raw json.RawMessage) (Input, coretools.Risk, error) {
	// JSON escaping and base64 can expand a valid bounded request. Twice the
	// wire limit plus bounded metadata accommodates both while still placing a
	// hard ceiling on decoder work.
	if int64(len(raw)) > e.maxRequest*2+DefaultMaxHeaderBytes+maxURLBytes {
		return Input{}, "", ErrRequestTooLarge
	}
	var input Input
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return Input{}, "", fmt.Errorf("decode HTTP input: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Input{}, "", err
	}

	input.Method = strings.ToUpper(strings.TrimSpace(input.Method))
	if err := e.validateMethod(input.Method); err != nil {
		return Input{}, "", err
	}
	parsedURL, err := parseRequestURL(input.URL)
	if err != nil {
		return Input{}, "", err
	}
	if err := e.validateURL(parsedURL); err != nil {
		return Input{}, "", err
	}
	input.URL = parsedURL.String()

	if input.Body != "" && input.BodyBase64 != "" {
		return Input{}, "", errors.New("body and body_base64 are mutually exclusive")
	}
	body, err := input.bodyBytes()
	if err != nil {
		return Input{}, "", err
	}
	if len(input.Headers) > maxHeaderCount {
		return Input{}, "", errors.New("too many HTTP request headers")
	}
	canonicalHeaders := make(map[string]string, len(input.Headers))
	for name, value := range input.Headers {
		canonical, err := validateCallerHeader(name, value)
		if err != nil {
			return Input{}, "", err
		}
		if _, duplicate := canonicalHeaders[canonical]; duplicate {
			return Input{}, "", fmt.Errorf("duplicate HTTP header %q", canonical)
		}
		canonicalHeaders[canonical] = value
	}
	input.Headers = canonicalHeaders

	if len(input.SecretHeaders) > maxSecretReferences {
		return Input{}, "", errors.New("too many HTTP secret references")
	}
	secretNames := make(map[string]struct{}, len(input.SecretHeaders))
	for index := range input.SecretHeaders {
		secret := &input.SecretHeaders[index]
		secret.Reference = strings.TrimSpace(secret.Reference)
		if !validSecretReference(secret.Reference) {
			return Input{}, "", fmt.Errorf("invalid secret reference at index %d", index)
		}
		canonical, err := validateSecretHeader(secret.Header, secret.Prefix)
		if err != nil {
			return Input{}, "", err
		}
		secret.Header = canonical
		if _, exists := input.Headers[canonical]; exists {
			return Input{}, "", fmt.Errorf("header %q cannot be both plaintext and secret-backed", canonical)
		}
		if _, exists := secretNames[strings.ToLower(canonical)]; exists {
			return Input{}, "", fmt.Errorf("duplicate secret-backed HTTP header %q", canonical)
		}
		secretNames[strings.ToLower(canonical)] = struct{}{}
	}

	if input.TimeoutSeconds < 0 {
		return Input{}, "", errors.New("timeout_seconds cannot be negative")
	}
	if input.TimeoutSeconds > 0 {
		if input.TimeoutSeconds > int((24*time.Hour)/time.Second) {
			return Input{}, "", fmt.Errorf("timeout_seconds cannot exceed the configured timeout of %s", e.timeout)
		}
		requested := time.Duration(input.TimeoutSeconds) * time.Second
		if requested <= 0 || requested > e.timeout {
			return Input{}, "", fmt.Errorf("timeout_seconds cannot exceed the configured timeout of %s", e.timeout)
		}
	}
	if err := e.checkRequestSize(input.Method, parsedURL, input.Headers, body, nil); err != nil {
		return Input{}, "", err
	}
	return input, Classify(input), nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("HTTP input must contain exactly one JSON value")
		}
		return fmt.Errorf("decode trailing HTTP input: %w", err)
	}
	return nil
}

func (input Input) bodyBytes() ([]byte, error) {
	if input.BodyBase64 == "" {
		return []byte(input.Body), nil
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(input.BodyBase64)
	if err != nil {
		return nil, errors.New("body_base64 is not valid canonical base64")
	}
	return decoded, nil
}

func (e *Executor) validateMethod(method string) error {
	if !validToken(method) {
		return fmt.Errorf("%w: %q", ErrMethodNotAllowed, method)
	}
	if _, allowed := e.allowedMethods[method]; !allowed {
		return fmt.Errorf("%w: %s", ErrMethodNotAllowed, method)
	}
	return nil
}

func (e *Executor) validateURL(value *url.URL) error {
	host := normalizeHostname(value.Hostname())
	if !e.hostAllowed(host) {
		return fmt.Errorf("%w: %s", ErrDomainNotAllowed, host)
	}
	port, err := effectivePort(value)
	if err != nil {
		return err
	}
	if len(e.allowedPorts) > 0 {
		if _, allowed := e.allowedPorts[port]; !allowed {
			return fmt.Errorf("%w: port %d", ErrDomainNotAllowed, port)
		}
	}
	return nil
}

func (e *Executor) hostAllowed(host string) bool {
	for _, rule := range e.allowedDomains {
		if rule.Match(host) {
			return true
		}
	}
	return false
}

func (e *Executor) checkRequestSize(method string, parsedURL *url.URL, headers map[string]string, body []byte, resolvedSecrets map[string]string) error {
	size := int64(len(method) + len(parsedURL.String()) + len(body))
	for name, value := range headers {
		size += int64(len(name) + len(value) + 4)
	}
	for name, value := range resolvedSecrets {
		size += int64(len(name) + len(value) + 4)
	}
	if size > e.maxRequest {
		return fmt.Errorf("%w: %d bytes exceeds %d", ErrRequestTooLarge, size, e.maxRequest)
	}
	return nil
}

// Assess returns an audit-safe policy input without accessing DNS, the network,
// or protected secret storage.
func (e *Executor) Assess(raw json.RawMessage) (Assessment, error) {
	input, risk, err := e.ParseAndValidate(raw)
	if err != nil {
		return Assessment{}, err
	}
	body, err := input.bodyBytes()
	if err != nil {
		return Assessment{}, err
	}
	return e.assessment(input, body, risk), nil
}

func (e *Executor) assessment(input Input, body []byte, risk coretools.Risk) Assessment {
	refs := make([]string, 0, len(input.SecretHeaders))
	for _, secret := range input.SecretHeaders {
		refs = append(refs, secret.Reference)
	}
	scopes := []string{"tool.http.request"}
	if len(refs) > 0 {
		scopes = append(scopes, "secret.resolve")
	}
	return Assessment{
		Method: input.Method, URL: auditURLString(input.URL), Risk: risk,
		ApprovalRequired: true, RequiredScopes: scopes, SecretReferences: refs,
		RequestBodyBytes: int64(len(body)), RequestBodySHA256: digest(body),
	}
}

func Classify(input Input) coretools.Risk {
	if len(input.SecretHeaders) > 0 {
		return coretools.RiskSecretAccess
	}
	return coretools.RiskExternalCommunication
}

func (e *Executor) Execute(ctx context.Context, raw json.RawMessage, _ func(coretools.Progress)) (json.RawMessage, error) {
	input, risk, err := e.ParseAndValidate(raw)
	if err != nil {
		return nil, err
	}
	body, err := input.bodyBytes()
	if err != nil {
		return nil, err
	}
	assessment := e.assessment(input, body, risk)
	startedAt := time.Now().UTC()
	result := Output{
		Method: assessment.Method, URL: assessment.URL, Risk: assessment.Risk,
		ApprovalRequired: assessment.ApprovalRequired, RequiredScopes: assessment.RequiredScopes,
		SecretReferences: assessment.SecretReferences, RequestBodyBytes: assessment.RequestBodyBytes,
		RequestBodySHA256: assessment.RequestBodySHA256, DryRun: input.DryRun,
		UntrustedContent: true, StartedAt: startedAt,
	}
	if input.DryRun {
		result.UntrustedContent = false
		result.FinishedAt = time.Now().UTC()
		result.DurationMS = result.FinishedAt.Sub(startedAt).Milliseconds()
		return marshalOutput(result, nil)
	}

	timeout := e.timeout
	if input.TimeoutSeconds > 0 {
		timeout = time.Duration(input.TimeoutSeconds) * time.Second
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resolvedHeaders, redactor, err := e.resolveSecretHeaders(requestContext, input.SecretHeaders)
	if err != nil {
		result.ErrorCode = errorCode(err)
		result.FinishedAt = time.Now().UTC()
		result.DurationMS = result.FinishedAt.Sub(startedAt).Milliseconds()
		return marshalOutput(result, err)
	}
	parsedURL, _ := url.Parse(input.URL)
	if err := e.checkRequestSize(input.Method, parsedURL, input.Headers, body, resolvedHeaders); err != nil {
		result.ErrorCode = errorCode(err)
		result.FinishedAt = time.Now().UTC()
		result.DurationMS = result.FinishedAt.Sub(startedAt).Milliseconds()
		return marshalOutput(result, err)
	}

	request, err := stdhttp.NewRequestWithContext(requestContext, input.Method, input.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("construct HTTP request: %w", err)
	}
	for name, value := range input.Headers {
		request.Header.Set(name, value)
	}
	for name, value := range resolvedHeaders {
		request.Header.Set(name, value)
	}
	if e.userAgent != "" {
		request.Header.Set("User-Agent", e.userAgent)
	}
	// Let net/http negotiate and transparently decode gzip so the configured
	// response limit applies to the bytes exposed to callers.
	request.Header.Del("Accept-Encoding")

	recorder := &hopRecorder{}
	roundTripper := &secureRoundTripper{executor: e, recorder: recorder, redactor: redactor}
	secretHeaderNames := make(map[string]struct{}, len(input.SecretHeaders))
	for _, secret := range input.SecretHeaders {
		secretHeaderNames[stdhttp.CanonicalHeaderKey(secret.Header)] = struct{}{}
	}
	client := &stdhttp.Client{
		Transport:     roundTripper,
		CheckRedirect: e.redirectPolicy(secretHeaderNames),
	}
	response, requestErr := client.Do(request)
	if requestErr != nil {
		result.Hops = recorder.Snapshot()
		result.TimedOut = errors.Is(requestContext.Err(), context.DeadlineExceeded)
		result.Cancelled = errors.Is(requestContext.Err(), context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
		safeErr := sanitizeExecutionError(requestErr, requestContext, ctx)
		result.ErrorCode = errorCode(safeErr)
		result.FinishedAt = time.Now().UTC()
		result.DurationMS = result.FinishedAt.Sub(startedAt).Milliseconds()
		return marshalOutput(result, safeErr)
	}
	defer response.Body.Close()

	responseBytes, truncated, readErr := readBounded(response.Body, e.maxResponse)
	responseBytes = redactor.RedactBytes(responseBytes)
	if int64(len(responseBytes)) > e.maxResponse {
		responseBytes = responseBytes[:e.maxResponse]
		truncated = true
	}
	result.StatusCode = response.StatusCode
	result.Status = response.Status
	result.Headers, result.RedactedResponseHeaders = sanitizeResponseHeaders(response.Header, redactor)
	result.DeclaredResponseBodyBytes = response.ContentLength
	result.BodyBytes = int64(len(responseBytes))
	result.BodySHA256 = digest(responseBytes)
	result.Truncated = truncated
	result.Hops = recorder.Snapshot()
	if utf8.Valid(responseBytes) {
		result.Body = string(responseBytes)
		result.BodyEncoding = "utf-8"
	} else {
		result.BodyBase64 = base64.StdEncoding.EncodeToString(responseBytes)
		result.BodyEncoding = "base64"
	}
	if readErr != nil {
		result.TimedOut = errors.Is(requestContext.Err(), context.DeadlineExceeded)
		result.Cancelled = errors.Is(requestContext.Err(), context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
		readErr = sanitizeExecutionError(readErr, requestContext, ctx)
		result.ErrorCode = errorCode(readErr)
	}
	result.FinishedAt = time.Now().UTC()
	result.DurationMS = result.FinishedAt.Sub(startedAt).Milliseconds()
	return marshalOutput(result, readErr)
}

func (e *Executor) resolveSecretHeaders(ctx context.Context, references []SecretReference) (map[string]string, secretRedactor, error) {
	if len(references) == 0 {
		return nil, secretRedactor{}, nil
	}
	if e.secretResolver == nil {
		return nil, secretRedactor{}, ErrSecretResolverRequired
	}
	values := make(map[string]string, len(references))
	redactor := secretRedactor{}
	for _, reference := range references {
		value, err := e.secretResolver.ResolveSecret(ctx, reference.Reference)
		if err != nil {
			// Resolver errors are intentionally not wrapped: keychain/provider
			// implementations have historically included credential values.
			return nil, secretRedactor{}, fmt.Errorf("%w: %s", ErrSecretResolution, reference.Reference)
		}
		if value == "" || len(value) > maxSecretBytes || !validHeaderValue(value) {
			return nil, secretRedactor{}, fmt.Errorf("%w: %s returned an invalid header value", ErrSecretResolution, reference.Reference)
		}
		values[reference.Header] = reference.Prefix + value
		redactor.values = append(redactor.values, []byte(value), []byte(reference.Prefix+value))
	}
	redactor.sort()
	return values, redactor, nil
}

func (e *Executor) redirectPolicy(secretHeaders map[string]struct{}) func(*stdhttp.Request, []*stdhttp.Request) error {
	return func(request *stdhttp.Request, via []*stdhttp.Request) error {
		if e.disableRedirects {
			return ErrTooManyRedirects
		}
		if len(via) > e.maxRedirects {
			return ErrTooManyRedirects
		}
		if err := e.validateMethod(request.Method); err != nil {
			return err
		}
		if err := validateURLStructure(request.URL); err != nil {
			return err
		}
		if err := e.validateURL(request.URL); err != nil {
			return err
		}
		if len(via) > 0 {
			previous := via[len(via)-1]
			if strings.EqualFold(previous.URL.Scheme, "https") && strings.EqualFold(request.URL.Scheme, "http") {
				return ErrRedirectDowngrade
			}
			if !sameOrigin(previous.URL, request.URL) {
				for name := range secretHeaders {
					request.Header.Del(name)
				}
			}
		}
		// Never send an automatic referrer: even non-credential query values
		// are considered private tool input.
		request.Header.Del("Referer")
		return nil
	}
}

func sameOrigin(a, b *url.URL) bool {
	aPort, aErr := effectivePort(a)
	bPort, bErr := effectivePort(b)
	return aErr == nil && bErr == nil && strings.EqualFold(a.Scheme, b.Scheme) &&
		normalizeHostname(a.Hostname()) == normalizeHostname(b.Hostname()) && aPort == bPort
}

func readBounded(reader io.Reader, limit int64) ([]byte, bool, error) {
	value, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if int64(len(value)) > limit {
		value = value[:limit]
		return value, true, err
	}
	return value, false, err
}

func marshalOutput(output Output, executionErr error) (json.RawMessage, error) {
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("encode HTTP output: %w", err)
	}
	return encoded, executionErr
}

func digest(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

func auditURLString(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return "[INVALID URL]"
	}
	query, queryErr := url.ParseQuery(parsed.RawQuery)
	if queryErr != nil {
		parsed.RawQuery = "[REDACTED]"
		parsed.Fragment = ""
		return parsed.String()
	}
	for name, values := range query {
		redacted := make([]string, len(values))
		for index := range redacted {
			redacted[index] = "[REDACTED]"
		}
		query[name] = redacted
	}
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	result := parsed.String()
	if len(result) <= maxURLBytes {
		return result
	}
	parsed.RawQuery = "[REDACTED]"
	result = parsed.String()
	if len(result) <= maxURLBytes {
		return result
	}
	return "[REDACTED URL]"
}

func sanitizeResponseHeaders(headers stdhttp.Header, redactor secretRedactor) (map[string][]string, []string) {
	result := make(map[string][]string)
	redactedNames := make([]string, 0)
	for name, values := range headers {
		canonical := stdhttp.CanonicalHeaderKey(name)
		if sensitiveHeader(canonical) {
			result[canonical] = []string{"[REDACTED]"}
			redactedNames = append(redactedNames, canonical)
			continue
		}
		clean := make([]string, len(values))
		for index, value := range values {
			if redactor.ContainsString(value) {
				clean[index] = "[REDACTED]"
			} else if strings.EqualFold(canonical, "Location") {
				clean[index] = auditURLString(value)
			} else {
				clean[index] = redactor.RedactString(value)
			}
		}
		result[canonical] = clean
	}
	sort.Strings(redactedNames)
	return result, redactedNames
}

type secretRedactor struct {
	values [][]byte
}

func (r *secretRedactor) sort() {
	sort.Slice(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
}

func (r secretRedactor) RedactBytes(value []byte) []byte {
	result := bytes.Clone(value)
	for _, secret := range r.values {
		if len(secret) > 0 {
			result = bytes.ReplaceAll(result, secret, []byte("[REDACTED]"))
		}
	}
	return result
}

func (r secretRedactor) RedactString(value string) string {
	return string(r.RedactBytes([]byte(value)))
}

func (r secretRedactor) ContainsString(value string) bool {
	for _, secret := range r.values {
		if len(secret) > 0 && strings.Contains(value, string(secret)) {
			return true
		}
	}
	return false
}

func errorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "TIMEOUT"
	case errors.Is(err, context.Canceled):
		return "CANCELLED"
	case errors.Is(err, ErrDomainNotAllowed):
		return "DOMAIN_NOT_ALLOWED"
	case errors.Is(err, ErrMethodNotAllowed):
		return "METHOD_NOT_ALLOWED"
	case errors.Is(err, ErrUnsafeAddress):
		return "UNSAFE_ADDRESS"
	case errors.Is(err, ErrRequestTooLarge):
		return "REQUEST_TOO_LARGE"
	case errors.Is(err, ErrPlaintextSecret):
		return "PLAINTEXT_SECRET"
	case errors.Is(err, ErrSecretResolverRequired):
		return "SECRET_RESOLVER_REQUIRED"
	case errors.Is(err, ErrSecretResolution):
		return "SECRET_RESOLUTION_FAILED"
	case errors.Is(err, ErrTooManyRedirects):
		return "REDIRECT_LIMIT"
	case errors.Is(err, ErrRedirectDowngrade):
		return "REDIRECT_DOWNGRADE"
	default:
		return "REQUEST_FAILED"
	}
}

func sanitizeExecutionError(err error, requestCtx, parentCtx context.Context) error {
	if errors.Is(parentCtx.Err(), context.Canceled) {
		return fmt.Errorf("HTTP request cancelled: %w", context.Canceled)
	}
	if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("HTTP request timed out: %w", context.DeadlineExceeded)
	}
	if errors.Is(requestCtx.Err(), context.Canceled) {
		return fmt.Errorf("HTTP request cancelled: %w", context.Canceled)
	}
	for _, known := range []error{
		ErrDomainNotAllowed, ErrMethodNotAllowed, ErrUnsafeAddress, ErrRequestTooLarge,
		ErrPlaintextSecret, ErrTooManyRedirects, ErrRedirectDowngrade,
	} {
		if errors.Is(err, known) {
			return known
		}
	}
	// net/http's url.Error includes the full request URL, which may contain
	// private query values. Do not propagate it into logs or model context.
	return ErrRequestFailed
}

func effectivePort(value *url.URL) (int, error) {
	if raw := value.Port(); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return 0, errors.New("invalid HTTP destination port")
		}
		return port, nil
	}
	if strings.EqualFold(value.Scheme, "https") {
		return 443, nil
	}
	return 80, nil
}

func parseRequestURL(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > maxURLBytes || hasControl(raw) {
		return nil, errors.New("HTTP URL is required, bounded to 8 KiB, and cannot contain control characters")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid HTTP URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if err := validateURLStructure(parsed); err != nil {
		return nil, err
	}
	host := normalizeHostname(parsed.Hostname())
	if parsed.Port() != "" {
		if _, err := effectivePort(parsed); err != nil {
			return nil, err
		}
	}
	if strings.Contains(host, ":") {
		parsed.Host = net.JoinHostPort(host, parsed.Port())
		if parsed.Port() == "" {
			parsed.Host = "[" + host + "]"
		}
	} else if parsed.Port() != "" {
		parsed.Host = net.JoinHostPort(host, parsed.Port())
	} else {
		parsed.Host = host
	}
	return parsed, nil
}

func validateURLStructure(parsed *url.URL) error {
	if parsed == nil {
		return errors.New("HTTP URL is required")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("HTTP URL scheme must be http or https")
	}
	if !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" {
		return errors.New("HTTP URL must be an absolute hierarchical URL with a host")
	}
	if parsed.User != nil {
		return ErrPlaintextSecret
	}
	if parsed.Fragment != "" {
		return errors.New("HTTP URL fragments are not allowed")
	}
	if strings.Contains(parsed.Hostname(), "%") {
		return errors.New("scoped IPv6 destinations are not allowed")
	}
	host := normalizeHostname(parsed.Hostname())
	if !validHostnameOrIP(host) {
		return errors.New("invalid or non-ASCII HTTP hostname")
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return errors.New("HTTP URL contains an invalid query string")
	}
	for name := range query {
		if sensitiveName(name) {
			return fmt.Errorf("%w: URL query parameter %q requires a secret-backed integration", ErrPlaintextSecret, name)
		}
	}
	_, err = effectivePort(parsed)
	return err
}

func validateCallerHeader(name, value string) (string, error) {
	canonical := stdhttp.CanonicalHeaderKey(strings.TrimSpace(name))
	if !validToken(canonical) || !validHeaderValue(value) {
		return "", fmt.Errorf("invalid HTTP header %q", name)
	}
	if forbiddenRequestHeader(canonical) {
		return "", fmt.Errorf("HTTP header %q is controlled by the executor", canonical)
	}
	if sensitiveHeader(canonical) {
		return "", fmt.Errorf("%w: header %q must use secret_headers", ErrPlaintextSecret, canonical)
	}
	return canonical, nil
}

func validateSecretHeader(name, prefix string) (string, error) {
	canonical := stdhttp.CanonicalHeaderKey(strings.TrimSpace(name))
	if !validToken(canonical) || forbiddenRequestHeader(canonical) {
		return "", fmt.Errorf("invalid secret-backed HTTP header %q", name)
	}
	if !validSecretPrefix(prefix) {
		return "", fmt.Errorf("invalid prefix for secret-backed HTTP header %q", canonical)
	}
	return canonical, nil
}

func validSecretPrefix(prefix string) bool {
	if prefix == "" {
		return true
	}
	if len(prefix) > 32 || !strings.HasSuffix(prefix, " ") || strings.Count(prefix, " ") != 1 {
		return false
	}
	return validToken(strings.TrimSuffix(prefix, " "))
}

func forbiddenRequestHeader(name string) bool {
	lower := strings.ToLower(name)
	if lower == "host" || lower == "content-length" || lower == "accept-encoding" || lower == "referer" {
		return true
	}
	if lower == "connection" || lower == "keep-alive" || lower == "te" || lower == "trailer" ||
		lower == "transfer-encoding" || lower == "upgrade" || strings.HasPrefix(lower, "proxy-") {
		return true
	}
	return false
}

func sensitiveHeader(name string) bool {
	lower := strings.ToLower(name)
	return lower == "authorization" || lower == "cookie" || lower == "set-cookie" ||
		sensitiveName(lower)
}

func sensitiveName(name string) bool {
	lower := strings.ToLower(name)
	for _, fragment := range []string{"authorization", "credential", "password", "passwd", "secret", "token", "api-key", "apikey", "api_key", "access-key", "signature"} {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	normalized := strings.NewReplacer("_", "-", ".", "-").Replace(lower)
	for _, part := range strings.Split(normalized, "-") {
		if part == "auth" || part == "key" {
			return true
		}
	}
	return false
}

func validSecretReference(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("._:/-", char) {
			continue
		}
		if index == 0 {
			return false
		}
		return false
	}
	first := value[0]
	return (first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || (first >= '0' && first <= '9')
}

func validToken(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') ||
			strings.ContainsRune("!#$%&'*+-.^_`|~", char) {
			continue
		}
		return false
	}
	return true
}

func validHeaderValue(value string) bool {
	for _, char := range value {
		if char == '\r' || char == '\n' || char == 0 || char == 0x7f || (char < 0x20 && char != '\t') {
			return false
		}
	}
	return true
}

func hasControl(value string) bool {
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return true
		}
	}
	return false
}

var _ coretools.Executor = (*Executor)(nil)
