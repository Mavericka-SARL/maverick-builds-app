package plan

import (
	"context"
	"fmt"
	"slices"

	"github.com/mavericks-engine/mavericks/pkg/license"
)

// Which editions a tenant can be on. In the hosted Playground (PLAYGROUND)
// every edition of the catalog: there they are the hosted offering's tiers.
// Anywhere else the licence key decides the edition the deployment runs,
// and a tenant's edition is at most that one — a deployment with no key, the
// Community Edition, has Community tenants only (owner, 2026-11-21: a
// self-hosted deployment must not offer Commercial or Enterprise).

// rank is key's place among the editions, or -1 when key names none.
func rank(key string) int {
	return slices.Index(license.Editions, license.Edition(key))
}

// Allowed reports whether a tenant can be put on the edition key.
func Allowed(key string, playground bool, deployment license.Edition) bool {
	if playground {
		return true
	}
	r := rank(key)
	return r >= 0 && r <= rank(string(deployment))
}

// Offered is the part of plans a tenant can be put on (Allowed), in order.
func Offered(plans []Plan, playground bool, deployment license.Edition) []Plan {
	out := make([]Plan, 0, len(plans))
	for _, p := range plans {
		if Allowed(p.Key, playground, deployment) {
			out = append(out, p)
		}
	}
	return out
}

// DefaultKey is the edition a tenant an administrator creates starts on when
// none is named (owner, 2026-11-21): in the Playground the Community Edition,
// as a sign-up; anywhere else the deployment's own edition — Community
// without a key, Commercial or Enterprise with one.
func DefaultKey(playground bool, deployment license.Edition) string {
	if playground {
		return string(license.EditionCommunity)
	}
	return string(deployment)
}

// NotAllowedError is the refusal for an edition a tenant cannot be put on.
func NotAllowedError(key string, deployment license.Edition) error {
	return fmt.Errorf("this deployment runs the %s Edition, so its tenants can be on that edition or below, not %q; "+
		"a licence key (Platform › License) adds the Commercial or Enterprise Edition", editionName(deployment), key)
}

func editionName(e license.Edition) string {
	switch e {
	case license.EditionCommercial:
		return "Commercial"
	case license.EditionEnterprise:
		return "Enterprise"
	case license.EditionCommunity:
	}
	return "Community"
}

// CapTenants moves every tenant whose edition is above the deployment's down
// to it, in db's core.customer and — in dedicated mode, where db is the
// control plane — its tenant directory, platform.tenant_database. It is the
// start-up half of Allowed for a deployment outside the Playground: tenants
// created before it, when an administrator could pick any edition, and the
// tenants of a key that was taken away. moved counts the core.customer rows.
func CapTenants(ctx context.Context, db DB, deployment license.Edition, directory bool) (moved int64, err error) {
	r := rank(string(deployment))
	if r < 0 {
		return 0, fmt.Errorf("unknown edition %q", deployment)
	}
	above := make([]string, 0, len(license.Editions))
	for _, e := range license.Editions[r+1:] {
		above = append(above, string(e))
	}
	if len(above) == 0 {
		return 0, nil
	}
	tag, err := db.Exec(ctx, `UPDATE core.customer SET plan = $1, updated_at = now() WHERE plan = ANY($2)`, string(deployment), above)
	if err != nil {
		return 0, err
	}
	if directory {
		if _, err := db.Exec(ctx, `UPDATE platform.tenant_database SET plan = $1, updated_at = now() WHERE plan = ANY($2)`, string(deployment), above); err != nil {
			return tag.RowsAffected(), err
		}
	}
	return tag.RowsAffected(), nil
}
