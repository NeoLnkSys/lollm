package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/proxy"
	"github.com/lollm/lollm/internal/secret"
)

// OpenAICompat is the generic adapter for every provider that speaks the
// OpenAI HTTP dialect (chat/completions + models). It rewrites the model
// name, applies provider auth/headers, injects stream usage options, and
// normalizes upstream errors.
type OpenAICompat struct {
	meta      Meta
	masterKey []byte
	clients   *proxy.Manager
}

// NewOpenAICompat builds the adapter for a provider. clients may be nil
// (a direct default client is used).
func NewOpenAICompat(meta Meta, masterKey []byte, clients *proxy.Manager) *OpenAICompat {
	return &OpenAICompat{meta: meta, masterKey: masterKey, clients: clients}
}

// Name returns the provider name.
func (a *OpenAICompat) Name() string { return a.meta.Name }

func (a *OpenAICompat) client(conn *db.Connection) *http.Client {
	if a.clients != nil {
		return a.clients.Client(conn.ProxyPoolID)
	}
	return http.DefaultClient
}

// Chat performs one chat completion against the provider (spec 3.1/3.2).
func (a *OpenAICompat) Chat(ctx context.Context, conn *db.Connection, req *ChatRequest) (*ChatResponse, error) {
	if req == nil || req.Body == nil {
		return nil, errors.New("providers: nil chat request body")
	}

	base, err := a.meta.BaseURL(conn)
	if err != nil {
		return nil, err
	}

	payload, err := buildPayload(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	a.applyAuth(httpReq, conn)
	for k, v := range a.meta.ExtraHeaders {
		httpReq.Header.Set(k, v)
	}

	resp, err := a.client(conn).Do(httpReq)
	if err != nil {
		return nil, classifyErr(err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return nil, normalizeError(resp.StatusCode, body, resp.Header)
	}
	return &ChatResponse{Status: resp.StatusCode, Stream: req.Stream, Body: resp.Body}, nil
}

// ListModels fetches the model ids visible to this connection.
func (a *OpenAICompat) ListModels(ctx context.Context, conn *db.Connection) ([]string, error) {
	base, err := a.meta.BaseURL(conn)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	a.applyAuth(httpReq, conn)
	for k, v := range a.meta.ExtraHeaders {
		httpReq.Header.Set(k, v)
	}

	resp, err := a.client(conn).Do(httpReq)
	if err != nil {
		return nil, classifyErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return nil, normalizeError(resp.StatusCode, body, resp.Header)
	}

	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&out); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

// buildPayload clones the client body, swaps in the target model, and (for
// streams) asks the provider to include a final usage chunk.
func buildPayload(req *ChatRequest) ([]byte, error) {
	body := make(map[string]any, len(req.Body)+1)
	for k, v := range req.Body {
		body[k] = v
	}
	body["model"] = req.Model

	if req.Stream {
		streamOpts := map[string]any{}
		if existing, ok := req.Body["stream_options"].(map[string]any); ok {
			for k, v := range existing {
				streamOpts[k] = v
			}
		}
		streamOpts["include_usage"] = true
		body["stream_options"] = streamOpts
	}
	return json.Marshal(body)
}

func (a *OpenAICompat) applyAuth(httpReq *http.Request, conn *db.Connection) {
	if a.meta.AuthStyle == AuthNone {
		return
	}
	if conn.APIKeyEncrypted == "" {
		return
	}
	key, err := secret.DecryptString(a.masterKey, conn.APIKeyEncrypted)
	if err != nil || key == "" {
		return // provider will answer 401; the normalizer reports it
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
}

// --- error normalization ------------------------------------------------------

// classifyErr maps transport-level failures to a ProviderError kind.
func classifyErr(err error) *ProviderError {
	pe := &ProviderError{Kind: KindNetwork, Message: err.Error()}
	switch {
	case errors.Is(err, context.Canceled):
		pe.Kind = KindCancelled
	case errors.Is(err, context.DeadlineExceeded):
		pe.Kind = KindTimeout
	default:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			pe.Kind = KindTimeout
		}
	}
	return pe
}

// normalizeError maps an HTTP error response to a ProviderError, extracting
// the message/code from the provider's JSON error body (OpenAI shape
// {"error":{...}} plus common variants) and honoring Retry-After.
func normalizeError(status int, body []byte, header http.Header) *ProviderError {
	pe := &ProviderError{Status: status}
	pe.Message, pe.Code = parseErrorBody(body)

	switch {
	case status == http.StatusTooManyRequests:
		pe.Kind = KindRateLimited
		if ra := header.Get("Retry-After"); ra != "" {
			pe.RetryAfter = parseRetryAfter(ra)
		}
	case status == http.StatusPaymentRequired:
		pe.Kind = KindQuota // e.g. OpenRouter insufficient credits
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		pe.Kind = KindAuth
	case status == http.StatusNotFound:
		pe.Kind = KindNotFound // usually "model not found"
	case status == http.StatusBadRequest, status == http.StatusRequestEntityTooLarge:
		pe.Kind = KindBadRequest
	case status == http.StatusUnprocessableEntity:
		// 422 (mis. pydantic "extra_forbidden"): payload ditolak provider ini —
		// provider lain mungkin menerimanya, jadi masih layak fallback.
		pe.Kind = KindUnprocessable
	case status >= 500:
		pe.Kind = KindServer
	default:
		pe.Kind = KindUnknown
	}
	return pe
}

// parseErrorBody pulls (message, code) out of provider error payloads.
func parseErrorBody(body []byte) (message, code string) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return truncate(string(body)), ""
	}
	if e, ok := raw["error"].(map[string]any); ok {
		if m, ok := e["message"].(string); ok {
			message = m
		}
		if c, ok := e["code"].(string); ok {
			code = c
		} else if c, ok := e["type"].(string); ok {
			code = c
		}
	}
	if message == "" {
		if m, ok := raw["message"].(string); ok {
			message = m
		}
	}
	if message == "" {
		if d, ok := raw["detail"].(string); ok { // some FastAPI-based providers
			message = d
		}
	}
	if message == "" {
		message = truncate(string(body))
	}
	return message, code
}

func parseRetryAfter(v string) *time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		d := time.Duration(secs) * time.Second
		return &d
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return &d
	}
	return nil
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// --- usage helpers (used by the API layer in P4) -------------------------------

// ParseUsage extracts prompt/completion token counts from a non-streaming
// OpenAI response body.
func ParseUsage(body []byte) (prompt, completion int) {
	var out struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &out) != nil || out.Usage == nil {
		return 0, 0
	}
	return out.Usage.PromptTokens, out.Usage.CompletionTokens
}

// ParseChunkUsage extracts usage from a streaming chat.completion.chunk data
// payload (present in the final chunk when include_usage was requested).
func ParseChunkUsage(data string) (prompt, completion int, ok bool) {
	var out struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(data), &out) != nil || out.Usage == nil {
		return 0, 0, false
	}
	return out.Usage.PromptTokens, out.Usage.CompletionTokens, true
}
