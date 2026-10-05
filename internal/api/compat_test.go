package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/providers"
)

// Grok CLI mengirim field ekstra (mis. "model_id") di pesan assistant;
// provider ketat menolaknya (422 extra_forbidden). sanitizeMessages harus
// membuang field non-standar dan mempertahankan field inti.
func TestSanitizeMessagesStripsExtraFields(t *testing.T) {
	body := map[string]any{
		"model": "Auto",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "ok", "model_id": "grok-4", "reasoning": "hmm"},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "result", "cache": true},
		},
	}
	sanitizeMessages(body)

	b, _ := json.Marshal(body["messages"])
	out := string(b)
	for _, banned := range []string{"model_id", "reasoning", "cache"} {
		if strings.Contains(out, banned) {
			t.Fatalf("field ekstra %q lolos: %s", banned, out)
		}
	}
	for _, want := range []string{"tool_call_id", `"content":"ok"`, `"role":"tool"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("field inti %q hilang: %s", want, out)
		}
	}
}

// Grok CLI juga mengirim field vendor level-atas (mis. "search_parameters"
// milik xAI); upstream Google/FastAPI menolaknya 400 "Cannot find field".
// sanitizeBody harus membuangnya dan mempertahankan field standar.
func TestSanitizeBodyStripsVendorFields(t *testing.T) {
	body := map[string]any{
		"model":                 "Auto",
		"messages":              []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":                false,
		"search_parameters":     map[string]any{"mode": "auto", "return_citations": true},
		"temperature":           0.7,
		"max_completion_tokens": 1024.0,
		"x_custom":              "boo",
	}
	sanitizeBody(body)

	if _, bad := body["search_parameters"]; bad {
		t.Fatal("field search_parameters lolos")
	}
	if _, bad := body["x_custom"]; bad {
		t.Fatal("field x_custom lolos")
	}
	for _, want := range []string{"model", "messages", "stream", "temperature", "max_completion_tokens"} {
		if _, ok := body[want]; !ok {
			t.Fatalf("field standar %q ikut terbuang", want)
		}
	}
}

// End-to-end: payload kotor dari klien tetap diterima upstream bersih.
func TestChatSanitizesBeforeUpstream(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()

	conn := addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	addCombo(t, store, "Auto", db.ComboModel{ConnectionID: conn.ID, Model: "mock-model", Priority: 1})

	resp, body := postChat(t, ts, key, `{"model":"Auto","max_tokens":50,"search_parameters":{"mode":"on"},"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"prev","model_id":"grok-4"},
		{"role":"user","content":"again"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	// upstream harus menerima body tanpa field vendor / model_id
	for _, rec := range m.Records() {
		if _, bad := rec.Body["search_parameters"]; bad {
			t.Fatal("field search_parameters sampai ke upstream")
		}
		if msgs, ok := rec.Body["messages"].([]any); ok {
			for _, mm := range msgs {
				if m2, ok := mm.(map[string]any); ok {
					if _, bad := m2["model_id"]; bad {
						t.Fatal("field model_id sampai ke upstream")
					}
				}
			}
		}
		if _, ok := rec.Body["max_tokens"]; !ok {
			t.Fatal("field standar max_tokens hilang dari payload upstream")
		}
	}
}
