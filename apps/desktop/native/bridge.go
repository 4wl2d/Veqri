package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/veqri/veqri/internal/auth"
	"github.com/veqri/veqri/internal/managedcore"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	desktopCoreModeEnvironment = "VEQRI_DESKTOP_CORE_MODE"
	managedCoreMode            = "managed"
	externalCoreMode           = "external"
	coreStartupTimeout         = 15 * time.Second
	coreErrorOutputLimit       = 8 << 10
)

type boundedTextBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *boundedTextBuffer) Write(value []byte) (int, error) {
	written := len(value)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(value) >= b.limit {
		b.data = append(b.data[:0], value[len(value)-b.limit:]...)
		return written, nil
	}
	overflow := len(b.data) + len(value) - b.limit
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, value...)
	return written, nil
}

func (b *boundedTextBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.data))
}

type capturedCoreErrorWriter struct {
	capture *boundedTextBuffer
	visible io.Writer
}

func (w capturedCoreErrorWriter) Write(value []byte) (int, error) {
	_, _ = w.capture.Write(value)
	if w.visible != nil {
		_, _ = w.visible.Write(value)
	}
	return len(value), nil
}

type RuntimeConfig struct {
	Mode       string `json:"mode"`
	APIBaseURL string `json:"api_base_url"`
	AuthToken  string `json:"auth_token"`
}

// Bridge owns native-only credential injection. The token is delivered to the
// in-process WebView at runtime and is never compiled into frontend assets.
type Bridge struct {
	mu           sync.Mutex
	ctx          context.Context
	core         *exec.Cmd
	coreInput    io.Closer
	coreDone     <-chan error
	ownerToken   string
	startupErr   error
	shuttingDown bool
	ready        bool
	coreExited   bool
	coreExitErr  error
}

func (b *Bridge) Startup(ctx context.Context) {
	b.mu.Lock()
	b.ctx = ctx
	b.startupErr = nil
	b.shuttingDown = false
	b.ready = false
	b.coreExited = false
	b.coreExitErr = nil
	b.mu.Unlock()
	baseURL, err := validateCoreURL(environment("VEQRI_URL", "http://127.0.0.1:7342"))
	if err != nil {
		b.setStartupError(err)
		return
	}

	mode := environment(desktopCoreModeEnvironment, managedCoreMode)
	switch mode {
	case externalCoreMode:
		return
	case managedCoreMode:
	default:
		b.setStartupError(fmt.Errorf("%s must be %q or %q", desktopCoreModeEnvironment, managedCoreMode, externalCoreMode))
		return
	}

	probeContext, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	err = probeVeqriCore(probeContext, baseURL, "")
	cancel()
	if err == nil {
		b.setStartupError(errors.New("a Veqri Core is already using this loopback origin; set VEQRI_DESKTOP_CORE_MODE=external to trust and use it"))
		return
	}
	if err := b.startManagedCore(baseURL); err != nil {
		b.setStartupError(err)
	}
}

func (b *Bridge) Shutdown(context.Context) {
	b.mu.Lock()
	b.shuttingDown = true
	input := b.coreInput
	command := b.core
	done := b.coreDone
	b.coreInput = nil
	b.core = nil
	b.coreDone = nil
	b.ownerToken = ""
	b.ctx = nil
	b.ready = false
	b.mu.Unlock()

	if input == nil {
		return
	}
	_ = input.Close()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(12 * time.Second):
		if command != nil && command.Process != nil {
			_ = command.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *Bridge) GetRuntimeConfig() (RuntimeConfig, error) {
	baseURL, err := validateCoreURL(environment("VEQRI_URL", "http://127.0.0.1:7342"))
	if err != nil {
		return RuntimeConfig{}, err
	}
	b.mu.Lock()
	startupErr := b.startupErr
	coreDone := b.coreDone
	ownerToken := b.ownerToken
	b.mu.Unlock()
	if startupErr != nil {
		return RuntimeConfig{}, startupErr
	}
	startupContext, cancel := context.WithTimeout(context.Background(), coreStartupTimeout)
	defer cancel()
	if err := waitForVeqriCore(startupContext, baseURL, ownerToken, coreDone); err != nil {
		return RuntimeConfig{}, err
	}

	token, err := loadAdminToken()
	if err != nil {
		return RuntimeConfig{}, err
	}
	verifyContext, verifyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer verifyCancel()
	if err := verifyVeqriCoreCredential(verifyContext, baseURL, token); err != nil {
		return RuntimeConfig{}, fmt.Errorf("verify Veqri Core credential: %w", err)
	}
	if err := b.markCoreReady(); err != nil {
		return RuntimeConfig{}, err
	}
	return RuntimeConfig{Mode: "live", APIBaseURL: baseURL, AuthToken: token}, nil
}

func loadAdminToken() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dataDir := environment("VEQRI_DATA_DIR", filepath.Join(home, ".veqri"))
	token, _, err := auth.ReadAdminToken(dataDir)
	if err != nil {
		return "", errors.New("Veqri admin token is unavailable after Core startup")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("Veqri admin token is empty")
	}
	return token, nil
}

func (b *Bridge) setStartupError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.startupErr = err
}

func (b *Bridge) startManagedCore(baseURL string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve desktop executable: %w", err)
	}
	workspaces, err := managedWorkspace()
	if err != nil {
		return err
	}
	ownerToken, err := auth.RandomToken(32)
	if err != nil {
		return fmt.Errorf("generate managed Core owner token: %w", err)
	}
	childInput, parentInput, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create managed Core lifecycle pipe: %w", err)
	}
	command := exec.Command(executable, managedCoreArgument)
	command.Dir = workspaces.directory
	command.Env, err = managedCoreEnvironment(os.Environ(), baseURL, workspaces.environmentValue, ownerToken)
	if err != nil {
		_ = childInput.Close()
		_ = parentInput.Close()
		return err
	}
	command.Stdin = childInput
	command.Stdout = os.Stdout
	coreErrors := &boundedTextBuffer{limit: coreErrorOutputLimit}
	command.Stderr = capturedCoreErrorWriter{capture: coreErrors, visible: os.Stderr}
	if err := command.Start(); err != nil {
		_ = childInput.Close()
		_ = parentInput.Close()
		return fmt.Errorf("start managed Veqri Core: %w", err)
	}
	_ = childInput.Close()
	done := make(chan error, 1)
	b.mu.Lock()
	b.core = command
	b.coreInput = parentInput
	b.coreDone = done
	b.ownerToken = ownerToken
	b.coreExited = false
	b.coreExitErr = nil
	b.mu.Unlock()
	go func() {
		waitErr := command.Wait()
		if details := coreErrors.String(); waitErr != nil && details != "" {
			waitErr = fmt.Errorf("%w: %s", waitErr, details)
		}
		runtimeContext, shouldQuit := b.recordManagedCoreExit(waitErr)
		done <- waitErr
		close(done)
		if shouldQuit && runtimeContext != nil {
			wailsruntime.Quit(runtimeContext)
		}
	}()
	return nil
}

func (b *Bridge) recordManagedCoreExit(err error) (context.Context, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.coreExited = true
	b.coreExitErr = err
	return b.ctx, b.ready && !b.shuttingDown
}

func (b *Bridge) markCoreReady() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.coreExited {
		if b.coreExitErr != nil {
			return fmt.Errorf("managed Veqri Core exited during startup: %w", b.coreExitErr)
		}
		return errors.New("managed Veqri Core exited during startup")
	}
	b.ready = true
	return nil
}

type managedWorkspaceConfig struct {
	directory        string
	environmentValue string
}

func managedWorkspace() (managedWorkspaceConfig, error) {
	if configured := strings.TrimSpace(os.Getenv("VEQRI_WORKSPACES")); configured != "" {
		var resolved []string
		for _, workspace := range strings.Split(configured, string(os.PathListSeparator)) {
			workspace = strings.TrimSpace(workspace)
			if workspace == "" {
				continue
			}
			absolute, err := filepath.Abs(workspace)
			if err != nil {
				return managedWorkspaceConfig{}, fmt.Errorf("resolve managed workspace: %w", err)
			}
			resolved = append(resolved, filepath.Clean(absolute))
		}
		if len(resolved) > 0 {
			return managedWorkspaceConfig{
				directory:        resolved[0],
				environmentValue: strings.Join(resolved, string(os.PathListSeparator)),
			}, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return managedWorkspaceConfig{}, fmt.Errorf("resolve home directory for managed workspace: %w", err)
	}
	workspace := filepath.Join(home, ".veqri", "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return managedWorkspaceConfig{}, fmt.Errorf("create managed workspace: %w", err)
	}
	if err := os.Chmod(workspace, 0o700); err != nil {
		return managedWorkspaceConfig{}, fmt.Errorf("secure managed workspace: %w", err)
	}
	return managedWorkspaceConfig{directory: workspace, environmentValue: workspace}, nil
}

func managedCoreEnvironment(current []string, baseURL, workspace, ownerToken string) ([]string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse managed Core URL: %w", err)
	}
	certificate := environmentValue(current, "VEQRI_TLS_CERT_FILE")
	key := environmentValue(current, "VEQRI_TLS_KEY_FILE")
	if parsed.Scheme == "https" && (certificate == "" || key == "") {
		return nil, errors.New("managed HTTPS Core requires VEQRI_TLS_CERT_FILE and VEQRI_TLS_KEY_FILE")
	}
	if parsed.Scheme == "http" && (certificate != "" || key != "") {
		return nil, errors.New("managed HTTP Core cannot use VEQRI_TLS_CERT_FILE or VEQRI_TLS_KEY_FILE")
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	address := net.JoinHostPort(strings.ToLower(parsed.Hostname()), port)
	pathNames := []string{
		"VEQRI_DATA_DIR",
		"VEQRI_DATABASE",
		"VEQRI_TLS_CERT_FILE",
		"VEQRI_TLS_KEY_FILE",
	}
	removedNames := append([]string{"VEQRI_ADDR", "VEQRI_WORKSPACES", managedcore.OwnerTokenEnvironment}, pathNames...)
	result := withoutEnvironment(current, removedNames...)
	result = append(result,
		"VEQRI_ADDR="+address,
		"VEQRI_WORKSPACES="+workspace,
		managedcore.OwnerTokenEnvironment+"="+ownerToken,
	)
	for _, name := range pathNames {
		value := environmentValue(current, name)
		if value == "" {
			continue
		}
		absolute, err := filepath.Abs(value)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", name, err)
		}
		result = append(result, name+"="+filepath.Clean(absolute))
	}
	return result, nil
}

func withoutEnvironment(values []string, names ...string) []string {
	blocked := make(map[string]struct{}, len(names))
	for _, name := range names {
		blocked[strings.ToUpper(name)] = struct{}{}
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		name, _, found := strings.Cut(value, "=")
		if !found {
			continue
		}
		if _, remove := blocked[strings.ToUpper(name)]; !remove {
			result = append(result, value)
		}
	}
	return result
}

func environmentValue(values []string, wanted string) string {
	for index := len(values) - 1; index >= 0; index-- {
		name, value, found := strings.Cut(values[index], "=")
		if found && strings.EqualFold(name, wanted) {
			return value
		}
	}
	return ""
}

func waitForVeqriCore(ctx context.Context, baseURL, ownerToken string, coreDone <-chan error) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastError error
	for {
		probeContext, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		lastError = probeVeqriCore(probeContext, baseURL, ownerToken)
		cancel()
		if lastError == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("Veqri Core did not become ready: %w", lastError)
		case err, ok := <-coreDone:
			if !ok || err == nil {
				return errors.New("managed Veqri Core stopped before becoming ready")
			}
			return fmt.Errorf("managed Veqri Core failed: %w", err)
		case <-ticker.C:
		}
	}
}

func probeVeqriCore(ctx context.Context, baseURL, ownerToken string) error {
	client := localCoreHTTPClient(500 * time.Millisecond)
	health, err := getCoreStatus(ctx, client, baseURL+"/healthz")
	if err != nil {
		return err
	}
	if health.Status != "ok" || health.ProtocolVersion != 1 {
		return errors.New("loopback endpoint is not a compatible Veqri Core")
	}
	expectedOwnerProof := managedcore.OwnerProof(ownerToken)
	if ownerToken != "" && subtle.ConstantTimeCompare([]byte(health.OwnerToken), []byte(expectedOwnerProof)) != 1 {
		return errors.New("loopback Veqri Core is not owned by this desktop process")
	}
	ready, err := getCoreStatus(ctx, client, baseURL+"/readyz")
	if err != nil {
		return err
	}
	if ready.Status != "ready" {
		return errors.New("Veqri Core is not ready")
	}
	return nil
}

func verifyVeqriCoreCredential(ctx context.Context, baseURL, token string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/protocol/negotiate", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Veqri-Client", "desktop")
	request.Header.Set("X-Veqri-Protocol-Version", "1")
	response, err := localCoreHTTPClient(2 * time.Second).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Core returned HTTP %d", response.StatusCode)
	}
	var negotiation struct {
		Selected struct {
			Major int `json:"major"`
		} `json:"selected"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&negotiation); err != nil {
		return fmt.Errorf("decode protocol negotiation: %w", err)
	}
	if negotiation.Selected.Major != 1 {
		return fmt.Errorf("Core selected protocol major %d, want 1", negotiation.Selected.Major)
	}
	return nil
}

func localCoreHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type coreStatus struct {
	Status          string `json:"status"`
	ProtocolVersion int    `json:"protocol_version"`
	OwnerToken      string `json:"-"`
}

func getCoreStatus(ctx context.Context, client *http.Client, endpoint string) (coreStatus, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return coreStatus{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return coreStatus{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return coreStatus{}, fmt.Errorf("%s returned HTTP %d", endpoint, response.StatusCode)
	}
	var status coreStatus
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&status); err != nil {
		return coreStatus{}, fmt.Errorf("decode %s: %w", endpoint, err)
	}
	status.OwnerToken = response.Header.Get(managedcore.OwnerTokenHeader)
	return status, nil
}

func validateCoreURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" {
		return "", errors.New("VEQRI_URL must be an absolute HTTP(S) loopback origin")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("VEQRI_URL must use HTTP or HTTPS")
	}
	if parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		parsed.RawFragment != "" || strings.Contains(value, "#") {
		return "", errors.New("VEQRI_URL must be an origin without credentials, path, query, or fragment")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return "", errors.New("desktop shell permits only a loopback Veqri Core URL")
	}
	if port := parsed.Port(); port != "" {
		parsedPort, portErr := strconv.Atoi(port)
		if portErr != nil || parsedPort < 1 || parsedPort > 65535 {
			return "", errors.New("VEQRI_URL port must be between 1 and 65535")
		}
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func environment(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
