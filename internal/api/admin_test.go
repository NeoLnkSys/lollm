package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/providers"
)

// doReq performs a JSON request against a handler, returning status + body.
func doReq(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestAdminConnectionCRUD(t *testing.T) {
	s, store, _, _ := newTestAPISrv(t)
	h := s.AdminHandler()

	// Create.
	code, body := doReq(t, h, "POST", "/api/connections",
		`{"name":"or-main","provider":"openrouter","api_key":"sk-live-xyz","models":["a","b"],"priority":2}`, nil)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(body), &created)
	if created.ID == "" {
		t.Fatal("no id")
	}
	if strings.Contains(body, "sk-live-xyz") || strings.Contains(body, "api_key_encrypted") {
		t.Fatalf("key material leaked in response: %s", body)
	}

	// List shows the key as set, never the ciphertext.
	code, body = doReq(t, h, "GET", "/api/connections", "", nil)
	if code != 200 || !strings.Contains(body, `"api_key_set":true`) {
		t.Fatalf("list: %d %s", code, body)
	}

	// Update with an empty key keeps the old ciphertext.
	stored, _ := store.GetConnection(context.Background(), created.ID)
	if stored == nil || stored.APIKeyEncrypted == "" {
		t.Fatal("stored connection missing ciphertext")
	}
	code, body = doReq(t, h, "PUT", "/api/connections/"+created.ID,
		`{"name":"or-main-2","provider":"openrouter","models":["a"]}`, nil)
	if code != 200 {
		t.Fatalf("update: %d %s", code, body)
	}
	after, _ := store.GetConnection(context.Background(), created.ID)
	if after.APIKeyEncrypted != stored.APIKeyEncrypted {
		t.Fatal("update with empty api_key must keep the old ciphertext")
	}
	if after.Name != "or-main-2" {
		t.Fatalf("name not updated: %s", after.Name)
	}

	// Delete.
	code, _ = doReq(t, h, "DELETE", "/api/connections/"+created.ID, "", nil)
	if code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if conns, _ := store.ListConnections(context.Background()); len(conns) != 0 {
		t.Fatal("connection still present")
	}
}

func TestAdminConnectionTestProbe(t *testing.T) {
	s, store, _, masterKey := newTestAPISrv(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()

	conn := addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	code, body := doReq(t, s.AdminHandler(), "POST", "/api/connections/"+conn.ID+"/test", "", nil)
	if code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("probe: %d %s", code, body)
	}
}

func TestAdminComboCRUD(t *testing.T) {
	s, store, _, _ := newTestAPISrv(t)
	h := s.AdminHandler()

	code, body := doReq(t, h, "POST", "/api/combos",
		`{"name":"fast","strategy":"nonsense","compression":"partial","models":[{"connection_id":"c1","model":"m","priority":1}]}`, nil)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var c struct {
		ID       string `json:"id"`
		Strategy string `json:"strategy"`
	}
	json.Unmarshal([]byte(body), &c)
	if c.Strategy != db.StrategyHealthAware {
		t.Fatalf("invalid strategy must normalize to health_aware, got %q", c.Strategy)
	}

	// Reject a combo without models.
	code, _ = doReq(t, h, "POST", "/api/combos", `{"name":"empty","models":[]}`, nil)
	if code != 400 {
		t.Fatalf("combo without models must 400, got %d", code)
	}

	code, _ = doReq(t, h, "DELETE", "/api/combos/"+c.ID, "", nil)
	if code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if combos, _ := store.ListCombos(context.Background()); len(combos) != 0 {
		t.Fatal("combo still present")
	}
}

func TestAdminAPIKeyLifecycle(t *testing.T) {
	s, store, _, masterKey := newTestAPISrv(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()
	conn := addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	addCombo(t, store, "Auto", db.ComboModel{ConnectionID: conn.ID, Model: "mock-model", Priority: 1})

	// Generate via dashboard.
	code, body := doReq(t, s.AdminHandler(), "POST", "/api/api-keys", `{"name":"playground"}`, nil)
	if code != 201 {
		t.Fatalf("create key: %d %s", code, body)
	}
	var out struct {
		Key string `json:"key"`
	}
	json.Unmarshal([]byte(body), &out)
	if !strings.HasPrefix(out.Key, "lollm-") {
		t.Fatalf("generated key must be lollm-…, got %q", out.Key)
	}

	// The fresh key must authenticate against the real API.
	apiTS := httptest.NewServer(s.Handler())
	defer apiTS.Close()
	req, _ := http.NewRequest("POST", apiTS.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"Auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+out.Key)
	resp, err := apiTS.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("new key must work on /v1: %d %s", resp.StatusCode, b)
	}

	// Revoke → 401.
	keys, _ := store.ListAPIKeys(context.Background())
	var id string
	for _, k := range keys {
		if k.Name == "playground" {
			id = k.ID
		}
	}
	if code, _ = doReq(t, s.AdminHandler(), "PATCH", "/api/api-keys/"+id, `{"is_active":false}`, nil); code != 200 {
		t.Fatalf("patch: %d", code)
	}
	req2, _ := http.NewRequest("POST", apiTS.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"Auto","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+out.Key)
	resp2, _ := apiTS.Client().Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("revoked key must 401, got %d", resp2.StatusCode)
	}

	// Delete.
	if code, _ = doReq(t, s.AdminHandler(), "DELETE", "/api/api-keys/"+id, "", nil); code != 200 {
		t.Fatalf("delete key: %d", code)
	}
}

func TestAdminSettingsValidation(t *testing.T) {
	s, _, _, _ := newTestAPISrv(t)
	h := s.AdminHandler()

	if code, _ := doReq(t, h, "PUT", "/api/settings", `{"compression.default":"bogus"}`, nil); code != 400 {
		t.Fatalf("bogus compression must 400, got %d", code)
	}
	if code, _ := doReq(t, h, "PUT", "/api/settings", `{"dashboard.admin_token_hash":"hax"}`, nil); code != 400 {
		t.Fatalf("non-whitelisted setting must 400, got %d", code)
	}
	if code, _ := doReq(t, h, "PUT", "/api/settings",
		`{"compression.default":"full","health_check.enabled":"true","health_check.interval_seconds":"30"}`, nil); code != 200 {
		t.Fatalf("valid settings must 200, got %d", code)
	}
	code, body := doReq(t, h, "GET", "/api/settings", "", nil)
	if code != 200 || !strings.Contains(body, `"compression.default":"full"`) {
		t.Fatalf("get settings: %d %s", code, body)
	}
}

func TestAdminDashboardToken(t *testing.T) {
	s, _, _, _ := newTestAPISrv(t)
	h := s.AdminHandler()

	// No token yet: open access.
	if code, _ := doReq(t, h, "GET", "/api/version", "", nil); code != 200 {
		t.Fatalf("open access failed: %d", code)
	}

	// Generate a token.
	code, body := doReq(t, h, "POST", "/api/dashboard-token", `{"action":"generate"}`, nil)
	if code != 200 {
		t.Fatalf("generate: %d %s", code, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	json.Unmarshal([]byte(body), &out)

	// Without the token → 401 with machine-readable code.
	code, body = doReq(t, h, "GET", "/api/version", "", nil)
	if code != 401 || !strings.Contains(body, "dashboard_token_required") {
		t.Fatalf("expected 401 dashboard_token_required, got %d %s", code, body)
	}
	// With the token → OK.
	code, _ = doReq(t, h, "GET", "/api/version", "", map[string]string{"X-LoLLM-Token": out.Token})
	if code != 200 {
		t.Fatalf("token access failed: %d", code)
	}
	// Bearer form also works.
	code, _ = doReq(t, h, "GET", "/api/version", "",
		map[string]string{"Authorization": "Bearer " + out.Token})
	if code != 200 {
		t.Fatalf("bearer token access failed: %d", code)
	}

	// Clear → open again.
	code, _ = doReq(t, h, "POST", "/api/dashboard-token", `{"action":"clear"}`,
		map[string]string{"X-LoLLM-Token": out.Token})
	if code != 200 {
		t.Fatalf("clear: %d", code)
	}
	if code, _ = doReq(t, h, "GET", "/api/version", "", nil); code != 200 {
		t.Fatalf("open access after clear failed: %d", code)
	}
}

func TestAdminOverviewAndUsage(t *testing.T) {
	s, store, key, _ := newTestAPISrv(t)
	if err := store.InsertUsageLog(context.Background(), &db.UsageLog{
		RequestID: "r1", APIKeyID: key, ComboName: "Auto", Model: "m",
		PromptTokens: 10, CompletionTokens: 5, TokensSaved: 7, LatencyMs: 100, StatusCode: 200,
	}); err != nil {
		t.Fatal(err)
	}
	h := s.AdminHandler()
	code, body := doReq(t, h, "GET", "/api/overview", "", nil)
	if code != 200 || !strings.Contains(body, `"requests_24h":1`) || !strings.Contains(body, `"tokens_saved":7`) {
		t.Fatalf("overview: %d %s", code, body)
	}
	code, body = doReq(t, h, "GET", "/api/usage?limit=10", "", nil)
	if code != 200 || !strings.Contains(body, `"combo_name":"Auto"`) {
		t.Fatalf("usage: %d %s", code, body)
	}
}

func TestAdminModelsEndpoint(t *testing.T) {
	s, store, _, masterKey := newTestAPISrv(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()
	conn := addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	addCombo(t, store, "Auto", db.ComboModel{ConnectionID: conn.ID, Model: "mock-model", Priority: 1})

	code, body := doReq(t, s.AdminHandler(), "GET", "/api/models", "", nil)
	if code != 200 {
		t.Fatalf("models: %d %s", code, body)
	}
	for _, want := range []string{`"Auto"`, "mock-model"} {
		if !strings.Contains(body, want) {
			t.Fatalf("models response missing %s: %s", want, body)
		}
	}
}

// Auth sanity: the admin handler must not be reachable on the public API mux.
func TestAdminNotOnPublicAPI(t *testing.T) {
	s, _, _, _ := newTestAPISrv(t)
	api := httptest.NewServer(s.Handler())
	defer api.Close()
	resp, err := api.Client().Get(api.URL + "/api/version")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("admin API must not be mounted on the public API port, got %d", resp.StatusCode)
	}
}
