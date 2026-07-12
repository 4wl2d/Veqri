package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coretools "github.com/veqri/veqri/core/tools"
)

type resolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (f resolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

func TestDefinitionAndAssessmentAreApprovalReady(t *testing.T) {
	executor, err := New(Config{
		AllowedDomains: []string{"api.example.com"},
		AllowedMethods: []string{stdhttp.MethodGet, stdhttp.MethodPost},
		Timeout:        12 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := executor.Definition()
	if definition.Name != "http" || definition.Risk != coretools.RiskExternalCommunication || !definition.ApprovalRequired {
		t.Fatalf("unexpected definition: %+v", definition)
	}
	if definition.DefaultTimeout != 12*time.Second || !definition.SupportsCancellation || definition.SupportsStreaming {
		t.Fatalf("unexpected capabilities: %+v", definition)
	}

	assessment, err := executor.Assess(mustJSON(t, Input{
		Method: stdhttp.MethodPost,
		URL:    "https://api.example.com/v1/items?q=private",
		Body:   "payload",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if assessment.Risk != coretools.RiskExternalCommunication || !assessment.ApprovalRequired {
		t.Fatalf("request was not classified for approval: %+v", assessment)
	}
	if strings.Contains(assessment.URL, "private") || !strings.Contains(assessment.URL, "%5BREDACTED%5D") {
		t.Fatalf("assessment URL did not redact query values: %q", assessment.URL)
	}
	if assessment.RequestBodyBytes != int64(len("payload")) || len(assessment.RequestBodySHA256) != 64 {
		t.Fatalf("missing audit body metadata: %+v", assessment)
	}

	secretAssessment, err := executor.Assess(mustJSON(t, Input{
		Method: stdhttp.MethodGet,
		URL:    "https://api.example.com/v1/items",
		SecretHeaders: []SecretReference{{
			Reference: "keychain/api", Header: "Authorization", Prefix: "Bearer ",
		}},
		DryRun: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if secretAssessment.Risk != coretools.RiskSecretAccess || !contains(secretAssessment.RequiredScopes, "secret.resolve") {
		t.Fatalf("secret-backed request has the wrong policy metadata: %+v", secretAssessment)
	}
}

func TestValidationRejectsAllowlistBypassesAndPlaintextAuth(t *testing.T) {
	executor, err := New(Config{
		AllowedDomains: []string{"*.example.com"},
		AllowedMethods: []string{stdhttp.MethodGet},
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  json.RawMessage
		want error
	}{
		{"apex does not match wildcard", mustJSON(t, Input{Method: "GET", URL: "https://example.com"}), ErrDomainNotAllowed},
		{"suffix confusion", mustJSON(t, Input{Method: "GET", URL: "https://attackerexample.com"}), ErrDomainNotAllowed},
		{"method", mustJSON(t, Input{Method: "POST", URL: "https://api.example.com"}), ErrMethodNotAllowed},
		{"userinfo", mustJSON(t, Input{Method: "GET", URL: "https://user:password@api.example.com"}), ErrPlaintextSecret},
		{"authorization header", mustJSON(t, Input{Method: "GET", URL: "https://api.example.com", Headers: map[string]string{"Authorization": "Bearer plaintext"}}), ErrPlaintextSecret},
		{"api key header", mustJSON(t, Input{Method: "GET", URL: "https://api.example.com", Headers: map[string]string{"X-API-Key": "plaintext"}}), ErrPlaintextSecret},
		{"query credential", mustJSON(t, Input{Method: "GET", URL: "https://api.example.com?api_key=plaintext"}), ErrPlaintextSecret},
		{"malformed query", mustJSON(t, Input{Method: "GET", URL: "https://api.example.com?safe=%zz"}), errors.New("invalid query")},
		{"fragment", mustJSON(t, Input{Method: "GET", URL: "https://api.example.com/#private"}), errors.New("fragment")},
		{"unknown input", json.RawMessage(`{"method":"GET","url":"https://api.example.com","surprise":true}`), errors.New("unknown")},
		{"hop by hop header", mustJSON(t, Input{Method: "GET", URL: "https://api.example.com", Headers: map[string]string{"Connection": "close"}}), errors.New("controlled")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, got := executor.ParseAndValidate(test.raw)
			if got == nil {
				t.Fatal("expected validation error")
			}
			if test.want == ErrDomainNotAllowed || test.want == ErrMethodNotAllowed || test.want == ErrPlaintextSecret {
				if !errors.Is(got, test.want) {
					t.Fatalf("got %v, want errors.Is(%v)", got, test.want)
				}
			} else if !strings.Contains(strings.ToLower(got.Error()), test.want.Error()) {
				t.Fatalf("got %v, want message containing %q", got, test.want)
			}
		})
	}

	if _, _, err := executor.ParseAndValidate(mustJSON(t, Input{Method: "GET", URL: "https://nested.api.example.com"})); err != nil {
		t.Fatalf("valid wildcard subdomain was rejected: %v", err)
	}
}

func TestUnsafeDNSAnswersAreDeniedBeforeDial(t *testing.T) {
	for _, answers := range [][]string{{"127.0.0.1"}, {"8.8.8.8", "169.254.169.254"}, {"::1"}, {"::127.0.0.1"}, {"fec0::1"}} {
		t.Run(strings.Join(answers, ","), func(t *testing.T) {
			var dials atomic.Int32
			executor, err := New(Config{
				AllowedDomains: []string{"allowed.test"},
				AllowedMethods: []string{stdhttp.MethodGet},
				Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
					result := make([]net.IPAddr, 0, len(answers))
					for _, answer := range answers {
						result = append(result, net.IPAddr{IP: net.ParseIP(answer)})
					}
					return result, nil
				}),
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					dials.Add(1)
					return nil, errors.New("must not dial")
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			raw, executeErr := executor.Execute(context.Background(), mustJSON(t, Input{Method: "GET", URL: "http://allowed.test/resource"}), nil)
			if !errors.Is(executeErr, ErrUnsafeAddress) {
				t.Fatalf("got %v, want unsafe address", executeErr)
			}
			if dials.Load() != 0 {
				t.Fatalf("unsafe destination was dialed %d times", dials.Load())
			}
			output := decodeOutput(t, raw)
			if output.ErrorCode != "UNSAFE_ADDRESS" || len(output.Hops) != 1 {
				t.Fatalf("missing safe failure audit metadata: %+v", output)
			}
		})
	}
}

func TestRequestUsesValidatedConcreteIPAndBoundsResponse(t *testing.T) {
	var receivedHost, receivedBody string
	server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		value, _ := io.ReadAll(request.Body)
		receivedHost = request.Host
		receivedBody = string(value)
		writer.Header().Set("X-Result", "safe")
		_, _ = io.WriteString(writer, "0123456789")
	}))
	defer server.Close()

	hostname := "allowed.test"
	target, port := targetURL(t, server.URL, hostname, "/echo")
	var dialed string
	executor := newLocalExecutor(t, []string{hostname}, []string{stdhttp.MethodPost}, port, 5,
		func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = address
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}, nil)

	raw, err := executor.Execute(context.Background(), mustJSON(t, Input{
		Method: "POST", URL: target, Body: "request-body", Headers: map[string]string{"Content-Type": "text/plain"},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	output := decodeOutput(t, raw)
	if receivedBody != "request-body" || receivedHost != net.JoinHostPort(hostname, fmt.Sprintf("%d", port)) {
		t.Fatalf("hostname/body were not preserved: host=%q body=%q", receivedHost, receivedBody)
	}
	dialHost, _, err := net.SplitHostPort(dialed)
	if err != nil || dialHost != "127.0.0.1" {
		t.Fatalf("dial was not pinned to the validated IP: %q (%v)", dialed, err)
	}
	if output.Body != "01234" || !output.Truncated || output.BodyBytes != 5 {
		t.Fatalf("response was not safely truncated: %+v", output)
	}
	if output.StatusCode != stdhttp.StatusOK || len(output.Hops) != 1 || output.Hops[0].ConnectedIP != "127.0.0.1" {
		t.Fatalf("missing HTTP audit metadata: %+v", output)
	}
	if output.RequestBodyBytes != int64(len("request-body")) || len(output.RequestBodySHA256) != 64 || !output.UntrustedContent {
		t.Fatalf("missing request/output trust metadata: %+v", output)
	}
}

func TestRequestSizeLimitIncludesURLHeadersAndBody(t *testing.T) {
	executor, err := New(Config{
		AllowedDomains:  []string{"allowed.test"},
		AllowedMethods:  []string{stdhttp.MethodPost},
		MaxRequestBytes: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = executor.ParseAndValidate(mustJSON(t, Input{
		Method: "POST", URL: "https://allowed.test/resource", Body: strings.Repeat("x", 64),
	}))
	if !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("got %v, want request size failure", err)
	}
}

func TestRedirectsAreRevalidatedAndFreshlyResolved(t *testing.T) {
	t.Run("allowed redirect", func(t *testing.T) {
		var resolverMu sync.Mutex
		resolverCalls := map[string]int{}
		var server *httptest.Server
		server = httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
			if strings.HasPrefix(request.Host, "first.test:") {
				_, port := targetURL(t, server.URL, "second.test", "/final?trace=private")
				writer.Header().Set("Location", fmt.Sprintf("http://second.test:%d/final?trace=private", port))
				writer.WriteHeader(stdhttp.StatusFound)
				return
			}
			_, _ = io.WriteString(writer, "done")
		}))
		defer server.Close()
		first, port := targetURL(t, server.URL, "first.test", "/start")
		executor := newLocalExecutor(t, []string{"first.test", "second.test"}, []string{"GET"}, port, 1024,
			nil, resolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
				resolverMu.Lock()
				resolverCalls[host]++
				resolverMu.Unlock()
				return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
			}))
		raw, err := executor.Execute(context.Background(), mustJSON(t, Input{Method: "GET", URL: first}), nil)
		if err != nil {
			t.Fatal(err)
		}
		output := decodeOutput(t, raw)
		if output.Body != "done" || len(output.Hops) != 2 {
			t.Fatalf("redirect did not complete with two audited hops: %+v", output)
		}
		resolverMu.Lock()
		firstCalls, secondCalls := resolverCalls["first.test"], resolverCalls["second.test"]
		resolverMu.Unlock()
		if firstCalls != 1 || secondCalls != 1 {
			t.Fatalf("each hop must be resolved exactly before its dial: %+v", resolverCalls)
		}
		if strings.Contains(output.Hops[1].URL, "private") {
			t.Fatalf("redirect audit leaked a query value: %+v", output.Hops[1])
		}
	})

	t.Run("blocked redirect", func(t *testing.T) {
		var reached atomic.Bool
		server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
			if strings.HasPrefix(request.Host, "first.test:") {
				_, port := targetURL(t, "http://127.0.0.1:1", "blocked.test", "/")
				writer.Header().Set("Location", fmt.Sprintf("http://blocked.test:%d/final", port))
				writer.WriteHeader(stdhttp.StatusFound)
				return
			}
			reached.Store(true)
		}))
		defer server.Close()
		first, port := targetURL(t, server.URL, "first.test", "/start")
		executor := newLocalExecutor(t, []string{"first.test"}, []string{"GET"}, port, 1024, nil, nil)
		raw, err := executor.Execute(context.Background(), mustJSON(t, Input{Method: "GET", URL: first}), nil)
		if !errors.Is(err, ErrDomainNotAllowed) {
			t.Fatalf("got %v, want blocked redirect", err)
		}
		if reached.Load() {
			t.Fatal("blocked redirect destination was reached")
		}
		output := decodeOutput(t, raw)
		if output.ErrorCode != "DOMAIN_NOT_ALLOWED" || len(output.Hops) != 1 {
			t.Fatalf("blocked redirect audit is incomplete: %+v", output)
		}
	})

	t.Run("redirect cannot introduce userinfo", func(t *testing.T) {
		server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, _ *stdhttp.Request) {
			writer.Header().Set("Location", "http://user:password@first.test/")
			writer.WriteHeader(stdhttp.StatusFound)
		}))
		defer server.Close()
		first, port := targetURL(t, server.URL, "first.test", "/start")
		executor := newLocalExecutor(t, []string{"first.test"}, []string{"GET"}, port, 1024, nil, nil)
		_, err := executor.Execute(context.Background(), mustJSON(t, Input{Method: "GET", URL: first}), nil)
		if !errors.Is(err, ErrPlaintextSecret) {
			t.Fatalf("got %v, want redirect userinfo rejection", err)
		}
	})
}

func TestSecretReferencesAreInjectedAndRedacted(t *testing.T) {
	const secretValue = "super-private-token"
	var receivedAuthorization string
	server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		receivedAuthorization = request.Header.Get("Authorization")
		writer.Header().Set("X-Echo", receivedAuthorization)
		writer.Header().Set("Set-Cookie", "session="+secretValue)
		_, _ = io.WriteString(writer, "reflected "+secretValue)
	}))
	defer server.Close()
	target, port := targetURL(t, server.URL, "secrets.test", "/")
	resolver := SecretResolverFunc(func(_ context.Context, reference string) (string, error) {
		if reference != "keychain/service-token" {
			return "", errors.New("unknown reference")
		}
		return secretValue, nil
	})
	executor := newLocalExecutor(t, []string{"secrets.test"}, []string{"GET"}, port, 2048, nil, nil, resolver)
	raw, err := executor.Execute(context.Background(), mustJSON(t, Input{
		Method: "GET", URL: target,
		SecretHeaders: []SecretReference{{Reference: "keychain/service-token", Header: "Authorization", Prefix: "Bearer "}},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if receivedAuthorization != "Bearer "+secretValue {
		t.Fatalf("secret was not injected at execution: %q", receivedAuthorization)
	}
	if strings.Contains(string(raw), secretValue) {
		t.Fatalf("secret leaked into structured output: %s", raw)
	}
	output := decodeOutput(t, raw)
	if output.Risk != coretools.RiskSecretAccess || output.Body != "reflected [REDACTED]" {
		t.Fatalf("secret output/risk was not sanitized: %+v", output)
	}
	if got := getHeader(output.Headers, "Set-Cookie"); got != "[REDACTED]" {
		t.Fatalf("sensitive response header was not redacted: %q", got)
	}
	if got := getHeader(output.Headers, "X-Echo"); got != "[REDACTED]" {
		t.Fatalf("reflected secret header was not redacted: %q", got)
	}
}

func TestCrossOriginRedirectStripsSecretBackedHeaders(t *testing.T) {
	const secretValue = "redirect-secret"
	var finalHeader string
	var server *httptest.Server
	server = httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		if strings.HasPrefix(request.Host, "first.test:") {
			_, port := targetURL(t, server.URL, "second.test", "/final")
			writer.Header().Set("Location", fmt.Sprintf("http://second.test:%d/final", port))
			writer.WriteHeader(stdhttp.StatusTemporaryRedirect)
			return
		}
		finalHeader = request.Header.Get("X-Service-Token")
		_, _ = io.WriteString(writer, "ok")
	}))
	defer server.Close()
	first, port := targetURL(t, server.URL, "first.test", "/start")
	executor := newLocalExecutor(t, []string{"first.test", "second.test"}, []string{"GET"}, port, 2048, nil, nil,
		SecretResolverFunc(func(context.Context, string) (string, error) { return secretValue, nil }))
	_, err := executor.Execute(context.Background(), mustJSON(t, Input{
		Method: "GET", URL: first,
		SecretHeaders: []SecretReference{{Reference: "keychain/redirect", Header: "X-Service-Token"}},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if finalHeader != "" {
		t.Fatalf("secret-backed header crossed origins: %q", finalHeader)
	}
}

func TestRedactionCannotExpandPastResponseLimit(t *testing.T) {
	server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, _ *stdhttp.Request) {
		_, _ = io.WriteString(writer, "aaaaa")
	}))
	defer server.Close()
	target, port := targetURL(t, server.URL, "redaction.test", "/")
	executor := newLocalExecutor(t, []string{"redaction.test"}, []string{"GET"}, port, 5, nil, nil,
		SecretResolverFunc(func(context.Context, string) (string, error) { return "a", nil }))
	raw, err := executor.Execute(context.Background(), mustJSON(t, Input{
		Method: "GET", URL: target,
		SecretHeaders: []SecretReference{{Reference: "keychain/short", Header: "X-Service-Key"}},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	output := decodeOutput(t, raw)
	if output.BodyBytes > 5 || !output.Truncated || strings.Contains(output.Body, "a") {
		t.Fatalf("redaction violated the configured response bound: %+v", output)
	}
}

func TestConfiguredTimeoutCoversResponseBodyAndReturnsAuditOutput(t *testing.T) {
	server := httptest.NewServer(stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		writer.WriteHeader(stdhttp.StatusOK)
		if flusher, ok := writer.(stdhttp.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-request.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	target, port := targetURL(t, server.URL, "timeout.test", "/")
	executor, err := New(Config{
		AllowedDomains: []string{"timeout.test"}, AllowedMethods: []string{"GET"},
		AllowedPorts: []int{port}, AllowedCIDRs: []string{"127.0.0.0/8"},
		Timeout: 40 * time.Millisecond,
		Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	raw, executeErr := executor.Execute(context.Background(), mustJSON(t, Input{Method: "GET", URL: target}), nil)
	if !errors.Is(executeErr, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", executeErr)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("configured timeout was not enforced promptly: %s", time.Since(started))
	}
	output := decodeOutput(t, raw)
	if !output.TimedOut || output.ErrorCode != "TIMEOUT" || output.FinishedAt.IsZero() {
		t.Fatalf("timeout did not produce audit-ready output: %+v", output)
	}
}

func TestDryRunDoesNotResolveSecretsOrUseNetwork(t *testing.T) {
	var resolves, dials atomic.Int32
	executor, err := New(Config{
		AllowedDomains: []string{"dry-run.test"}, AllowedMethods: []string{"POST"},
		Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return nil, errors.New("must not resolve DNS")
		}),
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("must not dial")
		},
	}, SecretResolverFunc(func(context.Context, string) (string, error) {
		resolves.Add(1)
		return "secret", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := executor.Execute(context.Background(), mustJSON(t, Input{
		Method: "POST", URL: "https://dry-run.test/action", DryRun: true,
		SecretHeaders: []SecretReference{{Reference: "keychain/dry", Header: "Authorization", Prefix: "Bearer "}},
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	output := decodeOutput(t, raw)
	if !output.DryRun || output.UntrustedContent || resolves.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("dry run caused side effects: output=%+v resolves=%d dials=%d", output, resolves.Load(), dials.Load())
	}
}

func newLocalExecutor(
	t *testing.T,
	domains, methods []string,
	port int,
	maxResponse int64,
	dial DialContextFunc,
	resolver Resolver,
	secretResolvers ...SecretResolver,
) *Executor {
	t.Helper()
	if resolver == nil {
		resolver = resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		})
	}
	executor, err := New(Config{
		AllowedDomains: domains, AllowedMethods: methods, AllowedPorts: []int{port},
		AllowedCIDRs: []string{"127.0.0.0/8"}, Resolver: resolver, DialContext: dial,
		MaxRequestBytes: 16 << 10, MaxResponseBytes: maxResponse,
	}, secretResolvers...)
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func targetURL(t *testing.T, serverURL, hostname, path string) (string, int) {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	portString := parsed.Port()
	var port int
	if _, err := fmt.Sscanf(portString, "%d", &port); err != nil {
		t.Fatalf("parse test server port %q: %v", portString, err)
	}
	return fmt.Sprintf("http://%s:%d%s", hostname, port, path), port
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func decodeOutput(t *testing.T, raw json.RawMessage) Output {
	t.Helper()
	var output Output
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatalf("decode output %s: %v", raw, err)
	}
	return output
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func getHeader(headers map[string][]string, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
