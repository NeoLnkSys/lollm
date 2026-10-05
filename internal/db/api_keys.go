package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const apiKeyCols = `id, name, key_hash, key_prefix, is_active, created_at, last_used_at`

func scanAPIKey(r rowScanner) (*APIKey, error) {
	var (
		k                APIKey
		created, lastUse sql.NullString
	)
	if err := r.Scan(&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.IsActive, &created, &lastUse); err != nil {
		return nil, err
	}
	k.CreatedAt = parseTime(created)
	k.LastUsedAt = parseTimePtr(lastUse)
	return &k, nil
}

// CreateAPIKey inserts an internal API key. KeyHash must be auth.HashKey(plain);
// the plain key is never stored.
func (s *Store) CreateAPIKey(ctx context.Context, k *APIKey) error {
	if k.ID == "" {
		k.ID = NewID("key_")
	}
	ts, now := nowPair()
	k.CreatedAt = ts
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_keys
		(id, name, key_hash, key_prefix, is_active, created_at) VALUES (?,?,?,?,?,?)`,
		k.ID, k.Name, k.KeyHash, k.KeyPrefix, k.IsActive, now)
	if err != nil {
		return fmt.Errorf("db: create api key: %w", err)
	}
	return nil
}

// GetAPIKey fetches a key record by id.
func (s *Store) GetAPIKey(ctx context.Context, id string) (*APIKey, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE id = ?`, id)
	k, err := scanAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("db: api key %q not found", id)
	}
	return k, err
}

// GetAPIKeyByHash fetches a key record by its SHA-256 hash (auth path).
func (s *Store) GetAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE key_hash = ?`, hash)
	k, err := scanAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("db: api key not found")
	}
	return k, err
}

// ListAPIKeys returns all keys, newest first.
func (s *Store) ListAPIKeys(ctx context.Context) ([]*APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+apiKeyCols+` FROM api_keys ORDER BY created_at DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SetAPIKeyActive enables or disables a key.
func (s *Store) SetAPIKeyActive(ctx context.Context, id string, active bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET is_active=? WHERE id=?`, active, id)
	return err
}

// TouchAPIKeyUsed stamps last_used_at for a key.
func (s *Store) TouchAPIKeyUsed(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at=? WHERE id=?`, nowUTC(), id)
	return err
}

// CountActiveAPIKeys returns the number of enabled keys.
func (s *Store) CountActiveAPIKeys(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE is_active = 1`).Scan(&n)
	return n, err
}

// DeleteAPIKey removes a key (revoke permanently).
func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id=?`, id)
	return err
}
