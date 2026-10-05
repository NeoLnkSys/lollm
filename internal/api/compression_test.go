package api

import (
	"context"
	"strings"
	"testing"

	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/providers"
)

// bigToolBody builds a tool-heavy request (the coding-agent shape).
func bigToolBody(nTools, chars int) string {
	var b strings.Builder
	b.WriteString(`{"model":"Auto","max_tokens":50,"messages":[`)
	b.WriteString(`{"role":"system","content":"You are a coding agent."},`)
	for i := 0; i < nTools; i++ {
		tool := strings.Repeat("x", chars)
		b.WriteString(`{"role":"tool","tool_call_id":"c` + itoa(i) + `","content":"` + tool + `"},`)
	}
	b.WriteString(`{"role":"user","content":"summarize"}]}`)
	return b.String()
}

func itoa(i int) string {
	return string(rune('0' + i))
}

// firstToolContent extracts the first tool message content received by the mock.
func firstToolContent(t *testing.T, m *providers.MockServer) (string, bool) {
	t.Helper()
	for _, rec := range m.Records() {
		msgs, ok := rec.Body["messages"].([]any)
		if !ok {
			continue
		}
		for _, msg := range msgs {
			mm, ok := msg.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := mm["role"].(string); role == "tool" {
				s, _ := mm["content"].(string)
				return s, true
			}
		}
	}
	return "", false
}

func TestCompressionHeaderAppliesFull(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()

	conn := addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	addCombo(t, store, "Auto", db.ComboModel{ConnectionID: conn.ID, Model: "mock-model", Priority: 1})

	resp, body := postChat(t, ts, key, bigToolBody(5, 4000),
		map[string]string{"X-LoLLM-Compression": "full"})
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	// The upstream must have received the summarized (short) first tool result.
	got, ok := firstToolContent(t, m)
	if !ok {
		t.Fatal("no tool message reached the upstream")
	}
	if len(got) > 200 {
		t.Fatalf("full mode must summarize old tool results upstream, got %d chars", len(got))
	}

	// The saving must be recorded in the usage log.
	logs, _ := store.ListUsageLogs(context.Background(), 10)
	var saved int
	for _, l := range logs {
		if l.StatusCode == 200 {
			saved += l.TokensSaved
		}
	}
	if saved <= 0 {
		t.Fatal("tokens_saved must be recorded on success rows")
	}
}

func TestCompressionDefaultFromSetting(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()

	conn := addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	addCombo(t, store, "Auto", db.ComboModel{ConnectionID: conn.ID, Model: "mock-model", Priority: 1})

	// Global default = partial (settings table, editable from the dashboard).
	if err := store.SetSetting(context.Background(), db.SettingCompressionDefault, "partial"); err != nil {
		t.Fatal(err)
	}
	defer store.SetSetting(context.Background(), db.SettingCompressionDefault, "off")

	resp, body := postChat(t, ts, key, bigToolBody(4, 5000), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	got, ok := firstToolContent(t, m)
	if !ok || len(got) > 1400 {
		t.Fatalf("setting-driven partial compression not applied: %d chars", len(got))
	}
}

func TestCompressionComboLevelAndHeaderOverride(t *testing.T) {
	ts, store, key, masterKey := newTestAPI(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()

	conn := addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	combo := addCombo(t, store, "Auto", db.ComboModel{ConnectionID: conn.ID, Model: "mock-model", Priority: 1})
	combo.Compression = "full"
	if err := store.UpdateCombo(context.Background(), combo); err != nil {
		t.Fatal(err)
	}

	// Combo-level full applies without any header.
	resp, body := postChat(t, ts, key, bigToolBody(5, 4000), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if got, ok := firstToolContent(t, m); !ok || len(got) > 200 {
		t.Fatalf("combo-level compression not applied: %d chars", len(got))
	}

	// Header off overrides the combo back to passthrough.
	m.Reset()
	resp, body = postChat(t, ts, key, bigToolBody(5, 4000),
		map[string]string{"X-LoLLM-Compression": "off"})
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if got, ok := firstToolContent(t, m); !ok || len(got) != 4000 {
		t.Fatalf("header off must override combo compression: %d chars", len(got))
	}
}
