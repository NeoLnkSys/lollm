package backup

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lollm/lollm/internal/auth"
	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/secret"
)

// newStore opens a migrated store with its own fresh master key.
func newStore(t *testing.T) (*db.Store, []byte) {
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
	key, err := secret.LoadOrCreateMasterKey(filepath.Join(dir, ".master.key"))
	if err != nil {
		t.Fatal(err)
	}
	return db.NewStore(d), key
}

// seedSource fills a store with one of everything, returning the plaintext
// provider key for verification.
func seedSource(t *testing.T, store *db.Store, masterKey []byte) string {
	t.Helper()
	ctx := context.Background()

	pool := &db.ProxyPool{Name: "relays", Proxies: []string{"socks5://127.0.0.1:1080", "http://127.0.0.1:8080"}}
	if err := store.CreateProxyPool(ctx, pool); err != nil {
		t.Fatal(err)
	}

	enc, err := secret.EncryptString(masterKey, "sk-live-secret-1")
	if err != nil {
		t.Fatal(err)
	}
	conn := &db.Connection{
		Name: "or-main", Provider: "openrouter", APIKeyEncrypted: enc,
		Priority: 1, Weight: 2, IsActive: true, ProxyPoolID: pool.ID,
		ModelsJSON: `["m1","m2"]`,
	}
	if err := store.CreateConnection(ctx, conn); err != nil {
		t.Fatal(err)
	}
	enc2, _ := secret.EncryptString(masterKey, "groq-key-2")
	conn2 := &db.Connection{
		Name: "gq-1", Provider: "groq", APIKeyEncrypted: enc2,
		Priority: 2, Weight: 1, IsActive: true,
	}
	if err := store.CreateConnection(ctx, conn2); err != nil {
		t.Fatal(err)
	}

	combo := &db.Combo{
		Name: "Auto", Strategy: db.StrategyHealthAware, Compression: "partial",
		Models: []db.ComboModel{{ConnectionID: conn.ID, Model: "m1", Priority: 1}},
	}
	if err := store.CreateCombo(ctx, combo); err != nil {
		t.Fatal(err)
	}

	cfg := &db.AgentConfig{
		Name: "default", Mode: db.AgentModeCollaborative, MaxRounds: 2,
		HideInternalSteps: true,
		Roles:             []db.AgentRole{{Name: "planner", Model: "Auto", SystemPrompt: "plan it"}},
	}
	if err := store.CreateAgentConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	plainKey := "lollm-roundtrip-key"
	if err := store.CreateAPIKey(ctx, &db.APIKey{
		Name: "test", KeyHash: auth.HashKey(plainKey), KeyPrefix: auth.DisplayPrefix(plainKey), IsActive: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSetting(ctx, db.SettingCompressionDefault, "full"); err != nil {
		t.Fatal(err)
	}
	return plainKey
}

// verifyRestored asserts the restored database matches the source seed.
func verifyRestored(t *testing.T, store *db.Store, masterKey []byte, clientKey string) {
	t.Helper()
	ctx := context.Background()

	conns, err := store.ListConnections(ctx)
	if err != nil || len(conns) != 2 {
		t.Fatalf("expected 2 connections, got %d (%v)", len(conns), err)
	}
	byName := map[string]*db.Connection{}
	for _, c := range conns {
		byName[c.Name] = c
	}
	// Keys must be re-encrypted with the NEW master key and still decrypt.
	got, err := secret.DecryptString(masterKey, byName["or-main"].APIKeyEncrypted)
	if err != nil || got != "sk-live-secret-1" {
		t.Fatalf("provider key mismatch after re-encryption: %q (%v)", got, err)
	}
	pools, _ := store.ListProxyPools(ctx)
	if len(pools) != 1 || pools[0].Name != "relays" || len(pools[0].Proxies) != 2 {
		t.Fatalf("pools not restored: %+v", pools)
	}
	if byName["or-main"].ProxyPoolID != pools[0].ID {
		t.Fatal("connection not re-linked to the restored pool")
	}

	combo, err := store.GetComboByName(ctx, "Auto")
	if err != nil {
		t.Fatal(err)
	}
	if len(combo.Models) != 1 || combo.Models[0].ConnectionID != byName["or-main"].ID {
		t.Fatalf("combo not re-linked by name: %+v", combo.Models)
	}
	if combo.Compression != "partial" {
		t.Fatalf("combo compression lost: %q", combo.Compression)
	}

	cfgs, _ := store.ListAgentConfigs(ctx)
	if len(cfgs) != 1 || len(cfgs[0].Roles) != 1 || cfgs[0].Roles[0].SystemPrompt != "plan it" {
		t.Fatalf("agent config not restored: %+v", cfgs)
	}

	// The pre-existing client key must keep authenticating (hash preserved).
	if _, err := store.GetAPIKeyByHash(ctx, auth.HashKey(clientKey)); err != nil {
		t.Fatal("client API key hash lost — existing keys would stop working")
	}

	if v, _, _ := store.GetSetting(ctx, db.SettingCompressionDefault); v != "full" {
		t.Fatalf("setting not restored: %q", v)
	}
}

// TestRoundtripPlainSecrets: export with plaintext secrets → wipe → import
// into a DIFFERENT database (different master key) → identical.
func TestRoundtripPlainSecrets(t *testing.T) {
	src, srcKey := newStore(t)
	seedSource(t, src, srcKey)

	b, err := Build(context.Background(), src, srcKey, Options{IncludeSecrets: true})
	if err != nil {
		t.Fatal(err)
	}
	if b.Secrets != SecretsPlain || b.Connections[0].APIKey != "sk-live-secret-1" {
		t.Fatalf("plain export must embed keys: mode=%s", b.Secrets)
	}

	dst, dstKey := newStore(t) // fresh DB, different master key
	stats, err := Restore(context.Background(), dst, dstKey, b, RestoreOptions{Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Connections != 2 || stats.Combos != 1 || stats.ProxyPools != 1 || stats.APIKeys != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	verifyRestored(t, dst, dstKey, "lollm-roundtrip-key")
}

// TestRoundtripPasswordSealed: PBKDF2+AES-GCM sealed secrets; wrong password
// fails, right password restores.
func TestRoundtripPasswordSealed(t *testing.T) {
	src, srcKey := newStore(t)
	seedSource(t, src, srcKey)

	b, err := Build(context.Background(), src, srcKey, Options{IncludeSecrets: true, Password: "hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Secrets != SecretsPBKDF2 || b.SealedBlob == "" || b.KDF == nil {
		t.Fatalf("expected sealed backup: %+v", b.KDF)
	}
	if b.Connections[0].APIKey != "" {
		t.Fatal("sealed backup must not carry plaintext keys")
	}

	dst, dstKey := newStore(t)
	// Wrong password must fail cleanly.
	if err := b.DecryptSecrets("wrong"); err == nil {
		t.Fatal("wrong password must fail")
	}
	// Restoring a still-sealed backup must be refused.
	if _, err := Restore(context.Background(), dst, dstKey, b, RestoreOptions{}); err == nil {
		t.Fatal("restore of sealed backup must be refused")
	}
	// Right password works.
	if err := b.DecryptSecrets("hunter2"); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), dst, dstKey, b, RestoreOptions{Replace: true}); err != nil {
		t.Fatal(err)
	}
	verifyRestored(t, dst, dstKey, "lollm-roundtrip-key")
}

// TestNoSecretsExport omits provider keys entirely (shareable backup).
func TestNoSecretsExport(t *testing.T) {
	src, srcKey := newStore(t)
	seedSource(t, src, srcKey)

	b, err := Build(context.Background(), src, srcKey, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if b.Secrets != SecretsNone {
		t.Fatalf("mode: %s", b.Secrets)
	}
	for _, c := range b.Connections {
		if c.APIKey != "" {
			t.Fatalf("no-secrets backup leaked a key: %s", c.Name)
		}
	}

	dst, dstKey := newStore(t)
	if _, err := Restore(context.Background(), dst, dstKey, b, RestoreOptions{Replace: true}); err != nil {
		t.Fatal(err)
	}
	// Everything except provider keys survives.
	conns, _ := dst.ListConnections(context.Background())
	if len(conns) != 2 || conns[0].APIKeyEncrypted != "" {
		t.Fatalf("no-secrets restore wrong: %+v", conns[0])
	}
	if _, err := dst.GetComboByName(context.Background(), "Auto"); err != nil {
		t.Fatal(err)
	}
}

// TestRestoreRejectsForeignFile: garbage JSON with the wrong format marker.
func TestRestoreRejectsForeignFile(t *testing.T) {
	_, key := newStore(t)
	b := &Backup{Format: "something-else", Secrets: "none"}
	if _, err := Restore(context.Background(), nil, key, b, RestoreOptions{}); err == nil {
		t.Fatal("foreign format must be rejected")
	}
}
