package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
		IssuedAt: exp.Add(-365 * 24 * time.Hour).UTC(), ExpiresAt: exp,
		Order: "ORD-2026-001", Agreement: "PFA-2026-10", Deployment: "acme-prod",
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
	if got.ID != want.ID || got.Edition != want.Edition || got.Customer != want.Customer || !got.ExpiresAt.Equal(want.ExpiresAt) ||
		got.Order != want.Order || got.Agreement != want.Agreement || got.Deployment != want.Deployment || got.Schedule != LatestSchedule() {
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
		tok, _ := Sign(enterpriseClaims(now.Add(60*24*time.Hour)), priv)
		m := Load(Options{Key: tok, PublicKey: pubB64, Now: clock})
		st := m.Status()
		if st.State != StateActive || st.Edition != EditionEnterprise || st.Source != "env" || st.Customer != "Acme Corp" ||
			st.Order != "ORD-2026-001" || st.Agreement != "PFA-2026-10" || st.Deployment != "acme-prod" || st.Schedule != LatestSchedule() || st.RenewalDue {
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
	t.Run("renewal is due within the notice period", func(t *testing.T) {
		tok, _ := Sign(enterpriseClaims(now.Add(RenewalNotice-time.Hour)), priv)
		st := Load(Options{Key: tok, PublicKey: pubB64, Now: clock}).Status()
		if st.State != StateActive || !st.RenewalDue || st.TransitionEndsAt != nil {
			t.Fatalf("unexpected status %+v", st)
		}
	})
	t.Run("a just-expired key is in transition: usable, not changeable", func(t *testing.T) {
		tok, _ := Sign(enterpriseClaims(now.Add(-time.Minute)), priv)
		m := Load(Options{Key: tok, PublicKey: pubB64, Now: clock})
		st := m.Status()
		if st.State != StateTransition || st.Edition != EditionEnterprise || st.RenewalDue || st.TransitionEndsAt == nil ||
			!st.TransitionEndsAt.Equal(st.ExpiresAt.Add(TransitionPeriod)) || len(st.Features) == 0 {
			t.Fatalf("unexpected status %+v", st)
		}
		if m.Has(FeatureSSO) || !m.Usable(FeatureSSO) {
			t.Error("transition: Has must be false and Usable true")
		}
		if m.RequireUse(FeatureAuditExport) != nil {
			t.Error("transition: reading a paid feature must be allowed")
		}
		err := m.Require(FeatureAuditExport)
		if err == nil || !strings.Contains(err.Error(), "read and export only") || !strings.Contains(err.Error(), "Audit export") {
			t.Errorf("transition Require = %v", err)
		}
	})
	t.Run("after the transition the key drops to community and says so", func(t *testing.T) {
		tok, _ := Sign(enterpriseClaims(now.Add(-TransitionPeriod-time.Minute)), priv)
		m := Load(Options{Key: tok, PublicKey: pubB64, Now: clock})
		st := m.Status()
		if st.State != StateExpired || st.Edition != EditionCommunity || st.Customer != "Acme Corp" || st.ExpiresAt == nil || st.TransitionEndsAt == nil || len(st.Features) != 0 {
			t.Fatalf("unexpected status %+v", st)
		}
		if m.Has(FeatureSSO) || m.Usable(FeatureSSO) {
			t.Error("expired key must not unlock features")
		}
		if err := m.Require(FeatureSSO); err == nil || !strings.Contains(err.Error(), "community edition") {
			t.Errorf("expired Require = %v", err)
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
		if m.Status().State != StateTransition {
			t.Fatal("should enter the transition without reloading")
		}
		tick = now.Add(TransitionPeriod + 2*time.Minute)
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
	t.Run("compiled-in public keys decode", func(t *testing.T) {
		if keys, err := verifierKeys(""); err != nil || len(keys) == 0 {
			t.Fatalf("trusted public keys are unusable: %v", err)
		}
	})
	t.Run("a self-signed key does not verify against the compiled-in keys", func(t *testing.T) {
		tok, _ := Sign(enterpriseClaims(now.Add(time.Hour)), priv)
		st := Load(Options{Key: tok, Now: clock}).Status()
		if st.State != StateInvalid || st.Edition != EditionCommunity {
			t.Fatalf("unexpected status %+v", st)
		}
	})
}

// TestSchedulesAreFrozen holds every released schedule to what was sold
// under it. If this fails, a released schedule was edited: revert it and
// append a new schedule instead (see Schedule).
func TestSchedulesAreFrozen(t *testing.T) {
	released := map[string]map[Edition][]Feature{
		"2026-10": {
			EditionCommercial: {"white_label"},
			EditionEnterprise: {"sso", "scim", "cell_history", "audit_export", "usage_analytics", "white_label", "tenant_ai_keys", "deployment_settings"},
		},
	}
	if len(schedules) < len(released) {
		t.Fatalf("a released schedule was removed: have %d, released %d", len(schedules), len(released))
	}
	for _, s := range schedules {
		want, ok := released[s.ID]
		if !ok {
			t.Errorf("schedule %s is not recorded here: add it to this test when releasing it", s.ID)
			continue
		}
		if len(s.Editions) != len(want) {
			t.Errorf("schedule %s editions changed: %v", s.ID, s.Editions)
		}
		for ed, fs := range want {
			got := s.Editions[ed]
			if strings.Join(featureStrings(got), ",") != strings.Join(featureStrings(fs), ",") {
				t.Errorf("schedule %s %s = %v, released as %v", s.ID, ed, got, fs)
			}
		}
	}
	for i := 1; i < len(schedules); i++ {
		if schedules[i-1].ID >= schedules[i].ID {
			t.Errorf("schedules out of order: %s then %s", schedules[i-1].ID, schedules[i].ID)
		}
	}
	for _, s := range schedules {
		for ed, fs := range s.Editions {
			for _, f := range fs {
				if _, ok := catalog[f]; !ok {
					t.Errorf("schedule %s %s names %q, which is not in the catalogue", s.ID, ed, f)
				}
			}
		}
	}
}

func featureStrings(fs []Feature) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = string(f)
	}
	return out
}

// TestNewFeaturesDoNotExtendOldKeys: a feature that a later schedule adds
// to an edition reaches only keys sold under that schedule.
func TestNewFeaturesDoNotExtendOldKeys(t *testing.T) {
	const later Feature = "forecast_ai"
	savedSchedules, savedCatalog := schedules, catalog
	t.Cleanup(func() { schedules, catalog = savedSchedules, savedCatalog })
	catalog = map[Feature]Info{later: {Key: later, Name: "Forecast AI"}}
	for k, v := range savedCatalog {
		catalog[k] = v
	}
	ent := append(append([]Feature{}, savedSchedules[len(savedSchedules)-1].Editions[EditionEnterprise]...), later)
	schedules = append(append([]Schedule{}, savedSchedules...), Schedule{ID: "2099-01", Editions: map[Edition][]Feature{EditionEnterprise: ent}})

	old := Claims{Edition: EditionEnterprise} // issued before schedules: FirstSchedule
	named := Claims{Edition: EditionEnterprise, Schedule: FirstSchedule}
	newer := Claims{Edition: EditionEnterprise, Schedule: "2099-01"}
	future := Claims{Edition: EditionEnterprise, Schedule: "2100-06"} // sold after this binary
	for name, c := range map[string]Claims{"unnamed": old, "first": named} {
		if slices.Contains(c.EffectiveFeatures(), later) {
			t.Errorf("%s key gained a feature added after it was sold", name)
		}
	}
	for name, c := range map[string]Claims{"newer": newer, "future": future} {
		if !slices.Contains(c.EffectiveFeatures(), later) {
			t.Errorf("%s key lacks a feature its schedule includes", name)
		}
	}
	if _, ok := resolveSchedule("2020-01"); ok {
		t.Error("a schedule older than any released resolved")
	}
}

func TestSignNamesASchedule(t *testing.T) {
	pub, priv := testKeys(t)
	exp := time.Now().Add(time.Hour)
	tok, err := Sign(Claims{Edition: EditionEnterprise, ExpiresAt: exp}, priv)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(tok, pub)
	if err != nil || c.Schedule != LatestSchedule() {
		t.Fatalf("Sign must default to the latest schedule: %+v, %v", c, err)
	}
	if _, err := Sign(Claims{Edition: EditionEnterprise, ExpiresAt: exp, Schedule: "2099-01"}, priv); err == nil {
		t.Error("an unknown schedule was signed")
	}
}

// TestKeysWithLimitsStillVerify: keys issued before 2026-10-07 could carry
// "limits"; they must keep verifying, with the field ignored.
func TestKeysWithLimitsStillVerify(t *testing.T) {
	pub, priv := testKeys(t)
	payload, _ := json.Marshal(map[string]any{
		"id": "old", "edition": "enterprise", "customer": "Acme Corp",
		"issued_at": "2026-09-20T00:00:00Z", "expires_at": "2027-09-20T00:00:00Z",
		"limits": map[string]int64{"max_users": 50},
	})
	body := Prefix + "." + base64.RawURLEncoding.EncodeToString(payload)
	tok := body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(body)))
	c, err := Parse(tok, pub)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.EffectiveFeatures()) != 8 {
		t.Errorf("an old enterprise key must keep its eight features, got %v", c.EffectiveFeatures())
	}
}

// TestParseAnyAcceptsEveryTrustedKey covers signing-key rotation: while two
// public keys are trusted, a licence signed by either verifies.
func TestParseAnyAcceptsEveryTrustedKey(t *testing.T) {
	oldPub, oldPriv := testKeys(t)
	newPub, newPriv := testKeys(t)
	_, strangerPriv := testKeys(t)
	trusted := []ed25519.PublicKey{oldPub, newPub}
	for name, priv := range map[string]ed25519.PrivateKey{"old": oldPriv, "new": newPriv} {
		tok, _ := Sign(enterpriseClaims(time.Now().Add(time.Hour)), priv)
		if _, err := ParseAny(tok, trusted); err != nil {
			t.Errorf("%s signer: %v", name, err)
		}
	}
	tok, _ := Sign(enterpriseClaims(time.Now().Add(time.Hour)), strangerPriv)
	if _, err := ParseAny(tok, trusted); !errors.Is(err, ErrSignature) {
		t.Errorf("stranger signer: got %v, want ErrSignature", err)
	}
	if _, err := ParseAny("MVX1.abc", trusted); !errors.Is(err, ErrMalformed) {
		t.Errorf("malformed: got %v, want ErrMalformed", err)
	}
}
