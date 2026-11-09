package sso

import (
	"context"
	"strings"

	"github.com/mavericks-engine/mavericks/pkg/keycloak"
)

// ReconcileBroker is what Reconcile needs of the identity provider.
type ReconcileBroker interface {
	ListIdentityProviders(ctx context.Context) ([]keycloak.IdentityProvider, error)
	PutIdentityProvider(ctx context.Context, idp keycloak.IdentityProvider) error
}

// licenceOffMark is set on a provider Reconcile switched off, so that it
// switches back on only those: one a tenant admin had switched off stays off.
const licenceOffMark = "mvxOffWithoutLicence"

// Reconcile makes the tenants' identity providers (alias "mvx-…") follow the
// licence: switched off in Keycloak while single sign-on is not usable, and
// back on when a key that includes it returns. The gateway's other checks
// stop discovery and first-login provisioning without a key, but a person
// already linked to a provider still signed in through it with a URL that
// named it (kc_idp_hint), so single sign-on kept working on a Community
// deployment (2026-10-14). It returns how many providers it changed.
func Reconcile(ctx context.Context, broker ReconcileBroker, usable bool) (int, error) {
	idps, err := broker.ListIdentityProviders(ctx)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, idp := range idps {
		if !strings.HasPrefix(idp.Alias, "mvx-") {
			continue // a provider the self-hoster added by hand is Keycloak's own
		}
		if idp.Config == nil {
			idp.Config = map[string]string{}
		}
		marked := idp.Config[licenceOffMark] == "true"
		switch {
		case !usable && idp.Enabled:
			idp.Enabled = false
			idp.Config[licenceOffMark] = "true"
		case usable && marked:
			idp.Enabled = true
			delete(idp.Config, licenceOffMark)
		default:
			continue
		}
		if err := broker.PutIdentityProvider(ctx, idp); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}
