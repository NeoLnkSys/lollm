package providers

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/secret"
)

const testProviderKey = "sk-test-1234567890"

// newTestAdapter builds an openrouter-configured adapter against a mock server.
func newTestAdapter(t *testing.T, m *MockServer) (*OpenAICompat, []byte) {
	t.Helper()
	masterKey := make([]byte, secret.MasterKeySize)
	for i := range masterKey {
		masterKey[i] = byte(i)
	}
	meta, ok := Lookup("openrouter")
	if !ok {
		t.Fatal("openrouter meta missing")
	}
	return NewOpenAICompat(meta, masterKey, nil), masterKey
}

// mockConn builds a connection pointed at the mock server.
func mockConn(t *testing.T, m *MockServer, masterKey []byte, mut ...func(*db.Connection)) *db.Connection {
	t.Helper()
	enc, err := secret.EncryptString(masterKey, testProviderKey)
	if err != nil {
		t.Fatal(err)
	}
	c := &db.Connection{
		ID: "conn_mock", Name: "mock", Provider: "openrouter",
		APIKeyEncrypted: enc, BaseURL: m.BaseURL(),
		IsActive: true, Status: db.StatusActive, Priority: 10, Weight: 1,
	}
	for _, f := range mut {
		f(c)
	}
	return c
}

func chatReq(model string, stream bool) *ChatRequest {
	return &ChatRequest{
		Body: map[string]any{
			"model":    "Auto", // client asked for a combo; routing rewrote it
			"stream":   stream,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		},
		Model:  model,
		Stream: stream,
	}
}

func TestChatNonStreaming(t *testing.T) {
	m := NewMockServer(MockOK())
	defer m.Close()
	a, key := newTestAdapter(t, m)
	conn := mockConn(t, m, key)

	resp, err := a.Chat(context.Background(), conn, chatReq("mock-model", false))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Status != 200 || resp.Stream {
		t.Fatalf("unexpected response: %+v", resp)
	}
	body, _ := io.ReadAll(resp.Body)
	if p, c := ParseUsage(body); p != 12 || c != 6 {
		t.Fatalf("usage parse: %d/%d", p, c)
	}

	recs := m.Records()
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if recs[0].Model != "mock-model" {
		t.Fatalf("model must be rewritten to the routing target: %q", recs[0].Model)
	}
	if recs[0].AuthHeader != "Bearer "+testProviderKey {
		t.Fatalf("auth header wrong: %q", recs[0].AuthHeader)
	}
	if recs[0].Header.Get("X-Title") != "LoLLM" {
		t.Fatal("openrouter extra header X-Title missing")
	}
}

func TestChatStreaming(t *testing.T) {
	m := NewMockServer(MockSSE())
	defer m.Close()
	a, key := newTestAdapter(t, m)
	conn := mockConn(t, m, key)

	resp, err := a.Chat(context.Background(), conn, chatReq("mock-model", true))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !resp.Stream {
		t.Fatal("expected streaming response")
	}

	// Walk the stream via the SSE parser and find usage + DONE.
	sc := NewSSE(resp.Body)
	var sawDone, sawUsage bool
	for sc.Next() {
		e := sc.Event()
		if e.Data == "[DONE]" {
			sawDone = true
			continue
		}
		if _, _, ok := ParseChunkUsage(e.Data); ok {
			sawUsage = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if !sawDone || !sawUsage {
		t.Fatalf("stream must contain usage chunk and DONE: done=%v usage=%v", sawDone, sawUsage)
	}

	// The adapter must have asked for include_usage on streams.
	rec := m.Records()[0]
	opts, ok := rec.Body["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("stream_options.include_usage must be set: %+v", rec.Body["stream_options"])
	}
}

func TestChatScriptedFallbackSequence(t *testing.T) {
	// First request 429s, second 500s, then OK — the fallback test pattern.
	m := NewMockServer(MockRateLimit(30), MockStatus(500), MockOK())
	defer m.Close()
	a, key := newTestAdapter(t, m)
	conn := mockConn(t, m, key)

	resp, err := a.Chat(context.Background(), conn, chatReq("mock-model", false))
	if err == nil {
		resp.Body.Close()
		t.Fatal("first call must fail with 429")
	}
	pe, ok := err.(*ProviderError)
	if !ok || pe.Kind != KindRateLimited {
		t.Fatalf("expected rate_limited, got %#v", err)
	}
	if pe.RetryAfter == nil || *pe.RetryAfter != 30*time.Second {
		t.Fatalf("Retry-After must parse to 30s: %v", pe.RetryAfter)
	}
	if !pe.Kind.Fallbackable() {
		t.Fatal("rate limiting must be fallbackable")
	}

	resp, err = a.Chat(context.Background(), conn, chatReq("mock-model", false))
	if err == nil {
		resp.Body.Close()
		t.Fatal("second call must fail with 500")
	}
	if pe := err.(*ProviderError); pe.Kind != KindServer {
		t.Fatalf("expected server error, got %#v", err)
	}

	resp, err = a.Chat(context.Background(), conn, chatReq("mock-model", false))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestChatErrorKinds(t *testing.T) {
	cases := []struct {
		name     string
		behavior MockBehavior
		kind     ErrKind
		fallback bool
	}{
		{"auth 401", MockStatus(401), KindAuth, true},
		{"quota 402", MockStatus(402), KindQuota, true},
		{"not found 404", MockStatus(404), KindNotFound, true},
		{"bad request 400", MockStatus(400), KindBadRequest, false},
		{"server 503", MockStatus(503), KindServer, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMockServer(tc.behavior)
			defer m.Close()
			a, key := newTestAdapter(t, m)
			conn := mockConn(t, m, key)

			resp, err := a.Chat(context.Background(), conn, chatReq("mock-model", false))
			if err == nil {
				resp.Body.Close()
				t.Fatal("expected error")
			}
			pe, ok := err.(*ProviderError)
			if !ok {
				t.Fatalf("expected *ProviderError, got %#v", err)
			}
			if pe.Kind != tc.kind {
				t.Fatalf("kind: want %s got %s (%s)", tc.kind, pe.Kind, pe.Message)
			}
			if pe.Message == "" {
				t.Fatal("error message must be extracted from the body")
			}
			if pe.Kind.Fallbackable() != tc.fallback {
				t.Fatalf("fallbackable: want %v", tc.fallback)
			}
		})
	}
}

func TestChatNetworkError(t *testing.T) {
	m := NewMockServer(MockOK())
	a, key := newTestAdapter(t, m)
	conn := mockConn(t, m, key)
	m.Close() // server is gone

	_, err := a.Chat(context.Background(), conn, chatReq("mock-model", false))
	pe, ok := err.(*ProviderError)
	if !ok {
		t.Fatalf("expected *ProviderError, got %#v", err)
	}
	if pe.Kind != KindNetwork {
		t.Fatalf("expected network error, got %s", pe.Kind)
	}
}

func TestChatTimeout(t *testing.T) {
	m := NewMockServer(MockHang(2))
	defer m.Close()
	a, key := newTestAdapter(t, m)
	conn := mockConn(t, m, key)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, err := a.Chat(ctx, conn, chatReq("mock-model", false))
	pe, ok := err.(*ProviderError)
	if !ok {
		t.Fatalf("expected *ProviderError, got %#v", err)
	}
	if pe.Kind != KindTimeout {
		t.Fatalf("expected timeout, got %s", pe.Kind)
	}
}

func TestChatCancelled(t *testing.T) {
	m := NewMockServer(MockHang(2))
	defer m.Close()
	a, key := newTestAdapter(t, m)
	conn := mockConn(t, m, key)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Chat(ctx, conn, chatReq("mock-model", false))
	if pe, ok := err.(*ProviderError); !ok || pe.Kind != KindCancelled {
		t.Fatalf("expected cancelled, got %#v", err)
	}
	if KindCancelled.Fallbackable() {
		t.Fatal("cancelled must not be fallbackable")
	}
}

func TestListModels(t *testing.T) {
	m := NewMockServer()
	defer m.Close()
	a, key := newTestAdapter(t, m)
	conn := mockConn(t, m, key)

	models, err := a.ListModels(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 || models[0] != "mock-model" {
		t.Fatalf("models: %v", models)
	}
}

func TestBaseURLPrecedenceAndErrors(t *testing.T) {
	a, key := newTestAdapter(t, m0())

	// Explicit connection base_url wins.
	m := NewMockServer(MockOK())
	defer m.Close()
	conn := mockConn(t, m, key)
	if u, _ := a.meta.BaseURL(conn); u != m.BaseURL() {
		t.Fatalf("conn base_url must win: %s", u)
	}

	// Provider default when the connection has none.
	conn2 := &db.Connection{Provider: "openrouter", APIKeyEncrypted: conn.APIKeyEncrypted}
	if u, _ := a.meta.BaseURL(conn2); u != "https://openrouter.ai/api/v1" {
		t.Fatalf("provider default expected: %s", u)
	}

	// Providers without a default must demand base_url.
	cfMeta, _ := Lookup("cloudflare-ai")
	if _, err := cfMeta.BaseURL(conn2); err == nil {
		t.Fatal("cloudflare-ai without base_url must error")
	}
}

func m0() *MockServer { return nil } // helper so TestBaseURLPrecedenceAndErrors compiles cleanly

func TestOllamaCloudSendsBearer(t *testing.T) {
	m := NewMockServer(MockOK())
	defer m.Close()
	masterKey := make([]byte, secret.MasterKeySize)
	meta, _ := Lookup("ollama")
	a := NewOpenAICompat(meta, masterKey, nil)

	// Cloud connection: encrypted key → Bearer auth (via mock upstream).
	enc, _ := secret.EncryptString(masterKey, "ollama-cloud-key")
	cloudConn := &db.Connection{Provider: "ollama", APIKeyEncrypted: enc,
		BaseURL: m.BaseURL(), IsActive: true, Status: db.StatusActive}
	resp, err := a.Chat(context.Background(), cloudConn, chatReq("mock-model", false))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if rec := m.Records()[0]; rec.AuthHeader != "Bearer ollama-cloud-key" {
		t.Fatalf("ollama cloud must send Bearer auth: %q", rec.AuthHeader)
	}
}

func TestLocalOllamaSendsNoAuth(t *testing.T) {
	m := NewMockServer(MockOK())
	defer m.Close()
	masterKey := make([]byte, secret.MasterKeySize)
	meta, _ := Lookup("ollama")
	a := NewOpenAICompat(meta, masterKey, nil)

	// Local server: explicit base_url and NO key → no Authorization header.
	localConn := &db.Connection{Provider: "ollama", APIKeyEncrypted: "",
		BaseURL: m.BaseURL(), IsActive: true, Status: db.StatusActive}
	resp, err := a.Chat(context.Background(), localConn, chatReq("mock-model", false))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if rec := m.Records()[0]; rec.AuthHeader != "" {
		t.Fatalf("local ollama must not send auth: %q", rec.AuthHeader)
	}
}

func TestLookupAndAliases(t *testing.T) {
	for _, name := range []string{"openrouter", "OpenRouter", "open-router"} {
		if m, ok := Lookup(name); !ok || m.Name != "openrouter" {
			t.Fatalf("lookup %q failed", name)
		}
	}
	if _, ok := Lookup("no-such-provider"); ok {
		t.Fatal("unknown provider must not resolve")
	}
	// Every provider named in the spec must be present.
	for _, name := range []string{"openrouter", "gemini", "groq", "mistral",
		"cloudflare-ai", "ollama", "poolside", "cerebras", "fireworks", "together"} {
		if _, ok := Lookup(name); !ok {
			t.Fatalf("spec provider %q missing from registry", name)
		}
	}
	if len(Names()) != len(registry) {
		t.Fatal("Names must list every provider")
	}
}
