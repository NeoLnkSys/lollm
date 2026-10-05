package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lollm/lollm/internal/auth"
	"github.com/lollm/lollm/internal/backup"
	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/providers"
)

// TestAdminExportNoSecrets: the dashboard export omits provider keys.
func TestAdminExportNoSecrets(t *testing.T) {
	s, store, _, masterKey := newTestAPISrv(t)
	m := providers.NewMockServer(providers.MockOK())
	defer m.Close()
	_ = addMockConn(t, store, masterKey, "upstream", m, []string{"mock-model"}, true)
	code, body := doReq(t, s.AdminHandler(), "GET", "/api/export", "", nil)
	if code != 200 {
		t.Fatalf("export: %d %s", code, body)
	}
	if !strings.Contains(body, `"format": "lollm-backup"`) {
		t.Fatalf("not a backup file: %s", body[:200])
	}
	if strings.Contains(body, "test-upstream-key") || strings.Contains(body, `"api_key":"`) {
		t.Fatalf("dashboard export must not leak provider keys: %s", body[:400])
	}
	if !strings.Contains(body, `"name": "upstream"`) {
		t.Fatal("connection missing from export")
	}
}

// TestAdminImportAddsConnection: posting a backup with a new connection
// imports it (merge mode) and it becomes usable.
func TestAdminImportAddsConnection(t *testing.T) {
	s, store, key, _ := newTestAPISrv(t)
	_ = key

	in := `{
		"format":"lollm-backup","version":1,"product":"LoLLM Synapse","secrets":"plain",
		"connections":[{"name":"imported-conn","provider":"mock","api_key":"imported-key","is_active":true,"models":["m"]}],
		"combos":[{"name":"imported-combo","strategy":"health_aware","models":[{"connection":"imported-conn","model":"m","priority":1}]}],
		"api_keys":[{"name":"imported-key","key_hash":"` + auth.HashKey("lollm-imported") + `","key_prefix":"lollm-…","is_active":true}],
		"settings":{"compression.default":"partial"}
	}`
	code, body := doReq(t, s.AdminHandler(), "POST", "/api/import", in, nil)
	if code != 200 {
		t.Fatalf("import: %d %s", code, body)
	}
	var out struct {
		Stats backup.Stats `json:"stats"`
	}
	json.Unmarshal([]byte(body), &out)
	if out.Stats.Connections != 1 || out.Stats.Combos != 1 || out.Stats.APIKeys != 1 {
		t.Fatalf("stats: %+v", out.Stats)
	}

	// The imported combo resolves and the imported client key authenticates.
	if _, err := store.GetComboByName(t.Context(), "imported-combo"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetAPIKeyByHash(t.Context(), auth.HashKey("lollm-imported")); err != nil {
		t.Fatal("imported api key hash missing")
	}
	if v, _, _ := store.GetSetting(t.Context(), db.SettingCompressionDefault); v != "partial" {
		t.Fatalf("setting not imported: %q", v)
	}
}

// TestAdminImportRejectsGarbage: wrong format / invalid JSON / sealed
// without password.
func TestAdminImportRejectsGarbage(t *testing.T) {
	s, _, _, _ := newTestAPISrv(t)
	h := s.AdminHandler()

	if code, _ := doReq(t, h, "POST", "/api/import", `{not json`, nil); code != 400 {
		t.Fatalf("invalid JSON must 400, got %d", code)
	}
	if code, _ := doReq(t, h, "POST", "/api/import", `{"format":"other"}`, nil); code != 400 {
		t.Fatalf("wrong format must 400, got %d", code)
	}
	sealed := `{"format":"lollm-backup","version":1,"secrets":"pbkdf2","kdf":{"salt":"AAAA","iterations":1},"sealed_secrets":"x"}`
	if code, body := doReq(t, h, "POST", "/api/import", sealed, nil); code != 400 || !strings.Contains(body, "password") {
		t.Fatalf("sealed without password must 400 mentioning password: %d %s", code, body)
	}
}
