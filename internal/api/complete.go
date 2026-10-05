package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/providers"
	"github.com/lollm/lollm/internal/routing"
)

// completeParams describes one routed non-streaming completion.
type completeParams struct {
	Body        map[string]any
	Model       string // combo name or plain model (used when Combo is nil)
	Combo       *db.Combo
	AgentRole   string // usage-log tag for agent pipelines ("" otherwise)
	RequestID   string
	APIKeyID    string
	TokensSaved int
}

// completeResult is a finished completion, ready to hand to a client.
type completeResult struct {
	ComboName  string
	Connection *db.Connection
	Model      string
	RawBody    []byte
	Content    string
	ToolCalls  []any
	Prompt     int
	Completion int
}

// complete performs a fully routed non-streaming chat completion: resolve a
// combo, walk the fallback chain, mark health, and log usage. It backs the
// plain non-streaming API path and every internal Agent Mode call.
func (s *Server) complete(ctx context.Context, p completeParams) (*completeResult, error) {
	conns, err := s.store.ListConnections(ctx)
	if err != nil {
		return nil, err
	}
	combo := p.Combo
	if combo == nil {
		combo, err = s.resolveComboModel(ctx, p.Model, conns)
		if err != nil {
			return nil, err
		}
	}

	req := &routing.Request{Capabilities: routing.DetectCapabilities(p.Body)}
	groups, report, err := s.engine.Resolve(combo, conns, req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", routing.ErrNoCandidates, skipReasons(report))
	}

	exclude := map[string]bool{}
	var lastErr error
	for _, g := range groups {
		for {
			conn := s.engine.PickFromGroup(g, "", exclude)
			if conn == nil {
				break
			}
			adapter := s.adapterFor(conn.Provider)
			start := time.Now()

			resp, err := adapter.Chat(ctx, conn, &providers.ChatRequest{
				Body: p.Body, Model: g.Model, Stream: false,
			})
			if err != nil {
				pe := toProviderError(err)
				lastErr = pe
				s.markFailure(ctx, conn, pe)
				s.logUsage(ctx, &db.UsageLog{
					RequestID: p.RequestID, APIKeyID: p.APIKeyID, ComboName: combo.Name,
					ConnectionID: conn.ID, Model: g.Model, AgentRole: p.AgentRole,
					LatencyMs: int(time.Since(start).Milliseconds()), StatusCode: pe.Status, Error: pe.Error(),
				})
				s.log.Warn("provider attempt failed",
					"request_id", p.RequestID, "combo", combo.Name, "connection", conn.Name,
					"model", g.Model, "agent_role", p.AgentRole, "kind", pe.Kind, "err", pe.Message)
				exclude[conn.ID] = true
				if !pe.Kind.Fallbackable() {
					return nil, pe
				}
				continue
			}

			// Read fully before reporting success: a truncated read falls
			// back to the next candidate.
			respBody, rerr := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
			resp.Body.Close()
			if rerr != nil {
				pe := &providers.ProviderError{Kind: providers.KindNetwork, Message: rerr.Error()}
				lastErr = pe
				s.markFailure(ctx, conn, pe)
				exclude[conn.ID] = true
				continue
			}

			prompt, completion := providers.ParseUsage(respBody)
			content, toolCalls := parseAssistantMessage(respBody)
			latencyMs := elapsedMs(start)
			s.markSuccess(ctx, conn, latencyMs)
			s.logUsage(ctx, &db.UsageLog{
				RequestID: p.RequestID, APIKeyID: p.APIKeyID, ComboName: combo.Name,
				ConnectionID: conn.ID, Model: g.Model, AgentRole: p.AgentRole,
				PromptTokens: prompt, CompletionTokens: completion,
				TokensSaved: p.TokensSaved,
				LatencyMs:   int(latencyMs), StatusCode: resp.Status,
			})
			s.log.Info("request served",
				"request_id", p.RequestID, "combo", combo.Name, "connection", conn.Name,
				"model", g.Model, "agent_role", p.AgentRole, "stream", false, "latency_ms", latencyMs)
			return &completeResult{
				ComboName: combo.Name, Connection: conn, Model: g.Model,
				RawBody: respBody, Content: content, ToolCalls: toolCalls,
				Prompt: prompt, Completion: completion,
			}, nil
		}
	}

	if lastErr == nil {
		lastErr = &providers.ProviderError{Kind: providers.KindUnknown, Message: "no candidates"}
	}
	return nil, lastErr
}

// resolveComboModel resolves a combo without header forcing (internal calls).
func (s *Server) resolveComboModel(ctx context.Context, modelName string, conns []*db.Connection) (*db.Combo, error) {
	if c, err := s.store.GetComboByName(ctx, modelName); err == nil {
		return c, nil
	}
	adHoc := routing.BuildAdHocCombo(modelName, conns)
	if len(adHoc.Models) == 0 {
		return nil, fmt.Errorf("model %q is not a combo and no connection lists it in its catalog", modelName)
	}
	return adHoc, nil
}

// skipReasons renders a routing report's skip list.
func skipReasons(report *routing.Report) string {
	var b strings.Builder
	for i, sk := range report.Skipped {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s/%s (%s)", sk.ConnectionID[:min(len(sk.ConnectionID), 12)], sk.Model, sk.Reason)
	}
	return b.String()
}

// writeCompleteError maps completion errors to client responses.
func (s *Server) writeCompleteError(w http.ResponseWriter, err error) {
	if errors.Is(err, routing.ErrNoCandidates) {
		writeOpenAIError(w, http.StatusServiceUnavailable, err.Error(), "server_error", "no_healthy_upstream")
		return
	}
	var pe *providers.ProviderError
	if errors.As(err, &pe) {
		writeProviderError(w, pe)
		return
	}
	writeOpenAIError(w, http.StatusBadGateway, err.Error(), "server_error", "all_providers_failed")
}

// parseAssistantMessage extracts the first choice's content and tool_calls.
func parseAssistantMessage(body []byte) (string, []any) {
	var out struct {
		Choices []struct {
			Message struct {
				Content   any   `json:"content"`
				ToolCalls []any `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &out) != nil || len(out.Choices) == 0 {
		return "", nil
	}
	content, _ := out.Choices[0].Message.Content.(string)
	return content, out.Choices[0].Message.ToolCalls
}
