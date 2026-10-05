package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lollm/lollm/internal/auth"
	"github.com/lollm/lollm/internal/config"
	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/providers"
	"github.com/lollm/lollm/internal/secret"
)

// newTestAPISrv builds a fully wired API server over a temp database.
// Returns the server (for handler-level tests), the store, a valid internal
// API key, and the master key (for encrypting mock upstreams).
func newTestAPISrv(t *testing.T) (*Server, *db.Store, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	store := db.NewStore(d)

	masterKey, err := secret.LoadOrCreateMasterKey(filepath.Join(dir, ".master.key"))
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(config.Default(), store, masterKey, log, "test")

	plain, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateAPIKey(context.Background(), &db.APIKey{
		Name: "test", KeyHash: auth.HashKey(plain), KeyPrefix: auth.DisplayPrefix(plain), IsActive: true,
	}); err != nil {
		t.Fatal(err)
	}
	return srv, store, plain, masterKey
}

// newTestAPI starts the wired API server on an httptest listener.
func newTestAPI(t *testing.T) (*httptest.Server, *db.Store, string, []byte) {
	t.Helper()
	srv, store, plain, masterKey := newTestAPISrv(t)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store, plain, masterKey
}

// addMockConn registers a connection pointing at a mock upstream.
func addMockConn(t *testing.T, store *db.Store, masterKey []byte, name string,
	m *providers.MockServer, models []string, active bool) *db.Connection {
	t.Helper()
	enc, err := secret.EncryptString(masterKey, "test-upstream-key")
	if err != nil {
		t.Fatal(err)
	}
	modelsJSON, _ := json.Marshal(models)
	c := &db.Connection{
		Name: name, Provider: "custom", APIKeyEncrypted: enc,
		BaseURL: m.BaseURL(), IsActive: active, Status: db.StatusActive,
		Priority: 10, Weight: 1, ModelsJSON: string(modelsJSON),
	}
	if err := store.CreateConnection(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func addCombo(t *testing.T, store *db.Store, name string, models ...db.ComboModel) *db.Combo {
	t.Helper()
	c := &db.Combo{Name: name, Strategy: db.StrategyHealthAware, Models: models}
	if err := store.CreateCombo(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func postChat(t *testing.T, ts *httptest.Server, key string, payload string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func TestHealthz(t *testing.T) {
	ts, _, _, _ := newTestAPI(t)
	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out["status"] != "ok" {
		t.Fatalf("healthz body: %v %v", out, err)
	}
}

func TestAuthEnforced(t *testing.T) {
	ts, _, key, _ := newTestAPI(t)

	resp, _ := ts.Client().Get(ts.URL + "/v1/models")
	if resp.StatusCode != 401 {
		t.Fatalf("missing key must 401, got %d", resp.StatusCode)
	}
	var errBody map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&errBody)
	if _, ok := errBody["error"]; !ok {
		t.Fatalf("error shape must be OpenAI-style: %v", errBody)
	}
	resp.Body.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer lollm-wrong")
	resp2, _ := ts.Client().Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("wrong key must 401, got %d", resp2.StatusCode)
	}

	req3, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req3.Header.Set("x-api-key", key)
	resp3, _ := ts.Client().Do(req3)
	resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("x-api-key must be accepted, got %d", resp3.StatusCode)
	}
}

func TestChatNonStreamingFallback(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)
	ctx := context.Background()

	failA := providers.NewMockServer(providers.MockRateLimit(30))
	defer failA.Close()
	okB := providers.NewMockServer(providers.MockOK())
	defer okB.Close()

	connA := addMockConn(t, store, masterKey, "upstream-a", failA, []string{"mock-model"}, true)
	connB := addMockConn(t, store, masterKey, "upstream-b", okB, []string{"mock-model"}, true)
	addCombo(t, store, "Auto",
		db.ComboModel{ConnectionID: connA.ID, Model: "mock-model", Priority: 1},
		db.ComboModel{ConnectionID: connB.ID, Model: "mock-model", Priority: 2})

	resp, body := postChat(t, ts, key,
		`{"model":"Auto","messages":[{"role":"user","content":"hello"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 after fallback, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Hello from mock") {
		t.Fatalf("unexpected body: %s", body)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Fatal("X-Request-Id must be set")
	}

	// The failing connection must be marked rate-limited with backoff.
	gotA, err := store.GetConnection(ctx, connA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotA.Status != db.StatusRateLimited || gotA.BackoffUntil == nil || gotA.ConsecutiveErrors != 1 {
		t.Fatalf("connA health wrong: %+v", gotA)
	}
	// The healthy one stays active with latency recorded.
	gotB, err := store.GetConnection(ctx, connB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotB.Status != db.StatusActive || gotB.LatencyEMAMs <= 0 {
		t.Fatalf("connB health wrong: %+v", gotB)
	}

	// Both attempts must be logged: one 429 failure, one 200 success.
	logs, err := store.ListUsageLogs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 {
		t.Fatalf("expected 2 usage rows (attempt per candidate), got %d", len(logs))
	}
	statuses := map[int]int{}
	for _, l := range logs {
		statuses[l.StatusCode]++
	}
	if statuses[429] != 1 || statuses[200] != 1 {
		t.Fatalf("usage rows wrong: %v", statuses)
	}
}

func TestChatStreamingFallback(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)
	ctx := context.Background()
	failA := providers.NewMockServer(providers.MockStatus(500))
	defer failA.Close()
	okB := providers.NewMockServer(providers.MockSSE())
	defer okB.Close()

	connA := addMockConn(t, store, masterKey, "upstream-a", failA, []string{"mock-model"}, true)
	connB := addMockConn(t, store, masterKey, "upstream-b", okB, []string{"mock-model"}, true)
	addCombo(t, store, "Auto",
		db.ComboModel{ConnectionID: connA.ID, Model: "mock-model", Priority: 1},
		db.ComboModel{ConnectionID: connB.ID, Model: "mock-model", Priority: 2})

	resp, body := postChat(t, ts, key,
		`{"model":"Auto","stream":true,"messages":[{"role":"user","content":"hello"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content type: %s", ct)
	}
	if !strings.Contains(body, "data: ") || !strings.Contains(body, "[DONE]") || !strings.Contains(body, "from mock") {
		t.Fatalf("stream body incomplete: %q", body)
	}

	// Usage from the stream's include_usage chunk must be recorded.
	logs, _ := store.ListUsageLogs(ctx, 10)
	if len(logs) != 2 {
		t.Fatalf("expected 2 usage rows, got %d", len(logs))
	}
	var success *db.UsageLog
	for _, l := range logs {
		if l.StatusCode == 200 {
			success = l
		}
	}
	if success == nil || success.PromptTokens != 12 || success.CompletionTokens != 6 {
		t.Fatalf("stream usage not captured: %+v", success)
	}
}

func TestChatComboHeaderForces(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)

	okB := providers.NewMockServer(providers.MockOK())
	defer okB.Close()
	connB := addMockConn(t, store, masterKey, "upstream-b", okB, []string{"mock-model"}, true)
	addCombo(t, store, "Auto",
		db.ComboModel{ConnectionID: connB.ID, Model: "mock-model", Priority: 1})

	// body.model is nonsense, but X-LoLLM-Combo forces the Auto combo.
	resp, body := postChat(t, ts, key,
		`{"model":"whatever-not-a-model","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-LoLLM-Combo": "Auto"})
	if resp.StatusCode != 200 || !strings.Contains(body, "Hello from mock") {
		t.Fatalf("forced combo failed: %d %s", resp.StatusCode, body)
	}

	// Unknown forced combo → 404.
	resp, body = postChat(t, ts, key,
		`{"model":"x","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-LoLLM-Combo": "DoesNotExist"})
	if resp.StatusCode != 404 {
		t.Fatalf("unknown forced combo must 404, got %d %s", resp.StatusCode, body)
	}
}

func TestChatUnknownModel(t *testing.T) {
	ts, _, key, _ := newTestAPI(t)
	resp, body := postChat(t, ts, key,
		`{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown model must 404, got %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "no-such-model") {
		t.Fatalf("error must name the model: %s", body)
	}
}

func TestChatNoHealthyCandidates(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)

	okUp := providers.NewMockServer(providers.MockOK())
	defer okUp.Close()
	conn := addMockConn(t, store, masterKey, "upstream", okUp, []string{"mock-model"}, false) // disabled
	addCombo(t, store, "Auto", db.ComboModel{ConnectionID: conn.ID, Model: "mock-model", Priority: 1})

	resp, body := postChat(t, ts, key,
		`{"model":"Auto","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 503 {
		t.Fatalf("expected 503, got %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "is_active=false") {
		t.Fatalf("skip reasons must be included for ops: %s", body)
	}
}

func TestChatRequestValidation(t *testing.T) {
	ts, _, key, _ := newTestAPI(t)

	resp, body := postChat(t, ts, key, `{not json`, nil)
	if resp.StatusCode != 400 || !strings.Contains(body, "invalid JSON") {
		t.Fatalf("bad json: %d %s", resp.StatusCode, body)
	}

	resp, body = postChat(t, ts, key, `{"model":"Auto"}`, nil)
	if resp.StatusCode != 400 || !strings.Contains(body, "messages") {
		t.Fatalf("missing messages: %d %s", resp.StatusCode, body)
	}

	resp, body = postChat(t, ts, key, `{"messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("missing model: %d %s", resp.StatusCode, body)
	}
}

func TestChatNonFallbackableBadRequest(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)

	bad := providers.NewMockServer(providers.MockStatus(400))
	defer bad.Close()
	conn := addMockConn(t, store, masterKey, "upstream", bad, []string{"mock-model"}, true)
	addCombo(t, store, "Auto", db.ComboModel{ConnectionID: conn.ID, Model: "mock-model", Priority: 1})

	resp, body := postChat(t, ts, key,
		`{"model":"Auto","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("provider 400 must surface as 400 (no fallback), got %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "mock status 400") {
		t.Fatalf("provider message should surface: %s", body)
	}
}

func TestModelsList(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)

	okUp := providers.NewMockServer(providers.MockOK())
	defer okUp.Close()
	addMockConn(t, store, masterKey, "upstream", okUp, []string{"model-x", "model-y"}, true)
	addCombo(t, store, "Auto", db.ComboModel{ConnectionID: "whatever", Model: "model-x", Priority: 1})

	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, m := range out.Data {
		ids[m.ID] = m.OwnedBy
	}
	if _, ok := ids["Auto"]; !ok {
		t.Fatalf("combo names must be listed as models: %v", ids)
	}
	if ids["model-x"] != "custom" {
		t.Fatalf("connection catalog models must be listed: %v", ids)
	}
}

func TestUnknownEndpoint404(t *testing.T) {
	ts, _, key, _ := newTestAPI(t)
	req, _ := http.NewRequest("GET", ts.URL+"/v1/nope", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}
