// Package secret provides AES-256-GCM encryption for provider API keys at
// rest, plus management of the master key that protects them.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// MasterKeySize is the required master key length (AES-256).
const MasterKeySize = 32

// Encrypt encrypts plaintext with AES-256-GCM and returns base64
// (nonce || ciphertext || tag).
func Encrypt(key, plaintext []byte) (string, error) {
	if len(key) != MasterKeySize {
		return "", fmt.Errorf("secret: master key must be %d bytes, got %d", MasterKeySize, len(key))
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

// Decrypt reverses Encrypt.
func Decrypt(key []byte, encoded string) ([]byte, error) {
	if len(key) != MasterKeySize {
		return nil, fmt.Errorf("secret: master key must be %d bytes, got %d", MasterKeySize, len(key))
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("secret: invalid base64 ciphertext: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("secret: ciphertext too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("secret: decrypt failed (wrong master key?): %w", err)
	}
	return plain, nil
}

// EncryptString is a convenience wrapper around Encrypt.
func EncryptString(key []byte, s string) (string, error) { return Encrypt(key, []byte(s)) }

// DecryptString is a convenience wrapper around Decrypt.
func DecryptString(key []byte, encoded string) (string, error) {
	b, err := Decrypt(key, encoded)
	return string(b), err
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
