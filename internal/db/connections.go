package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const connectionCols = `id, name, provider, api_key_encrypted, base_url, priority, weight,
	is_active, status, last_used_at, consecutive_errors, backoff_until, latency_ema_ms,
	proxy_pool_id, models_json, created_at, updated_at`

func scanConnection(r rowScanner) (*Connection, error) {
	var (
		c                                       Connection
		baseURL, poolID, modelsJSON             sql.NullString
		lastUsed, backoff, createdAt, updatedAt sql.NullString
	)
	if err := r.Scan(
		&c.ID, &c.Name, &c.Provider, &c.APIKeyEncrypted, &baseURL, &c.Priority, &c.Weight,
		&c.IsActive, &c.Status, &lastUsed, &c.ConsecutiveErrors, &backoff, &c.LatencyEMAMs,
		&poolID, &modelsJSON, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	c.BaseURL = baseURL.String
	c.ProxyPoolID = poolID.String
	c.ModelsJSON = modelsJSON.String
	c.LastUsedAt = parseTimePtr(lastUsed)
	c.BackoffUntil = parseTimePtr(backoff)
	c.CreatedAt = parseTime(createdAt)
	c.UpdatedAt = parseTime(updatedAt)
	return &c, nil
}

// CreateConnection inserts a connection, filling defaults and timestamps.
// The API key must already be encrypted (secret.EncryptString).
func (s *Store) CreateConnection(ctx context.Context, c *Connection) error {
	if c.ID == "" {
		c.ID = NewID("conn_")
	}
	ts, now := nowPair()
	c.CreatedAt, c.UpdatedAt = ts, ts
	_, err := s.db.ExecContext(ctx, `INSERT INTO connections
		(id, name, provider, api_key_encrypted, base_url, priority, weight, is_active, status,
		 consecutive_errors, latency_ema_ms, proxy_pool_id, models_json, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.Name, c.Provider, c.APIKeyEncrypted, nullStr(c.BaseURL),
		nzInt(c.Priority, 100), nzInt(c.Weight, 1), c.IsActive, defStr(c.Status, StatusActive),
		c.ConsecutiveErrors, c.LatencyEMAMs, nullStr(c.ProxyPoolID), defStr(c.ModelsJSON, "[]"),
		now, now)
	if err != nil {
		return fmt.Errorf("db: create connection: %w", err)
	}
	return nil
}

// GetConnection fetches a connection by id.
func (s *Store) GetConnection(ctx context.Context, id string) (*Connection, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+connectionCols+` FROM connections WHERE id = ?`, id)
	c, err := scanConnection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("db: connection %q not found", id)
	}
	return c, err
}

// ListConnections returns all connections ordered by priority, then name.
func (s *Store) ListConnections(ctx context.Context) ([]*Connection, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+connectionCols+` FROM connections ORDER BY priority ASC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Connection
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateConnection overwrites all mutable fields of a connection.
func (s *Store) UpdateConnection(ctx context.Context, c *Connection) error {
	now := nowUTC()
	c.UpdatedAt = parseTime(sql.NullString{String: now, Valid: true})
	res, err := s.db.ExecContext(ctx, `UPDATE connections SET
		name=?, provider=?, api_key_encrypted=?, base_url=?, priority=?, weight=?,
		is_active=?, status=?, consecutive_errors=?, latency_ema_ms=?, proxy_pool_id=?,
		models_json=?, last_used_at=?, backoff_until=?, updated_at=?
		WHERE id=?`,
		c.Name, c.Provider, c.APIKeyEncrypted, nullStr(c.BaseURL), c.Priority, c.Weight,
		c.IsActive, defStr(c.Status, StatusActive), c.ConsecutiveErrors, c.LatencyEMAMs,
		nullStr(c.ProxyPoolID), defStr(c.ModelsJSON, "[]"),
		timeArg(c.LastUsedAt), timeArg(c.BackoffUntil), now, c.ID)
	if err != nil {
		return fmt.Errorf("db: update connection: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("db: connection %q not found", c.ID)
	}
	return nil
}

// DeleteConnection removes a connection.
func (s *Store) DeleteConnection(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connections WHERE id = ?`, id)
	return err
}

// SetConnectionStatus updates status and backoff (nil backoff clears it).
func (s *Store) SetConnectionStatus(ctx context.Context, id, status string, backoffUntil *time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE connections SET status=?, backoff_until=?, updated_at=? WHERE id=?`,
		status, timeArg(backoffUntil), nowUTC(), id)
	return err
}

// MarkConnectionResult records the outcome of a proxied call:
//
//   - success: resets the error streak + status, folds latency into the EMA
//     (smoothing factor 0.3), and stamps last_used_at
//   - failure: bumps the error streak and applies the given status/backoff
func (s *Store) MarkConnectionResult(ctx context.Context, id string, success bool,
	failStatus string, backoffUntil *time.Time, latencyMs float64) error {
	now := nowUTC()
	if success {
		_, err := s.db.ExecContext(ctx, `UPDATE connections SET
			status='active', consecutive_errors=0, backoff_until=NULL, last_used_at=?,
			latency_ema_ms = CASE WHEN latency_ema_ms <= 0 THEN ?
			                     ELSE latency_ema_ms*0.7 + ?*0.3 END,
			updated_at=?
			WHERE id=?`, now, latencyMs, latencyMs, now, id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE connections SET
		consecutive_errors = consecutive_errors + 1,
		status=?, backoff_until=?, last_used_at=?, updated_at=?
		WHERE id=?`, defStr(failStatus, StatusUnavailable), timeArg(backoffUntil), now, now, id)
	return err
}
