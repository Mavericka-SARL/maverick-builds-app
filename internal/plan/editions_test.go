package plan

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/license"
)

// Outside the Playground a tenant's edition is at most the deployment's: a
// deployment with no licence key offers the Community Edition only. The
// Playground offers every edition of its catalog.
func TestEditionsOffered(t *testing.T) {
	catalog := []Plan{{Key: "community"}, {Key: "commercial"}, {Key: "enterprise"}, {Key: "legacy-gold"}}
	keys := func(ps []Plan) (out []string) {
		for _, p := range ps {
			out = append(out, p.Key)
		}
		return out
	}
	for _, tc := range []struct {
		playground bool
		deployment license.Edition
		want       []string
		def        string
	}{
		{false, license.EditionCommunity, []string{"community"}, "community"},
		{false, license.EditionCommercial, []string{"community", "commercial"}, "commercial"},
		{false, license.EditionEnterprise, []string{"community", "commercial", "enterprise"}, "enterprise"},
		{true, license.EditionCommunity, []string{"community", "commercial", "enterprise", "legacy-gold"}, "community"},
	} {
		got := keys(Offered(catalog, tc.playground, tc.deployment))
		if len(got) != len(tc.want) {
			t.Fatalf("playground=%v %s: offered %v, want %v", tc.playground, tc.deployment, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("playground=%v %s: offered %v, want %v", tc.playground, tc.deployment, got, tc.want)
			}
		}
		if d := DefaultKey(tc.playground, tc.deployment); d != tc.def {
			t.Fatalf("playground=%v %s: default %q, want %q", tc.playground, tc.deployment, d, tc.def)
		}
		if !Allowed(tc.def, tc.playground, tc.deployment) {
			t.Fatalf("playground=%v %s: the default %q is not allowed", tc.playground, tc.deployment, tc.def)
		}
	}
}

// CapTenants moves the tenants above the deployment's edition down to it,
// in core.customer and the tenant directory, and leaves the rest alone.
func TestCapTenants(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	ids := map[string]string{}
	for _, p := range []string{"community", "commercial", "enterprise", "legacy-gold"} {
		ids[p] = q(`INSERT INTO core.customer (name, plan) VALUES ($1, $1) RETURNING id::text`, p)
		if _, err := pool.Exec(ctx, `INSERT INTO platform.tenant_database (customer_id, name, plan, database, status)
			VALUES ($1::uuid, $2, $2, 'tenant_'||$2, 'ready')`, ids[p], p); err != nil {
			t.Fatal(err)
		}
	}
	planOf := func(id string) (customer, directory string) {
		return q(`SELECT plan FROM core.customer WHERE id = $1::uuid`, id), q(`SELECT plan FROM platform.tenant_database WHERE customer_id = $1::uuid`, id)
	}

	// A Commercial key: only the Enterprise tenant is above it.
	if moved, err := CapTenants(ctx, pool, license.EditionCommercial, true); err != nil || moved != 1 {
		t.Fatalf("cap at commercial: moved %d, %v; want 1", moved, err)
	}
	if c, dir := planOf(ids["enterprise"]); c != "commercial" || dir != "commercial" {
		t.Fatalf("enterprise tenant under a Commercial key: %s / %s", c, dir)
	}

	// No key: everything above Community moves; an edition the catalog does
	// not rank is not this rule's to touch.
	if moved, err := CapTenants(ctx, pool, license.EditionCommunity, true); err != nil || moved != 2 {
		t.Fatalf("cap at community: moved %d, %v; want 2", moved, err)
	}
	for p, want := range map[string]string{"community": "community", "commercial": "community", "enterprise": "community", "legacy-gold": "legacy-gold"} {
		if c, dir := planOf(ids[p]); c != want || dir != want {
			t.Fatalf("%s tenant without a key: %s / %s, want %s", p, c, dir, want)
		}
	}
	if moved, err := CapTenants(ctx, pool, license.EditionCommunity, true); err != nil || moved != 0 {
		t.Fatalf("second cap: moved %d, %v; want nothing to do", moved, err)
	}
	// An Enterprise key leaves every tenant where it is.
	if moved, err := CapTenants(ctx, pool, license.EditionEnterprise, true); err != nil || moved != 0 {
		t.Fatalf("cap at enterprise: moved %d, %v", moved, err)
	}
}
