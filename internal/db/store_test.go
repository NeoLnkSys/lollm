package db

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/lollm/lollm/internal/secret"
)

func newTestStore(t *testing.T) (*Store, []byte) {
	t.Helper()
	dir := t.TempDir()
	d, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := Migrate(d); err != nil {
		t.Fatal(err)
	}
	masterKey, err := secret.LoadOrCreateMasterKey(filepath.Join(dir, ".master.key"))
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(d), masterKey
}

func TestMigrateCreatesTables(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	rows, err := store.DB().QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables[name] = true
	}
	for _, want := range []string{"connections", "combos", "proxy_pools", "api_keys",
		"settings", "usage_logs", "agent_configs", "schema_migrations"} {
		if !tables[want] {
			t.Fatalf("table %s missing (have %v)", want, tables)
		}
	}
}

func TestMigrateIdempotent(t *testing.T) {
	store, _ := newTestStore(t)
	if err := Migrate(store.DB()); err != nil {
		t.Fatalf("second Migrate call must be a no-op: %v", err)
	}
}

func TestSeedIfEmpty(t *testing.T) {
	store, masterKey := newTestStore(t)
	ctx := context.Background()

	if err := SeedIfEmpty(ctx, store, masterKey); err != nil {
		t.Fatal(err)
	}
	conns, err := store.ListConnections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(conns) != 2 {
		t.Fatalf("expected 2 demo connections, got %d", len(conns))
	}
	for _, c := range conns {
		if c.IsActive {
			t.Fatalf("demo connection %s must be inactive", c.Name)
		}
		if c.ModelsJSON == "" || len(c.Models()) == 0 {
			t.Fatalf("demo connection %s must ship a model catalog", c.Name)
		}
	}

	combo, err := store.GetComboByName(ctx, "Auto")
	if err != nil {
		t.Fatal(err)
	}
	if combo.Strategy != StrategyHealthAware || len(combo.Models) != 3 {
		t.Fatalf("unexpected Auto combo: %+v", combo)
	}

	if _, err := store.DefaultAgentConfig(ctx); err != nil {
		t.Fatalf("default agent config missing: %v", err)
	}
	if v, ok, _ := store.GetSetting(ctx, SettingCompressionDefault); !ok || v != "off" {
		t.Fatalf("default compression setting missing: %q", v)
	}

	// Idempotent: seeding again must not duplicate.
	if err := SeedIfEmpty(ctx, store, masterKey); err != nil {
		t.Fatal(err)
	}
	conns2, _ := store.ListConnections(ctx)
	if len(conns2) != 2 {
		t.Fatalf("seed not idempotent: %d connections", len(conns2))
	}
}

func TestConnectionLifecycle(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	c := &Connection{
		Name: "test-openrouter", Provider: "openrouter",
		APIKeyEncrypted: "enc-placeholder", Priority: 5, Weight: 2,
		IsActive: true, ModelsJSON: `["m1"]`,
	}
	if err := store.CreateConnection(ctx, c); err != nil {
		t.Fatal(err)
	}
	if len(c.ID) == 0 || c.CreatedAt.IsZero() {
		t.Fatal("Create must fill ID and timestamps")
	}

	got, err := store.GetConnection(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "test-openrouter" || got.Priority != 5 || got.Weight != 2 ||
		!got.IsActive || got.Status != StatusActive {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if len(got.Models()) != 1 || got.Models()[0] != "m1" {
		t.Fatalf("models mismatch: %v", got.Models())
	}

	got.Name = "renamed"
	got.IsActive = false
	if err := store.UpdateConnection(ctx, got); err != nil {
		t.Fatal(err)
	}
	got2, _ := store.GetConnection(ctx, c.ID)
	if got2.Name != "renamed" || got2.IsActive {
		t.Fatalf("update not persisted: %+v", got2)
	}

	if err := store.DeleteConnection(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetConnection(ctx, c.ID); err == nil {
		t.Fatal("deleted connection must not be found")
	}
}

func TestMarkConnectionResult(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	c := &Connection{Name: "c", Provider: "groq", APIKeyEncrypted: "x", IsActive: true}
	store.CreateConnection(ctx, c)

	// Success: EMA seeds at the first latency.
	if err := store.MarkConnectionResult(ctx, c.ID, true, "", nil, 100); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetConnection(ctx, c.ID)
	if got.Status != StatusActive || got.ConsecutiveErrors != 0 || got.BackoffUntil != nil {
		t.Fatalf("success must reset health: %+v", got)
	}
	if math.Abs(got.LatencyEMAMs-100) > 0.001 {
		t.Fatalf("EMA seed: got %v", got.LatencyEMAMs)
	}

	// Second success: EMA smoothing 0.7/0.3 -> 130.
	store.MarkConnectionResult(ctx, c.ID, true, "", nil, 200)
	got, _ = store.GetConnection(ctx, c.ID)
	if math.Abs(got.LatencyEMAMs-130) > 0.001 {
		t.Fatalf("EMA smoothing: got %v", got.LatencyEMAMs)
	}

	// Failure: streak + status + backoff.
	backoff := time.Now().UTC().Add(30 * time.Second)
	if err := store.MarkConnectionResult(ctx, c.ID, false, StatusRateLimited, &backoff, 0); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetConnection(ctx, c.ID)
	if got.ConsecutiveErrors != 1 || got.Status != StatusRateLimited {
		t.Fatalf("failure must bump streak and status: %+v", got)
	}
	if got.BackoffUntil == nil || got.BackoffUntil.Before(time.Now().UTC()) {
		t.Fatalf("backoff must be in the future: %v", got.BackoffUntil)
	}

	// Recovery: success resets everything.
	store.MarkConnectionResult(ctx, c.ID, true, "", nil, 50)
	got, _ = store.GetConnection(ctx, c.ID)
	if got.Status != StatusActive || got.ConsecutiveErrors != 0 || got.BackoffUntil != nil {
		t.Fatalf("recovery must reset: %+v", got)
	}
}

func TestComboCRUD(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	conn := &Connection{Name: "c", Provider: "openrouter", APIKeyEncrypted: "x", IsActive: true}
	store.CreateConnection(ctx, conn)

	combo := &Combo{
		Name:     "Code",
		Strategy: StrategySequential,
		Models: []ComboModel{
			{ConnectionID: conn.ID, Model: "anthropic/claude-sonnet-4", Priority: 1},
			{ConnectionID: conn.ID, Model: "google/gemini-2.5-flash", Priority: 2},
		},
	}
	if err := store.CreateCombo(ctx, combo); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetComboByName(ctx, "Code")
	if err != nil {
		t.Fatal(err)
	}
	if got.Strategy != StrategySequential || len(got.Models) != 2 || got.Models[0].Model != "anthropic/claude-sonnet-4" {
		t.Fatalf("combo roundtrip mismatch: %+v", got)
	}

	got.Strategy = StrategyRoundRobin
	if err := store.UpdateCombo(ctx, got); err != nil {
		t.Fatal(err)
	}
	got2, _ := store.GetCombo(ctx, got.ID)
	if got2.Strategy != StrategyRoundRobin {
		t.Fatal("combo update not persisted")
	}

	// Duplicate names must fail (UNIQUE).
	dup := &Combo{Name: "Code", Models: []ComboModel{{ConnectionID: conn.ID, Model: "m"}}}
	if err := store.CreateCombo(ctx, dup); err == nil {
		t.Fatal("duplicate combo name must be rejected")
	}

	if err := store.DeleteCombo(ctx, got.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetComboByName(ctx, "Code"); err == nil {
		t.Fatal("deleted combo must not be found")
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	k := &APIKey{Name: "cursor", KeyHash: "abc123", KeyPrefix: "lollm-Ab3…", IsActive: true}
	if err := store.CreateAPIKey(ctx, k); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetAPIKeyByHash(ctx, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "cursor" || !got.IsActive || got.LastUsedAt != nil {
		t.Fatalf("key roundtrip mismatch: %+v", got)
	}

	if err := store.TouchAPIKeyUsed(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetAPIKey(ctx, k.ID)
	if got.LastUsedAt == nil {
		t.Fatal("last_used_at must be stamped")
	}

	n, err := store.CountActiveAPIKeys(ctx)
	if err != nil || n != 1 {
		t.Fatalf("active key count = %d, %v", n, err)
	}
	if err := store.SetAPIKeyActive(ctx, k.ID, false); err != nil {
		t.Fatal(err)
	}
	if n, _ = store.CountActiveAPIKeys(ctx); n != 0 {
		t.Fatalf("revoked key still counted: %d", n)
	}

	// Duplicate hash must fail (UNIQUE).
	dup := &APIKey{Name: "dup", KeyHash: "abc123", IsActive: true}
	if err := store.CreateAPIKey(ctx, dup); err == nil {
		t.Fatal("duplicate key hash must be rejected")
	}
}

func TestSettings(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if _, ok, _ := store.GetSetting(ctx, "k"); ok {
		t.Fatal("unset key must report not-found")
	}
	if err := store.SetSetting(ctx, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSetting(ctx, "k", "v2"); err != nil { // upsert
		t.Fatal(err)
	}
	if v, ok, _ := store.GetSetting(ctx, "k"); !ok || v != "v2" {
		t.Fatalf("upsert failed: %q %v", v, ok)
	}
	all, err := store.AllSettings(ctx)
	if err != nil || all["k"] != "v2" {
		t.Fatalf("AllSettings: %v %v", all, err)
	}
}

func TestUsageLogs(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		l := &UsageLog{
			RequestID: "req-1", ComboName: "Auto", Model: "m",
			PromptTokens: 100, CompletionTokens: 50, LatencyMs: 200 + i, StatusCode: 200,
		}
		if err := store.InsertUsageLog(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	errLog := &UsageLog{RequestID: "req-2", StatusCode: 429, Error: "rate limited"}
	if err := store.InsertUsageLog(ctx, errLog); err != nil {
		t.Fatal(err)
	}

	logs, err := store.ListUsageLogs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 4 {
		t.Fatalf("expected 4 logs, got %d", len(logs))
	}
	// Newest first.
	if logs[0].RequestID != "req-2" || logs[0].Error != "rate limited" {
		t.Fatalf("ordering wrong: %+v", logs[0])
	}
	if logs[3].LatencyMs != 200 {
		t.Fatalf("oldest log should have latency 200: %+v", logs[3])
	}
}

func TestAgentConfigs(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	a := &AgentConfig{
		Name: "debate-pipeline",
		Mode: AgentModeDebate,
		Roles: []AgentRole{
			{Name: "pro", Model: "Auto", SystemPrompt: "argue for"},
			{Name: "con", Model: "Auto", SystemPrompt: "argue against"},
			{Name: "judge", Model: "Auto", SystemPrompt: "pick the winner"},
		},
		MaxRounds:         3,
		HideInternalSteps: false,
	}
	if err := store.CreateAgentConfig(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetAgentConfig(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != AgentModeDebate || len(got.Roles) != 3 || got.MaxRounds != 3 || got.HideInternalSteps {
		t.Fatalf("agent config roundtrip mismatch: %+v", got)
	}

	got.Roles = got.Roles[:2]
	if err := store.UpdateAgentConfig(ctx, got); err != nil {
		t.Fatal(err)
	}
	got2, _ := store.GetAgentConfig(ctx, a.ID)
	if len(got2.Roles) != 2 {
		t.Fatal("agent config update not persisted")
	}

	if err := store.DeleteAgentConfig(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetAgentConfig(ctx, a.ID); err == nil {
		t.Fatal("deleted agent config must not be found")
	}
}

func TestProxyPools(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	p := &ProxyPool{Name: "us-rotating", Proxies: []string{"http://p1:8080", "socks5://p2:1080"}}
	if err := store.CreateProxyPool(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetProxyPool(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Proxies) != 2 || got.Proxies[1] != "socks5://p2:1080" {
		t.Fatalf("pool roundtrip mismatch: %+v", got)
	}

	// Connection referencing the pool keeps working across delete (FK SET NULL).
	conn := &Connection{Name: "c", Provider: "openrouter", APIKeyEncrypted: "x",
		IsActive: true, ProxyPoolID: p.ID}
	store.CreateConnection(ctx, conn)
	if err := store.DeleteProxyPool(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	gotConn, err := store.GetConnection(ctx, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotConn.ProxyPoolID != "" {
		t.Fatalf("proxy_pool_id must be NULLed after pool delete, got %q", gotConn.ProxyPoolID)
	}
}
