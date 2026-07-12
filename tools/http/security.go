package http

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

type domainRule struct {
	host     string
	wildcard bool
}

func parseDomainRule(raw string) (domainRule, error) {
	value := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if value == "" || value == "*" || strings.ContainsAny(value, "/?#@") {
		return domainRule{}, errors.New("domain must be an exact hostname/IP or a *.example.com wildcard")
	}
	wildcard := strings.HasPrefix(value, "*.")
	if wildcard {
		value = strings.TrimPrefix(value, "*.")
		if strings.Contains(value, "*") || !strings.Contains(value, ".") {
			return domainRule{}, errors.New("wildcards require a registrable-looking suffix such as *.example.com")
		}
	} else if strings.Contains(value, "*") {
		return domainRule{}, errors.New("wildcard is supported only as the leftmost *.")
	}
	if !validHostnameOrIP(value) {
		return domainRule{}, errors.New("invalid or non-ASCII domain")
	}
	if wildcard {
		if _, err := netip.ParseAddr(value); err == nil {
			return domainRule{}, errors.New("IP literals cannot use wildcards")
		}
	}
	return domainRule{host: normalizeHostname(value), wildcard: wildcard}, nil
}

func (r domainRule) Match(host string) bool {
	host = normalizeHostname(host)
	if !r.wildcard {
		return host == r.host
	}
	// *.example.com matches a.example.com and deeper subdomains, but not the
	// apex or attackerexample.com.
	return len(host) > len(r.host)+1 && strings.HasSuffix(host, "."+r.host)
}

func (r domainRule) String() string {
	if r.wildcard {
		return "*." + r.host
	}
	return r.host
}

func normalizeHostname(value string) string {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if address, err := netip.ParseAddr(value); err == nil {
		return address.Unmap().String()
	}
	return value
}

func validHostnameOrIP(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	if address, err := netip.ParseAddr(value); err == nil {
		return address.Zone() == ""
	}
	for _, char := range value {
		if char > 127 {
			return false
		}
	}
	labels := strings.Split(value, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
				(char >= '0' && char <= '9') || char == '-' {
				continue
			}
			return false
		}
	}
	return true
}

type ipPrefix struct {
	prefix netip.Prefix
}

func parseAllowedCIDRs(values []string) ([]ipPrefix, error) {
	result := make([]ipPrefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("invalid explicitly allowed CIDR %q", value)
		}
		prefix = prefix.Masked()
		if prefix.Addr().Is4In6() {
			address := prefix.Addr().Unmap()
			bits := prefix.Bits() - 96
			if bits < 0 {
				return nil, fmt.Errorf("invalid mapped IPv4 CIDR %q", value)
			}
			prefix = netip.PrefixFrom(address, bits).Masked()
		}
		result = append(result, ipPrefix{prefix: prefix})
	}
	return result, nil
}

func (p ipPrefix) Contains(address netip.Addr) bool {
	return p.prefix.Contains(address.Unmap())
}

var deniedAddressRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // Current network / unspecified.
	netip.MustParsePrefix("10.0.0.0/8"),      // Private-use.
	netip.MustParsePrefix("100.64.0.0/10"),   // Shared address space.
	netip.MustParsePrefix("127.0.0.0/8"),     // Loopback.
	netip.MustParsePrefix("169.254.0.0/16"),  // Link-local, includes common metadata IPs.
	netip.MustParsePrefix("172.16.0.0/12"),   // Private-use.
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments.
	netip.MustParsePrefix("192.0.2.0/24"),    // Documentation.
	netip.MustParsePrefix("192.88.99.0/24"),  // Deprecated relay anycast.
	netip.MustParsePrefix("192.168.0.0/16"),  // Private-use.
	netip.MustParsePrefix("198.18.0.0/15"),   // Benchmarking.
	netip.MustParsePrefix("198.51.100.0/24"), // Documentation.
	netip.MustParsePrefix("203.0.113.0/24"),  // Documentation.
	netip.MustParsePrefix("224.0.0.0/4"),     // Multicast.
	netip.MustParsePrefix("240.0.0.0/4"),     // Reserved and limited broadcast.

	netip.MustParsePrefix("::/96"),          // Deprecated IPv4-compatible addresses and unspecified.
	netip.MustParsePrefix("::1/128"),        // Loopback.
	netip.MustParsePrefix("64:ff9b::/96"),   // IPv4/IPv6 translation (can encode private IPv4).
	netip.MustParsePrefix("64:ff9b:1::/48"), // Local-use translation prefix.
	netip.MustParsePrefix("100::/64"),       // Discard-only.
	netip.MustParsePrefix("2001::/23"),      // IETF protocol assignments, including Teredo.
	netip.MustParsePrefix("2001:db8::/32"),  // Documentation.
	netip.MustParsePrefix("2002::/16"),      // 6to4 (can encode private IPv4).
	netip.MustParsePrefix("3fff::/20"),      // Documentation.
	netip.MustParsePrefix("5f00::/16"),      // Segment-routing SIDs.
	netip.MustParsePrefix("fc00::/7"),       // Unique-local.
	netip.MustParsePrefix("fe80::/10"),      // Link-local.
	netip.MustParsePrefix("fec0::/10"),      // Deprecated site-local.
	netip.MustParsePrefix("ff00::/8"),       // Multicast.
}

func (e *Executor) resolveAndValidate(ctx context.Context, hostname string) ([]netip.Addr, error) {
	hostname = normalizeHostname(hostname)
	if literal, err := netip.ParseAddr(hostname); err == nil {
		literal = literal.Unmap()
		if err := e.validateAddress(literal); err != nil {
			return nil, err
		}
		return []netip.Addr{literal}, nil
	}

	resolved, err := e.resolver.LookupIPAddr(ctx, hostname)
	if err != nil {
		return nil, errors.New("HTTP destination DNS resolution failed")
	}
	if len(resolved) == 0 || len(resolved) > maxResolvedIPs {
		return nil, errors.New("HTTP destination DNS returned an invalid number of addresses")
	}
	addresses := make([]netip.Addr, 0, len(resolved))
	seen := make(map[netip.Addr]struct{}, len(resolved))
	for _, item := range resolved {
		if item.Zone != "" {
			return nil, ErrUnsafeAddress
		}
		address, ok := netip.AddrFromSlice(item.IP)
		if !ok {
			return nil, ErrUnsafeAddress
		}
		address = address.Unmap()
		if err := e.validateAddress(address); err != nil {
			// Fail the complete resolution when any answer is unsafe. This avoids
			// nondeterministic selection from mixed public/private DNS answers.
			return nil, err
		}
		if _, duplicate := seen[address]; duplicate {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	if len(addresses) == 0 {
		return nil, errors.New("HTTP destination DNS returned no usable addresses")
	}
	return addresses, nil
}

func (e *Executor) validateAddress(address netip.Addr) error {
	if !address.IsValid() || address.Zone() != "" {
		return ErrUnsafeAddress
	}
	address = address.Unmap()
	for _, allowed := range e.allowedCIDRs {
		if allowed.Contains(address) {
			return nil
		}
	}
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() ||
		address.IsMulticast() || address.IsUnspecified() {
		return fmt.Errorf("%w: %s", ErrUnsafeAddress, address)
	}
	for _, prefix := range deniedAddressRanges {
		if prefix.Contains(address) {
			return fmt.Errorf("%w: %s", ErrUnsafeAddress, address)
		}
	}
	return nil
}

type hopRecorder struct {
	mu   sync.Mutex
	hops []Hop
}

func (r *hopRecorder) Append(value Hop) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hops = append(r.hops, value)
}

func (r *hopRecorder) Snapshot() []Hop {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Hop, len(r.hops))
	copy(result, r.hops)
	return result
}

type secureRoundTripper struct {
	executor *Executor
	recorder *hopRecorder
	redactor secretRedactor
}

func (t *secureRoundTripper) RoundTrip(request *stdhttp.Request) (*stdhttp.Response, error) {
	started := time.Now()
	hop := Hop{Method: request.Method, URL: t.redactor.RedactString(auditURLString(request.URL.String()))}
	finish := func(response *stdhttp.Response, err error) {
		if response != nil {
			hop.StatusCode = response.StatusCode
		}
		if err != nil {
			hop.ErrorCode = errorCode(err)
		}
		hop.DurationMS = time.Since(started).Milliseconds()
		t.recorder.Append(hop)
	}

	if err := t.executor.validateMethod(request.Method); err != nil {
		finish(nil, err)
		return nil, err
	}
	if err := validateURLStructure(request.URL); err != nil {
		finish(nil, err)
		return nil, err
	}
	if err := t.executor.validateURL(request.URL); err != nil {
		finish(nil, err)
		return nil, err
	}
	addresses, err := t.executor.resolveAndValidate(request.Context(), request.URL.Hostname())
	if err != nil {
		finish(nil, err)
		return nil, err
	}
	hop.ResolvedIPs = make([]string, len(addresses))
	for index, address := range addresses {
		hop.ResolvedIPs[index] = address.String()
	}
	port, err := effectivePort(request.URL)
	if err != nil {
		finish(nil, err)
		return nil, err
	}

	var connectedMu sync.Mutex
	connectedIP := ""
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		var lastErr error
		for _, address := range addresses {
			if network == "tcp4" && !address.Is4() {
				continue
			}
			if network == "tcp6" && !address.Is6() {
				continue
			}
			concrete := net.JoinHostPort(address.String(), fmt.Sprintf("%d", port))
			connection, dialErr := t.executor.dialContext(ctx, network, concrete)
			if dialErr == nil {
				connectedMu.Lock()
				connectedIP = address.String()
				connectedMu.Unlock()
				return connection, nil
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			lastErr = errors.New("no resolved address matched the requested network")
		}
		return nil, lastErr
	}

	transport := &stdhttp.Transport{
		Proxy:                  nil,
		DialContext:            dial,
		ForceAttemptHTTP2:      true,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: t.executor.maxHeaders,
		ResponseHeaderTimeout:  minDuration(15*time.Second, t.executor.timeout),
		TLSHandshakeTimeout:    minDuration(10*time.Second, t.executor.timeout),
		ExpectContinueTimeout:  minDuration(time.Second, t.executor.timeout),
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: request.URL.Hostname(),
		},
	}
	response, roundTripErr := transport.RoundTrip(request)
	connectedMu.Lock()
	hop.ConnectedIP = connectedIP
	connectedMu.Unlock()
	if response == nil {
		transport.CloseIdleConnections()
		finish(nil, roundTripErr)
		return nil, roundTripErr
	}
	response.Body = &transportBody{ReadCloser: response.Body, transport: transport}
	finish(response, roundTripErr)
	return response, roundTripErr
}

type transportBody struct {
	ReadCloser io.ReadCloser
	transport  *stdhttp.Transport
	once       sync.Once
}

func (b *transportBody) Read(value []byte) (int, error) {
	return b.ReadCloser.Read(value)
}

func (b *transportBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.transport.CloseIdleConnections)
	return err
}

var _ stdhttp.RoundTripper = (*secureRoundTripper)(nil)
