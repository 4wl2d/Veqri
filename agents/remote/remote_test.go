package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	coreagents "github.com/veqri/veqri/core/agents"
	"github.com/veqri/veqri/core/tasks"
)

func TestRunnerStreamsProgressAndResult(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("authorization = %q", got)
		}
		if got := request.Header.Get("X-Veqri-Agent-Protocol"); got != "1" {
			t.Errorf("protocol header = %q", got)
		}
		var body Request
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body.Version != 1 || body.Task.ID != "task-1" {
			t.Errorf("request = %#v", body)
		}
		writer.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = fmt.Fprintln(writer, `{"type":"progress","progress":{"percent":40,"message":"working"}}`)
		_, _ = fmt.Fprintln(writer, `{"type":"result","result":{"structured":{"ok":true},"written_summary":"done","spoken_summary":"done","artifacts":null,"partial":false}}`)
	}))
	defer server.Close()

	runner, err := New(Config{
		Endpoint: server.URL, StaticBearerToken: "test-token", AllowInsecureLoopback: true,
		Definition: coreagents.Definition{ID: "remote-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var progress []coreagents.Progress
	result, err := runner.Run(context.Background(), tasks.Task{ID: "task-1"}, func(value coreagents.Progress) {
		progress = append(progress, value)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 1 || progress[0].Percent != 40 {
		t.Fatalf("progress = %#v", progress)
	}
	if result.WrittenSummary != "done" || string(result.Structured) != `{"ok":true}` {
		t.Fatalf("result = %#v", result)
	}
	definition := runner.Definition()
	if definition.ExecutionMode != coreagents.ModeHTTP || !definition.SupportsStreaming || !definition.SupportsCancellation {
		t.Fatalf("definition = %#v", definition)
	}
}

func TestRunnerCancelsRequestAndCallsCancelEndpoint(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	cancelSeen := make(chan CancelRequest, 1)
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/run", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/x-ndjson")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		once.Do(func() { close(started) })
		<-request.Context().Done()
	})
	mux.HandleFunc("/cancel", func(writer http.ResponseWriter, request *http.Request) {
		var body CancelRequest
		_ = json.NewDecoder(request.Body).Decode(&body)
		cancelSeen <- body
		writer.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	runner, err := New(Config{
		Endpoint: server.URL + "/run", CancelURL: server.URL + "/cancel",
		StaticBearerToken: "test-token", AllowInsecureLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, runErr := runner.Run(ctx, tasks.Task{ID: "cancel-me"}, nil)
		done <- runErr
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("run request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	select {
	case body := <-cancelSeen:
		if body.TaskID != "cancel-me" {
			t.Fatalf("cancel body = %#v", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cooperative cancel request was not sent")
	}
}

func TestRunnerEnforcesResponseLimit(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"written_summary":"` + strings.Repeat("x", 512) + `"}`))
	}))
	defer server.Close()
	runner, err := New(Config{
		Endpoint: server.URL, StaticBearerToken: "token", AllowInsecureLoopback: true,
		MaxResponseBytes: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), tasks.Task{ID: "large"}, nil)
	if !errors.Is(err, ErrResponseTooLarge) || !errors.Is(err, ErrProtocol) {
		t.Fatalf("Run error = %v", err)
	}
}

func TestRunnerDoesNotExposeRemoteErrorBody(t *testing.T) {
	t.Parallel()
	const secret = "remote-error-secret-must-not-persist"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(strings.Repeat(secret, 1000)))
	}))
	defer server.Close()
	runner, err := New(Config{
		Endpoint: server.URL, StaticBearerToken: "token", AllowInsecureLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background(), tasks.Task{ID: "remote-error"}, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") || strings.Contains(err.Error(), secret) {
		t.Fatalf("Run error exposed remote body: %v", err)
	}
}

func TestNewRejectsUnauthenticatedOrInsecureRemoteEndpoint(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{Endpoint: "https://agent.example/run"}); !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("missing authentication error = %v", err)
	}
	if _, err := New(Config{Endpoint: "http://agent.example/run", StaticBearerToken: "token", AllowInsecureLoopback: true}); !errors.Is(err, ErrInsecureEndpoint) {
		t.Fatalf("insecure endpoint error = %v", err)
	}
}

func TestRunnerDoesNotFollowRedirectsWithCredentials(t *testing.T) {
	t.Parallel()
	followed := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/run", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/other", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/other", func(writer http.ResponseWriter, _ *http.Request) {
		followed <- struct{}{}
		_, _ = writer.Write([]byte(`{"written_summary":"unsafe"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	runner, err := New(Config{Endpoint: server.URL + "/run", StaticBearerToken: "token", AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), tasks.Task{ID: "redirect"}, nil); err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("Run error = %v", err)
	}
	select {
	case <-followed:
		t.Fatal("credentialed redirect was followed")
	default:
	}
}
