package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunHelpAliasesSucceedWithoutClientConfiguration(t *testing.T) {
	t.Setenv("VEQRI_URL", "://invalid")
	for _, argument := range []string{"help", "-h", "--help"} {
		t.Run(argument, func(t *testing.T) {
			if err := run([]string{argument}); err != nil {
				t.Fatalf("run(%q): %v", argument, err)
			}
		})
	}
}

func TestParseEmitArgumentsSupportsDocumentedAndLegacyOrdering(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
	}{
		{name: "documented event first", arguments: []string{"build.completed", "--data", "event.json", "--task"}},
		{name: "legacy flags first", arguments: []string{"--data", "event.json", "--task", "build.completed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options, err := parseEmitArguments(tt.arguments)
			if err != nil {
				t.Fatalf("parseEmitArguments(): %v", err)
			}
			if options.eventType != "build.completed" || options.dataPath != "event.json" || !options.createTask {
				t.Fatalf("options = %+v", options)
			}
		})
	}
}

func TestParseEmitArgumentsRejectsMissingOrExtraEventTypes(t *testing.T) {
	for _, arguments := range [][]string{
		{},
		{"--task"},
		{"build.completed", "extra"},
	} {
		if _, err := parseEmitArguments(arguments); err == nil {
			t.Errorf("parseEmitArguments(%q) unexpectedly succeeded", arguments)
		}
	}
}

func TestEmitDocumentedOrderingSendsEventData(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(dataPath, []byte(`{"goal":"review"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/events" || request.Method != http.MethodPost {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		var body struct {
			Type       string         `json:"type"`
			Data       map[string]any `json:"data"`
			CreateTask bool           `json:"create_task"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.Type != "build.completed" || body.Data["goal"] != "review" || !body.CreateTask {
			t.Errorf("event body = %+v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"accepted":true}`))
	}))
	defer server.Close()

	cli := &client{baseURL: server.URL, token: "test-token", http: server.Client()}
	if err := cli.emit(context.Background(), []string{"build.completed", "--data", dataPath, "--task"}); err != nil {
		t.Fatalf("emit(): %v", err)
	}
}

func TestShellUsageDocumentsWaitFlag(t *testing.T) {
	const shellSyntax = "veqri shell [--cwd PATH] [--dry-run] [--wait] COMMAND [ARGS...]"
	if !strings.Contains(usageText, shellSyntax) {
		t.Fatalf("global usage does not contain %q", shellSyntax)
	}
	err := (&client{}).shell(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), shellSyntax) {
		t.Fatalf("empty shell arguments error = %v", err)
	}
}

func TestValidateBaseURLProtectsAdminCredential(t *testing.T) {
	allowed := map[string]string{
		"http://127.0.0.1:7342/": "http://127.0.0.1:7342",
		"http://[::1]:7342":      "http://[::1]:7342",
		"https://core.example":   "https://core.example",
	}
	for input, expected := range allowed {
		actual, err := validateBaseURL(input)
		if err != nil || actual != expected {
			t.Errorf("validateBaseURL(%q) = %q, %v; want %q", input, actual, err, expected)
		}
	}
	for _, input := range []string{
		"http://192.168.1.20:7342", "http://core.example", "https://token@core.example",
		"https://core.example/path", "ftp://core.example", "https://core.example?q=1",
	} {
		if _, err := validateBaseURL(input); err == nil {
			t.Errorf("validateBaseURL(%q) unexpectedly succeeded", input)
		}
	}
}
