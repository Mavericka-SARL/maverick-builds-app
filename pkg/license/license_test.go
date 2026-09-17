package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// enterpriseClaims dates the key relative to its expiry, never to the real
// clock: the manager tests run against a fixed fake "now", and a key issued
// by the wall clock is refused as "not yet valid" once the calendar moves
// more than the skew allowance past that fake now (it did, 2026-09-17).
func enterpriseClaims(exp time.Time) Claims {
	return Claims{
		ID: "lic-1", Edition: EditionEnterprise, Customer: "Acme Corp", Contact: "ops@acme.test",
		IssuedAt: exp.Add(-31 * 24 * time.Hour).UTC(), ExpiresAt: exp,
		Limits: map[string]int64{"max_users": 50},
	}
}

func TestSignThenParseRoundTrips(t *testing.T) {
	pub, priv := testKeys(t)
	want := enterpriseClaims(time.Now().Add(365 * 24 * time.Hour).UTC().Truncate(time.Second))
	tok, err := Sign(want, priv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, Prefix+".") || strings.Count(tok, ".") != 2 {
		t.Fatalf("token has unexpected shape: %q", tok)
	}
	got, err := Parse(tok, pub)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.Edition != want.Edition || got.Customer != want.Customer || !got.ExpiresAt.Equal(want.ExpiresAt) || got.Limits["max_users"] != 50 {
		t.Fatalf("claims changed in transit: %+v vs %+v", got, want)
	}
}

func TestParseRejectsTamperingAndWrongKey(t *testing.T) {
	pub, priv := testKeys(t)
	otherPub, _ := testKeys(t)
	tok, err := Sign(enterpriseClaims(time.Now().Add(time.Hour)), priv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(tok, otherPub); !errors.Is(err, ErrSignature) {
		t.Errorf("wrong key: got %v, want ErrSignature", err)
	}
	parts := strings.Split(tok, ".")
	// Flip one byte of the payload: same length, different content.
	payload := []byte(parts[1])
	if payload[3] == 'A' {
		payload[3] = 'B'
	} else {
		payload[3] = 'A'
	}
	tampered := parts[0] + "." + string(payload) + "." + parts[2]
	if _, err := Parse(tampered, pub); !errors.Is(err, ErrSignature) {
		t.Errorf("tampered payload: got %v, want ErrSignature", err)
	}
	// A different prefix under the same signature must not verify either.
	if _, err := Parse("MVX2."+parts[1]+"."+parts[2], pub); !errors.Is(err, ErrMalformed) {
		t.Errorf("foreign prefix: got %v, want ErrMalformed", err)
	}
	for _, bad := range []string{"", "MVX1", "MVX1.abc", "not.a.token.at.all", "MVX1.!!!.###"} {
		if _, err := Parse(bad, pub); !errors.Is(err, ErrMalformed) {
			t.Errorf("%q: got %v, want ErrMalformed", bad, err)
		}
	}
}

func TestSignRefusesCommunityAndMissingExpiry(t *testing.T) {
	_, priv := testKeys(t)
	if _, err := Sign(Claims{Edition: EditionCommunity, ExpiresAt: time.Now()}, priv); !errors.Is(err, ErrEdition) {
		t.Errorf("community key: got %v, want ErrEdition", err)
	}
	if _, err := Sign(Claims{Edition: "platinum", ExpiresAt: time.Now()}, priv); !errors.Is(err, ErrEdition) {
		t.Errorf("unknown edition: got %v, want ErrEdition", err)
	}
	if _, err := Sign(Claims{Edition: EditionEnterprise}, priv); err == nil {
		t.Error("missing expiry accepted")
	}
}

func TestEditionFeatureDefaults(t *testing.T) {
	ent := Claims{Edition: EditionEnterprise}
	if got := ent.EffectiveFeatures(); len(got) != len(Catalog()) {
		t.Fatalf("enterprise should unlock every catalog feature, got %v", got)
	}
	com := Claims{Edition: EditionCommercial}
	if got := com.EffectiveFeatures(); len(got) != 1 || got[0] != FeatureWhiteLabel {
		t.Fatalf("commercial defaults = %v, want [white_label]", got)
	}
	// Extras add known features and drop unknown ones.
	com.Features = []Feature{FeatureSSO, "time_travel"}
	got := com.EffectiveFeatures()
	if len(got) != 2 || got[0] != FeatureSSO || got[1] != FeatureWhiteLabel {
		t.Fatalf("commercial + extras = %v, want [sso white_label]", got)
	}
	if RequiredEdition(FeatureSSO) != EditionEnterprise || RequiredEdition(FeatureWhiteLabel) != EditionCommercial {
		t.Error("required editions are wrong")
	}
	if msg := UnavailableError(FeatureSSO, EditionCommunity).Error(); !strings.Contains(msg, "Single sign-on") || !strings.Contains(msg, "enterprise") || !strings.Contains(msg, "community") {
		t.Errorf("unhelpful message: %s", msg)
	}
}

func TestManagerStates(t *testing.T) {
	pub, priv := testKeys(t)
	pubB64 := EncodeKey(pub)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	t.Run("no key is community", func(t *testing.T) {
		st := Load(Options{Now: clock}).Status()
		if st.Edition != EditionCommunity || st.State != StateCommunity || st.Source != "none" || len(st.Features) != 0 || len(st.Catalog) == 0 {
			t.Fatalf("unexpected status %+v", st)
		}
	})
	t.Run("nil manager is community", func(t *testing.T) {
		var m *Manager
		if m.Edition() != EditionCommunity || m.Has(FeatureSSO) || m.Require(FeatureSSO) == nil {
			t.Fatal("nil manager must behave as community")
		}
	})
	t.Run("valid key from env", func(t *testing.T) {
		tok, _ := Sign(enterpriseClaims(now.Add(30*24*time.Hour)), priv)
		m := Load(Options{Key: tok, PublicKey: pubB64, Now: clock})
		st := m.Status()
		if st.State != StateActive || st.Edition != EditionEnterprise || st.Source != "env" || st.Customer != "Acme Corp" || st.Limits["max_users"] != 50 {
			t.Fatalf("unexpected status %+v", st)
		}
		if !m.Has(FeatureAuditExport) || m.Require(FeatureAuditExport) != nil {
			t.Error("enterprise key should unlock audit export")
		}
	})
	t.Run("valid key from file", func(t *testing.T) {
		tok, _ := Sign(Claims{Edition: EditionCommercial, Customer: "Beta GmbH", IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}, priv)
		path := filepath.Join(t.TempDir(), "mavericks.license")
		if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		m := Load(Options{File: path, PublicKey: pubB64, Now: clock})
		st := m.Status()
		if st.State != StateActive || st.Edition != EditionCommercial || st.Source != "file" {
			t.Fatalf("unexpected status %+v", st)
		}
		if !m.Has(FeatureWhiteLabel) || m.Has(FeatureSSO) {
			t.Error("commercial key should unlock white-label only")
		}
		if err := m.Require(FeatureSSO); err == nil || !strings.Contains(err.Error(), "commercial edition") {
			t.Errorf("Require(sso) = %v", err)
		}
	})
	t.Run("missing file is invalid, not community", func(t *testing.T) {
		st := Load(Options{File: filepath.Join(t.TempDir(), "nope"), PublicKey: pubB64, Now: clock}).Status()
		if st.State != StateInvalid || st.Edition != EditionCommunity || st.Error == "" {
			t.Fatalf("unexpected status %+v", st)
		}
	})
	t.Run("expired key drops to community and says so", func(t *testing.T) {
		tok, _ := Sign(enterpriseClaims(now.Add(-time.Minute)), priv)
		m := Load(Options{Key: tok, PublicKey: pubB64, Now: clock})
		st := m.Status()
		if st.State != StateExpired || st.Edition != EditionCommunity || st.Customer != "Acme Corp" || st.ExpiresAt == nil {
			t.Fatalf("unexpected status %+v", st)
		}
		if m.Has(FeatureSSO) {
			t.Error("expired key must not unlock features")
		}
	})
	t.Run("expiry is evaluated at call time", func(t *testing.T) {
		tick := now
		tok, _ := Sign(enterpriseClaims(now.Add(time.Minute)), priv)
		m := Load(Options{Key: tok, PublicKey: pubB64, Now: func() time.Time { return tick }})
		if m.Status().State != StateActive {
			t.Fatal("should be active before expiry")
		}
		tick = now.Add(2 * time.Minute)
		if m.Status().State != StateExpired {
			t.Fatal("should flip to expired without reloading")
		}
	})
	t.Run("not yet valid", func(t *testing.T) {
		c := enterpriseClaims(now.Add(48 * time.Hour))
		c.IssuedAt = now.Add(3 * 24 * time.Hour)
		tok, _ := Sign(c, priv)
		st := Load(Options{Key: tok, PublicKey: pubB64, Now: clock}).Status()
		if st.State != StateInvalid || !strings.Contains(st.Error, "not valid before") {
			t.Fatalf("unexpected status %+v", st)
		}
	})
	t.Run("wrong verifier key is invalid", func(t *testing.T) {
		tok, _ := Sign(enterpriseClaims(now.Add(time.Hour)), priv)
		other, _ := testKeys(t)
		st := Load(Options{Key: tok, PublicKey: EncodeKey(other), Now: clock}).Status()
		if st.State != StateInvalid || !strings.Contains(st.Error, "signature") {
			t.Fatalf("unexpected status %+v", st)
		}
	})
	t.Run("compiled-in public key decodes", func(t *testing.T) {
		if _, err := DecodePublicKey(defaultPublicKeyB64); err != nil {
			t.Fatalf("default public key is unusable: %v", err)
		}
	})
}
