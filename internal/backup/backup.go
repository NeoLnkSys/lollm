// Package backup implements the portable backup.json format (spec section
// 3.8: Export / Import). A backup carries every routable object —
// connections, combos, proxy pools, agent configs, API-key hashes (so
// existing client keys keep working) and settings — in a single JSON file
// that can be restored into any LoLLM database, even one with a different
// master key.
//
// Secret handling:
//
//	secrets=none    provider API keys are omitted (safe to share)
//	secrets=plain   provider API keys in plaintext (chmod 0600 file!)
//	secrets=pbkdf2  keys sealed with AES-256-GCM under a PBKDF2 key
//	                derived from a password (--password)
package backup

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lollm/lollm/internal/db"
	"github.com/lollm/lollm/internal/secret"
)

// Format identifies the file; Version gates schema changes.
const (
	Format        = "lollm-backup"
	FormatVersion = 1

	pbkdf2Iterations = 600_000
)

// Secret modes.
const (
	SecretsNone   = "none"
	SecretsPlain  = "plain"
	SecretsPBKDF2 = "pbkdf2"
)

// KDF records how the sealed secrets blob was derived.
type KDF struct {
	Salt       string `json:"salt"` // base64
	Iterations int    `json:"iterations"`
}

// Connection is the portable form of a provider connection.
type Connection struct {
	Name      string   `json:"name"`
	Provider  string   `json:"provider"`
	APIKey    string   `json:"api_key,omitempty"` // plaintext (mode=plain only)
	BaseURL   string   `json:"base_url,omitempty"`
	ProxyPool string   `json:"proxy_pool,omitempty"` // pool NAME
	Priority  int      `json:"priority"`
	Weight    int      `json:"weight"`
	IsActive  bool     `json:"is_active"`
	Models    []string `json:"models,omitempty"`
}

// ProxyPool is a named list of proxies.
type ProxyPool struct {
	Name    string   `json:"name"`
	Proxies []string `json:"proxies"`
}

// ComboModel references a connection by NAME (portable across databases).
type ComboModel struct {
	Connection string `json:"connection"`
	Model      string `json:"model"`
	Priority   int    `json:"priority"`
}

// Combo is the portable form of a combo.
type Combo struct {
	Name             string       `json:"name"`
	Strategy         string       `json:"strategy"`
	Compression      string       `json:"compression,omitempty"`
	AgentModeEnabled bool         `json:"agent_mode_enabled"`
	Models           []ComboModel `json:"models"`
}

// AgentConfig is the portable form of an Agent Mode pipeline.
type AgentConfig struct {
	Name              string         `json:"name"`
	Mode              string         `json:"mode"`
	MaxRounds         int            `json:"max_rounds"`
	HideInternalSteps bool           `json:"hide_internal_steps"`
	Roles             []db.AgentRole `json:"roles"`
}

// APIKey carries the SHA-256 hash so existing client keys survive a
// migration (hashes are not secrets).
type APIKey struct {
	Name      string `json:"name"`
	KeyHash   string `json:"key_hash"`
	KeyPrefix string `json:"key_prefix"`
	IsActive  bool   `json:"is_active"`
}

// Backup is the whole portable configuration.
type Backup struct {
	Format       string            `json:"format"`
	Version      int               `json:"version"`
	Product      string            `json:"product"`
	ExportedAt   time.Time         `json:"exported_at"`
	Secrets      string            `json:"secrets"`
	KDF          *KDF              `json:"kdf,omitempty"`
	SealedBlob   string            `json:"sealed_secrets,omitempty"` // mode=pbkdf2
	Connections  []Connection      `json:"connections"`
	ProxyPools   []ProxyPool       `json:"proxy_pools"`
	Combos       []Combo           `json:"combos"`
	AgentConfigs []AgentConfig     `json:"agent_configs"`
	APIKeys      []APIKey          `json:"api_keys"`
	Settings     map[string]string `json:"settings"`
}

// Options controls Build.
type Options struct {
	IncludeSecrets bool   // embed provider API keys
	Password       string // seal secrets under this password (PBKDF2-AES-GCM)
}

// RestoreOptions controls Restore.
type RestoreOptions struct{ Replace bool }

// Stats reports what a Restore touched.
type Stats struct {
	Connections  int `json:"connections"`
	Combos       int `json:"combos"`
	AgentConfigs int `json:"agent_configs"`
	APIKeys      int `json:"api_keys"`
	ProxyPools   int `json:"proxy_pools"`
	Settings     int `json:"settings"`
}

// Build snapshots the store into a portable Backup.
func Build(ctx context.Context, store *db.Store, masterKey []byte, opts Options) (*Backup, error) {
	b := &Backup{
		Format: Format, Version: FormatVersion, Product: "LoLLM Synapse",
		ExportedAt: time.Now().UTC(), Secrets: SecretsNone,
	}

	conns, err := store.ListConnections(ctx)
	if err != nil {
		return nil, err
	}
	keysByName := map[string]string{} // for the sealed blob
	for _, c := range conns {
		pc := Connection{
			Name: c.Name, Provider: c.Provider, BaseURL: c.BaseURL,
			Priority: c.Priority, Weight: c.Weight, IsActive: c.IsActive,
			Models: c.Models(),
		}
		if c.ProxyPoolID != "" {
			if pool, err := store.GetProxyPool(ctx, c.ProxyPoolID); err == nil {
				pc.ProxyPool = pool.Name
			}
		}
		if c.APIKeyEncrypted != "" {
			plain, err := secret.DecryptString(masterKey, c.APIKeyEncrypted)
			if err != nil {
				return nil, fmt.Errorf("decrypt key for %s: %w", c.Name, err)
			}
			keysByName[c.Name] = plain
			if opts.IncludeSecrets && opts.Password == "" {
				pc.APIKey = plain
			}
		}
		b.Connections = append(b.Connections, pc)
	}

	if opts.IncludeSecrets && opts.Password != "" {
		blob, err := json.Marshal(keysByName)
		if err != nil {
			return nil, err
		}
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		key, err := deriveKey(opts.Password, salt)
		if err != nil {
			return nil, err
		}
		sealed, err := secret.Encrypt(key, blob)
		if err != nil {
			return nil, err
		}
		b.Secrets = SecretsPBKDF2
		b.KDF = &KDF{Salt: base64.StdEncoding.EncodeToString(salt), Iterations: pbkdf2Iterations}
		b.SealedBlob = sealed
	} else if opts.IncludeSecrets {
		b.Secrets = SecretsPlain
	}

	pools, err := store.ListProxyPools(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range pools {
		b.ProxyPools = append(b.ProxyPools, ProxyPool{Name: p.Name, Proxies: p.Proxies})
	}

	combos, err := store.ListCombos(ctx)
	if err != nil {
		return nil, err
	}
	idToName := map[string]string{}
	for _, c := range conns {
		idToName[c.ID] = c.Name
	}
	for _, c := range combos {
		bc := Combo{
			Name: c.Name, Strategy: c.Strategy, Compression: c.Compression,
			AgentModeEnabled: c.AgentModeEnabled,
		}
		for _, m := range c.Models {
			bc.Models = append(bc.Models, ComboModel{
				Connection: idToName[m.ConnectionID], Model: m.Model, Priority: m.Priority,
			})
		}
		b.Combos = append(b.Combos, bc)
	}

	cfgs, err := store.ListAgentConfigs(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range cfgs {
		b.AgentConfigs = append(b.AgentConfigs, AgentConfig{
			Name: a.Name, Mode: a.Mode, MaxRounds: a.MaxRounds,
			HideInternalSteps: a.HideInternalSteps, Roles: a.Roles,
		})
	}

	keys, err := store.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		b.APIKeys = append(b.APIKeys, APIKey{
			Name: k.Name, KeyHash: k.KeyHash, KeyPrefix: k.KeyPrefix, IsActive: k.IsActive,
		})
	}

	if b.Settings, err = store.AllSettings(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

// DecryptSecrets unseals a pbkdf2 backup's provider keys into the
// connections' APIKey fields (in place).
func (b *Backup) DecryptSecrets(password string) error {
	if b.Secrets != SecretsPBKDF2 {
		return nil
	}
	if b.KDF == nil || b.SealedBlob == "" {
		return errors.New("backup: sealed secrets missing KDF parameters")
	}
	salt, err := base64.StdEncoding.DecodeString(b.KDF.Salt)
	if err != nil {
		return fmt.Errorf("backup: bad kdf salt: %w", err)
	}
	key, err := deriveKey(password, salt)
	if err != nil {
		return err
	}
	blob, err := secret.Decrypt(key, b.SealedBlob)
	if err != nil {
		return errors.New("backup: wrong password (decrypt failed)")
	}
	var keysByName map[string]string
	if err := json.Unmarshal(blob, &keysByName); err != nil {
		return fmt.Errorf("backup: sealed blob corrupt: %w", err)
	}
	for i := range b.Connections {
		b.Connections[i].APIKey = keysByName[b.Connections[i].Name]
	}
	b.Secrets = SecretsPlain
	b.KDF, b.SealedBlob = nil, ""
	return nil
}

// Restore imports a backup into the store, re-encrypting provider keys with
// the store's own master key. Objects are upserted by name; Replace wipes
// the affected tables first.
func Restore(ctx context.Context, store *db.Store, masterKey []byte, b *Backup, opts RestoreOptions) (*Stats, error) {
	if b.Format != Format {
		return nil, fmt.Errorf("backup: not a %s file (format=%q)", Format, b.Format)
	}
	if b.Secrets == SecretsPBKDF2 {
		return nil, errors.New("backup: secrets are password-sealed; decrypt first (DecryptSecrets)")
	}
	st := &Stats{}

	if opts.Replace {
		for _, c := range mustList(ctx, store.ListConnections) {
			_ = store.DeleteConnection(ctx, c.ID)
		}
		for _, c := range mustList(ctx, store.ListCombos) {
			_ = store.DeleteCombo(ctx, c.ID)
		}
		for _, a := range mustList(ctx, store.ListAgentConfigs) {
			_ = store.DeleteAgentConfig(ctx, a.ID)
		}
		for _, k := range mustList(ctx, store.ListAPIKeys) {
			_ = store.DeleteAPIKey(ctx, k.ID)
		}
		for _, p := range mustList(ctx, store.ListProxyPools) {
			_ = store.DeleteProxyPool(ctx, p.ID)
		}
	}

	// 1) Proxy pools (connections reference them by name).
	poolID := map[string]string{}
	for _, p := range existingPools(ctx, store) {
		poolID[p.Name] = p.ID
	}
	for _, bp := range b.ProxyPools {
		if id, ok := poolID[bp.Name]; ok {
			p := bp.toDB()
			p.ID = id
			if err := store.UpdateProxyPool(ctx, p); err != nil {
				return nil, fmt.Errorf("update pool %s: %w", bp.Name, err)
			}
		} else {
			p := bp.toDB()
			if err := store.CreateProxyPool(ctx, p); err != nil {
				return nil, fmt.Errorf("create pool %s: %w", bp.Name, err)
			}
			poolID[bp.Name] = p.ID
		}
		st.ProxyPools++
	}

	// 2) Connections.
	connID := map[string]string{}
	for _, c := range existingConns(ctx, store) {
		connID[c.Name] = c.ID
	}
	for _, bc := range b.Connections {
		c := bc.toDB()
		c.ProxyPoolID = poolID[bc.ProxyPool]
		if bc.APIKey != "" {
			enc, err := secret.EncryptString(masterKey, bc.APIKey)
			if err != nil {
				return nil, fmt.Errorf("encrypt key for %s: %w", bc.Name, err)
			}
			c.APIKeyEncrypted = enc
		}
		if id, ok := connID[bc.Name]; ok {
			c.ID = id
			if c.APIKeyEncrypted == "" {
				// keep the existing ciphertext when the backup carries no key
				if prev, err := store.GetConnection(ctx, id); err == nil {
					c.APIKeyEncrypted = prev.APIKeyEncrypted
				}
			}
			if err := store.UpdateConnection(ctx, c); err != nil {
				return nil, fmt.Errorf("update connection %s: %w", bc.Name, err)
			}
		} else {
			if err := store.CreateConnection(ctx, c); err != nil {
				return nil, fmt.Errorf("create connection %s: %w", bc.Name, err)
			}
			connID[bc.Name] = c.ID
		}
		st.Connections++
	}

	// 3) Combos (resolve connection names to fresh IDs).
	for _, bcb := range b.Combos {
		c := &db.Combo{
			Name: bcb.Name, Strategy: bcb.Strategy, Compression: bcb.Compression,
			AgentModeEnabled: bcb.AgentModeEnabled,
		}
		for _, m := range bcb.Models {
			id, ok := connID[m.Connection]
			if !ok {
				return nil, fmt.Errorf("combo %s references unknown connection %q", bcb.Name, m.Connection)
			}
			c.Models = append(c.Models, db.ComboModel{ConnectionID: id, Model: m.Model, Priority: m.Priority})
		}
		if prev, err := store.GetComboByName(ctx, bcb.Name); err == nil {
			c.ID = prev.ID
			if err := store.UpdateCombo(ctx, c); err != nil {
				return nil, fmt.Errorf("update combo %s: %w", bcb.Name, err)
			}
		} else if err := store.CreateCombo(ctx, c); err != nil {
			return nil, fmt.Errorf("create combo %s: %w", bcb.Name, err)
		}
		st.Combos++
	}

	// 4) Agent configs.
	haveCfg := map[string]string{} // name → id
	if cfgs, err := store.ListAgentConfigs(ctx); err == nil {
		for _, a := range cfgs {
			haveCfg[a.Name] = a.ID
		}
	}
	for _, ba := range b.AgentConfigs {
		a := &db.AgentConfig{
			Name: ba.Name, Mode: ba.Mode, MaxRounds: ba.MaxRounds,
			HideInternalSteps: ba.HideInternalSteps, Roles: ba.Roles,
		}
		if id, ok := haveCfg[ba.Name]; ok {
			a.ID = id
			if err := store.UpdateAgentConfig(ctx, a); err != nil {
				return nil, fmt.Errorf("update agent config %s: %w", ba.Name, err)
			}
		} else if err := store.CreateAgentConfig(ctx, a); err != nil {
			return nil, fmt.Errorf("create agent config %s: %w", ba.Name, err)
		}
		st.AgentConfigs++
	}

	// 5) API keys — upsert by hash so existing client keys keep working.
	for _, bk := range b.APIKeys {
		if bk.KeyHash == "" {
			continue
		}
		if _, err := store.GetAPIKeyByHash(ctx, bk.KeyHash); err == nil {
			continue // same key already present
		}
		k := &db.APIKey{
			Name: bk.Name, KeyHash: bk.KeyHash, KeyPrefix: bk.KeyPrefix, IsActive: bk.IsActive,
		}
		if err := store.CreateAPIKey(ctx, k); err != nil {
			return nil, fmt.Errorf("create api key %s: %w", bk.Name, err)
		}
		st.APIKeys++
	}

	// 6) Settings.
	for k, v := range b.Settings {
		if err := store.SetSetting(ctx, k, v); err != nil {
			return nil, fmt.Errorf("set setting %s: %w", k, err)
		}
		st.Settings++
	}
	return st, nil
}

// --- helpers -----------------------------------------------------------------

func deriveKey(password string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, 32)
}

func (p ProxyPool) toDB() *db.ProxyPool {
	return &db.ProxyPool{ID: "", Name: p.Name, Proxies: p.Proxies}
}

func (c Connection) toDB() *db.Connection {
	models, _ := json.Marshal(c.Models)
	return &db.Connection{
		Name: c.Name, Provider: c.Provider, BaseURL: c.BaseURL,
		Priority: c.Priority, Weight: c.Weight, IsActive: c.IsActive,
		ModelsJSON: string(models),
	}
}

func existingPools(ctx context.Context, store *db.Store) []*db.ProxyPool {
	pools, err := store.ListProxyPools(ctx)
	if err != nil {
		return nil
	}
	return pools
}

func existingConns(ctx context.Context, store *db.Store) []*db.Connection {
	conns, err := store.ListConnections(ctx)
	if err != nil {
		return nil
	}
	return conns
}

// mustList adapts the store's List* functions for the Replace wipe
// (errors are swallowed: delete-what-exists).
func mustList[T any](ctx context.Context, f func(context.Context) ([]*T, error)) []*T {
	out, err := f(ctx)
	if err != nil {
		return nil
	}
	return out
}
