package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

const comboCols = `id, name, strategy, models_json, agent_mode_enabled, agent_config_id, compression, created_at, updated_at`

func scanCombo(r rowScanner) (*Combo, error) {
	var (
		c                                        Combo
		modelsJSON, agentCfgID, created, updated sql.NullString
		compression                              sql.NullString
	)
	if err := r.Scan(&c.ID, &c.Name, &c.Strategy, &modelsJSON, &c.AgentModeEnabled,
		&agentCfgID, &compression, &created, &updated); err != nil {
		return nil, err
	}
	c.AgentConfigID = agentCfgID.String
	c.Compression = compression.String
	if modelsJSON.Valid && modelsJSON.String != "" {
		if err := json.Unmarshal([]byte(modelsJSON.String), &c.Models); err != nil {
			return nil, fmt.Errorf("db: combo %s models_json: %w", c.ID, err)
		}
	}
	c.CreatedAt = parseTime(created)
	c.UpdatedAt = parseTime(updated)
	return &c, nil
}

// CreateCombo inserts a combo.
func (s *Store) CreateCombo(ctx context.Context, c *Combo) error {
	if c.ID == "" {
		c.ID = NewID("combo_")
	}
	modelsJSON, err := json.Marshal(c.Models)
	if err != nil {
		return err
	}
	ts, now := nowPair()
	c.CreatedAt, c.UpdatedAt = ts, ts
	_, err = s.db.ExecContext(ctx, `INSERT INTO combos
		(id, name, strategy, models_json, agent_mode_enabled, agent_config_id, compression, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		c.ID, c.Name, defStr(c.Strategy, StrategyHealthAware), string(modelsJSON),
		c.AgentModeEnabled, nullStr(c.AgentConfigID), defStr(c.Compression, ""), now, now)
	if err != nil {
		return fmt.Errorf("db: create combo: %w", err)
	}
	return nil
}

// GetCombo fetches a combo by id.
func (s *Store) GetCombo(ctx context.Context, id string) (*Combo, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+comboCols+` FROM combos WHERE id = ?`, id)
	c, err := scanCombo(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("db: combo %q not found", id)
	}
	return c, err
}

// GetComboByName fetches a combo by its unique name (routing path).
func (s *Store) GetComboByName(ctx context.Context, name string) (*Combo, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+comboCols+` FROM combos WHERE name = ?`, name)
	c, err := scanCombo(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("db: combo %q not found", name)
	}
	return c, err
}

// ListCombos returns all combos ordered by name.
func (s *Store) ListCombos(ctx context.Context) ([]*Combo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+comboCols+` FROM combos ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Combo
	for rows.Next() {
		c, err := scanCombo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCombo overwrites the mutable fields of a combo.
func (s *Store) UpdateCombo(ctx context.Context, c *Combo) error {
	modelsJSON, err := json.Marshal(c.Models)
	if err != nil {
		return err
	}
	now := nowUTC()
	res, err := s.db.ExecContext(ctx, `UPDATE combos SET
		name=?, strategy=?, models_json=?, agent_mode_enabled=?, agent_config_id=?, compression=?, updated_at=?
		WHERE id=?`,
		c.Name, defStr(c.Strategy, StrategyHealthAware), string(modelsJSON),
		c.AgentModeEnabled, nullStr(c.AgentConfigID), defStr(c.Compression, ""), now, c.ID)
	if err != nil {
		return fmt.Errorf("db: update combo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("db: combo %q not found", c.ID)
	}
	return nil
}

// DeleteCombo removes a combo.
func (s *Store) DeleteCombo(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM combos WHERE id = ?`, id)
	return err
}
