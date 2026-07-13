package coreapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/veqri/veqri/agents/general"
	"github.com/veqri/veqri/agents/mock"
	"github.com/veqri/veqri/agents/remote"
	"github.com/veqri/veqri/agents/stdio"
	"github.com/veqri/veqri/core/agents"
	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/core/policy"
	"github.com/veqri/veqri/core/voice"
	"github.com/veqri/veqri/internal/api"
	"github.com/veqri/veqri/internal/auth"
	"github.com/veqri/veqri/internal/config"
	"github.com/veqri/veqri/internal/secrets"
	"github.com/veqri/veqri/internal/stream"
	"github.com/veqri/veqri/tools/shell"
)

// Run starts Veqri Core and blocks until ctx is cancelled or the HTTP server
// exits. Keeping the runtime in a package lets the standalone daemon and the
// native desktop application share exactly the same Core implementation.
func Run(ctx context.Context, version string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := resolveConnectorSecrets(&cfg); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)}))
	listener, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Address, err)
	}
	defer listener.Close()
	adminToken, tokenPath, err := auth.LoadOrCreateAdminToken(cfg.DataDir, cfg.AuthToken)
	if err != nil {
		return err
	}
	store, err := persistence.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	workspaceList, err := workspaces()
	if err != nil {
		return err
	}
	shellExecutor, err := shell.New(workspaceList, nil, []string{
		adminToken, cfg.WebhookSecret, cfg.SlackSigningSecret, cfg.MattermostToken,
		cfg.ManagedCoreOwnerToken,
	})
	if err != nil {
		return err
	}
	registry := agents.NewRegistry()
	for _, definition := range []struct{ id, name, capability string }{
		{"builtin.general", "General dialog agent", "dialog"},
		{"builtin.planner", "Task planner", "planning"},
		{"builtin.coding", "Coding agent", "coding"},
		{"builtin.research", "Research agent", "research"},
		{"builtin.automation", "Desktop automation agent", "automation"},
		{"builtin.mock", "Mock deterministic agent", "testing"},
	} {
		if err := registry.Register(mock.New(definition.id, definition.name, definition.capability, 25*time.Millisecond)); err != nil {
			return err
		}
	}
	if cfg.RemoteAgentEndpoint != "" {
		runner, err := remote.New(remote.Config{
			Endpoint: cfg.RemoteAgentEndpoint, AllowInsecureLoopback: true,
			TokenSource: remote.TokenSourceFunc(func(context.Context) (string, error) {
				return secrets.Resolve(cfg.RemoteAgentTokenRef)
			}),
			Definition: externalAgentDefinition(cfg.RemoteAgentID, "Authenticated HTTP agent", agents.ModeHTTP),
		})
		if err != nil {
			return fmt.Errorf("configure remote agent: %w", err)
		}
		if err := registry.Register(runner); err != nil {
			return err
		}
	}
	if cfg.StdioAgentCommand != "" {
		runner, err := stdio.New(stdio.Config{
			Command: cfg.StdioAgentCommand, Args: cfg.StdioAgentArgs,
			WorkingDirectory: workspaceList[0], InheritEnvironment: false,
			Definition: externalAgentDefinition(cfg.StdioAgentID, "Structured local subprocess agent", agents.ModeStdio),
		})
		if err != nil {
			return fmt.Errorf("configure stdio agent: %w", err)
		}
		if err := registry.Register(runner); err != nil {
			return err
		}
	}
	if err := registry.Register(general.NewSynthesizer(store)); err != nil {
		return err
	}
	hub := stream.New()
	policyEngine := policy.NewEngine()
	var persistedKillSwitches policy.KillSwitches
	if err := store.GetSetting(ctx, "kill_switches", &persistedKillSwitches); err == nil {
		policyEngine.LoadKillSwitches(persistedKillSwitches)
	}
	var persistedEmergencyStop bool
	if err := store.GetSetting(ctx, "emergency_stop", &persistedEmergencyStop); err == nil {
		policyEngine.SetEmergencyStop(persistedEmergencyStop)
	}
	runtimeEngine := agents.NewRuntime(store, registry, shellExecutor, hub, logger, cfg.WorkerCount)
	runtimeEngine.SetExecutionGates(
		func() bool { return !policyEngine.EmergencyStop() },
		func(agentID string) bool { return !policyEngine.AgentDisabled(agentID) },
	)
	runtimeEngine.SetDeliveryGate(func(connectorID string) bool { return !policyEngine.ConnectorDisabled(connectorID) })
	if err := runtimeEngine.Start(ctx); err != nil {
		return err
	}
	media := voice.NewSimulatedTransport()
	serverAPI := api.NewServer(cfg, store, adminToken, runtimeEngine, registry, policyEngine,
		shellExecutor, hub, media, voice.MockTTS{ChunkDelay: 15 * time.Millisecond}, logger)
	serverAPI.StartBackground(ctx)
	httpServer := &http.Server{
		Addr: cfg.Address, Handler: serverAPI.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 0, IdleTimeout: 2 * time.Minute,
		MaxHeaderBytes: 32 << 10,
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("Veqri Core started", "version", version, "address", listener.Addr().String(),
			"database", cfg.DatabasePath, "admin_token_source", tokenPath,
			"media_transport", media.Name(), "workspaces", workspaceList)
		if cfg.TLSCertFile != "" {
			serverErrors <- httpServer.ServeTLS(listener, cfg.TLSCertFile, cfg.TLSKeyFile)
			return
		}
		serverErrors <- httpServer.Serve(listener)
	}()
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		logger.Info("Veqri Core stopped")
		return nil
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func externalAgentDefinition(id, name string, mode agents.ExecutionMode) agents.Definition {
	return agents.Definition{
		ID: id, DisplayName: name, Description: "Explicitly configured production adapter",
		Capabilities:      []string{"dialog", "planning", "coding", "research", "automation"},
		AcceptedTaskTypes: []string{"dialog", "planning", "coding", "research", "automation"},
		InputSchema:       []byte(`{"type":"object"}`), OutputSchema: []byte(`{"type":"object"}`),
		ToolScopes: []string{}, TrustLevel: agents.TrustKnown, ConcurrencyLimit: 2,
		Health: agents.HealthUnknown, ExecutionMode: mode,
		SupportsCancellation: true, SupportsStreaming: true, UpdatedAt: time.Now().UTC(),
	}
}

func resolveConnectorSecrets(cfg *config.Config) error {
	values := []struct {
		name      string
		direct    *string
		reference string
	}{
		{name: "generic webhook", direct: &cfg.WebhookSecret, reference: cfg.WebhookSecretRef},
		{name: "Slack signing", direct: &cfg.SlackSigningSecret, reference: cfg.SlackSigningSecretRef},
		{name: "Mattermost", direct: &cfg.MattermostToken, reference: cfg.MattermostTokenRef},
	}
	for _, value := range values {
		if value.reference == "" {
			continue
		}
		resolved, err := secrets.Resolve(value.reference)
		if err != nil {
			return fmt.Errorf("resolve %s secret reference: %w", value.name, err)
		}
		*value.direct = resolved
	}
	return nil
}

func workspaces() ([]string, error) {
	configured := os.Getenv("VEQRI_WORKSPACES")
	if configured != "" {
		var result []string
		for _, value := range strings.Split(configured, string(os.PathListSeparator)) {
			if strings.TrimSpace(value) != "" {
				result = append(result, value)
			}
		}
		if len(result) > 0 {
			return result, nil
		}
	}
	current, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return []string{filepath.Clean(current)}, nil
}

func logLevel(value string) slog.Level {
	switch strings.ToLower(value) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
