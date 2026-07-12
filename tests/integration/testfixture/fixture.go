package testfixture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veqri/veqri/agents/general"
	"github.com/veqri/veqri/agents/mock"
	coreagents "github.com/veqri/veqri/core/agents"
	"github.com/veqri/veqri/core/persistence"
	"github.com/veqri/veqri/core/policy"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/core/voice"
	"github.com/veqri/veqri/internal/api"
	"github.com/veqri/veqri/internal/config"
	"github.com/veqri/veqri/internal/stream"
	"github.com/veqri/veqri/tools/shell"
)

const (
	AdminToken       = "integration-admin-token-0123456789-abcdef"
	SlackSecret      = "integration-slack-signing-secret"
	WebhookSecret    = "integration-webhook-signing-secret"
	MattermostSecret = "integration-mattermost-token"
	DefaultTimeout   = 10 * time.Second
)

// Options controls only deterministic test seams. Every request still goes
// through the production HTTP handlers, persistence layer, runtime and policy.
type Options struct {
	DatabasePath string
	Workspace    string
	WorkerCount  int
	NoWorkers    bool
	AgentDelay   time.Duration
	TTS          voice.StreamingTTS
	Media        voice.MediaTransport
	Runners      []coreagents.Runner
}

type Fixture struct {
	Store      *persistence.Store
	Registry   *coreagents.Registry
	Runtime    *coreagents.Runtime
	Hub        *stream.Hub
	Policy     *policy.Engine
	Shell      *shell.Executor
	HTTP       *httptest.Server
	Client     *http.Client
	Database   string
	Workspace  string
	AdminToken string

	cancel    context.CancelFunc
	closeOnce sync.Once
}

func New(t testing.TB, options Options) *Fixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	databasePath := options.DatabasePath
	if databasePath == "" {
		databasePath = filepath.Join(t.TempDir(), "veqri-integration.db")
	}
	workspace := options.Workspace
	if workspace == "" {
		workspace = t.TempDir()
	}
	store, err := persistence.Open(ctx, databasePath)
	if err != nil {
		cancel()
		t.Fatalf("open fixture database: %v", err)
	}
	shellExecutor, err := shell.New([]string{workspace}, nil, []string{
		AdminToken, SlackSecret, WebhookSecret, MattermostSecret,
	})
	if err != nil {
		_ = store.Close()
		cancel()
		t.Fatalf("create shell fixture: %v", err)
	}

	registry := coreagents.NewRegistry()
	overrides := make(map[string]coreagents.Runner, len(options.Runners))
	for _, runner := range options.Runners {
		overrides[runner.Definition().ID] = runner
	}
	defaults := []struct{ id, name, capability string }{
		{"builtin.general", "General dialog agent", "dialog"},
		{"builtin.planner", "Task planner", "planning"},
		{"builtin.coding", "Coding agent", "coding"},
		{"builtin.research", "Research agent", "research"},
		{"builtin.automation", "Desktop automation agent", "automation"},
		{"builtin.mock", "Mock deterministic agent", "testing"},
	}
	for _, definition := range defaults {
		runner := coreagents.Runner(mock.New(definition.id, definition.name, definition.capability, options.AgentDelay))
		if override, ok := overrides[definition.id]; ok {
			runner = override
			delete(overrides, definition.id)
		}
		if err := registry.Register(runner); err != nil {
			_ = store.Close()
			cancel()
			t.Fatalf("register %s: %v", definition.id, err)
		}
	}
	if override, ok := overrides["builtin.synthesizer"]; ok {
		if err := registry.Register(override); err != nil {
			t.Fatalf("register synthesizer override: %v", err)
		}
		delete(overrides, "builtin.synthesizer")
	} else if err := registry.Register(general.NewSynthesizer(store)); err != nil {
		t.Fatalf("register synthesizer: %v", err)
	}
	for id, runner := range overrides {
		if err := registry.Register(runner); err != nil {
			t.Fatalf("register custom runner %s: %v", id, err)
		}
	}

	hub := stream.New()
	policyEngine := policy.NewEngine()
	workers := options.WorkerCount
	if workers == 0 && !options.NoWorkers {
		workers = 4
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtimeEngine := coreagents.NewRuntime(store, registry, shellExecutor, hub, logger, workers)
	if err := runtimeEngine.Start(ctx); err != nil {
		_ = store.Close()
		cancel()
		t.Fatalf("start runtime: %v", err)
	}
	media := options.Media
	if media == nil {
		media = voice.NewSimulatedTransport()
	}
	tts := options.TTS
	if tts == nil {
		tts = voice.MockTTS{ChunkDelay: 2 * time.Millisecond}
	}
	cfg := config.Config{
		Address:             "127.0.0.1:0",
		DataDir:             filepath.Dir(databasePath),
		DatabasePath:        databasePath,
		AuthToken:           AdminToken,
		RetentionDays:       30,
		TranscriptRetention: true,
		WebhookSecret:       WebhookSecret,
		SlackSigningSecret:  SlackSecret,
		MattermostToken:     MattermostSecret,
		STTProvider:         "mock",
		TTSProvider:         "mock",
		MediaTransport:      "simulated",
		WorkerCount:         max(workers, 1),
		ShutdownTimeout:     time.Second,
	}
	server := api.NewServer(cfg, store, AdminToken, runtimeEngine, registry, policyEngine,
		shellExecutor, hub, media, tts, logger)
	server.StartBackground(ctx)
	httpServer := httptest.NewServer(server.Handler())
	fixture := &Fixture{
		Store: store, Registry: registry, Runtime: runtimeEngine, Hub: hub,
		Policy: policyEngine, Shell: shellExecutor, HTTP: httpServer,
		Client: httpServer.Client(), Database: databasePath, Workspace: workspace,
		AdminToken: AdminToken, cancel: cancel,
	}
	t.Cleanup(fixture.Close)
	return fixture
}

func (f *Fixture) Close() {
	f.closeOnce.Do(func() {
		f.cancel()
		f.HTTP.Close()
		_ = f.Store.Close()
	})
}

type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (f *Fixture) JSON(t testing.TB, method, path, token string, body any, headers map[string]string) Response {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s %s request: %v", method, path, err)
		}
	}
	request, err := http.NewRequest(method, f.HTTP.URL+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("create %s %s request: %v", method, path, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Veqri-Protocol-Version", "1")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := f.Client.Do(request)
	if err != nil {
		t.Fatalf("perform %s %s request: %v", method, path, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s %s response: %v", method, path, err)
	}
	return Response{Status: response.StatusCode, Header: response.Header.Clone(), Body: responseBody}
}

func Decode[T any](t testing.TB, response Response) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body, &value); err != nil {
		t.Fatalf("decode status %d response %q: %v", response.Status, response.Body, err)
	}
	return value
}

func RequireStatus(t testing.TB, response Response, expected int) {
	t.Helper()
	if response.Status != expected {
		t.Fatalf("unexpected HTTP status: got %d, want %d; body=%s", response.Status, expected, response.Body)
	}
}

func Eventually(t testing.TB, timeout time.Duration, description string, check func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ready, err := check()
		if ready && err == nil {
			return
		}
		if err != nil {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				t.Fatalf("timed out waiting for %s: %v", description, lastErr)
			}
			t.Fatalf("timed out waiting for %s", description)
		}
		timer := time.NewTimer(5 * time.Millisecond)
		<-timer.C
	}
}

func (f *Fixture) WaitTask(t testing.TB, taskID string, wanted ...tasks.Status) tasks.Task {
	t.Helper()
	allowed := make(map[tasks.Status]bool, len(wanted))
	for _, status := range wanted {
		allowed[status] = true
	}
	var result tasks.Task
	Eventually(t, DefaultTimeout, fmt.Sprintf("task %s in %v", taskID, wanted), func() (bool, error) {
		item, err := f.Store.GetTask(context.Background(), taskID)
		if err != nil {
			return false, err
		}
		result = item
		if item.Status.Terminal() && !allowed[item.Status] {
			return false, fmt.Errorf("task reached unexpected terminal status %s: %s", item.Status, item.Error)
		}
		if !allowed[item.Status] {
			return false, fmt.Errorf("last observed status %s (progress %d, message %q)", item.Status, item.Progress, item.ProgressMessage)
		}
		return true, nil
	})
	return result
}

type PairedDevice struct {
	ID         string
	Credential string
}

func (f *Fixture) PairAndroid(t testing.TB, name string) PairedDevice {
	return f.pairAndroid(t, name, nil)
}

func (f *Fixture) PairAndroidWithRetention(t testing.TB, name string, retainTranscript bool) PairedDevice {
	return f.pairAndroid(t, name, &retainTranscript)
}

func (f *Fixture) pairAndroid(t testing.TB, name string, retainTranscript *bool) PairedDevice {
	t.Helper()
	created := f.JSON(t, http.MethodPost, "/v1/pairings", f.AdminToken, map[string]any{}, nil)
	RequireStatus(t, created, http.StatusCreated)
	var pairing struct {
		Code string `json:"code"`
	}
	pairing = Decode[struct {
		Code string `json:"code"`
	}](t, created)
	claim := map[string]any{
		"code": pairing.Code, "name": name, "platform": "android",
		"client_protocol_version": 1,
		"capabilities":            map[string]any{"text": true, "voice": true, "approvals": true},
	}
	if retainTranscript != nil {
		claim["retain_transcript"] = *retainTranscript
	}
	claimed := f.JSON(t, http.MethodPost, "/v1/pairings/claim", "", claim, nil)
	RequireStatus(t, claimed, http.StatusCreated)
	result := Decode[struct {
		DeviceID   string `json:"device_id"`
		Credential string `json:"credential"`
	}](t, claimed)
	if result.DeviceID == "" || result.Credential == "" {
		t.Fatalf("pairing response omitted device identity: %s", claimed.Body)
	}
	return PairedDevice{ID: result.DeviceID, Credential: result.Credential}
}

func (f *Fixture) WebSocketURL(path string) string {
	parsed, err := url.Parse(f.HTTP.URL)
	if err != nil {
		panic(err)
	}
	parsed.Scheme = "ws"
	parsed.Path = "/" + strings.TrimPrefix(path, "/")
	return parsed.String()
}
