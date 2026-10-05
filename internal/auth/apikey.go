// Package auth handles internal API keys: the keys that coding tools present
// to the gateway (Bearer token / API key). Plain keys are never stored —
// only their SHA-256 hashes.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"io"
)

const (
	// KeyPrefix marks regular internal API keys.
	KeyPrefix = "lollm-"
	// AdminTokenPrefix marks dashboard admin tokens.
	AdminTokenPrefix = "lollm-admin-"
)

// GenerateKey returns a fresh random key with the given prefix, e.g.
// "lollm-" + 43 base64url chars (32 bytes of entropy).
func GenerateKey(prefix string) (string, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// GenerateAPIKey returns a new internal API key (plain text).
func GenerateAPIKey() (string, error) { return GenerateKey(KeyPrefix) }

// GenerateAdminToken returns a new dashboard admin token (plain text).
func GenerateAdminToken() (string, error) { return GenerateKey(AdminTokenPrefix) }

// HashKey returns the SHA-256 hex digest of a key.
func HashKey(plain string) string {
	h := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(h[:])
}

// VerifyKeyHash reports whether plain matches the stored hash, in constant time.
func VerifyKeyHash(plain, storedHash string) bool {
	return subtle.ConstantTimeCompare([]byte(HashKey(plain)), []byte(storedHash)) == 1
}

// DisplayPrefix returns a non-secret, recognizable prefix for lists and the UI.
func DisplayPrefix(plain string) string {
	if len(plain) <= 12 {
		return plain
	}
	return plain[:12] + "…"
}
