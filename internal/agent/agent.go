// Package agent implements Agent Mode (spec section 3.7): multi-agent
// pipelines whose intermediate steps stay internal — only the reviewed final
// answer is returned to the client.
//
// Modes:
//
//	collaborative — planner → (reviewer → revise)×max_rounds → finalizer
//	debate        — several agents answer in parallel, a judge picks/merges
//	parallel      — several agents answer in parallel, a merger synthesizes
//
// Every internal call is a fully routed completion (fallback, health
// marking, usage logging with the agent role) issued through the Completer
// interface, which the API server implements.
package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lollm/lollm/internal/db"
)

// Timeouts and token budgets for internal calls.
const (
	RoleTimeout       = 60 * time.Second
	TotalTimeout      = 5 * time.Minute
	internalMaxTokens = 4096
	internalMinTokens = 1024
)

// CallMeta tags one internal completion for logging.
type CallMeta struct {
	Role        string
	RequestID   string
	APIKeyID    string
	TokensSaved int // recorded on the first call only (original payload savings)
}

// Result is one internal completion outcome.
type Result struct {
	Content          string
	ToolCalls        []any
	Raw              map[string]any // full provider response (tool-call passthrough)
	PromptTokens     int
	CompletionTokens int
	ServedBy         string // "connection/model" for diagnostics
}

// Completer performs one routed, non-streaming chat completion for a model
// or combo name. Implemented by the API server; faked in tests.
type Completer interface {
	Complete(ctx context.Context, model string, body map[string]any, meta CallMeta) (Result, error)
}

// Request is one agent pipeline invocation.
type Request struct {
	Body        map[string]any // the client's original request body
	Config      *db.AgentConfig
	Mode        string // explicit mode override (model name suffix); "" = config
	RequestID   string
	APIKeyID    string
	ComboName   string // default combo for roles with an empty model
	TokensSaved int    // compression savings from the original payload
}

// Step is a progress notification (surfaced as SSE events when the client
// opted out of hide_internal_steps).
type Step struct {
	Role   string `json:"role"`
	Status string `json:"status"` // started | finished
	Round  int    `json:"round,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Outcome is the pipeline result delivered to the client.
type Outcome struct {
	// FinalText is the reviewed final answer (nil ToolPassthrough).
	FinalText string
	// ToolPassthrough is the raw planner response to forward verbatim when
	// the planner emitted tool_calls (coding agents must execute them).
	ToolPassthrough map[string]any
	// Aggregated usage across all internal calls.
	PromptTokens     int
	CompletionTokens int
	Calls            int
}

func (o *Outcome) add(res Result) {
	o.PromptTokens += res.PromptTokens
	o.CompletionTokens += res.CompletionTokens
	o.Calls++
}

// Runner executes pipelines.
type Runner struct {
	Completer Completer
	OnStep    func(Step) // optional progress callback

	mu sync.Mutex // guards OnStep: fan-out roles emit steps concurrently
}

func (r *Runner) step(st Step) {
	if r.OnStep != nil {
		r.mu.Lock()
		r.OnStep(st)
		r.mu.Unlock()
	}
}

// Run executes the requested pipeline.
func (r *Runner) Run(ctx context.Context, req *Request) (*Outcome, error) {
	ctx, cancel := context.WithTimeout(ctx, TotalTimeout)
	defer cancel()

	mode := req.Mode
	if mode == "" && req.Config != nil {
		mode = req.Config.Mode
	}
	switch mode {
	case db.AgentModeDebate, db.AgentModeParallel:
		return r.fanOut(ctx, req, mode)
	default:
		return r.collaborative(ctx, req)
	}
}

// --- shared plumbing -----------------------------------------------------------

// chat issues one internal completion. It clones the original body, swaps in
// the role's messages/model, forces non-streaming, and raises tiny
// max_tokens budgets (reasoning models need headroom).
func (r *Runner) chat(ctx context.Context, req *Request, role db.AgentRole, tag string,
	messages []any, tokensSaved int) (Result, error) {

	model := strings.TrimSpace(role.Model)
	if model == "" {
		model = req.ComboName
	}
	if strings.HasPrefix(strings.ToLower(model), "agent-") {
		return Result{}, fmt.Errorf(
			"agent role %q references agent pipeline %q — recursive agent mode is not allowed",
			role.Name, model)
	}

	body := make(map[string]any, len(req.Body)+3)
	for k, v := range req.Body {
		body[k] = v
	}
	body["messages"] = messages
	body["model"] = model
	body["stream"] = false
	delete(body, "stream_options")
	if mt, ok := body["max_tokens"].(float64); !ok || mt < internalMinTokens {
		body["max_tokens"] = float64(internalMaxTokens)
	}

	cctx, cancel := context.WithTimeout(ctx, RoleTimeout)
	defer cancel()

	res, err := r.Completer.Complete(cctx, model, body, CallMeta{
		Role: tag, RequestID: req.RequestID, APIKeyID: req.APIKeyID, TokensSaved: tokensSaved,
	})
	if err != nil {
		return Result{}, fmt.Errorf("agent role %q (model %s): %w", role.Name, model, err)
	}
	return res, nil
}

func origMessages(body map[string]any) []any {
	msgs, _ := body["messages"].([]any)
	return msgs
}

// roleMessages prepends the role's system prompt to the original conversation.
func roleMessages(orig []any, systemPrompt string) []any {
	out := make([]any, 0, len(orig)+1)
	if systemPrompt != "" {
		out = append(out, map[string]any{"role": "system", "content": systemPrompt})
	}
	return append(out, orig...)
}

func withAssistant(msgs []any, text string) []any {
	return append(msgs, map[string]any{"role": "assistant", "content": text})
}

func withUser(msgs []any, text string) []any {
	return append(msgs, map[string]any{"role": "user", "content": text})
}

// roleMap resolves the three collaborative roles from the config, falling
// back to built-in defaults (model = the request's combo).
func roleMap(cfg *db.AgentConfig) map[string]db.AgentRole {
	m := map[string]db.AgentRole{}
	if cfg != nil {
		for _, role := range cfg.Roles {
			m[strings.ToLower(role.Name)] = role
		}
	}
	def := func(name, prompt string) db.AgentRole {
		if role, ok := m[name]; ok {
			return role
		}
		return db.AgentRole{Name: name, SystemPrompt: prompt}
	}
	m["planner"] = def("planner", defaultPlannerPrompt)
	m["reviewer"] = def("reviewer", defaultReviewerPrompt)
	m["finalizer"] = def("finalizer", defaultFinalizerPrompt)
	return m
}

// orderedRoles returns the config's roles in order (fan-out modes use all but
// the last as generators and the last as judge/merger).
func orderedRoles(cfg *db.AgentConfig) []db.AgentRole {
	if cfg != nil && len(cfg.Roles) >= 2 {
		return cfg.Roles
	}
	m := roleMap(cfg)
	return []db.AgentRole{m["planner"], m["reviewer"], m["finalizer"]}
}

const (
	defaultPlannerPrompt   = "You are the main planner. Produce a complete, working first draft of the requested answer."
	defaultReviewerPrompt  = "You are a strict reviewer. Criticize the draft: find bugs, logical errors, missing edge cases, and unclear parts. Be specific and actionable."
	defaultFinalizerPrompt = "You produce the final polished answer, applying the reviewer's fixes. Output only the final answer, nothing else."

	reviewInstruction    = "Review the assistant's draft answer above critically. List concrete problems: bugs, logical errors, missing edge cases, unclear explanations. Be specific and brief."
	reviseInstruction    = "Revise your draft answer according to the reviewer's critique.\n\nReviewer's critique:\n%s"
	finalizeInstruction  = "Produce the final answer to the user's original request by applying the reviewer's critique to the draft above. Output ONLY the final answer, no commentary.\n\nReviewer's critique:\n%s"
	debateGenInstruction = "Answer the user's request as convincingly as you can from your assigned perspective."
	parallelGenTemplate  = "Answer the user's request completely and correctly."
	judgeDebateTemplate  = "Several candidate answers to the user's request follow. Pick the best answer, merge their strengths, and output ONLY the single best final answer.\n\n%s"
	judgeParallelTmpl    = "Several independent answers to the user's request follow. Merge them into one complete, correct final answer. Output ONLY the final answer.\n\n%s"
)

// --- collaborative ----------------------------------------------------------------

func (r *Runner) collaborative(ctx context.Context, req *Request) (*Outcome, error) {
	roles := roleMap(req.Config)
	planner, reviewer, finalizer := roles["planner"], roles["reviewer"], roles["finalizer"]
	orig := origMessages(req.Body)
	out := &Outcome{}

	// 1) Planner draft.
	r.step(Step{Role: "planner", Status: "started"})
	res, err := r.chat(ctx, req, planner, "planner",
		roleMessages(orig, planner.SystemPrompt), req.TokensSaved)
	if err != nil {
		return nil, err
	}
	out.add(res)
	if len(res.ToolCalls) > 0 {
		// Coding agents: tool calls must be executed by the client — forward
		// them verbatim. Reviewing tool calls only adds latency.
		r.step(Step{Role: "planner", Status: "finished", Detail: "tool_calls passthrough"})
		out.ToolPassthrough = res.Raw
		return out, nil
	}
	draft := res.Content
	r.step(Step{Role: "planner", Status: "finished", Round: 1})

	// 2) Review rounds.
	rounds := 1
	if req.Config != nil && req.Config.MaxRounds > rounds {
		rounds = req.Config.MaxRounds
	}
	critique := ""
	for round := 1; round <= rounds; round++ {
		r.step(Step{Role: "reviewer", Status: "started", Round: round})
		rres, err := r.chat(ctx, req, reviewer, fmt.Sprintf("reviewer(r%d)", round),
			withUser(withAssistant(roleMessages(orig, reviewer.SystemPrompt), draft), reviewInstruction), 0)
		if err != nil {
			return nil, err
		}
		out.add(rres)
		critique = rres.Content
		r.step(Step{Role: "reviewer", Status: "finished", Round: round})

		if round < rounds {
			r.step(Step{Role: "planner", Status: "started", Round: round + 1, Detail: "revise"})
			vres, err := r.chat(ctx, req, planner, fmt.Sprintf("revise(r%d)", round),
				withUser(withAssistant(roleMessages(orig, planner.SystemPrompt), draft),
					fmt.Sprintf(reviseInstruction, critique)), 0)
			if err != nil {
				return nil, err
			}
			out.add(vres)
			draft = vres.Content
			r.step(Step{Role: "planner", Status: "finished", Round: round + 1, Detail: "revise"})
		}
	}

	// 3) Finalizer.
	r.step(Step{Role: "finalizer", Status: "started"})
	fres, err := r.chat(ctx, req, finalizer, "finalizer",
		withUser(withAssistant(roleMessages(orig, finalizer.SystemPrompt), draft),
			fmt.Sprintf(finalizeInstruction, critique)), 0)
	if err != nil {
		return nil, err
	}
	out.add(fres)
	out.FinalText = fres.Content
	r.step(Step{Role: "finalizer", Status: "finished"})
	return out, nil
}

// --- debate / parallel --------------------------------------------------------------

func (r *Runner) fanOut(ctx context.Context, req *Request, mode string) (*Outcome, error) {
	roles := orderedRoles(req.Config)
	generators := roles[:len(roles)-1]
	judge := roles[len(roles)-1]
	orig := origMessages(req.Body)
	out := &Outcome{}

	results := make([]Result, len(generators))
	errs := make([]error, len(generators))
	var wg sync.WaitGroup
	for i, gen := range generators {
		wg.Add(1)
		go func(i int, gen db.AgentRole) {
			defer wg.Done()
			r.step(Step{Role: gen.Name, Status: "started"})
			instruction := parallelGenTemplate
			if mode == db.AgentModeDebate {
				instruction = debateGenInstruction
			}
			ts := 0
			if i == 0 {
				ts = req.TokensSaved
			}
			res, err := r.chat(ctx, req, gen, gen.Name,
				withUser(roleMessages(orig, gen.SystemPrompt), instruction), ts)
			results[i], errs[i] = res, err
			status := "finished"
			if err != nil {
				status = "failed"
			}
			r.step(Step{Role: gen.Name, Status: status})
		}(i, gen)
	}
	wg.Wait()

	succeeded := 0
	for i := range generators {
		if errs[i] != nil {
			continue
		}
		out.add(results[i])
		// Tool calls from any generator are forwarded verbatim (the client
		// must execute them; judging tool calls adds nothing).
		if len(results[i].ToolCalls) > 0 {
			out.ToolPassthrough = results[i].Raw
			return out, nil
		}
		succeeded++
	}
	if succeeded == 0 {
		for _, err := range errs {
			if err != nil {
				return nil, err
			}
		}
		return nil, fmt.Errorf("agent mode %s: all generators failed", mode)
	}

	var candidates strings.Builder
	n := 0
	for i := range generators {
		if errs[i] != nil {
			continue
		}
		n++
		fmt.Fprintf(&candidates, "=== Candidate %d (%s) ===\n%s\n\n", n, generators[i].Name, results[i].Content)
	}
	instruction := fmt.Sprintf(judgeParallelTmpl, candidates.String())
	if mode == db.AgentModeDebate {
		instruction = fmt.Sprintf(judgeDebateTemplate, candidates.String())
	}

	r.step(Step{Role: judge.Name, Status: "started", Detail: "judge"})
	jres, err := r.chat(ctx, req, judge, judge.Name+"(judge)",
		withUser(roleMessages(orig, judge.SystemPrompt), instruction), 0)
	if err != nil {
		return nil, err
	}
	out.add(jres)
	out.FinalText = jres.Content
	r.step(Step{Role: judge.Name, Status: "finished", Detail: "judge"})
	return out, nil
}
