package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"github.com/veqri/veqri/core/agents"
	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/core/policy"
	"github.com/veqri/veqri/core/voice"
	"github.com/veqri/veqri/internal/auth"
	"github.com/veqri/veqri/internal/config"
	"github.com/veqri/veqri/internal/managedcore"
	"github.com/veqri/veqri/internal/stream"
	"github.com/veqri/veqri/tools/shell"
)

type Server struct {
	config          config.Config
	store           *persistence.Store
	authenticator   *auth.Authenticator
	adminToken      string
	runtime         *agents.Runtime
	registry        *agents.Registry
	policy          *policy.Engine
	shell           *shell.Executor
	hub             *stream.Hub
	media           voice.MediaTransport
	tts             voice.StreamingTTS
	logger          *slog.Logger
	startedAt       time.Time
	limitMu         sync.Mutex
	limiters        map[string]*requestLimiter
	pairingLimitMu  sync.Mutex
	pairingAttempts map[string][]time.Time
	pairingGlobal   []time.Time
	voiceMu         sync.Mutex
	voiceCancels    map[string]context.CancelFunc
	voiceByConv     map[string]string
	voiceQueues     map[string][]voiceDeliveryJob
	voiceSpeaking   map[string]bool
	mediaSessions   map[string]voice.MediaSession
	desktopSequence atomic.Uint64
	desktopRevision atomic.Int64
	deviceMu        sync.Mutex
	deviceSockets   map[string]map[*websocket.Conn]struct{}
}

const retentionSweepInterval = 6 * time.Hour

func NewServer(cfg config.Config, store *persistence.Store, adminToken string,
	runtime *agents.Runtime, registry *agents.Registry, policyEngine *policy.Engine,
	shellExecutor *shell.Executor, hub *stream.Hub, media voice.MediaTransport,
	tts voice.StreamingTTS, logger *slog.Logger) *Server {
	return &Server{
		config: cfg, store: store, authenticator: auth.New(adminToken, store),
		adminToken: adminToken, runtime: runtime, registry: registry, policy: policyEngine,
		shell: shellExecutor, hub: hub, media: media, tts: tts, logger: logger,
		startedAt: time.Now().UTC(), limiters: make(map[string]*requestLimiter),
		pairingAttempts: make(map[string][]time.Time),
		voiceCancels:    make(map[string]context.CancelFunc), voiceByConv: make(map[string]string),
		voiceQueues: make(map[string][]voiceDeliveryJob), voiceSpeaking: make(map[string]bool),
		mediaSessions: make(map[string]voice.MediaSession),
		deviceSockets: make(map[string]map[*websocket.Conn]struct{}),
	}
}

type requestLimiter struct {
	bucket   *rate.Limiter
	lastSeen time.Time
}

type voiceDeliveryJob struct {
	TaskID string
	Text   string
}

func (s *Server) StartBackground(ctx context.Context) {
	s.recoverVoiceRouting(ctx)
	s.startRetentionSweeps(ctx)
	go s.voiceDeliveryLoop(ctx)
	go s.recoverPendingEvents(ctx)
	go func() {
		<-ctx.Done()
		if err := s.closeAllMediaSessions(); err != nil {
			s.logger.Warn("close voice media during shutdown", "error", err)
		}
	}()
}

func (s *Server) startRetentionSweeps(ctx context.Context) {
	if _, enabled := retentionCutoff(time.Now().UTC(), s.config.RetentionDays); !enabled {
		s.logger.Info("automatic retention disabled", "retention_days", 0)
		return
	}
	go func() {
		if cutoff, ok := retentionCutoff(time.Now().UTC(), s.config.RetentionDays); ok {
			s.applyRetentionSweep(ctx, cutoff)
		}
		ticker := time.NewTicker(retentionSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if nextCutoff, ok := retentionCutoff(now.UTC(), s.config.RetentionDays); ok {
					s.applyRetentionSweep(ctx, nextCutoff)
				}
			}
		}
	}()
}

func retentionCutoff(now time.Time, retentionDays int) (time.Time, bool) {
	if retentionDays <= 0 {
		return time.Time{}, false
	}
	return now.UTC().AddDate(0, 0, -retentionDays), true
}

func (s *Server) applyRetentionSweep(ctx context.Context, cutoff time.Time) {
	result, err := s.store.ApplyRetentionSweep(ctx, cutoff, time.Now().UTC())
	if err != nil {
		s.logger.Error("automatic retention sweep failed", "error", err)
		return
	}
	s.logger.Info("automatic retention sweep completed",
		"cutoff", result.Cutoff, "turns_deleted", result.TurnsDeleted,
		"tasks_scrubbed", result.TasksScrubbed, "events_scrubbed", result.EventsScrubbed,
		"audit_entries_deleted", result.AuditEntriesDeleted)
	s.hub.Publish(stream.Event{Type: "retention.sweep", Payload: result})
	s.hub.Publish(stream.Event{Type: "snapshot.changed", Payload: map[string]any{"reason": "retention.sweep"}})
}

func (s *Server) recoverVoiceRouting(ctx context.Context) {
	sessions, err := s.store.ListVoiceSessions(ctx, true, 1000)
	if err != nil {
		s.logger.Error("recover voice routing", "error", err)
		return
	}
	s.voiceMu.Lock()
	defer s.voiceMu.Unlock()
	for _, session := range sessions {
		s.voiceByConv[session.ConversationID] = session.ID
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.Handle("GET /metrics", s.adminOnly(http.HandlerFunc(s.handleMetrics)))
	pairingClaim := s.pairingClaimRateLimit(http.HandlerFunc(s.handleClaimPairing))
	mux.Handle("POST /v1/pairings/claim", pairingClaim)
	mux.Handle("POST /v1/pairing/claim", pairingClaim)
	mux.HandleFunc("POST /v1/connectors/slack/events", s.handleSlackEvent)
	mux.HandleFunc("POST /v1/connectors/mattermost/outgoing", s.handleMattermostEvent)
	mux.HandleFunc("POST /v1/webhooks/{connectorID}", s.handleGenericWebhook)

	mux.Handle("POST /v1/protocol/negotiate", s.authenticated(http.HandlerFunc(s.handleNegotiate)))
	mux.Handle("POST /v1/connectors/simulate/{kind}", s.adminOnly(http.HandlerFunc(s.handleConnectorSimulator)))
	mux.Handle("POST /v1/pairings", s.adminOnly(http.HandlerFunc(s.handleCreatePairing)))
	mux.Handle("GET /v1/devices", s.adminOnly(http.HandlerFunc(s.handleDevices)))
	mux.Handle("POST /v1/devices/self/credential-rotation/prepare", s.authenticated(http.HandlerFunc(s.handlePrepareDeviceCredentialRotation)))
	mux.HandleFunc("POST /v1/devices/self/credential-rotation/confirm", s.handleConfirmDeviceCredentialRotation)
	mux.Handle("POST /v1/devices/self/credential-rotation/cancel", s.authenticated(http.HandlerFunc(s.handleCancelDeviceCredentialRotation)))
	mux.Handle("POST /v1/devices/{id}/revoke", s.adminOnly(http.HandlerFunc(s.handleRevokeDevice)))
	mux.Handle("POST /v1/ask", s.authenticated(http.HandlerFunc(s.handleAsk)))
	mux.Handle("PUT /v1/conversations/{id}/transcript-retention", s.authenticated(http.HandlerFunc(s.handleTranscriptRetention)))
	mux.Handle("POST /v1/events", s.adminOnly(http.HandlerFunc(s.handleLocalEvent)))
	mux.Handle("POST /v1/events/{id}/replay", s.adminOnly(http.HandlerFunc(s.handleReplayEvent)))
	mux.Handle("GET /v1/tasks", s.authenticated(http.HandlerFunc(s.handleTasks)))
	mux.Handle("GET /v1/tasks/{id}", s.authenticated(http.HandlerFunc(s.handleTask)))
	mux.Handle("GET /v1/tasks/{id}/graph", s.authenticated(http.HandlerFunc(s.handleTaskGraph)))
	mux.Handle("POST /v1/tasks/{id}/cancel", s.authenticated(http.HandlerFunc(s.handleCancelTask)))
	mux.Handle("POST /v1/tasks/{id}/priority", s.authenticated(http.HandlerFunc(s.handleTaskPriority)))
	mux.Handle("POST /v1/tasks/{id}/dismiss", s.authenticated(http.HandlerFunc(s.handleDismissTask)))
	mux.Handle("POST /v1/tools/shell", s.adminOnly(http.HandlerFunc(s.handleShell)))
	mux.Handle("GET /v1/approvals", s.authenticated(http.HandlerFunc(s.handleApprovals)))
	mux.Handle("POST /v1/approvals/{id}/approve", s.authenticated(http.HandlerFunc(s.handleApprove)))
	mux.Handle("POST /v1/approvals/{id}/deny", s.authenticated(http.HandlerFunc(s.handleDeny)))
	mux.Handle("POST /v1/voice/calls", s.adminOnly(http.HandlerFunc(s.handleStartCall)))
	mux.Handle("GET /v1/voice/sessions/{id}", s.authenticated(http.HandlerFunc(s.handleVoiceSession)))
	mux.Handle("POST /v1/voice/sessions/{id}/answer", s.authenticated(http.HandlerFunc(s.handleAnswerCall)))
	mux.Handle("POST /v1/voice/sessions/{id}/transcript", s.authenticated(http.HandlerFunc(s.handleVoiceTranscript)))
	mux.Handle("POST /v1/voice/sessions/{id}/interrupt", s.authenticated(http.HandlerFunc(s.handleInterruptVoice)))
	mux.Handle("POST /v1/voice/sessions/{id}/reconnect", s.authenticated(http.HandlerFunc(s.handleReconnectVoice)))
	mux.Handle("POST /v1/voice/sessions/{id}/end", s.authenticated(http.HandlerFunc(s.handleEndVoice)))
	mux.Handle("GET /v1/audit", s.adminOnly(http.HandlerFunc(s.handleAudit)))
	mux.Handle("GET /v1/diagnostics", s.adminOnly(http.HandlerFunc(s.handleDiagnostics)))
	mux.Handle("POST /v1/emergency-stop", s.adminOnly(http.HandlerFunc(s.handleEmergencyStop)))
	mux.Handle("GET /v1/stream", http.HandlerFunc(s.handleWebSocket))
	mux.Handle("GET /v1/device/events", http.HandlerFunc(s.handleDeviceWebSocket))

	// Desktop contract aliases. The desktop remains a transport-only client.
	mux.Handle("GET /api/v1/desktop/snapshot", s.adminOnly(http.HandlerFunc(s.handleDesktopSnapshot)))
	mux.Handle("POST /api/v1/desktop/actions", s.adminOnly(http.HandlerFunc(s.handleDesktopAction)))
	mux.Handle("GET /api/v1/events", http.HandlerFunc(s.handleDesktopWebSocket))

	return s.securityHeaders(s.cors(s.rateLimit(mux)))
}

type principalContextKey struct{}

func principalFromContext(ctx context.Context) auth.Principal {
	principal, _ := ctx.Value(principalContextKey{}).(auth.Principal)
	return principal
}

func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		token := auth.BearerToken(request.Header.Get("Authorization"))
		principal, err := s.authenticator.Authenticate(request.Context(), token)
		if err != nil {
			writeError(writer, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		if !supportedProtocol(request.Header.Get("X-Veqri-Protocol-Version")) {
			writeError(writer, http.StatusUpgradeRequired, "protocol_version", "protocol version 1 is required")
			return
		}
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal)))
	})
}

func (s *Server) adminOnly(next http.Handler) http.Handler {
	return s.authenticated(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if principalFromContext(request.Context()).Kind != "admin" {
			writeError(writer, http.StatusForbidden, "admin_required", "local administrator credential required")
			return
		}
		next.ServeHTTP(writer, request)
	}))
}

func supportedProtocol(value string) bool {
	if value == "1" {
		return true
	}
	major, minor, found := strings.Cut(value, ".")
	if !found || major != "1" || minor == "" {
		return false
	}
	for _, character := range minor {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func (s *Server) pairingClaimRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		now := time.Now().UTC()
		if !s.allowPairingClaim(pairingClaimAddress(request), now) {
			writer.Header().Set("Retry-After", "60")
			writeError(writer, http.StatusTooManyRequests, "pairing_rate_limited", "pairing claim rate exceeded")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func pairingClaimAddress(request *http.Request) string {
	address := requestIP(request)
	withoutZone, _, _ := strings.Cut(address, "%")
	ip := net.ParseIP(withoutZone)
	if ip == nil {
		return address
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

const (
	pairingClaimWindow      = time.Minute
	pairingClaimsPerIP      = 5
	pairingClaimsGlobal     = 30
	pairingAttemptMapTarget = 256
)

func (s *Server) allowPairingClaim(address string, now time.Time) bool {
	s.pairingLimitMu.Lock()
	defer s.pairingLimitMu.Unlock()
	cutoff := now.Add(-pairingClaimWindow)
	s.pairingGlobal = recentAttempts(s.pairingGlobal, cutoff)
	// Check the global ceiling before allocating per-address state. This keeps
	// an exhausted global limiter from becoming an attacker-controlled map-growth path.
	if len(s.pairingGlobal) >= pairingClaimsGlobal {
		return false
	}
	if s.pairingAttempts == nil {
		s.pairingAttempts = make(map[string][]time.Time)
	}
	attempts := recentAttempts(s.pairingAttempts[address], cutoff)
	if len(attempts) >= pairingClaimsPerIP {
		s.pairingAttempts[address] = attempts
		return false
	}
	attempts = append(attempts, now)
	s.pairingAttempts[address] = attempts
	s.pairingGlobal = append(s.pairingGlobal, now)
	if len(s.pairingAttempts) > pairingAttemptMapTarget {
		for candidate, timestamps := range s.pairingAttempts {
			timestamps = recentAttempts(timestamps, cutoff)
			if len(timestamps) == 0 {
				delete(s.pairingAttempts, candidate)
			} else {
				s.pairingAttempts[candidate] = timestamps
			}
		}
	}
	return true
}

func recentAttempts(attempts []time.Time, cutoff time.Time) []time.Time {
	firstRecent := 0
	for firstRecent < len(attempts) && !attempts[firstRecent].After(cutoff) {
		firstRecent++
	}
	return attempts[firstRecent:]
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		origin := request.Header.Get("Origin")
		if origin != "" {
			if !localOrigin(origin) {
				writeError(writer, http.StatusForbidden, "origin_denied", "only loopback web origins are allowed")
				return
			}
			writer.Header().Set("Access-Control-Allow-Origin", origin)
			writer.Header().Set("Vary", "Origin")
			writer.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Veqri-Protocol-Version, X-Veqri-Client")
			writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		}
		if request.Method == http.MethodOptions {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func localOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return false
	}
	// Wails v2 uses these fixed origins for packaged desktop webviews. They are
	// application-local origins, not arbitrary LAN hosts.
	if parsed.Scheme == "wails" && parsed.Host == "wails" && parsed.Port() == "" {
		return true
	}
	if parsed.Scheme == "http" && strings.EqualFold(parsed.Host, "wails.localhost") && parsed.Port() == "" {
		return true
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		now := time.Now().UTC()
		key := requestIP(request)
		s.limitMu.Lock()
		entry := s.limiters[key]
		if entry == nil {
			entry = &requestLimiter{bucket: rate.NewLimiter(rate.Limit(25), 50)}
			s.limiters[key] = entry
		}
		entry.lastSeen = now
		allowed := entry.bucket.Allow()
		if len(s.limiters) > 1024 {
			for address, candidate := range s.limiters {
				if now.Sub(candidate.lastSeen) > 10*time.Minute {
					delete(s.limiters, address)
				}
			}
		}
		s.limitMu.Unlock()
		if !allowed {
			writer.Header().Set("Retry-After", "1")
			writeError(writer, http.StatusTooManyRequests, "rate_limited", "request rate exceeded")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) handleHealth(writer http.ResponseWriter, _ *http.Request) {
	if s.config.ManagedCoreOwnerToken != "" {
		writer.Header().Set(managedcore.OwnerTokenHeader, managedcore.OwnerProof(s.config.ManagedCoreOwnerToken))
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok", "version": "0.1.0", "protocol_version": 1,
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
	})
}

func (s *Server) timeSinceStart() time.Duration { return time.Since(s.startedAt) }

func (s *Server) handleReady(writer http.ResponseWriter, request *http.Request) {
	if err := s.store.Ping(request.Context()); err != nil {
		writeError(writer, http.StatusServiceUnavailable, "database_unavailable", "database is not ready")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) handleNegotiate(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"selected": map[string]any{"major": 1, "minor": 0,
			"capabilities": []string{"tasks", "approvals", "simulated_voice", "websocket_events"}},
	})
}

func (s *Server) handleWebSocket(writer http.ResponseWriter, request *http.Request) {
	token := auth.BearerToken(request.Header.Get("Authorization"))
	if token == "" {
		token = websocketProtocolToken(request.Header.Get("Sec-WebSocket-Protocol"))
	}
	principal, err := s.authenticator.Authenticate(request.Context(), token)
	if err != nil || principal.Kind != "admin" {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "WebSocket authentication required")
		return
	}
	if !hasWebSocketProtocol(request.Header.Values("Sec-WebSocket-Protocol"), "veqri.v1") {
		writeError(writer, http.StatusUpgradeRequired, "websocket_protocol", "WebSocket subprotocol veqri.v1 is required")
		return
	}
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		Subprotocols:   []string{"veqri.v1"},
		OriginPatterns: desktopWebSocketOrigins(),
	})
	if err != nil {
		s.logger.Warn("accept websocket", "error", err)
		return
	}
	defer func() { _ = connection.Close(websocket.StatusNormalClosure, "stream ended") }()
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	eventsChannel := s.hub.Subscribe(ctx, 128)
	if err := writeWebSocketJSON(ctx, connection, stream.Event{Type: "stream.connected",
		Payload: map[string]any{"principal": principal, "protocol_version": 1}}); err != nil {
		return
	}
	pingTicker := time.NewTicker(20 * time.Second)
	defer pingTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-eventsChannel:
			if !ok || writeWebSocketJSON(ctx, connection, event) != nil {
				return
			}
		case <-pingTicker.C:
			pingContext, pingCancel := context.WithTimeout(ctx, 5*time.Second)
			err := connection.Ping(pingContext)
			pingCancel()
			if err != nil {
				return
			}
		}
	}
}

func desktopWebSocketOrigins() []string {
	return []string{"localhost:*", "127.0.0.1:*", "[::1]:*", "wails", "wails.localhost"}
}

func websocketProtocolToken(header string) string {
	for _, protocol := range strings.Split(header, ",") {
		protocol = strings.TrimSpace(protocol)
		if encoded, ok := strings.CutPrefix(protocol, "veqri.auth."); ok {
			decoded, err := base64.RawURLEncoding.DecodeString(encoded)
			if err == nil {
				return string(decoded)
			}
		}
	}
	return ""
}

func hasWebSocketProtocol(headers []string, wanted string) bool {
	for _, header := range headers {
		for _, protocol := range strings.Split(header, ",") {
			if strings.TrimSpace(protocol) == wanted {
				return true
			}
		}
	}
	return false
}

func writeWebSocketJSON(ctx context.Context, connection *websocket.Conn, value any) error {
	writeContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return connection.Write(writeContext, websocket.MessageText, mustJSON(value))
}

func requestIP(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}
	return host
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		message := "request body must contain exactly one JSON value"
		if err != nil {
			message = err.Error()
		}
		writeError(writer, http.StatusBadRequest, "invalid_json", message)
		return false
	}
	return true
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		return
	}
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Errorf("marshal internal API value: %w", err))
	}
	return encoded
}

func persistenceStatus(err error) int {
	switch {
	case errors.Is(err, persistence.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, persistence.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, persistence.ErrExpired):
		return http.StatusGone
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) verifyVoiceOwner(ctx context.Context, session conversation.VoiceSession) error {
	principal := principalFromContext(ctx)
	if principal.Kind == "admin" || (principal.Kind == "device" && principal.ID == session.DeviceID) {
		return nil
	}
	return errors.New("voice session belongs to another device")
}
