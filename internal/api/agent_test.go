package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/providers"
)

// countUpstream returns how many chat completions the mock upstream served.
func countUpstream(m *providers.MockServer) int {
	n := 0
	for _, rec := range m.Records() {
		if strings.HasSuffix(rec.Path, "/chat/completions") {
			n++
		}
	}
	return n
}

// agentRolesFromLogs returns the set of agent roles recorded in usage logs.
func agentRolesFromLogs(t *testing.T, store *db.Store) map[string]bool {
	t.Helper()
	logs, err := store.ListUsageLogs(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]bool{}
	for _, l := range logs {
		if l.AgentRole != "" && l.StatusCode == 200 {
			roles[l.AgentRole] = true
		}
	}
	return roles
}

// TestAgentCollaborativeByModel: model "agent-auto" runs the seeded
// collaborative pipeline (planner → reviewer×2 rounds → finalizer = 5 calls)
// and returns only the final answer in an OpenAI-shaped body.
func TestAgentCollaborativeByModel(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()

	conn := addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	addCombo(t, store, "Auto", db.ComboModel{ConnectionID: conn.ID, Model: "mock-model", Priority: 1})

	// Stored pipeline config: two review rounds.
	if err := store.CreateAgentConfig(context.Background(), &db.AgentConfig{
		Name: "Auto", Mode: db.AgentModeCollaborative, MaxRounds: 2,
		Roles: []db.AgentRole{
			{Name: "planner", Model: "Auto", SystemPrompt: "plan"},
			{Name: "reviewer", Model: "Auto", SystemPrompt: "review"},
			{Name: "finalizer", Model: "Auto", SystemPrompt: "finalize"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	resp, body := postChat(t, ts, key,
		`{"model":"agent-auto","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Seeded default config: MaxRounds 2 → planner, reviewer(r1), revise(r1),
	// reviewer(r2), finalizer.
	if n := countUpstream(m); n != 5 {
		t.Fatalf("expected 5 internal calls, upstream saw %d", n)
	}
	for _, rec := range m.Records() {
		if strings.HasSuffix(rec.Path, "/chat/completions") && rec.Stream {
			t.Fatal("internal agent calls must be non-streaming")
		}
	}

	roles := agentRolesFromLogs(t, store)
	for _, want := range []string{"planner", "reviewer(r1)", "revise(r1)", "reviewer(r2)", "finalizer"} {
		if !roles[want] {
			t.Fatalf("usage log missing agent role %q; got %v", want, roles)
		}
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Agent struct {
			Mode  string `json:"mode"`
			Calls int    `json:"calls"`
		} `json:"agent"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "Hello from mock" {
		t.Fatalf("bad final answer: %s", body)
	}
	if out.Agent.Mode != "collaborative" || out.Agent.Calls != 5 {
		t.Fatalf("bad agent block: %+v", out.Agent)
	}
	if out.Usage.PromptTokens != 5*12 || out.Usage.CompletionTokens != 5*6 {
		t.Fatalf("usage must aggregate all internal calls, got %+v", out.Usage)
	}
}

// TestAgentModeHeaderTrue: X-LoLLM-Agent-Mode: true activates the pipeline
// for a normal combo request (spec 3.7 activation path).
func TestAgentModeHeaderTrue(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()

	conn := addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	addCombo(t, store, "Auto", db.ComboModel{ConnectionID: conn.ID, Model: "mock-model", Priority: 1})

	// Stored config with two review rounds (same shape the CLI seeds).
	if err := store.CreateAgentConfig(context.Background(), &db.AgentConfig{
		Name: "Auto", Mode: db.AgentModeCollaborative, MaxRounds: 2,
		Roles: []db.AgentRole{
			{Name: "planner", Model: "Auto", SystemPrompt: "plan"},
			{Name: "reviewer", Model: "Auto", SystemPrompt: "review"},
			{Name: "finalizer", Model: "Auto", SystemPrompt: "finalize"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	resp, body := postChat(t, ts, key,
		`{"model":"Auto","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-LoLLM-Agent-Mode": "true"})
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if n := countUpstream(m); n != 5 {
		t.Fatalf("header must trigger the collaborative pipeline: %d upstream calls", n)
	}
}

// TestAgentDebateStreaming: model "agent-debate" with stream=true emits
// agent.step progress events, then the final answer as chat.completion.chunk
// events, then agent.done — never the intermediate candidates.
func TestAgentDebateStreaming(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()

	conn := addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	addCombo(t, store, "Auto", db.ComboModel{ConnectionID: conn.ID, Model: "mock-model", Priority: 1})

	resp, body := postChat(t, ts, key,
		`{"model":"agent-debate","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected SSE content type, got %q", ct)
	}

	// 2 generators + 1 judge.
	if n := countUpstream(m); n != 3 {
		t.Fatalf("expected 3 internal calls (2 generators + judge), got %d", n)
	}
	if c := strings.Count(body, "event: agent.step"); c < 6 {
		t.Fatalf("expected >=6 agent.step events (start/finish per role), got %d: %s", c, body)
	}
	if !strings.Contains(body, "Hello from mock") {
		t.Fatalf("final answer missing from stream: %s", body)
	}
	if !strings.Contains(body, "event: agent.done") || !strings.Contains(body, `"mode":"debate"`) {
		t.Fatalf("agent.done event missing: %s", body)
	}
	// Only the judge's answer may reach the client.
	if c := strings.Count(body, "event: chat.completion.chunk"); c < 2 {
		t.Fatalf("expected content + finish chunks, got %d", c)
	}
}

// TestAgentToolPassthrough: when the planner responds with tool_calls, the
// raw frame is forwarded verbatim so coding-agent clients can execute them.
func TestAgentToolPassthrough(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)
	toolBody := `{"id":"chatcmpl-tools","object":"chat.completion","created":1730000000,` +
		`"model":"mock-model","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/etc\"}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	m := providers.NewMockServer(providers.MockOKBody(toolBody))
	defer m.Close()

	conn := addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	addCombo(t, store, "Auto", db.ComboModel{ConnectionID: conn.ID, Model: "mock-model", Priority: 1})

	resp, body := postChat(t, ts, key,
		`{"model":"agent-auto","messages":[{"role":"user","content":"read the file"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if n := countUpstream(m); n != 1 {
		t.Fatalf("tool_calls must short-circuit the pipeline: %d upstream calls", n)
	}
	if !strings.Contains(body, `"read_file"`) || !strings.Contains(body, `"call_1"`) {
		t.Fatalf("tool_calls must be forwarded verbatim: %s", body)
	}
}

// TestAgentRecursiveGuard: a role pointing at another agent-* pipeline must
// be rejected instead of recursing.
func TestAgentRecursiveGuard(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()

	conn := addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	addCombo(t, store, "Auto", db.ComboModel{ConnectionID: conn.ID, Model: "mock-model", Priority: 1})

	err := store.CreateAgentConfig(context.Background(), &db.AgentConfig{
		Name: "Auto", Mode: db.AgentModeDebate,
		Roles: []db.AgentRole{
			{Name: "generator", Model: "agent-auto", SystemPrompt: "x"}, // recursion!
			{Name: "merger", Model: "Auto", SystemPrompt: "y"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, body := postChat(t, ts, key,
		`{"model":"agent-debate","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode == 200 {
		t.Fatalf("recursive agent config must fail, got 200: %s", body)
	}
	if !strings.Contains(body, "recursive") {
		t.Fatalf("error must explain the recursion guard: %s", body)
	}
}
