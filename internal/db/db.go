// Package db opens the SQLite database, runs migrations, and exposes
// repositories for all LoLLM state (connections, combos, proxy pools, API
// keys, settings, usage logs, agent configs).
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (CGO-free static builds)
)

// timeLayout is the canonical SQLite datetime format used throughout the
// schema: UTC text "YYYY-MM-DD HH:MM:SS".
const timeLayout = "2006-01-02 15:04:05"

// Open opens (creating if needed) the SQLite database with sensible pragmas.
//
// A single connection is used on purpose: it serializes all access and makes
// SQLITE_BUSY impossible, which is plenty for a local gateway's throughput.
func Open(path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("db: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("db: create %s: %w", dir, err)
		}
	}
	dsn := path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(1)" // NORMAL
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}
	d.SetMaxOpenConns(1)
	d.SetMaxIdleConns(1)
	d.SetConnMaxLifetime(0)
	if err := d.Ping(); err != nil {
		d.Close()
		return nil, fmt.Errorf("db: ping %s: %w", path, err)
	}
	return d, nil
}

func nowUTC() string { return time.Now().UTC().Format(timeLayout) }
