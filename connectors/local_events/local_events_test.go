package local_events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/veqri/veqri/connectors/webhook"
)

func TestHTTPClientSignsLoopbackRequest(t *testing.T) {
	fixedNow := time.Unix(1_700_000_000, 0).UTC()
	secret := "local-signing-secret"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		verifier := webhook.Verifier{Secret: secret, Now: func() time.Time { return fixedNow }}
		if _, err := verifier.Verify(request, body); err != nil {
			t.Errorf("verify signature: %v", err)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer local-token" {
			t.Errorf("authorization = %q", got)
		}
		if got := request.Header.Get("X-Veqri-Protocol-Version"); got != "1" {
			t.Errorf("protocol version = %q", got)
		}
		if got := request.Header.Get("Idempotency-Key"); got != "event-1" {
			t.Errorf("idempotency header = %q", got)
		}
		var event Event
		if err := json.Unmarshal(body, &event); err != nil || event.Type != "build.completed" {
			t.Errorf("event = %#v, error = %v", event, err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"accepted":true,"event_id":"stored-1"}`)
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{
		Endpoint: server.URL, SigningSecret: secret, BearerToken: "local-token",
		Now: func() time.Time { return fixedNow }, Nonce: func() (string, error) { return "0123456789abcdef", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := client.Emit(context.Background(), Event{
		Type: "build.completed", Data: json.RawMessage(`{"ok":true}`), IdempotencyKey: "event-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Accepted || receipt.EventID != "stored-1" {
		t.Fatalf("receipt = %#v", receipt)
	}
}

func TestHTTPClientRejectsNonLocalEndpoint(t *testing.T) {
	if _, err := NewHTTPClient(HTTPConfig{Endpoint: "https://example.com/v1/events", SigningSecret: "secret"}); !errors.Is(err, ErrNonLocalEndpoint) {
		t.Fatalf("NewHTTPClient error = %v", err)
	}
}

func TestHTTPClientDoesNotFollowRedirectsWithCredentials(t *testing.T) {
	followed := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/other", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/other", func(writer http.ResponseWriter, _ *http.Request) {
		followed <- struct{}{}
		_, _ = io.WriteString(writer, `{"accepted":true}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := NewHTTPClient(HTTPConfig{Endpoint: server.URL + "/events", SigningSecret: "secret", BearerToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Emit(context.Background(), Event{Type: "test.event", Data: json.RawMessage(`{}`)}); err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("Emit error = %v", err)
	}
	select {
	case <-followed:
		t.Fatal("credentialed redirect was followed")
	default:
	}
}

func TestStdioDecoderStrictAndBounded(t *testing.T) {
	decoder := StdioDecoder{MaxEventBytes: 256}
	var events []Event
	err := decoder.Decode(strings.NewReader("\n"+`{"type":"ide.saved","data":{"path":"main.go"}}`+"\n"), func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "ide.saved" {
		t.Fatalf("events = %#v", events)
	}
	if err := decoder.Decode(strings.NewReader(`{"type":"x","data":{},"unknown":true}`+"\n"), func(Event) error { return nil }); err == nil {
		t.Fatal("unknown field was accepted")
	}
	limited := StdioDecoder{MaxEventBytes: 16}
	if err := limited.Decode(strings.NewReader(strings.Repeat("x", 128)+"\n"), func(Event) error { return nil }); !errors.Is(err, ErrStdioEventTooLarge) {
		t.Fatalf("oversized event error = %v", err)
	}
}

func TestLocalEventProcessHelper(t *testing.T) {
	if os.Getenv("VEQRI_PROCESS_HELPER") == "" {
		return
	}
	_, _ = fmt.Fprint(os.Stdout, "hello")
	_, _ = fmt.Fprint(os.Stderr, "problem")
	os.Exit(7)
}

func TestProcessAdapterReportsNonZeroCompletionAndBoundsOutput(t *testing.T) {
	adapter, err := NewProcessAdapter(ProcessConfig{
		Command: os.Args[0], Args: []string{"-test.run=TestLocalEventProcessHelper"},
		Environment: map[string]string{"VEQRI_PROCESS_HELPER": "1"},
		EventType:   "test.completed", MaxOutputBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := adapter.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var completion ProcessCompletion
	if err := json.Unmarshal(event.Data, &completion); err != nil {
		t.Fatal(err)
	}
	if completion.ExitCode != 7 || completion.Stdout != "hell" || completion.Stderr != "prob" || !completion.Truncated {
		t.Fatalf("completion = %#v", completion)
	}
	if completion.CancellationScope == "" || event.IdempotencyKey == "" {
		t.Fatalf("completion scope/idempotency = %#v / %q", completion, event.IdempotencyKey)
	}
	if _, err := NewProcessAdapter(ProcessConfig{Command: "sh"}); !errors.Is(err, ErrProcessShellUnsupported) {
		t.Fatalf("shell error = %v", err)
	}
}

func TestFilesystemPollerEmitsModification(t *testing.T) {
	root := t.TempDir()
	path := root + string(os.PathSeparator) + "watched.txt"
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	poller, err := NewFilesystemPoller(FilesystemConfig{Root: root, Paths: []string{"watched.txt"}, Interval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 4)
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx, func(event Event) error { events <- event; return nil }) }()
	time.Sleep(30 * time.Millisecond)
	if err := os.WriteFile(path, []byte("one-two"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		var change FilesystemChange
		if err := json.Unmarshal(event.Data, &change); err != nil {
			t.Fatal(err)
		}
		if change.Path != "watched.txt" || change.Change != FileModified {
			t.Fatalf("change = %#v", change)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("filesystem modification event was not emitted")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("poller error = %v", err)
	}
}

func TestFilesystemPollerRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := outside + string(os.PathSeparator) + "private.txt"
	if err := os.WriteFile(outsideFile, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, root+string(os.PathSeparator)+"escape"); err != nil {
		t.Fatal(err)
	}
	poller, err := NewFilesystemPoller(FilesystemConfig{Root: root, Paths: []string{"escape"}, Interval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := poller.Run(ctx, func(Event) error { return nil }); err == nil || !bytes.Contains([]byte(err.Error()), []byte("outside")) {
		t.Fatalf("poller error = %v", err)
	}
}
