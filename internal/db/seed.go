package db

import (
	"context"
	"fmt"

	"github.com/lollm/lollm/internal/secret"
)

// SeedIfEmpty populates a fresh database with default settings and realistic
// demo data (spec section 9): two inactive demo connections, an "Auto" combo,
// and a default Agent Mode config. It never overwrites existing rows, so it
// is safe to call on every startup.
func SeedIfEmpty(ctx context.Context, s *Store, masterKey []byte) error {
	// 1) Default settings (INSERT OR IGNORE keeps user edits).
	defaults := map[string]string{
		SettingCompressionDefault:  "off",
		SettingHealthCheckEnabled:  "true",
		SettingHealthCheckInterval: "60",
	}
	for k, v := range defaults {
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`, k, v); err != nil {
			return fmt.Errorf("db: seed settings: %w", err)
		}
	}

	// 2) Demo connections + "Auto" combo.
	conns, err := s.ListConnections(ctx)
	if err != nil {
		return err
	}
	if len(conns) > 0 {
		return nil // already seeded (or user data exists)
	}

	// Demo keys are encrypted placeholders — the connections ship inactive so
	// they never receive real traffic until the user edits them.
	encOR, err := secret.EncryptString(masterKey, "demo-key-replace-me")
	if err != nil {
		return fmt.Errorf("db: seed connection key: %w", err)
	}
	encGQ := encOR // same placeholder value; distinct ciphertext not required

	orConn := &Connection{
		Name:            "demo-openrouter",
		Provider:        "openrouter",
		APIKeyEncrypted: encOR,
		Priority:        10,
		Weight:          1,
		IsActive:        false, // demo data — enable after adding a real key
		Status:          StatusActive,
		ModelsJSON:      `["anthropic/claude-sonnet-4", "google/gemini-2.5-flash", "openai/gpt-4o-mini"]`,
	}
	gqConn := &Connection{
		Name:            "demo-groq",
		Provider:        "groq",
		APIKeyEncrypted: encGQ,
		Priority:        20,
		Weight:          1,
		IsActive:        false,
		Status:          StatusActive,
		ModelsJSON:      `["llama-3.3-70b-versatile", "llama-3.1-8b-instant"]`,
	}
	if err := s.CreateConnection(ctx, orConn); err != nil {
		return err
	}
	if err := s.CreateConnection(ctx, gqConn); err != nil {
		return err
	}

	autoCombo := &Combo{
		Name:     "Auto",
		Strategy: StrategyHealthAware,
		Models: []ComboModel{
			{ConnectionID: orConn.ID, Model: "google/gemini-2.5-flash", Priority: 1},
			{ConnectionID: orConn.ID, Model: "anthropic/claude-sonnet-4", Priority: 2},
			{ConnectionID: gqConn.ID, Model: "llama-3.3-70b-versatile", Priority: 3},
		},
	}
	if err := s.CreateCombo(ctx, autoCombo); err != nil {
		return err
	}
	return nil
}
