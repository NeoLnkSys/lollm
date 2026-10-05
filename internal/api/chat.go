package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lollm/lollm/internal/compression"
	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/health"
	"github.com/lollm/lollm/internal/providers"
	"github.com/lollm/lollm/internal/routing"
)

const (
	maxBodyBytes       = 20 << 20 // 20 MiB request bodies
	maxRespBytes       = 32 << 20 // 32 MiB non-streaming responses
	contentJSON        = "application/json"
	contentSSE         = "text/event-stream"
	statusClientClosed = 499 // nginx convention: client went away
)

// handleChatCompletions is the gateway's front door (spec 3.1/7): authenticate
// (middleware), resolve a combo, and proxy to the first healthy provider with
// automatic fallback. Fallback is only allowed before the first byte reaches
// the client; after that the stream is committed.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestID := RequestID(ctx)
	apiKeyID := APIKeyID(ctx)

	// --- parse request ---------------------------------------------------------
	var body map[string]any
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error", "invalid_json")
		return
	}
	if msgs, ok := body["messages"].([]any); !ok || len(msgs) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "'messages' must be a non-empty array.", "invalid_request_error", "invalid_request")
		return
	}
	modelName, _ := body["model"].(string)
	if strings.TrimSpace(modelName) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "'model' is required (a combo name or a plain model id).", "invalid_request_error", "invalid_request")
		return
	}
	stream, _ := body["stream"].(bool)

	// --- Agent Mode via model name (spec 3.7): agent-auto / agent-debate /
	// agent-parallel ---------------------------------------------------------
	if ok, mode := agentModeFromModel(modelName); ok {
		s.handleAgentRequest(w, r, body, stream, agentBaseCombo(r), mode, modelName, apiKeyID, requestID)
		return
	}

	// --- resolve combo ----------------------------------------------------------
	conns, err := s.store.ListConnections(ctx)
	if err != nil {
		s.log.Error("list connections", "err", err)
		writeOpenAIError(w, http.StatusInternalServerError, "database error", "server_error", "internal_error")
		return
	}

	combo, err := s.resolveCombo(r, modelName, conns)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error(), "invalid_request_error", "model_not_found")
		return
	}

	// --- Agent Mode via combo flag or header (spec 3.7) --------------------------
	if am := headerAgentMode(r); combo.AgentModeEnabled || am != "" {
		s.handleAgentRequest(w, r, body, stream, combo.Name, am, modelName, apiKeyID, requestID)
		return
	}

	// Token compression (spec 3.5): header > combo > global setting > off.
	// Applied after capability detection and session hashing so neither is
	// affected, and before the fallback loop so every attempt uses the same
	// compressed payload.
	tokensSaved := 0
	if mode := s.effectiveCompression(r, combo); mode != compression.Off {
		opts := compression.DefaultOptions()
		opts.Mode = mode
		tokensSaved = compression.Apply(body, opts)
		if tokensSaved > 0 {
			s.log.Info("compression applied",
				"request_id", requestID, "mode", mode, "tokens_saved", tokensSaved)
		}
	}

	// --- non-streaming: routed completion with fallback, written only once
	// a complete answer exists ----------------------------------------------------
	if !stream {
		res, err := s.complete(ctx, completeParams{
			Body: body, Combo: combo, RequestID: requestID,
			APIKeyID: apiKeyID, TokensSaved: tokensSaved,
		})
		if err != nil {
			s.writeCompleteError(w, err)
			return
		}
		w.Header().Set("Content-Type", contentJSON)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(res.RawBody)
		return
	}

	// --- streaming fallback chain -------------------------------------------------
	req := &routing.Request{
		Capabilities: routing.DetectCapabilities(body),
		SessionKey:   sessionKey(r, body),
	}
	groups, report, err := s.engine.Resolve(combo, conns, req)
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("no eligible provider for %q right now. Skipped: %s", combo.Name, skipReasons(report)),
			"server_error", "no_healthy_upstream")
		return
	}

	exclude := map[string]bool{}
	var lastErr error
	for _, g := range groups {
		for {
			conn := s.engine.PickFromGroup(g, req.SessionKey, exclude)
			if conn == nil {
				break // this group is exhausted
			}
			adapter := s.adapterFor(conn.Provider)
			start := time.Now()

			resp, err := adapter.Chat(ctx, conn, &providers.ChatRequest{
				Body: body, Model: g.Model, Stream: true,
			})
			if err != nil {
				pe := toProviderError(err)
				lastErr = pe
				s.markFailure(ctx, conn, pe)
				s.logUsage(ctx, &db.UsageLog{
					RequestID: requestID, APIKeyID: apiKeyID, ComboName: combo.Name,
					ConnectionID: conn.ID, Model: g.Model,
					LatencyMs: int(time.Since(start).Milliseconds()), StatusCode: pe.Status, Error: pe.Error(),
				})
				s.log.Warn("provider attempt failed",
					"request_id", requestID, "combo", combo.Name,
					"connection", conn.Name, "provider", conn.Provider, "model", g.Model,
					"kind", pe.Kind, "err", pe.Message)

				exclude[conn.ID] = true
				if !pe.Kind.Fallbackable() {
					writeProviderError(w, pe)
					return
				}
				continue // next connection / group
			}

			s.relayStream(w, ctx, resp, conn, combo.Name, g.Model, requestID, apiKeyID, start, tokensSaved)
			return
		}
	}

	// Every candidate failed.
	msg := "all providers failed"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusBadGateway, msg, "server_error", "all_providers_failed")
}

// resolveCombo picks the combo: X-LoLLM-Combo header > model name as combo >
// ad-hoc combo from connections listing the model.
func (s *Server) resolveCombo(r *http.Request, modelName string, conns []*db.Connection) (*db.Combo, error) {
	ctx := r.Context()
	if forced := r.Header.Get("X-LoLLM-Combo"); forced != "" {
		c, err := s.store.GetComboByName(ctx, forced)
		if err != nil {
			return nil, fmt.Errorf("combo %q (X-LoLLM-Combo) not found", forced)
		}
		return c, nil
	}
	if c, err := s.store.GetComboByName(ctx, modelName); err == nil {
		return c, nil
	}
	adHoc := routing.BuildAdHocCombo(modelName, conns)
	if len(adHoc.Models) == 0 {
		return nil, fmt.Errorf("model %q is not a combo and no connection lists it in its catalog", modelName)
	}
	return adHoc, nil
}

// sessionKey derives the sticky-session key: X-LoLLM-Session header, else a
// stable hash of the first user message.
func sessionKey(r *http.Request, body map[string]any) string {
	if k := r.Header.Get("X-LoLLM-Session"); k != "" {
		return k
	}
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := msg["role"].(string); role != "user" {
				continue
			}
			raw, _ := json.Marshal(msg["content"])
			sum := sha256.Sum256(raw)
			return hex.EncodeToString(sum[:8])
		}
	}
	return ""
}

// --- streaming relay ----------------------------------------------------------------

// relayStream forwards the upstream SSE stream to the client. The first event
// is read before anything is written, so a stream that dies immediately can
// still fall back; once the first event is written, we are committed.
func (s *Server) relayStream(w http.ResponseWriter, ctx context.Context, resp *providers.ChatResponse,
	conn *db.Connection, comboName, model, requestID, apiKeyID string, start time.Time, tokensSaved int) {

	sc := providers.NewSSE(resp.Body)
	first := true
	var prompt, completion int
	committed := false

	writeHeader := func() {
		w.Header().Set("Content-Type", contentSSE)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
	}

	for sc.Next() {
		if first {
			// Nothing has reached the client yet — a failure here (empty
			// stream) means Next() returned false; if we got an event, commit.
			writeHeader()
			committed = true
			first = false
			// TTFB is the latency signal for streams.
			s.markSuccess(ctx, conn, elapsedMs(start))
		}
		e := sc.Event()
		if p, c, ok := providers.ParseChunkUsage(e.Data); ok {
			prompt, completion = p, c
		}
		writeSSEEvent(w, e)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	streamErr := sc.Err()
	resp.Body.Close()

	status := http.StatusOK
	errText := ""
	switch {
	case streamErr != nil && ctx.Err() != nil:
		// Client disconnected mid-stream: not the provider's fault.
		status = statusClientClosed
	case streamErr != nil:
		// Upstream died mid-stream after we committed — we cannot fall back;
		// record the failure so health-aware routing avoids this connection.
		status = http.StatusBadGateway
		errText = streamErr.Error()
		s.markFailure(ctx, conn, &providers.ProviderError{Kind: providers.KindNetwork, Message: errText})
		s.log.Warn("stream died mid-response", "request_id", requestID, "connection", conn.Name, "err", errText)
	}

	s.logUsage(ctx, &db.UsageLog{
		RequestID: requestID, APIKeyID: apiKeyID, ComboName: comboName,
		ConnectionID: conn.ID, Model: model,
		PromptTokens: prompt, CompletionTokens: completion,
		TokensSaved: tokensSaved,
		LatencyMs:   int(time.Since(start).Milliseconds()), StatusCode: status, Error: errText,
	})
	s.log.Info("request served",
		"request_id", requestID, "combo", comboName, "connection", conn.Name,
		"model", model, "stream", true, "committed", committed,
		"latency_ms", time.Since(start).Milliseconds())
}

func writeSSEEvent(w io.Writer, e providers.SSEEvent) {
	if e.Name != "" && e.Name != "message" {
		fmt.Fprintf(w, "event: %s\n", e.Name)
	}
	fmt.Fprintf(w, "data: %s\n\n", e.Data)
}

// --- health marking --------------------------------------------------------------------

func (s *Server) markSuccess(ctx context.Context, conn *db.Connection, latencyMs float64) {
	if err := s.store.MarkConnectionResult(ctx, conn.ID, true, "", nil, latencyMs); err != nil {
		s.log.Warn("mark success failed", "connection", conn.ID, "err", err)
	}
}

// markFailure records a failed attempt: status + exponential backoff per
// spec 3.2/3.4 (429 → rate_limited, auth/quota → unavailable, others →
// unavailable with short backoff).
func (s *Server) markFailure(ctx context.Context, conn *db.Connection, pe *providers.ProviderError) {
	streak := conn.ConsecutiveErrors + 1 // snapshot may be stale by one; acceptable
	var (
		status  = db.StatusUnavailable
		backoff time.Duration
	)
	switch pe.Kind {
	case providers.KindRateLimited:
		status = db.StatusRateLimited
		backoff = expBackoff(30*time.Second, streak, 15*time.Minute)
		if pe.RetryAfter != nil && *pe.RetryAfter > backoff {
			backoff = *pe.RetryAfter
		}
	case providers.KindAuth:
		backoff = 60 * time.Minute // dead key/billing: stay down for a while
	case providers.KindQuota:
		backoff = 30 * time.Minute
	case providers.KindCancelled:
		return // client went away — no health signal
	default:
		backoff = expBackoff(60*time.Second, streak, 10*time.Minute)
	}
	until := time.Now().UTC().Add(backoff)
	if err := s.store.MarkConnectionResult(ctx, conn.ID, false, status, &until, 0); err != nil {
		s.log.Warn("mark failure failed", "connection", conn.ID, "err", err)
	}
	s.events.Publish(health.EventConnectionFailed, conn.ID, conn.Name,
		string(pe.Kind)+": "+pe.Message)
}

func expBackoff(base time.Duration, streak int, max time.Duration) time.Duration {
	if streak < 1 {
		streak = 1
	}
	d := base
	for i := 1; i < streak; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}

func (s *Server) logUsage(ctx context.Context, l *db.UsageLog) {
	if err := s.store.InsertUsageLog(ctx, l); err != nil {
		s.log.Warn("insert usage log failed", "err", err)
	}
}

// --- error mapping ---------------------------------------------------------------------

func toProviderError(err error) *providers.ProviderError {
	var pe *providers.ProviderError
	if errors.As(err, &pe) {
		return pe
	}
	return &providers.ProviderError{Kind: providers.KindUnknown, Message: err.Error()}
}

// writeProviderError maps a non-fallbackable provider error to a client
// response. Bad requests are the client's fault (400); everything else is a
// gateway/upstream problem (502).
func writeProviderError(w http.ResponseWriter, pe *providers.ProviderError) {
	status := http.StatusBadGateway
	errType := "server_error"
	code := string(pe.Kind)
	if pe.Kind == providers.KindBadRequest {
		status = http.StatusBadRequest
		errType = "invalid_request_error"
	}
	writeOpenAIError(w, status, pe.Error(), errType, code)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// elapsedMs returns sub-millisecond-precise elapsed milliseconds (a fast mock
// or cached response can complete in under 1 ms, and truncating to 0 would
// keep the latency EMA empty forever).
func elapsedMs(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000.0
}
