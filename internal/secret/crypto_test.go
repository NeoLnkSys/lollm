package secret

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptDecryptRoundtrip(t *testing.T) {
	key := make([]byte, MasterKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatal(err)
	}
	const plain = "sk-or-v1-super-secret-provider-key"
	enc, err := EncryptString(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	if enc == plain {
		t.Fatal("ciphertext must differ from plaintext")
	}
	got, err := DecryptString(key, enc)
	if err != nil {
		t.Fatal(err)
	}
	if got != plain {
		t.Fatalf("roundtrip mismatch: %q != %q", got, plain)
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	key1 := make([]byte, MasterKeySize)
	key2 := make([]byte, MasterKeySize)
	io.ReadFull(rand.Reader, key1)
	io.ReadFull(rand.Reader, key2)

	enc, err := EncryptString(key1, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecryptString(key2, enc); err == nil {
		t.Fatal("decrypting with the wrong master key must fail")
	}
}

func TestDecryptTamperedCiphertextFails(t *testing.T) {
	key := make([]byte, MasterKeySize)
	io.ReadFull(rand.Reader, key)
	enc, _ := EncryptString(key, "secret")
	tampered := enc[:len(enc)-4] + "AAAA"
	if _, err := DecryptString(key, tampered); err == nil {
		t.Fatal("tampered ciphertext must fail authentication")
	}
}

func TestEncryptRejectsBadKeySize(t *testing.T) {
	if _, err := EncryptString([]byte("short"), "x"); err == nil {
		t.Fatal("short master key must be rejected")
	}
}

func TestMasterKeyCreateAndReload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOLLM_MASTER_KEY", "")
	path := filepath.Join(dir, ".master.key")

	key1, err := LoadOrCreateMasterKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(key1) != MasterKeySize {
		t.Fatalf("key size %d", len(key1))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("master key file must be 0600, got %o", perm)
	}

	key2, err := LoadOrCreateMasterKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(key1) != string(key2) {
		t.Fatal("reload must return the same key")
	}
}

func TestMasterKeyEnvOverride(t *testing.T) {
	envKey := make([]byte, MasterKeySize)
	io.ReadFull(rand.Reader, envKey)
	t.Setenv("LOLLM_MASTER_KEY", base64.StdEncoding.EncodeToString(envKey))

	got, err := LoadOrCreateMasterKey("/nonexistent/.master.key")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(envKey) {
		t.Fatal("env master key must win over the file")
	}
}

func TestMasterKeyEnvInvalid(t *testing.T) {
	t.Setenv("LOLLM_MASTER_KEY", "not-base64!!!")
	if _, err := LoadOrCreateMasterKey(t.TempDir() + "/.master.key"); err == nil {
		t.Fatal("invalid env master key must error")
	}
	t.Setenv("LOLLM_MASTER_KEY", base64.StdEncoding.EncodeToString([]byte("too-short")))
	if _, err := LoadOrCreateMasterKey(t.TempDir() + "/.master.key"); err == nil {
		t.Fatal("wrong-size env master key must error")
	}
}
