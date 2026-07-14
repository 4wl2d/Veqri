package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAllowPairingClaimEnforcesPerIPLimitWithoutChargingRejectedAttemptsGlobally(t *testing.T) {
	server := &Server{}
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	const exhaustedAddress = "192.0.2.10"

	for attempt := 1; attempt <= pairingClaimsPerIP; attempt++ {
		if !server.allowPairingClaim(exhaustedAddress, now) {
			t.Fatalf("attempt %d for one IP was denied; want first %d allowed", attempt, pairingClaimsPerIP)
		}
	}
	if server.allowPairingClaim(exhaustedAddress, now) {
		t.Fatalf("attempt %d for one IP was allowed", pairingClaimsPerIP+1)
	}

	// Repeated requests already rejected by the per-IP ceiling must not consume
	// the global budget and deny an unrelated peer.
	for attempt := 0; attempt < pairingClaimsGlobal; attempt++ {
		if server.allowPairingClaim(exhaustedAddress, now) {
			t.Fatalf("rejected IP was unexpectedly admitted again on attempt %d", attempt+pairingClaimsPerIP+2)
		}
	}
	if !server.allowPairingClaim("192.0.2.11", now) {
		t.Fatal("a distinct IP was denied after another IP exhausted only its own limit")
	}
}

func TestAllowPairingClaimEnforcesGlobalLimitAcrossDistinctIPs(t *testing.T) {
	server := &Server{}
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)

	for attempt := 1; attempt <= pairingClaimsGlobal; attempt++ {
		address := fmt.Sprintf("198.51.100.%d", attempt)
		if !server.allowPairingClaim(address, now) {
			t.Fatalf("global attempt %d was denied; want first %d allowed", attempt, pairingClaimsGlobal)
		}
	}
	if server.allowPairingClaim("203.0.113.1", now) {
		t.Fatalf("global attempt %d was allowed", pairingClaimsGlobal+1)
	}
}

func TestAllowPairingClaimExpiresAttemptsAtExactWindowBoundary(t *testing.T) {
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)

	t.Run("per IP", func(t *testing.T) {
		server := &Server{}
		const address = "192.0.2.20"
		for attempt := 0; attempt < pairingClaimsPerIP; attempt++ {
			if !server.allowPairingClaim(address, now) {
				t.Fatalf("setup attempt %d was denied", attempt+1)
			}
		}
		if server.allowPairingClaim(address, now.Add(pairingClaimWindow-time.Nanosecond)) {
			t.Fatal("per-IP attempt was allowed before the rolling window elapsed")
		}
		if !server.allowPairingClaim(address, now.Add(pairingClaimWindow)) {
			t.Fatal("per-IP attempt was denied at the exact rolling-window boundary")
		}
	})

	t.Run("global", func(t *testing.T) {
		server := &Server{}
		for attempt := 1; attempt <= pairingClaimsGlobal; attempt++ {
			if !server.allowPairingClaim(fmt.Sprintf("203.0.113.%d", attempt), now) {
				t.Fatalf("setup attempt %d was denied", attempt)
			}
		}
		if server.allowPairingClaim("198.51.100.100", now.Add(pairingClaimWindow-time.Nanosecond)) {
			t.Fatal("global attempt was allowed before the rolling window elapsed")
		}
		if !server.allowPairingClaim("198.51.100.100", now.Add(pairingClaimWindow)) {
			t.Fatal("global attempt was denied at the exact rolling-window boundary")
		}
	})
}

func TestPairingClaimAliasesShareLimitByRemoteAddress(t *testing.T) {
	server := &Server{}
	reached := 0
	claimHandler := server.pairingClaimRateLimit(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		reached++
		writer.WriteHeader(http.StatusNoContent)
	}))
	mux := http.NewServeMux()
	mux.Handle("POST /v1/pairing/claim", claimHandler)
	mux.Handle("POST /v1/pairings/claim", claimHandler)

	paths := []string{
		"/v1/pairing/claim",
		"/v1/pairings/claim",
		"/v1/pairing/claim",
		"/v1/pairings/claim",
		"/v1/pairing/claim",
		"/v1/pairings/claim",
	}
	for index, path := range paths {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		request.RemoteAddr = "192.0.2.30:43210"
		request.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", index+1))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)

		wantStatus := http.StatusNoContent
		if index == pairingClaimsPerIP {
			wantStatus = http.StatusTooManyRequests
		}
		if response.Code != wantStatus {
			t.Fatalf("request %d to %s returned %d, want %d", index+1, path, response.Code, wantStatus)
		}
		if wantStatus == http.StatusTooManyRequests && response.Header().Get("Retry-After") == "" {
			t.Fatal("rate-limited pairing response omitted Retry-After")
		}
	}
	if reached != pairingClaimsPerIP {
		t.Fatalf("downstream handler reached %d times, want %d", reached, pairingClaimsPerIP)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/pairing/claim", nil)
	request.RemoteAddr = "192.0.2.31:43210"
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("distinct RemoteAddr returned %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestServerHandlerPairingClaimAliasesShareLimiter(t *testing.T) {
	server := &Server{
		limiters:        make(map[string]*requestLimiter),
		pairingAttempts: make(map[string][]time.Time),
	}
	handler := server.Handler()
	paths := []string{
		"/v1/pairing/claim", "/v1/pairings/claim", "/v1/pairing/claim",
		"/v1/pairings/claim", "/v1/pairing/claim", "/v1/pairings/claim",
	}
	for index, path := range paths {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		request.RemoteAddr = "192.0.2.35:43210"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		wantStatus := http.StatusBadRequest
		if index == pairingClaimsPerIP {
			wantStatus = http.StatusTooManyRequests
		}
		if response.Code != wantStatus {
			t.Fatalf("request %d to production route %s returned %d, want %d: %s",
				index+1, path, response.Code, wantStatus, response.Body.String())
		}
	}
}

func TestPairingClaimRateLimitGroupsIPv6PeersByPrefix(t *testing.T) {
	server := &Server{}
	reached := 0
	handler := server.pairingClaimRateLimit(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		reached++
		writer.WriteHeader(http.StatusNoContent)
	}))
	for attempt := 0; attempt <= pairingClaimsPerIP; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/pairing/claim", nil)
		request.RemoteAddr = fmt.Sprintf("[2001:db8:abcd:1234::%x]:43210", attempt+1)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		wantStatus := http.StatusNoContent
		if attempt == pairingClaimsPerIP {
			wantStatus = http.StatusTooManyRequests
		}
		if response.Code != wantStatus {
			t.Fatalf("IPv6 /64 attempt %d returned %d, want %d", attempt+1, response.Code, wantStatus)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/pairing/claim", nil)
	request.RemoteAddr = "[2001:db8:abcd:1235::1]:43210"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("distinct IPv6 /64 returned %d, want %d", response.Code, http.StatusNoContent)
	}
	if reached != pairingClaimsPerIP+1 {
		t.Fatalf("pairing handler reached %d times, want %d", reached, pairingClaimsPerIP+1)
	}
}

func TestAllowPairingClaimEnforcesLimitsConcurrently(t *testing.T) {
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)

	t.Run("per IP", func(t *testing.T) {
		server := &Server{}
		var admitted atomic.Int64
		var workers sync.WaitGroup
		for attempt := 0; attempt < 100; attempt++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				if server.allowPairingClaim("192.0.2.40", now) {
					admitted.Add(1)
				}
			}()
		}
		workers.Wait()
		if got := admitted.Load(); got != pairingClaimsPerIP {
			t.Fatalf("concurrent per-IP admissions = %d, want %d", got, pairingClaimsPerIP)
		}
	})

	t.Run("global", func(t *testing.T) {
		server := &Server{}
		var admitted atomic.Int64
		var workers sync.WaitGroup
		for attempt := 0; attempt < 100; attempt++ {
			attempt := attempt
			workers.Add(1)
			go func() {
				defer workers.Done()
				if server.allowPairingClaim(fmt.Sprintf("198.51.100.%d", attempt), now) {
					admitted.Add(1)
				}
			}()
		}
		workers.Wait()
		if got := admitted.Load(); got != pairingClaimsGlobal {
			t.Fatalf("concurrent global admissions = %d, want %d", got, pairingClaimsGlobal)
		}
	})
}
