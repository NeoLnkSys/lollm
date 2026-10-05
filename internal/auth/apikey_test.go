package auth

import (
	"strings"
	"testing"
)

func TestGenerateAPIKeyFormat(t *testing.T) {
	plain, err := GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, KeyPrefix) {
		t.Fatalf("key %q missing prefix %q", plain, KeyPrefix)
	}
	// 32 bytes -> 43 base64url chars.
	if len(plain) != len(KeyPrefix)+43 {
		t.Fatalf("unexpected key length %d", len(plain))
	}
}

func TestGenerateKeyUniqueness(t *testing.T) {
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		k, err := GenerateAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		if seen[k] {
			t.Fatalf("duplicate key generated: %s", k)
		}
		seen[k] = true
	}
}

func TestHashAndVerify(t *testing.T) {
	plain, _ := GenerateAPIKey()
	h := HashKey(plain)
	if h == plain {
		t.Fatal("hash must differ from plain key")
	}
	if !VerifyKeyHash(plain, h) {
		t.Fatal("correct key must verify")
	}
	if VerifyKeyHash("lollm-wrong-key", h) {
		t.Fatal("wrong key must not verify")
	}
}

func TestAdminTokenPrefix(t *testing.T) {
	tok, err := GenerateAdminToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, AdminTokenPrefix) {
		t.Fatalf("admin token %q missing prefix", tok)
	}
}

func TestDisplayPrefixMasks(t *testing.T) {
	plain, _ := GenerateAPIKey()
	d := DisplayPrefix(plain)
	if d == plain {
		t.Fatal("display prefix must not reveal the full key")
	}
	if len([]rune(d)) > 14 {
		t.Fatalf("display prefix too long: %q", d)
	}
	// Without the trailing ellipsis, the prefix must match the key start.
	if !strings.HasPrefix(plain, strings.TrimSuffix(d, "…")) {
		t.Fatalf("display prefix %q must match the key start", d)
	}
}
