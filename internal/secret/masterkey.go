package secret

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MasterKeyPath returns the master key file path for a database directory
// (e.g. filepath.Dir(cfg.DBPath) + "/.master.key").
func MasterKeyPath(dbDir string) string {
	return filepath.Join(dbDir, ".master.key")
}

// LoadOrCreateMasterKey resolves the master key:
//
//  1. $LOLLM_MASTER_KEY (base64, 32 bytes) — wins when set
//  2. the key file at path (base64, 32 bytes)
//  3. a freshly generated key, written to path with 0600 permissions
func LoadOrCreateMasterKey(path string) ([]byte, error) {
	if env := strings.TrimSpace(os.Getenv("LOLLM_MASTER_KEY")); env != "" {
		return decodeMasterKey(env, "LOLLM_MASTER_KEY")
	}

	if data, err := os.ReadFile(path); err == nil {
		return decodeMasterKey(strings.TrimSpace(string(data)), path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("secret: read master key %s: %w", path, err)
	}

	key := make([]byte, MasterKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("secret: generate master key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("secret: create key dir: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key) + "\n"
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return nil, fmt.Errorf("secret: write master key %s: %w", path, err)
	}
	return key, nil
}

func decodeMasterKey(encoded, source string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("secret: %s is not valid base64: %w", source, err)
	}
	if len(key) != MasterKeySize {
		return nil, fmt.Errorf("secret: %s must decode to %d bytes, got %d", source, MasterKeySize, len(key))
	}
	return key, nil
}
