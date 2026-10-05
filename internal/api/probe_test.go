package api

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/lollm/lollm/internal/config"
	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/providers"
	"github.com/lollm/lollm/internal/secret"
)

// newProbeServer builds a Server over the standard test fixtures.
func newProbeServer(t *testing.T) (*Server, *db.Store, []byte) {
	t.Helper()
	_, store, _, masterKey := newTestAPI(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(config.Default(), store, masterKey, log, "test"), store, masterKey
}

func TestProbeDefaultUsesListModels(t *testing.T) {
	srv, store, masterKey := newProbeServer(t)

	// Non-CF/Ollama provider → GET /models probe (free).
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()
	conn := addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	if err := srv.Probe(context.Background(), conn); err != nil {
		t.Fatalf("healthy connection must probe clean: %v", err)
	}
	// The mock only records chat requests: a successful probe with zero chat
	// records proves the /models path was used.
	if recs := m.Records(); len(recs) != 0 {
		t.Fatalf("default probe must hit /models, not chat: %s", recs[0].Path)
	}

	// A 500 from the provider must surface as an error.
	m2 := providers.NewMockServer(providers.MockStatus(500))
	defer m2.Close()
	conn2 := addMockConn(t, store, masterKey, "upstream2", m2, []string{"mock-model"}, true)
	if err := srv.Probe(context.Background(), conn2); err == nil {
		t.Fatal("failing upstream must fail the probe")
	}
}

func TestProbeChatPathForCloudflareAndOllama(t *testing.T) {
	srv, _, masterKey := newProbeServer(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()

	// Cloudflare: no GET /models — must prove via a real chat completion.
	enc, _ := secret.EncryptString(masterKey, "cf-key")
	cf := &db.Connection{ID: "conn_cf", Name: "cf", Provider: "cloudflare-ai",
		APIKeyEncrypted: enc, BaseURL: m.BaseURL(), IsActive: true, Status: db.StatusActive}
	if err := srv.Probe(context.Background(), cf); err != nil {
		t.Fatalf("cloudflare probe must use chat: %v", err)
	}
	rec := m.Records()[0]
	if rec.Path != "/v1/chat/completions" {
		t.Fatalf("cloudflare probe must hit chat, got %s", rec.Path)
	}
	if rec.Model != "@cf/meta/llama-3.3-70b-instruct-fp8-fast" {
		t.Fatalf("cloudflare default probe model wrong: %s", rec.Model)
	}

	// Ollama: prefer a small known model from the catalog.
	m.Reset()
	encO, _ := secret.EncryptString(masterKey, "ol-key")
	ol := &db.Connection{ID: "conn_ol", Name: "ol", Provider: "ollama",
		APIKeyEncrypted: encO, BaseURL: m.BaseURL(), IsActive: true, Status: db.StatusActive,
		ModelsJSON: `["deepseek-v4-pro:0813","gpt-oss:20b"]`}
	if err := srv.Probe(context.Background(), ol); err != nil {
		t.Fatalf("ollama probe must use chat: %v", err)
	}
	rec = m.Records()[0]
	if rec.Model != "gpt-oss:20b" {
		t.Fatalf("ollama probe must prefer the small model: %s", rec.Model)
	}
	if mt, _ := rec.Body["max_tokens"].(float64); mt != 1 {
		t.Fatalf("probe must be cheap (max_tokens=1): %v", rec.Body["max_tokens"])
	}
}
