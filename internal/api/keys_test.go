package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/providers"
	"github.com/lollm/lollm/internal/secret"
)

// addMultiKeyConn registers a connection carrying several API keys.
func addMultiKeyConn(t *testing.T, store *db.Store, masterKey []byte, name string,
	m *providers.MockServer, keys []string, models []string) *db.Connection {
	t.Helper()
	enc, err := secret.EncryptString(masterKey, strings.Join(keys, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	modelsJSON, _ := json.Marshal(models)
	c := &db.Connection{
		Name: name, Provider: "custom", APIKeyEncrypted: enc,
		BaseURL: m.BaseURL(), IsActive: true, Status: db.StatusActive,
		Priority: 10, Weight: 1, ModelsJSON: string(modelsJSON),
	}
	if err := store.CreateConnection(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

// Multi-key fallback DALAM satu connection: ketika key pertama kena rate
// limit, request harus otomatis diulang dengan key berikutnya pada connection
// yang sama (bukan langsung lompat ke connection lain).
func TestChatRetriesRateLimitWithNextKey(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)
	// Upstream: 429 untuk key-1, sukses untuk key-2.
	m := providers.NewMockServer(
		providers.MockStatus(429),
		providers.MockOK(),
	)
	defer m.Close()

	conn := addMultiKeyConn(t, store, masterKey, "cf-multi", m,
		[]string{"key-alpha", "key-beta"}, []string{"mock-model"})
	addCombo(t, store, "Auto", db.ComboModel{ConnectionID: conn.ID, Model: "mock-model", Priority: 1})

	resp, body := postChat(t, ts, key,
		`{"model":"Auto","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 via key rotation, got %d: %s", resp.StatusCode, body)
	}

	recs := m.Records()
	if len(recs) != 2 {
		t.Fatalf("expected 2 upstream attempts (key1 429 → key2 ok), got %d", len(recs))
	}
	if recs[0].AuthHeader == recs[1].AuthHeader {
		t.Fatalf("retry must use a DIFFERENT key: %q == %q", recs[0].AuthHeader, recs[1].AuthHeader)
	}
	if !strings.Contains(recs[1].AuthHeader, "key-beta") {
		t.Fatalf("second attempt should use key-beta: %q", recs[1].AuthHeader)
	}
}

// Koneksi single-key TIDAK boleh retry saat 429 (tidak ada key lain) —
// langsung fallback ke connection berikutnya di combo.
func TestSingleKeyNoRetryFallsToNextConnection(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)
	m := providers.NewMockServer(providers.MockStatus(429)) // selalu 429
	defer m.Close()

	c1 := addMockConn(t, store, masterKey, "limited", m, []string{"mock-model"}, true)
	c2 := addMockConn(t, store, masterKey, "healthy", m, []string{"mock-model"}, true)
	addCombo(t, store, "Auto",
		db.ComboModel{ConnectionID: c1.ID, Model: "mock-model", Priority: 1},
		db.ComboModel{ConnectionID: c2.ID, Model: "mock-model", Priority: 2})

	resp, _ := postChat(t, ts, key,
		`{"model":"Auto","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode == 200 {
		t.Fatal("upstream always 429s; request must not succeed")
	}
	// Satu attempt per connection (single-key = tanpa retry).
	if n := len(m.Records()); n != 2 {
		t.Fatalf("expected exactly 2 attempts (one per connection), got %d", n)
	}
}

// Admin API: api_key multiline → satu connection multi-key dengan
// api_key_count yang benar.
func TestAdminMultiKeyConnection(t *testing.T) {
	s, _, _, _ := newTestAPISrv(t)
	h := s.AdminHandler()

	code, body := doReq(t, h, "POST", "/api/connections",
		`{"name":"cf-pool","provider":"cloudflare-ai","api_key":"k1\nk2\n\n k3 \n","models":["gpt"]}`, nil)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	if !strings.Contains(body, `"api_key_count":3`) {
		t.Fatalf("api_key_count must be 3: %s", body)
	}

	// Update dengan api_key kosong mempertahankan key lama.
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(body), &created)
	code, body = doReq(t, h, "PUT", "/api/connections/"+created.ID,
		`{"name":"cf-pool","provider":"cloudflare-ai","models":["gpt"]}`, nil)
	if code != 200 || !strings.Contains(body, `"api_key_count":3`) {
		t.Fatalf("update keep keys: %d %s", code, body)
	}
}

// /api/models (dashboard): model dikelompokkan per PROVIDER dan didedup —
// dua key pada provider sama tidak boleh menggandakan model.
func TestAdminModelsGroupedPerProviderDeduped(t *testing.T) {
	s, store, _, masterKey := newTestAPISrv(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()

	addMockConn(t, store, masterKey, "groq-1", m, []string{"a", "b"}, true)
	addMockConn(t, store, masterKey, "groq-2", m, []string{"b", "c"}, true)

	// Provider mock conn di atas adalah "custom"; buat satu conn provider lain.
	c2 := &db.Connection{Name: "or-1", Provider: "openrouter", IsActive: true,
		ModelsJSON: `["x","x"]`}
	if err := store.CreateConnection(context.Background(), c2); err != nil {
		t.Fatal(err)
	}

	code, body := doReq(t, s.AdminHandler(), "GET", "/api/models", "", nil)
	if code != 200 {
		t.Fatalf("models: %d %s", code, body)
	}
	var out struct {
		Providers map[string][]string `json:"providers"`
	}
	json.Unmarshal([]byte(body), &out)

	if len(out.Providers["custom"]) != 3 {
		t.Fatalf("custom must be deduped to [a b c]: %v", out.Providers["custom"])
	}
	if len(out.Providers["openrouter"]) != 1 {
		t.Fatalf("openrouter must be deduped to [x]: %v", out.Providers["openrouter"])
	}
}
