package secretbox

import (
	"strings"
	"testing"
)

func TestRoundTripAndModes(t *testing.T) {
	t.Setenv(EnvVar, "")
	t.Setenv(LegacyEnvVar, "")
	if Configured() {
		t.Fatal("no key: must not be configured")
	}
	if v, _ := Encrypt("plain"); v != "plain" {
		t.Fatalf("plaintext mode altered the value: %q", v)
	}

	t.Setenv(LegacyEnvVar, "old-name-still-works")
	if !Configured() {
		t.Fatal("the legacy variable must still configure the box")
	}
	sealed, err := Encrypt("sk-secret")
	if err != nil || !strings.HasPrefix(sealed, prefix) || strings.Contains(sealed, "sk-secret") {
		t.Fatalf("sealed=%q err=%v", sealed, err)
	}
	if back, err := Decrypt(sealed); err != nil || back != "sk-secret" {
		t.Fatalf("back=%q err=%v", back, err)
	}
	if back, err := Decrypt("legacy-plain"); err != nil || back != "legacy-plain" {
		t.Fatalf("legacy row: back=%q err=%v", back, err)
	}

	t.Setenv(EnvVar, "new-name-wins")
	if _, err := Decrypt(sealed); err == nil {
		t.Fatal("a value sealed under another key must not open")
	}
	t.Setenv(EnvVar, "")
	t.Setenv(LegacyEnvVar, "")
	if _, err := Decrypt(sealed); err == nil || !strings.Contains(err.Error(), EnvVar) {
		t.Fatalf("no key for an encrypted value: err=%v", err)
	}
}
