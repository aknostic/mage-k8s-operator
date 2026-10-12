package controller

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Guards ADR 003 — the operator must emit MAGENTO_CRYPT_KEY as
// "base64<base64-of-32-raw-bytes>" so Magento's Encryptor::decodeKey
// strips the prefix and base64-decodes the remainder into the 32 raw
// bytes libsodium wants. Changing this format is an ADR decision, not
// a code tweak.
func TestNewCryptKeyFormat(t *testing.T) {
	for range 50 {
		k := newCryptKey()
		if !strings.HasPrefix(k, "base64") {
			t.Fatalf("missing %q prefix: %q", "base64", k)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(k, "base64"))
		if err != nil {
			t.Fatalf("base64 body invalid: %v (key=%q)", err, k)
		}
		if len(raw) != 32 {
			t.Fatalf("decoded length = %d, want 32 (key=%q)", len(raw), k)
		}
	}
}

// Two consecutive keys must not match — crypto/rand is the source and
// collisions would indicate a seeding bug, not just improbable luck.
func TestNewCryptKeyIsRandom(t *testing.T) {
	a, b := newCryptKey(), newCryptKey()
	if a == b {
		t.Fatalf("newCryptKey returned the same value twice: %q", a)
	}
}
