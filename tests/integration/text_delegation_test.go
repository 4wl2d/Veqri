package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	coreagents "github.com/veqri/veqri/core/agents"
	"github.com/veqri/veqri/core/conversation"
	"github.com/veqri/veqri/core/tasks"
	"github.com/veqri/veqri/tests/integration/testfixture"
)

func TestAuthenticatedTextDelegatesInParallelAndSynthesizesPersistedTurn(t *testing.T) {
	gate := newParallelGate(2)
	fixture := testfixture.New(t, testfixture.Options{
		WorkerCount: 4,
		Runners: []coreagents.Runner{
			&parallelRunner{id: "builtin.coding", displayName: "Coding fixture", gate: gate},
			&parallelRunner{id: "builtin.research", displayName: "Research fixture", gate: gate},
		},
	})

	streamContext, cancelStream := context.WithTimeout(context.Background(), testfixture.DefaultTimeout)
	defer cancelStream()
	connection, response, err := websocket.Dial(streamContext, fixture.WebSocketURL("/v1/stream"), &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + fixture.AdminToken}},
		Subprotocols: []string{"veqri.v1"},
	})
	if err != nil {
		if response != nil {
			t.Fatalf("dial authenticated event stream: %v (HTTP %s)", err, response.Status)
		}
		t.Fatalf("dial authenticated event stream: %v", err)
	}
	defer connection.CloseNow()
	readStreamEvent(t, streamContext, connection) // stream.connected

	eventsChannel := make(chan wireEvent, 64)
	readErrors := make(chan error, 1)
	go func() {
		for {
			_, raw, readErr := connection.Read(streamContext)
			if readErr != nil {
				readErrors <- readErr
				return
			}
			var event wireEvent
			if unmarshalErr := json.Unmarshal(raw, &event); unmarshalErr != nil {
				readErrors <- unmarshalErr
				return
			}
			eventsChannel <- event
		}
	}()

	request := fixture.JSON(t, http.MethodPost, "/v1/ask", fixture.AdminToken, map[string]any{
		"text":             "Inspect the repository and cross-check the implementation evidence",
		"conversation_key": "integration:parallel-text",
		"idempotency_key":  "parallel-text-001",
		"agent_ids":        []string{"builtin.coding", "builtin.research"},
	}, nil)
	testfixture.RequireStatus(t, request, http.StatusAccepted)
	created := testfixture.Decode[struct {
		Task      tasks.Task `json:"task"`
		Duplicate bool       `json:"duplicate"`
	}](t, request)
	if created.Duplicate {
		t.Fatal("first authenticated request was unexpectedly reported as duplicate")
	}
	if created.Task.AssignedAgentID != "builtin.synthesizer" || created.Task.TaskType != "synthesis" {
		t.Fatalf("root was not routed through the synthesizer: %+v", created.Task)
	}

	progressAgents := make(map[string]bool)
	assistantFinal := false
	rootCompleted := false
	var observed []string
	for !rootCompleted || !assistantFinal || len(progressAgents) < 2 {
		select {
		case event := <-eventsChannel:
			observed = append(observed, event.Type+":"+event.TaskID)
			switch event.Type {
			case "task.progress":
				var progressed tasks.Task
				if err := json.Unmarshal(event.Payload, &progressed); err != nil {
					t.Fatalf("decode task.progress payload: %v", err)
				}
				if progressed.AssignedAgentID == "builtin.coding" || progressed.AssignedAgentID == "builtin.research" {
					progressAgents[progressed.AssignedAgentID] = true
				}
			case "conversation.turn.final":
				var turn conversation.Turn
				if err := json.Unmarshal(event.Payload, &turn); err != nil {
					t.Fatalf("decode final turn payload: %v", err)
				}
				assistantFinal = assistantFinal || (event.TaskID == created.Task.ID && turn.Role == conversation.RoleAssistant && turn.ConversationID == created.Task.ConversationID)
			case "task.completed":
				rootCompleted = rootCompleted || event.TaskID == created.Task.ID
			}
		case readErr := <-readErrors:
			t.Fatalf("read event stream before completion: %v", readErr)
		case <-streamContext.Done():
			root, _ := fixture.Store.GetTask(context.Background(), created.Task.ID)
			t.Fatalf("event stream deadline before progress/final events: progress=%v assistant_final=%v root_completed=%v root=%+v observed=%v",
				progressAgents, assistantFinal, rootCompleted, root, observed)
		}
	}

	completed := fixture.WaitTask(t, created.Task.ID, tasks.StatusCompleted)
	if completed.Progress != 100 || completed.UserFacingSummary == "" || len(completed.Result) == 0 {
		t.Fatalf("synthesized root was not durably completed: %+v", completed)
	}
	var synthesis struct {
		Agreements     []string `json:"agreements"`
		FailedSubtasks []string `json:"failed_subtasks"`
		Written        string   `json:"written"`
	}
	if err := json.Unmarshal(completed.Result, &synthesis); err != nil {
		t.Fatalf("decode synthesized result: %v", err)
	}
	if len(synthesis.Agreements) != 2 || len(synthesis.FailedSubtasks) != 0 || synthesis.Written == "" {
		t.Fatalf("unexpected synthesis: %+v", synthesis)
	}

	nodes, dependencies, err := fixture.Store.GetTaskGraph(context.Background(), completed.RootTaskID)
	if err != nil {
		t.Fatalf("load persisted task graph: %v", err)
	}
	if len(nodes) != 3 || len(dependencies) != 2 {
		t.Fatalf("unexpected persisted graph: %d nodes, %d dependencies", len(nodes), len(dependencies))
	}
	gate.assertOverlap(t)
	for _, node := range nodes {
		if node.Status != tasks.StatusCompleted {
			t.Fatalf("graph node %s (%s) is %s, want COMPLETED", node.ID, node.AssignedAgentID, node.Status)
		}
		if node.CorrelationID != completed.CorrelationID || node.CausationID == nil {
			t.Fatalf("graph node lost correlation metadata: %+v", node)
		}
	}

	turns, err := fixture.Store.ListTurns(context.Background(), completed.ConversationID, 20)
	if err != nil {
		t.Fatalf("list persisted turns: %v", err)
	}
	if len(turns) != 2 || turns[0].Role != conversation.RoleUser || turns[1].Role != conversation.RoleAssistant || !turns[1].Final {
		t.Fatalf("unexpected persisted conversation turns: %+v", turns)
	}
	if turns[1].Text != completed.UserFacingSummary {
		t.Fatalf("assistant turn is not the synthesized root result: turn=%q root=%q", turns[1].Text, completed.UserFacingSummary)
	}
	if turns[0].CorrelationID != completed.CorrelationID || turns[1].CorrelationID != completed.CorrelationID {
		t.Fatalf("turn correlation IDs do not match task %s: %+v", completed.CorrelationID, turns)
	}

	var eventCount int
	if err := fixture.Store.DB().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM events WHERE idempotency_key = ? AND processing_status = 'PROCESSED'", "parallel-text-001").Scan(&eventCount); err != nil {
		t.Fatalf("count persisted normalized event: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("persisted normalized event count = %d, want 1", eventCount)
	}
}

type wireEvent struct {
	Type    string          `json:"type"`
	TaskID  string          `json:"task_id"`
	Payload json.RawMessage `json:"payload"`
}

func readStreamEvent(t testing.TB, ctx context.Context, connection *websocket.Conn) wireEvent {
	t.Helper()
	_, raw, err := connection.Read(ctx)
	if err != nil {
		t.Fatalf("read event stream: %v", err)
	}
	var event wireEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatalf("decode event stream payload: %v", err)
	}
	return event
}

type parallelGate struct {
	mu       sync.Mutex
	want     int
	release  chan struct{}
	started  map[string]time.Time
	finished map[string]time.Time
}

func newParallelGate(want int) *parallelGate {
	return &parallelGate{
		want: want, release: make(chan struct{}),
		started: make(map[string]time.Time), finished: make(map[string]time.Time),
	}
}

func (g *parallelGate) start(agentID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.started[agentID] = time.Now()
	if len(g.started) == g.want {
		close(g.release)
	}
}

func (g *parallelGate) finish(agentID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.finished[agentID] = time.Now()
}

func (g *parallelGate) assertOverlap(t testing.TB) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.started) != g.want || len(g.finished) != g.want {
		t.Fatalf("parallel runners did not all execute: started=%v finished=%v", g.started, g.finished)
	}
	latestStart := time.Time{}
	earliestFinish := time.Time{}
	for id, started := range g.started {
		finished := g.finished[id]
		if started.After(latestStart) {
			latestStart = started
		}
		if earliestFinish.IsZero() || finished.Before(earliestFinish) {
			earliestFinish = finished
		}
	}
	if !latestStart.Before(earliestFinish) {
		t.Fatalf("agent execution intervals did not overlap: started=%v finished=%v", g.started, g.finished)
	}
}

type parallelRunner struct {
	id          string
	displayName string
	gate        *parallelGate
}

func (r *parallelRunner) Definition() coreagents.Definition {
	return coreagents.Definition{
		ID: r.id, DisplayName: r.displayName, Description: "deterministic parallel integration runner",
		Capabilities: []string{"deterministic"}, AcceptedTaskTypes: []string{"coding", "research"},
		InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
		TrustLevel: coreagents.TrustTrusted, ConcurrencyLimit: 1, Health: coreagents.HealthHealthy,
		ExecutionMode: coreagents.ModeBuiltin, SupportsCancellation: true, SupportsStreaming: true,
		UpdatedAt: time.Now().UTC(),
	}
}

func (r *parallelRunner) Run(ctx context.Context, task tasks.Task, progress func(coreagents.Progress)) (coreagents.Result, error) {
	r.gate.start(r.id)
	defer r.gate.finish(r.id)
	progress(coreagents.Progress{Percent: 10, Message: r.displayName + " started"})
	select {
	case <-ctx.Done():
		return coreagents.Result{}, ctx.Err()
	case <-r.gate.release:
	}
	progress(coreagents.Progress{Percent: 85, Message: r.displayName + " completed"})
	structured, err := json.Marshal(map[string]any{"agent_id": r.id, "answer": "evidence from " + r.displayName})
	if err != nil {
		return coreagents.Result{}, fmt.Errorf("marshal parallel result: %w", err)
	}
	return coreagents.Result{
		Structured: structured, WrittenSummary: "Evidence produced by " + r.displayName,
		SpokenSummary: r.displayName + " finished.",
	}, nil
}
