package aiassistant

import (
	"strings"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Setenv("AI_KEY_ENCRYPTION_SECRET", "test-secret-123")

	enc, err := EncryptAPIKey("sk-super-secret-key")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !strings.HasPrefix(enc, "enc:v1:") {
		t.Fatalf("expected enc:v1: prefix, got %q", enc)
	}
	dec, err := DecryptAPIKey(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if dec != "sk-super-secret-key" {
		t.Fatalf("round-trip mismatch: %q", dec)
	}
}

func TestDecryptLegacyPlaintextPassthrough(t *testing.T) {
	t.Setenv("AI_KEY_ENCRYPTION_SECRET", "test-secret-123")
	got, err := DecryptAPIKey("sk-plaintext-legacy")
	if err != nil || got != "sk-plaintext-legacy" {
		t.Fatalf("legacy passthrough failed: %q %v", got, err)
	}
}

func TestDecryptWrongSecretFails(t *testing.T) {
	t.Setenv("AI_KEY_ENCRYPTION_SECRET", "secret-a")
	enc, err := EncryptAPIKey("sk-key")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	t.Setenv("AI_KEY_ENCRYPTION_SECRET", "secret-b")
	if _, err := DecryptAPIKey(enc); err == nil {
		t.Fatal("expected decrypt failure with wrong secret")
	}
}

func TestNoSecretPlaintextMode(t *testing.T) {
	t.Setenv("AI_KEY_ENCRYPTION_SECRET", "")
	got, err := EncryptAPIKey("sk-abc")
	if err != nil || got != "sk-abc" {
		t.Fatalf("plaintext mode failed: %q %v", got, err)
	}
}

func TestDecryptEncryptedWithoutSecretErrors(t *testing.T) {
	t.Setenv("AI_KEY_ENCRYPTION_SECRET", "secret-a")
	enc, err := EncryptAPIKey("sk-key")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	t.Setenv("AI_KEY_ENCRYPTION_SECRET", "")
	if _, err := DecryptAPIKey(enc); err == nil {
		t.Fatal("expected error decrypting without a configured secret")
	}
}
