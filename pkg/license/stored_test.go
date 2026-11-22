package license

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A key installed in the console (UseStored) is in force at once and can be
// taken away; Check refuses what installing should not accept; a key from
// the configuration wins over the console and cannot be replaced from it.
func TestStoredKey(t *testing.T) {
	pub, priv := testKeys(t)
	pubB64 := EncodeKey(pub)
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	sign := func(c Claims) string {
		t.Helper()
		tok, err := Sign(c, priv)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	commercial := enterpriseClaims(now.Add(365 * 24 * time.Hour))
	commercial.ID, commercial.Edition = "lic-c", EditionCommercial
	good := sign(commercial)

	m := Load(Options{PublicKey: pubB64, Now: clock})
	if m.FromConfig() || m.Stored() != nil || m.Status().Source != "none" {
		t.Fatalf("keyless manager: config=%v stored=%v %+v", m.FromConfig(), m.Stored(), m.Status())
	}

	// What Check refuses.
	_, otherPriv := testKeys(t)
	foreign, err := Sign(commercial, otherPriv)
	if err != nil {
		t.Fatal(err)
	}
	expired := enterpriseClaims(now.Add(-time.Hour))
	for name, tok := range map[string]string{
		"empty":            "  ",
		"not a key":        "hello",
		"another signer":   foreign,
		"already expired":  sign(expired),
		"tampered payload": strings.Replace(good, ".", ".x", 1),
	} {
		if _, err := m.Check(tok); err == nil {
			t.Errorf("Check(%s) accepted", name)
		}
	}
	if _, err := m.Check(sign(expired)); err == nil || !strings.Contains(err.Error(), "expired on") {
		t.Errorf("an expired key's refusal says when: %v", err)
	}
	// …but a stored key read back after it expired still loads (Verify),
	// so its transition runs.
	if _, err := m.Verify(sign(expired)); err != nil {
		t.Errorf("Verify of an expired key: %v", err)
	}

	// Install, then remove.
	claims, err := m.Check("\n" + good + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.UseStored(claims); err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if st.Edition != EditionCommercial || st.State != StateActive || st.Source != "console" || st.LicenseID != "lic-c" || m.Stored() == nil {
		t.Fatalf("after install: %+v", st)
	}
	if err := m.UseStored(nil); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); st.Edition != EditionCommunity || st.Source != "none" || m.Stored() != nil {
		t.Fatalf("after removal: %+v", st)
	}

	// A configured key wins: the console can neither replace nor remove it.
	ent := enterpriseClaims(now.Add(365 * 24 * time.Hour))
	cm := Load(Options{Key: sign(ent), PublicKey: pubB64, Now: clock})
	if !cm.FromConfig() {
		t.Fatal("a configured key is not reported as such")
	}
	if err := cm.UseStored(claims); !errors.Is(err, ErrConfigured) {
		t.Fatalf("install over a configured key: %v", err)
	}
	if err := cm.UseStored(nil); !errors.Is(err, ErrConfigured) {
		t.Fatalf("removal of a configured key: %v", err)
	}
	if st := cm.Status(); st.Edition != EditionEnterprise || st.Source != "env" || cm.Stored() != nil {
		t.Fatalf("configured key after refused changes: %+v", st)
	}
	// A configured file that cannot be read still counts as configured.
	if fm := Load(Options{File: "/nonexistent/key", PublicKey: pubB64, Now: clock}); !fm.FromConfig() {
		t.Fatal("a configured file is not reported as configured")
	}
}
