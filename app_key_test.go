package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
)

// TestParseAppKeyAcceptsTheDocumentedShape proves that the exact config.json
// shape documented in README.md ("Injecting a pre-issued credential") --
// a 128-hex-char full Ed25519 private key -- is accepted by the current CLI
// binary. This is the shape the SDK's appKey().export() actually produces.
func TestParseAppKeyAcceptsTheDocumentedShape(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	hexKey := hex.EncodeToString(priv) // 64 bytes -> 128 hex chars, seed||pub
	if len(hexKey) != 128 {
		t.Fatalf("test fixture itself is wrong length: got %d hex chars, want 128", len(hexKey))
	}

	got, err := parseAppKey(hexKey)
	if err != nil {
		t.Fatalf("parseAppKey rejected the documented config.json shape: %v", err)
	}
	if string(got) != string(priv) {
		t.Fatalf("parseAppKey returned a different key than it was given")
	}
}

// TestParseAppKeyRejectsBareSeedWithClearError is the regression test for the
// packet's reported hitch: the old comment said app_key was a 32-byte seed,
// the old length check (`< 32`) let a 32-byte value through, and it only
// failed later, deep inside ed25519 signing, with a confusing panic/error.
// It must now fail immediately, here, with a message that says exactly what
// is wrong and how to fix it.
func TestParseAppKeyRejectsBareSeedWithClearError(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize) // 32 bytes -- the old, wrong shape
	hexSeed := hex.EncodeToString(seed)    // 64 hex chars

	_, err := parseAppKey(hexSeed)
	if err == nil {
		t.Fatal("parseAppKey accepted a bare 32-byte seed; it must require the full 64-byte key")
	}
	msg := err.Error()
	for _, want := range []string{"32 bytes", "64-byte", "128 hex"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing expected detail %q", msg, want)
		}
	}
}

func TestParseAppKeyRejectsInvalidHex(t *testing.T) {
	if _, err := parseAppKey("not hex at all"); err == nil {
		t.Fatal("parseAppKey accepted non-hex input")
	}
}
