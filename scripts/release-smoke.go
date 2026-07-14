package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/veqri/veqri/internal/buildinfo"
	"github.com/veqri/veqri/internal/managedcore"
)

const smokeToken = "veqri-release-smoke-token-0123456789abcdef"

type smokeConfig struct {
	corePath string
	coreArg  string
	cliPath  string
	timeout  time.Duration
}

func main() {
	config := smokeConfig{}
	flag.StringVar(&config.corePath, "core", defaultBinaryPath("veqri-core", runtime.GOOS), "path to the built Veqri Core executable")
	flag.StringVar(&config.coreArg, "core-arg", "", "optional argument used to start Core from a multicall desktop executable")
	flag.StringVar(&config.cliPath, "cli", defaultBinaryPath("veqri", runtime.GOOS), "path to the built Veqri CLI executable")
	flag.DurationVar(&config.timeout, "timeout", 30*time.Second, "maximum time for the release smoke test")
	flag.Parse()

	if err := runSmoke(config); err != nil {
		fmt.Fprintln(os.Stderr, "release smoke:", err)
		os.Exit(1)
	}
	fmt.Printf("release smoke passed on %s/%s\n", runtime.GOOS, runtime.GOARCH)
}

func runSmoke(config smokeConfig) error {
	if config.timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	paths := []struct {
		label string
		value *string
	}{
		{label: "Core", value: &config.corePath},
		{label: "CLI", value: &config.cliPath},
	}
	for _, path := range paths {
		absolute, err := filepath.Abs(*path.value)
		if err != nil {
			return fmt.Errorf("resolve %s executable %q: %w", path.label, *path.value, err)
		}
		*path.value = absolute
		info, err := os.Stat(absolute)
		if err != nil {
			return fmt.Errorf("locate %s executable %q: %w", path.label, absolute, err)
		}
		if info.IsDir() {
			return fmt.Errorf("%s executable %q is a directory", path.label, absolute)
		}
	}

	root, err := os.MkdirTemp("", "veqri-release-smoke-")
	if err != nil {
		return fmt.Errorf("create smoke directory: %w", err)
	}
	defer os.RemoveAll(root)
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return fmt.Errorf("create smoke workspace: %w", err)
	}

	address, err := reserveLoopbackAddress()
	if err != nil {
		return err
	}
	baseURL := "http://" + address
	environmentOverrides := map[string]string{
		"VEQRI_ADDR":              address,
		"VEQRI_AUTH_TOKEN":        smokeToken,
		"VEQRI_DATABASE":          filepath.Join(root, "veqri.db"),
		"VEQRI_DATA_DIR":          root,
		"VEQRI_KEYCHAIN_DISABLED": "true",
		"VEQRI_RETENTION_DAYS":    "0",
		"VEQRI_URL":               baseURL,
		"VEQRI_WORKSPACES":        workspace,
	}
	if config.coreArg != "" {
		environmentOverrides[managedcore.OwnerTokenEnvironment] = "release-smoke-managed-owner-token-0123456789abcdef"
	}
	environment := mergeEnvironment(os.Environ(), environmentOverrides)

	var coreArguments []string
	if config.coreArg != "" {
		coreArguments = append(coreArguments, config.coreArg)
	}
	coreCommand := exec.Command(config.corePath, coreArguments...)
	coreCommand.Env = environment
	coreCommand.Dir = workspace
	var coreOutput bytes.Buffer
	var coreErrors bytes.Buffer
	coreCommand.Stdout = &coreOutput
	coreCommand.Stderr = &coreErrors
	coreInput, err := coreCommand.StdinPipe()
	if err != nil {
		return fmt.Errorf("create Core lifecycle pipe: %w", err)
	}
	if err := coreCommand.Start(); err != nil {
		_ = coreInput.Close()
		return fmt.Errorf("start Core: %w", err)
	}
	coreDone := make(chan error, 1)
	go func() { coreDone <- coreCommand.Wait() }()
	defer stopCore(coreCommand, coreDone, coreInput, config.coreArg != "")

	ctx, cancel := context.WithTimeout(context.Background(), config.timeout)
	defer cancel()
	httpClient := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if err := waitUntilReady(ctx, httpClient, baseURL, coreDone, &coreErrors); err != nil {
		return err
	}
	versionPayload, err := runCLIJSON(ctx, config.cliPath, environment, "version", "--json")
	if err != nil {
		return err
	}
	cliBuildInfo, err := buildInfoFromPayload("CLI version", versionPayload)
	if err != nil {
		return err
	}
	healthPayload, err := runCLIJSON(ctx, config.cliPath, environment, "status")
	if err != nil {
		return err
	}
	if err := requireJSONField("CLI status", healthPayload, "status", "ok"); err != nil {
		return err
	}
	healthBuildInfo, err := buildInfoFromPayload("Core health", healthPayload)
	if err != nil {
		return err
	}
	if err := requireMatchingBuildInfo(cliBuildInfo, healthBuildInfo, "CLI", "Core health"); err != nil {
		return err
	}
	if err := runCLI(ctx, config.cliPath, environment, "diagnostics", "database_ok", true); err != nil {
		return err
	}

	snapshot, err := authenticatedJSON(ctx, httpClient, http.MethodGet, baseURL+"/api/v1/desktop/snapshot", nil)
	if err != nil {
		return fmt.Errorf("load desktop snapshot: %w", err)
	}
	if protocol, ok := snapshot["protocol_version"].(float64); !ok || protocol != 1 {
		return fmt.Errorf("desktop snapshot protocol_version = %v, want 1", snapshot["protocol_version"])
	}
	core, ok := snapshot["core"].(map[string]any)
	if !ok {
		return errors.New("desktop snapshot is missing core build metadata")
	}
	snapshotBuildInfo, err := buildInfoFromPayload("desktop snapshot Core", core)
	if err != nil {
		return err
	}
	if err := requireMatchingBuildInfo(cliBuildInfo, snapshotBuildInfo, "CLI", "desktop snapshot Core"); err != nil {
		return err
	}

	action := map[string]any{
		"request_id": "release-smoke-settings",
		"action": map[string]any{
			"type":  "settings.update",
			"patch": map[string]any{"theme": "system"},
		},
	}
	response, err := authenticatedJSON(ctx, httpClient, http.MethodPost, baseURL+"/api/v1/desktop/actions", action)
	if err != nil {
		return fmt.Errorf("perform desktop action: %w", err)
	}
	if accepted, ok := response["accepted"].(bool); !ok || !accepted {
		return fmt.Errorf("desktop action accepted = %v, want true", response["accepted"])
	}

	after, err := authenticatedJSON(ctx, httpClient, http.MethodGet, baseURL+"/api/v1/desktop/snapshot", nil)
	if err != nil {
		return fmt.Errorf("reload desktop snapshot: %w", err)
	}
	settings, ok := after["settings"].(map[string]any)
	if !ok || settings["theme"] != "system" {
		return fmt.Errorf("desktop theme = %v, want system", settings["theme"])
	}
	return nil
}

func defaultBinaryPath(name, goos string) string {
	if goos == "windows" {
		name += ".exe"
	}
	return filepath.Join("build", "bin", name)
}

func reserveLoopbackAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("reserve loopback address: %w", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", fmt.Errorf("release loopback address: %w", err)
	}
	return address, nil
}

func mergeEnvironment(base []string, overrides map[string]string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	canonical := make(map[string]string, len(base)+len(overrides))
	for _, entry := range base {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" {
			continue
		}
		key := environmentKey(name)
		canonical[key] = name
		values[key] = value
	}
	for name, value := range overrides {
		key := environmentKey(name)
		canonical[key] = name
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, canonical[key]+"="+values[key])
	}
	return result
}

func environmentKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

func waitUntilReady(ctx context.Context, client *http.Client, baseURL string, coreDone <-chan error, coreErrors *bytes.Buffer) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/readyz", nil)
		if err != nil {
			return err
		}
		response, requestErr := client.Do(request)
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case err := <-coreDone:
			return fmt.Errorf("Core exited before readiness: %v: %s", err, boundedText(coreErrors.String(), 2<<10))
		case <-ctx.Done():
			return fmt.Errorf("wait for Core readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func runCLI(ctx context.Context, cliPath string, environment []string, command, requiredField string, requiredValue any) error {
	payload, err := runCLIJSON(ctx, cliPath, environment, command)
	if err != nil {
		return err
	}
	return requireJSONField("CLI "+command, payload, requiredField, requiredValue)
}

func runCLIJSON(ctx context.Context, cliPath string, environment []string, arguments ...string) (map[string]any, error) {
	cliContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cliCommand := exec.CommandContext(cliContext, cliPath, arguments...)
	cliCommand.Env = environment
	output, err := cliCommand.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("run CLI %s: %w: %s", strings.Join(arguments, " "), err, boundedText(string(output), 2<<10))
	}
	var payload map[string]any
	if err := json.Unmarshal(output, &payload); err != nil {
		return nil, fmt.Errorf("decode CLI %s output: %w", strings.Join(arguments, " "), err)
	}
	return payload, nil
}

func requireJSONField(label string, payload map[string]any, requiredField string, requiredValue any) error {
	value, exists := payload[requiredField]
	if !exists {
		return fmt.Errorf("%s output is missing %q", label, requiredField)
	}
	if requiredValue != nil && value != requiredValue {
		return fmt.Errorf("%s %s = %v, want %v", label, requiredField, value, requiredValue)
	}
	return nil
}

func buildInfoFromPayload(label string, payload map[string]any) (buildinfo.Info, error) {
	field := func(name string) (string, error) {
		value, ok := payload[name].(string)
		if !ok || value == "" {
			return "", fmt.Errorf("%s is missing string %q", label, name)
		}
		return value, nil
	}
	version, err := field("version")
	if err != nil {
		return buildinfo.Info{}, err
	}
	commit, err := field("commit")
	if err != nil {
		return buildinfo.Info{}, err
	}
	buildTime, err := field("build_time")
	if err != nil {
		return buildinfo.Info{}, err
	}
	raw := buildinfo.Info{Version: version, Commit: commit, BuildTime: buildTime}
	info, err := buildinfo.Parse(raw.Version, raw.Commit, raw.BuildTime)
	if err != nil {
		return buildinfo.Info{}, fmt.Errorf("%s contains invalid build metadata: %w", label, err)
	}
	if info != raw {
		return buildinfo.Info{}, fmt.Errorf("%s build metadata is not in canonical form", label)
	}
	return raw, nil
}

func requireMatchingBuildInfo(expected, actual buildinfo.Info, expectedLabel, actualLabel string) error {
	if actual != expected {
		return fmt.Errorf("%s build metadata %+v does not match %s %+v", actualLabel, actual, expectedLabel, expected)
	}
	return nil
}

func authenticatedJSON(ctx context.Context, client *http.Client, method, endpoint string, body any) (map[string]any, error) {
	var requestBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		requestBody = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, requestBody)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+smokeToken)
	request.Header.Set("X-Veqri-Client", "desktop")
	request.Header.Set("X-Veqri-Protocol-Version", "1")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s: %s", response.Status, boundedText(string(raw), 2<<10))
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func stopCore(command *exec.Cmd, done <-chan error, input io.Closer, waitForInput bool) {
	if command.Process == nil || (command.ProcessState != nil && command.ProcessState.Exited()) {
		_ = input.Close()
		return
	}
	_ = input.Close()
	if waitForInput {
		select {
		case <-done:
			return
		case <-time.After(5 * time.Second):
		}
	}
	if runtime.GOOS == "windows" {
		_ = command.Process.Kill()
	} else if err := command.Process.Signal(os.Interrupt); err != nil {
		_ = command.Process.Kill()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}
}

func boundedText(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if len(value) <= maximum {
		return value
	}
	return value[:maximum] + "…"
}
