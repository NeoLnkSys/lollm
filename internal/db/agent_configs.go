package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

const agentConfigCols = `id, name, mode, roles_json, max_rounds, hide_internal_steps, created_at, updated_at`

func scanAgentConfig(r rowScanner) (*AgentConfig, error) {
	var (
		a                           AgentConfig
		rolesJSON, created, updated sql.NullString
	)
	if err := r.Scan(&a.ID, &a.Name, &a.Mode, &rolesJSON, &a.MaxRounds, &a.HideInternalSteps,
		&created, &updated); err != nil {
		return nil, err
	}
	if rolesJSON.Valid && rolesJSON.String != "" {
		if err := json.Unmarshal([]byte(rolesJSON.String), &a.Roles); err != nil {
			return nil, fmt.Errorf("db: agent config %s roles_json: %w", a.ID, err)
		}
	}
	a.CreatedAt = parseTime(created)
	a.UpdatedAt = parseTime(updated)
	return &a, nil
}

// CreateAgentConfig inserts an Agent Mode pipeline definition.
func (s *Store) CreateAgentConfig(ctx context.Context, a *AgentConfig) error {
	if a.ID == "" {
		a.ID = NewID("agent_")
	}
	rolesJSON, err := json.Marshal(a.Roles)
	if err != nil {
		return err
	}
	ts, now := nowPair()
	a.CreatedAt, a.UpdatedAt = ts, ts
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_configs
		(id, name, mode, roles_json, max_rounds, hide_internal_steps, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		a.ID, a.Name, defStr(a.Mode, AgentModeCollaborative), string(rolesJSON),
		nzInt(a.MaxRounds, 2), a.HideInternalSteps, now, now)
	if err != nil {
		return fmt.Errorf("db: create agent config: %w", err)
	}
	return nil
}

// GetAgentConfig fetches an agent config by id.
func (s *Store) GetAgentConfig(ctx context.Context, id string) (*AgentConfig, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+agentConfigCols+` FROM agent_configs WHERE id = ?`, id)
	a, err := scanAgentConfig(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("db: agent config %q not found", id)
	}
	return a, err
}

// ListAgentConfigs returns all agent configs, oldest first.
func (s *Store) ListAgentConfigs(ctx context.Context) ([]*AgentConfig, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+agentConfigCols+` FROM agent_configs ORDER BY created_at ASC, rowid ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AgentConfig
	for rows.Next() {
		a, err := scanAgentConfig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DefaultAgentConfig returns the first (oldest) agent config, or an error if
// none exists.
func (s *Store) DefaultAgentConfig(ctx context.Context) (*AgentConfig, error) {
	configs, err := s.ListAgentConfigs(ctx)
	if err != nil {
		return nil, err
	}
	if len(configs) == 0 {
		return nil, errors.New("db: no agent configs defined")
	}
	return configs[0], nil
}

// UpdateAgentConfig overwrites the mutable fields of an agent config.
func (s *Store) UpdateAgentConfig(ctx context.Context, a *AgentConfig) error {
	rolesJSON, err := json.Marshal(a.Roles)
	if err != nil {
		return err
	}
	now := nowUTC()
	res, err := s.db.ExecContext(ctx, `UPDATE agent_configs SET
		name=?, mode=?, roles_json=?, max_rounds=?, hide_internal_steps=?, updated_at=?
		WHERE id=?`,
		a.Name, defStr(a.Mode, AgentModeCollaborative), string(rolesJSON),
		a.MaxRounds, a.HideInternalSteps, now, a.ID)
	if err != nil {
		return fmt.Errorf("db: update agent config: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("db: agent config %q not found", a.ID)
	}
	return nil
}

// DeleteAgentConfig removes an agent config.
func (s *Store) DeleteAgentConfig(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agent_configs WHERE id = ?`, id)
	return err
}
