package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/lollm/lollm/internal/agent"
	"github.com/lollm/lollm/internal/db"
)

// agentModelPrefix is the reserved model-name namespace for Agent Mode
// (spec 3.7): agent-auto, agent-debate, agent-parallel.
const agentModelPrefix = "agent-"

// agentModelModes maps the reserved model names to pipeline modes.
var agentModelModes = map[string]string{
	"agent-auto":     db.AgentModeCollaborative,
	"agent-debate":   db.AgentModeDebate,
	"agent-parallel": db.AgentModeParallel,
}

// agentModeHeader activates Agent Mode (spec 3.7). Value is "true" (default
// collaborative mode) or an explicit mode name.
const agentModeHeader = "X-LoLLM-Agent-Mode"

// agentModeFromModel reports whether the requested model name selects an
// Agent Mode pipeline, and which mode.
func agentModeFromModel(model string) (bool, string) {
	if !strings.HasPrefix(model, agentModelPrefix) {
		return false, ""
	}
	mode, ok := agentModelModes[model]
	return ok, mode
}

// headerAgentMode returns the mode forced via the X-LoLLM-Agent-Mode header:
// "true" selects the default collaborative pipeline, anything else must name
// a mode explicitly.
func headerAgentMode(r *http.Request) string {
	v := strings.ToLower(strings.TrimSpace(r.Header.Get(agentModeHeader)))
	switch v {
	case "true", "1", db.AgentModeCollaborative:
		return db.AgentModeCollaborative
	case db.AgentModeDebate, db.AgentModeParallel:
		return v
	}
	return ""
}

// agentBaseCombo returns the combo an agent pipeline should route through:
// the X-LoLLM-Combo header if present, else "Auto".
func agentBaseCombo(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-LoLLM-Combo")); v != "" {
		return v
	}
	return "Auto"
}

// agentCompleter adapts the API server into the agent Runner's Completer
// interface: every internal call is a fully routed, fallback-protected,
// health-tracked, usage-logged completion.
type agentCompleter struct {
	s         *Server
	requestID string
	apiKeyID  string
}

func (c *agentCompleter) Complete(ctx context.Context, model string, body map[string]any, meta agent.CallMeta) (agent.Result, error) {
	res, err := c.s.complete(ctx, completeParams{
		Body: body, Model: model, AgentRole: meta.Role,
		RequestID: c.requestID, APIKeyID: c.apiKeyID,
		TokensSaved: meta.TokensSaved,
	})
	if err != nil {
		return agent.Result{}, err
	}
	var raw map[string]any
	_ = json.Unmarshal(res.RawBody, &raw)
	return agent.Result{
		Content:          res.Content,
		ToolCalls:        res.ToolCalls,
		Raw:              raw,
		PromptTokens:     res.Prompt,
		CompletionTokens: res.Completion,
		ServedBy:         res.Connection.Name + "/" + res.Model,
	}, nil
}

// handleAgentRequest runs an Agent Mode pipeline and writes the response.
// Non-streaming requests get a plain OpenAI-compatible JSON body; streaming
// requests receive live agent.step SSE events (unless the config hides
// internal steps), then the final answer as chat.completion.chunk events.
func (s *Server) handleAgentRequest(w http.ResponseWriter, r *http.Request, body map[string]any, stream bool, comboName, mode, modelLabel, apiKeyID, requestID string) {
	if mode == "" {
		mode = db.AgentModeCollaborative
	}
	cfg := s.loadAgentConfig(r.Context(), mode, comboName)

	runner := &agent.Runner{Completer: &agentCompleter{s: s, requestID: requestID, apiKeyID: apiKeyID}}
	req := &agent.Request{
		Body: body, Config: cfg, Mode: mode,
		RequestID: requestID, APIKeyID: apiKeyID, ComboName: comboName,
	}

	if !stream {
		out, err := runner.Run(r.Context(), req)
		if err != nil {
			s.writeCompleteError(w, err)
			return
		}
		if out.ToolPassthrough != nil {
			b, _ := json.Marshal(out.ToolPassthrough)
			w.Header().Set("Content-Type", contentJSON)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(b)
			return
		}
		w.Header().Set("Content-Type", contentJSON)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buildAgentResponse(modelLabel, mode, requestID, out))
		return
	}

	// Streaming: SSE.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl, canFlush := w.(http.Flusher)

	emitStep := func(agent.Step) {} // hidden by default (config decides)
	if canFlush && !cfg.HideInternalSteps {
		emitStep = func(st agent.Step) {
			b, _ := json.Marshal(st)
			fmt.Fprintf(w, "event: agent.step\ndata: %s\n\n", b)
			fl.Flush()
		}
	}
	runner.OnStep = emitStep

	out, err := runner.Run(r.Context(), req)
	if err != nil {
		if canFlush {
			b, _ := json.Marshal(map[string]string{"error": err.Error()})
			fmt.Fprintf(w, "event: agent.error\ndata: %s\n\n", b)
			fl.Flush()
		} else {
			s.writeCompleteError(w, err)
		}
		return
	}

	if !canFlush {
		// Client transport can't stream: send one buffered JSON body.
		w.Header().Set("Content-Type", contentJSON)
		_, _ = w.Write(buildAgentResponse(modelLabel, mode, requestID, out))
		return
	}

	if out.ToolPassthrough != nil {
		// Relay the planner's tool-call frame as normal chunk events so
		// coding-agent clients can execute the calls.
		if choices, ok := out.ToolPassthrough["choices"].([]any); ok {
			for _, ch := range choices {
				cb, _ := json.Marshal(map[string]any{"choices": []any{ch}})
				fmt.Fprintf(w, "event: chat.completion.chunk\ndata: %s\n\n", cb)
			}
			fl.Flush()
		}
		sendDone(w, fl, modelLabel, mode, out)
		return
	}

	for _, tok := range chunkText(out.FinalText) {
		sendChunk(w, fl, modelLabel, requestID, tok, nil)
	}
	sendChunk(w, fl, modelLabel, requestID, "", "stop")
	sendDone(w, fl, modelLabel, mode, out)
}

func sendChunk(w http.ResponseWriter, fl http.Flusher, model, id, content string, finish any) {
	chunk := map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []map[string]any{{
			"index": 0, "finish_reason": finish,
			"delta": map[string]any{"content": content},
		}},
	}
	b, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "event: chat.completion.chunk\ndata: %s\n\n", b)
	fl.Flush()
}

func sendDone(w http.ResponseWriter, fl http.Flusher, model, mode string, out *agent.Outcome) {
	done := map[string]any{
		"model": model, "mode": mode, "calls": out.Calls,
		"prompt_tokens": out.PromptTokens, "completion_tokens": out.CompletionTokens,
	}
	b, _ := json.Marshal(done)
	fmt.Fprintf(w, "event: agent.done\ndata: %s\n\n", b)
	fl.Flush()
}

// loadAgentConfig finds a stored agent config for the mode, else a default.
func (s *Server) loadAgentConfig(ctx context.Context, mode, comboName string) *db.AgentConfig {
	if s.store != nil {
		if cfgs, err := s.store.ListAgentConfigs(ctx); err == nil {
			for _, c := range cfgs {
				if c.Mode == mode && c.Name == comboName {
					return c
				}
			}
			for _, c := range cfgs {
				if c.Mode == mode {
					return c
				}
			}
		}
	}
	return &db.AgentConfig{Mode: mode, Name: comboName} // built-in defaults
}

// buildAgentResponse wraps a pipeline outcome in an OpenAI-compatible body.
func buildAgentResponse(model, mode, requestID string, out *agent.Outcome) []byte {
	b, _ := json.Marshal(map[string]any{
		"id": requestID, "object": "chat.completion",
		"model": model,
		"choices": []map[string]any{{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{
				"role": "assistant", "content": out.FinalText,
			},
		}},
		"usage": map[string]any{
			"prompt_tokens":     out.PromptTokens,
			"completion_tokens": out.CompletionTokens,
		},
		"agent": map[string]any{
			"mode": mode, "calls": out.Calls,
		},
	})
	return b
}

// chunkText splits text into small streaming chunks.
func chunkText(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	cur := ""
	for _, w := range strings.Fields(s) {
		if cur == "" {
			cur = w
		} else if len(cur)+1+len(w) <= 24 {
			cur += " " + w
		} else {
			out = append(out, cur)
			cur = w
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
