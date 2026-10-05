package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

func scanProxyPool(r rowScanner) (*ProxyPool, error) {
	var (
		p           ProxyPool
		proxiesJSON sql.NullString
		created     sql.NullString
	)
	if err := r.Scan(&p.ID, &p.Name, &proxiesJSON, &created); err != nil {
		return nil, err
	}
	if proxiesJSON.Valid && proxiesJSON.String != "" {
		if err := json.Unmarshal([]byte(proxiesJSON.String), &p.Proxies); err != nil {
			return nil, fmt.Errorf("db: proxy pool %s proxies_json: %w", p.ID, err)
		}
	}
	p.CreatedAt = parseTime(created)
	return &p, nil
}

// CreateProxyPool inserts a proxy pool.
func (s *Store) CreateProxyPool(ctx context.Context, p *ProxyPool) error {
	if p.ID == "" {
		p.ID = NewID("pool_")
	}
	proxiesJSON, err := json.Marshal(p.Proxies)
	if err != nil {
		return err
	}
	p.CreatedAt, _ = nowPair()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO proxy_pools (id, name, proxies_json, created_at) VALUES (?,?,?,?)`,
		p.ID, p.Name, string(proxiesJSON), nowUTC())
	if err != nil {
		return fmt.Errorf("db: create proxy pool: %w", err)
	}
	return nil
}

// GetProxyPool fetches a pool by id.
func (s *Store) GetProxyPool(ctx context.Context, id string) (*ProxyPool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, proxies_json, created_at FROM proxy_pools WHERE id = ?`, id)
	p, err := scanProxyPool(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("db: proxy pool %q not found", id)
	}
	return p, err
}

// ListProxyPools returns all proxy pools.
func (s *Store) ListProxyPools(ctx context.Context) ([]*ProxyPool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, proxies_json, created_at FROM proxy_pools ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProxyPool
	for rows.Next() {
		p, err := scanProxyPool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateProxyPool overwrites a pool's name and proxy list.
func (s *Store) UpdateProxyPool(ctx context.Context, p *ProxyPool) error {
	proxiesJSON, err := json.Marshal(p.Proxies)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE proxy_pools SET name=?, proxies_json=? WHERE id=?`,
		p.Name, string(proxiesJSON), p.ID)
	return err
}

// DeleteProxyPool removes a pool (connections referencing it fall back to a
// direct connection via ON DELETE SET NULL).
func (s *Store) DeleteProxyPool(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM proxy_pools WHERE id = ?`, id)
	return err
}
