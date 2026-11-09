package sso

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/pkg/keycloak"
)

type fakeIdPs struct {
	idps map[string]keycloak.IdentityProvider
}

func (f *fakeIdPs) ListIdentityProviders(context.Context) ([]keycloak.IdentityProvider, error) {
	var out []keycloak.IdentityProvider
	for _, v := range f.idps {
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeIdPs) PutIdentityProvider(_ context.Context, idp keycloak.IdentityProvider) error {
	f.idps[idp.Alias] = idp
	return nil
}

// Without a key that includes single sign-on, the tenants' providers are
// switched off in Keycloak — a linked person signed in through one with a
// URL that named it (2026-10-14) — and back on when a key returns. A
// provider a tenant admin had switched off, or one the self-hoster added by
// hand, is left as it is.
func TestReconcileFollowsTheLicence(t *testing.T) {
	f := &fakeIdPs{idps: map[string]keycloak.IdentityProvider{
		"mvx-on":     {Alias: "mvx-on", Enabled: true, Config: map[string]string{"clientSecret": keycloak.MaskedSecret}},
		"mvx-off":    {Alias: "mvx-off", Enabled: false},
		"corp-azure": {Alias: "corp-azure", Enabled: true},
	}}
	if n, err := Reconcile(context.Background(), f, false); err != nil || n != 1 {
		t.Fatalf("without the licence: changed %d, %v; want 1", n, err)
	}
	if f.idps["mvx-on"].Enabled || !f.idps["corp-azure"].Enabled || f.idps["mvx-off"].Enabled {
		t.Fatalf("without the licence: %+v", f.idps)
	}
	if n, _ := Reconcile(context.Background(), f, false); n != 0 {
		t.Errorf("a second pass changed %d", n)
	}
	if n, err := Reconcile(context.Background(), f, true); err != nil || n != 1 {
		t.Fatalf("with the licence again: changed %d, %v; want 1", n, err)
	}
	on := f.idps["mvx-on"]
	if !on.Enabled || on.Config[licenceOffMark] != "" || on.Config["clientSecret"] != keycloak.MaskedSecret {
		t.Errorf("mvx-on after the key returned: %+v", on)
	}
	if f.idps["mvx-off"].Enabled {
		t.Error("a provider switched off by its tenant was switched on")
	}
}
