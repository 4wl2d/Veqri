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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/veqri/veqri/internal/auth"
)

const protocolVersion = "1"

type client struct {
	baseURL string
	token   string
	http    *http.Client
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "veqri:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		printUsage()
		return errors.New("a command is required")
	}
	if arguments[0] == "help" || arguments[0] == "-h" || arguments[0] == "--help" {
		printUsage()
		return nil
	}
	cli, err := newClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch arguments[0] {
	case "status":
		return cli.printRequest(ctx, http.MethodGet, "/healthz", nil, false)
	case "diagnostics":
		return cli.printRequest(ctx, http.MethodGet, "/v1/diagnostics", nil, true)
	case "ask":
		return cli.ask(ctx, arguments[1:])
	case "emit":
		return cli.emit(ctx, arguments[1:])
	case "task":
		return cli.task(ctx, arguments[1:])
	case "approve", "deny":
		if len(arguments) != 2 {
			return fmt.Errorf("usage: veqri %s APPROVAL_ID", arguments[0])
		}
		return cli.printRequest(ctx, http.MethodPost, "/v1/approvals/"+arguments[1]+"/"+arguments[0], map[string]any{}, true)
	case "pair":
		return cli.printRequest(ctx, http.MethodPost, "/v1/pairings", map[string]any{}, true)
	case "call":
		if len(arguments) != 2 {
			return errors.New("usage: veqri call DEVICE_NAME_OR_ID")
		}
		return cli.call(ctx, arguments[1])
	case "shell":
		return cli.shell(ctx, arguments[1:])
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

type emitOptions struct {
	eventType  string
	dataPath   string
	createTask bool
}

func parseEmitArguments(arguments []string) (emitOptions, error) {
	flags := flag.NewFlagSet("emit", flag.ContinueOnError)
	dataPath := flags.String("data", "", "JSON data file")
	createTask := flags.Bool("task", false, "create a task from data.text or data.goal")

	var eventType string
	flagArguments := arguments
	// The documented form is `emit EVENT_TYPE --data ... --task`. Go's flag
	// package stops parsing at the first positional argument, so parse that
	// leading event type separately while retaining the legacy flags-first form.
	if len(arguments) > 0 && !strings.HasPrefix(arguments[0], "-") {
		eventType = arguments[0]
		flagArguments = arguments[1:]
	}
	if err := flags.Parse(flagArguments); err != nil {
		return emitOptions{}, err
	}
	if eventType == "" {
		if flags.NArg() != 1 {
			return emitOptions{}, errors.New("usage: veqri emit EVENT_TYPE [--data file.json] [--task]")
		}
		eventType = flags.Arg(0)
	} else if flags.NArg() != 0 {
		return emitOptions{}, errors.New("usage: veqri emit EVENT_TYPE [--data file.json] [--task]")
	}
	if strings.TrimSpace(eventType) == "" {
		return emitOptions{}, errors.New("event type cannot be empty")
	}
	return emitOptions{eventType: eventType, dataPath: *dataPath, createTask: *createTask}, nil
}

func newClient() (*client, error) {
	baseURL, err := validateBaseURL(env("VEQRI_URL", "http://127.0.0.1:7342"))
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dataDir := env("VEQRI_DATA_DIR", filepath.Join(home, ".veqri"))
	token, _, _ := auth.ReadAdminToken(dataDir)
	httpClient := &http.Client{Timeout: 30 * time.Second}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client{baseURL: baseURL, token: token, http: httpClient}, nil
}

func validateBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" {
		return "", errors.New("VEQRI_URL must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("VEQRI_URL cannot contain credentials, a path, query, or fragment")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("VEQRI_URL must use HTTP or HTTPS")
	}
	if parsed.Scheme == "http" {
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("VEQRI_URL requires HTTPS outside loopback")
		}
	}
	parsed.Path = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func (c *client) ask(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("ask", flag.ContinueOnError)
	conversation := flags.String("conversation", "cli:default", "stable conversation key")
	agentsText := flags.String("agents", "", "comma-separated agent IDs")
	wait := flags.Bool("wait", false, "wait for terminal task state")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if text == "" {
		return errors.New("usage: veqri ask [--wait] [--agents id,id] \"request\"")
	}
	var agentIDs []string
	for _, value := range strings.Split(*agentsText, ",") {
		if value = strings.TrimSpace(value); value != "" {
			agentIDs = append(agentIDs, value)
		}
	}
	var response struct {
		Task struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"task"`
	}
	if err := c.request(ctx, http.MethodPost, "/v1/ask", map[string]any{
		"text": text, "conversation_key": *conversation,
		"idempotency_key": fmt.Sprintf("cli:%d", time.Now().UTC().UnixNano()), "agent_ids": agentIDs,
	}, true, &response); err != nil {
		return err
	}
	if !*wait {
		return printJSON(response)
	}
	return c.waitTask(ctx, response.Task.ID)
}

func (c *client) emit(ctx context.Context, arguments []string) error {
	options, err := parseEmitArguments(arguments)
	if err != nil {
		return err
	}
	data := json.RawMessage(`{}`)
	if options.dataPath != "" {
		raw, err := os.ReadFile(options.dataPath)
		if err != nil {
			return err
		}
		if !json.Valid(raw) {
			return errors.New("event data file is not valid JSON")
		}
		data = raw
	}
	return c.printRequest(ctx, http.MethodPost, "/v1/events", map[string]any{
		"type": options.eventType, "data": data, "create_task": options.createTask,
		"idempotency_key": fmt.Sprintf("cli-event:%d", time.Now().UTC().UnixNano()),
	}, true)
}

func (c *client) task(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: veqri task list|show|cancel TASK_ID")
	}
	switch arguments[0] {
	case "list":
		return c.printRequest(ctx, http.MethodGet, "/v1/tasks", nil, true)
	case "show":
		if len(arguments) != 2 {
			return errors.New("usage: veqri task show TASK_ID")
		}
		return c.printRequest(ctx, http.MethodGet, "/v1/tasks/"+arguments[1], nil, true)
	case "cancel":
		if len(arguments) != 2 {
			return errors.New("usage: veqri task cancel TASK_ID")
		}
		return c.printRequest(ctx, http.MethodPost, "/v1/tasks/"+arguments[1]+"/cancel", map[string]any{}, true)
	default:
		return fmt.Errorf("unknown task command %q", arguments[0])
	}
}

func (c *client) call(ctx context.Context, nameOrID string) error {
	var devicesResponse struct {
		Devices []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"devices"`
	}
	if err := c.request(ctx, http.MethodGet, "/v1/devices", nil, true, &devicesResponse); err != nil {
		return err
	}
	deviceID := ""
	for _, device := range devicesResponse.Devices {
		if device.ID == nameOrID || strings.EqualFold(device.Name, nameOrID) {
			deviceID = device.ID
			break
		}
	}
	if deviceID == "" {
		return errors.New("paired device was not found")
	}
	return c.printRequest(ctx, http.MethodPost, "/v1/voice/calls", map[string]any{"device_id": deviceID}, true)
}

func (c *client) shell(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("shell", flag.ContinueOnError)
	workingDirectory := flags.String("cwd", "", "working directory")
	dryRun := flags.Bool("dry-run", false, "show validated execution without running")
	wait := flags.Bool("wait", false, "wait for completion (only if no approval is required)")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() < 1 {
		return errors.New("usage: veqri shell [--cwd PATH] [--dry-run] [--wait] COMMAND [ARGS...]")
	}
	var response struct {
		Task struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"task"`
		Approval any `json:"approval"`
	}
	if err := c.request(ctx, http.MethodPost, "/v1/tools/shell", map[string]any{
		"input": map[string]any{"command": flags.Arg(0), "args": flags.Args()[1:],
			"working_directory": *workingDirectory, "dry_run": *dryRun},
		"idempotency_key": fmt.Sprintf("cli-shell:%d", time.Now().UTC().UnixNano()),
	}, true, &response); err != nil {
		return err
	}
	if *wait && response.Task.Status != "WAITING_FOR_APPROVAL" {
		return c.waitTask(ctx, response.Task.ID)
	}
	return printJSON(response)
}

func (c *client) waitTask(ctx context.Context, id string) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(10 * time.Minute)
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return errors.New("timed out waiting for task")
		case <-ticker.C:
			var response struct {
				Task map[string]any `json:"task"`
			}
			if err := c.request(ctx, http.MethodGet, "/v1/tasks/"+id, nil, true, &response); err != nil {
				return err
			}
			status, _ := response.Task["status"].(string)
			if terminalStatus(status) {
				return printJSON(response)
			}
		}
	}
}

func (c *client) printRequest(ctx context.Context, method, path string, body any, authenticated bool) error {
	var result any
	if err := c.request(ctx, method, path, body, authenticated, &result); err != nil {
		return err
	}
	return printJSON(result)
}

func (c *client) request(ctx context.Context, method, path string, body any, authenticated bool, target any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Veqri-Protocol-Version", protocolVersion)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authenticated {
		if c.token == "" {
			return errors.New("no admin token found; set VEQRI_AUTH_TOKEN or start the core once")
		}
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("core returned %s: %s", response.Status, strings.TrimSpace(string(raw)))
	}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode core response: %w", err)
	}
	return nil
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func terminalStatus(status string) bool {
	switch status {
	case "COMPLETED", "PARTIALLY_COMPLETED", "FAILED", "CANCELLED", "TIMED_OUT":
		return true
	default:
		return false
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

const usageText = `Veqri CLI

Usage:
  veqri status
  veqri ask [--wait] [--agents id,id] "request"
  veqri emit EVENT_TYPE --data file.json [--task]
  veqri task list
  veqri task show TASK_ID
  veqri task cancel TASK_ID
  veqri approve APPROVAL_ID
  veqri deny APPROVAL_ID
  veqri pair
  veqri call DEVICE_NAME_OR_ID
  veqri shell [--cwd PATH] [--dry-run] [--wait] COMMAND [ARGS...]
  veqri diagnostics`

func printUsage() {
	fmt.Fprintln(os.Stderr, usageText)
}
