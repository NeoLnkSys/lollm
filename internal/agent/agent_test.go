package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lollm/lollm/internal/db"
)

// fakeCompleter records calls and scripts responses per role tag.
type fakeCompleter struct {
	mu    sync.Mutex
	calls []fakeCall
	next  func(call int, role, model string, msgs []any) (Result, error)
}

type fakeCall struct {
	Role     string
	Model    string
	Messages []any
	Body     map[string]any
}

func (f *fakeCompleter) Complete(_ context.Context, model string, body map[string]any, meta CallMeta) (Result, error) {
	f.mu.Lock()
	msgs, _ := body["messages"].([]any)
	f.calls = append(f.calls, fakeCall{Role: meta.Role, Model: model, Messages: msgs, Body: body})
	n := len(f.calls)
	f.mu.Unlock()
	return f.next(n, meta.Role, model, msgs)
}

func (f *fakeCompleter) roles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.Role)
	}
	return out
}

func res(content string, p, c int) Result {
	return Result{Content: content, PromptTokens: p, CompletionTokens: c,
		Raw: map[string]any{"content": content}}
}

func baseRequest(cfg *db.AgentConfig) *Request {
	return &Request{
		Body:      map[string]any{"model": "agent-auto", "messages": []any{map[string]any{"role": "user", "content": "q"}}},
		Config:    cfg,
		RequestID: "req_test", APIKeyID: "key_x", ComboName: "Auto",
	}
}

func cfg(roles ...db.AgentRole) *db.AgentConfig {
	c := &db.AgentConfig{Name: "test", Mode: db.AgentModeCollaborative, Roles: roles, MaxRounds: 1}
	if len(roles) == 0 {
		c.Roles = []db.AgentRole{
			{Name: "planner", Model: "Auto", SystemPrompt: "PLAN"},
			{Name: "reviewer", Model: "Auto", SystemPrompt: "REVIEW"},
			{Name: "finalizer", Model: "Auto", SystemPrompt: "FINALIZE"},
		}
	}
	return c
}

func TestCollaborativeHappyPath(t *testing.T) {
	f := &fakeCompleter{next: func(n int, role, _ string, _ []any) (Result, error) {
		switch role {
		case "planner":
			return res("DRAFT", 10, 5), nil
		case "reviewer(r1)":
			return res("CRITIQUE", 20, 8), nil
		case "finalizer":
			return res("FINAL", 30, 12), nil
		}
		return Result{}, fmt.Errorf("unexpected role %q", role)
	}}
	r := &Runner{Completer: f}
	out, err := r.Run(context.Background(), baseRequest(cfg()))
	if err != nil {
		t.Fatal(err)
	}

	if got := f.roles(); strings.Join(got, ",") != "planner,reviewer(r1),finalizer" {
		t.Fatalf("call sequence wrong: %v", got)
	}
	if out.FinalText != "FINAL" {
		t.Fatalf("final text: %q", out.FinalText)
	}
	if out.PromptTokens != 60 || out.CompletionTokens != 25 || out.Calls != 3 {
		t.Fatalf("usage aggregation wrong: %+v", out)
	}

	// Reviewer sees the draft and its own system prompt.
	revCall := f.calls[1]
	foundDraft, foundSystem := false, false
	for _, m := range revCall.Messages {
		mm := m.(map[string]any)
		if mm["role"] == "assistant" && mm["content"] == "DRAFT" {
			foundDraft = true
		}
		if mm["role"] == "system" && mm["content"] == "REVIEW" {
			foundSystem = true
		}
	}
	if !foundDraft || !foundSystem {
		t.Fatalf("reviewer messages wrong: %+v", revCall.Messages)
	}
}

func TestCollaborativeTwoRounds(t *testing.T) {
	f := &fakeCompleter{next: func(n int, role, _ string, _ []any) (Result, error) {
		switch role {
		case "planner":
			return res("DRAFT-v1", 0, 0), nil
		case "reviewer(r1)":
			return res("C1", 0, 0), nil
		case "revise(r1)":
			return res("DRAFT-v2", 0, 0), nil
		case "reviewer(r2)":
			return res("C2", 0, 0), nil
		case "finalizer":
			return res("FINAL", 0, 0), nil
		}
		return Result{}, fmt.Errorf("unexpected role %q", role)
	}}
	c := cfg()
	c.MaxRounds = 2
	r := &Runner{Completer: f}
	out, err := r.Run(context.Background(), baseRequest(c))
	if err != nil {
		t.Fatal(err)
	}
	want := "planner,reviewer(r1),revise(r1),reviewer(r2),finalizer"
	if got := strings.Join(f.roles(), ","); got != want {
		t.Fatalf("rounds sequence: got %s want %s", got, want)
	}
	// The second review must see the revised draft.
	sawV2 := false
	for _, m := range f.calls[3].Messages {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "assistant" && mm["content"] == "DRAFT-v2" {
			sawV2 = true
		}
	}
	if !sawV2 {
		t.Fatal("second review must receive the revised draft")
	}
	// The finalizer must receive the second critique.
	sawC2 := false
	for _, m := range f.calls[4].Messages {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "user" && strings.Contains(mm["content"].(string), "C2") {
			sawC2 = true
		}
	}
	if !sawC2 {
		t.Fatal("finalizer must receive the latest critique")
	}
	if out.FinalText != "FINAL" {
		t.Fatalf("final: %q", out.FinalText)
	}
}

func TestToolCallsPassthrough(t *testing.T) {
	f := &fakeCompleter{next: func(n int, role, _ string, _ []any) (Result, error) {
		if role == "planner" {
			return Result{Content: "", ToolCalls: []any{map[string]any{"id": "call_1"}},
				Raw: map[string]any{"choices": []any{"tool-call-response"}}}, nil
		}
		return Result{}, fmt.Errorf("no further calls expected, got %q", role)
	}}
	r := &Runner{Completer: f}
	out, err := r.Run(context.Background(), baseRequest(cfg()))
	if err != nil {
		t.Fatal(err)
	}
	if out.ToolPassthrough == nil {
		t.Fatal("tool-call response must be forwarded verbatim")
	}
	if len(f.calls) != 1 {
		t.Fatalf("review must be skipped on tool calls, calls: %v", f.roles())
	}
}

func TestDebateFanOut(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	f := &fakeCompleter{next: func(n int, role, _ string, _ []any) (Result, error) {
		mu.Lock()
		seen = append(seen, role)
		mu.Unlock()
		switch role {
		case "pro", "con":
			return res("ANSWER-"+role, 5, 5), nil
		case "judge(judge)":
			return res("WINNER", 7, 7), nil
		}
		return Result{}, fmt.Errorf("unexpected role %q", role)
	}}
	c := &db.AgentConfig{Mode: db.AgentModeDebate, Roles: []db.AgentRole{
		{Name: "pro", Model: "Auto", SystemPrompt: "argue pro"},
		{Name: "con", Model: "Auto", SystemPrompt: "argue con"},
		{Name: "judge", Model: "Auto", SystemPrompt: "pick"},
	}}
	r := &Runner{Completer: f}
	out, err := r.Run(context.Background(), baseRequest(c))
	if err != nil {
		t.Fatal(err)
	}
	if out.FinalText != "WINNER" {
		t.Fatalf("judge output must be final: %q", out.FinalText)
	}
	if len(f.calls) != 3 {
		t.Fatalf("debate must run 2 generators + judge: %v", f.roles())
	}
	// The judge must see both candidates.
	judgeMsgs := f.calls[2].Messages
	blob := fmt.Sprint(judgeMsgs)
	if !strings.Contains(blob, "ANSWER-pro") || !strings.Contains(blob, "ANSWER-con") {
		t.Fatal("judge must receive all candidates")
	}
}

func TestParallelFanOutAndToleratesOneFailure(t *testing.T) {
	f := &fakeCompleter{next: func(n int, role, _ string, _ []any) (Result, error) {
		switch role {
		case "a":
			return res("A-ANSWER", 1, 1), nil
		case "b":
			return Result{}, fmt.Errorf("generator b exploded")
		case "merger(judge)":
			return res("MERGED", 2, 2), nil
		}
		return Result{}, fmt.Errorf("unexpected role %q", role)
	}}
	c := &db.AgentConfig{Mode: db.AgentModeParallel, Roles: []db.AgentRole{
		{Name: "a", Model: "Auto"}, {Name: "b", Model: "Auto"}, {Name: "merger", Model: "Auto"},
	}}
	r := &Runner{Completer: f}
	out, err := r.Run(context.Background(), baseRequest(c))
	if err != nil {
		t.Fatal(err)
	}
	if out.FinalText != "MERGED" {
		t.Fatalf("merger output must be final: %q", out.FinalText)
	}
	// One failing generator must not kill the pipeline.
	if !strings.Contains(fmt.Sprint(f.calls[2].Messages), "A-ANSWER") {
		t.Fatal("merger must receive the surviving candidate")
	}
}

func TestAllGeneratorsFailed(t *testing.T) {
	f := &fakeCompleter{next: func(n int, role, _ string, _ []any) (Result, error) {
		return Result{}, fmt.Errorf("boom-%s", role)
	}}
	c := &db.AgentConfig{Mode: db.AgentModeParallel, Roles: []db.AgentRole{
		{Name: "a", Model: "Auto"}, {Name: "b", Model: "Auto"}, {Name: "merger", Model: "Auto"},
	}}
	r := &Runner{Completer: f}
	if _, err := r.Run(context.Background(), baseRequest(c)); err == nil {
		t.Fatal("all generators failing must error the pipeline")
	}
}

func TestRecursionGuard(t *testing.T) {
	c := cfg()
	c.Roles[0].Model = "agent-deeper" // planner references another pipeline
	r := &Runner{Completer: &fakeCompleter{}}
	_, err := r.Run(context.Background(), baseRequest(c))
	if err == nil || !strings.Contains(err.Error(), "recursive") {
		t.Fatalf("recursion guard must fire: %v", err)
	}
}

func TestEmptyRoleModelUsesCombo(t *testing.T) {
	var gotModel string
	f := &fakeCompleter{next: func(n int, role, model string, _ []any) (Result, error) {
		if role == "planner" {
			gotModel = model
		}
		return res("x", 0, 0), nil
	}}
	c := cfg()
	c.Roles[0].Model = "" // inherit the request's combo
	r := &Runner{Completer: f}
	if _, err := r.Run(context.Background(), baseRequest(c)); err != nil {
		t.Fatal(err)
	}
	if gotModel != "Auto" {
		t.Fatalf("empty role model must fall back to the combo: %q", gotModel)
	}
}

func TestInternalMaxTokensFloor(t *testing.T) {
	f := &fakeCompleter{next: func(n int, role, model string, _ []any) (Result, error) {
		return res("x", 0, 0), nil
	}}
	r := &Runner{Completer: f}
	req := baseRequest(cfg())
	req.Body["max_tokens"] = float64(50) // tiny client budget
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	mt, _ := f.calls[0].Body["max_tokens"].(float64)
	f.mu.Unlock()
	if mt != float64(internalMaxTokens) {
		t.Fatalf("internal max_tokens must be raised to %d, got %v", internalMaxTokens, mt)
	}
}

func TestStepCallbackFires(t *testing.T) {
	var steps []Step
	f := &fakeCompleter{next: func(n int, role, _ string, _ []any) (Result, error) {
		return res("x", 0, 0), nil
	}}
	r := &Runner{Completer: f, OnStep: func(s Step) { steps = append(steps, s) }}
	if _, err := r.Run(context.Background(), baseRequest(cfg())); err != nil {
		t.Fatal(err)
	}
	if len(steps) < 6 { // planner 2 + reviewer 2 + finalizer 2
		t.Fatalf("expected step events, got %d: %+v", len(steps), steps)
	}
	if steps[0].Role != "planner" || steps[0].Status != "started" {
		t.Fatalf("first step wrong: %+v", steps[0])
	}
}
