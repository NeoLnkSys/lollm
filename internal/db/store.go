package db

import (
	"context"
	"database/sql"
)

// Store wraps the database handle and exposes all repositories as methods.
type Store struct {
	db *sql.DB
}

// NewStore builds a Store over an open, migrated database.
func NewStore(d *sql.DB) *Store { return &Store{db: d} }

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle (used by later phases for transactions).
func (s *Store) DB() *sql.DB { return s.db }

// SQLiteVersion reports the bundled SQLite library version (for `doctor`).
func (s *Store) SQLiteVersion(ctx context.Context) (string, error) {
	var v string
	if err := s.db.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&v); err != nil {
		return "", err
	}
	return v, nil
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}
