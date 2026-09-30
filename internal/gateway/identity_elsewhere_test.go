package gateway

import (
	"context"
	"slices"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// With a database per tenant one identity can have a user row in the
// control plane and in tenant databases. Removing it from one removes the
// person from that database: the identity-provider account stays while
// another database holds the subject, and goes with the last row. Deleting
// it on the word of one tenant's administrator cut the person off every
// other tenant (2026-09-30).
func TestRemovingAnAccountKeepsAnIdentityAnotherDatabaseHolds(t *testing.T) {
	ctx := context.Background()
	control := testdb.New(t, migrationfs.FS, ".")
	router := tenantdb.New(control, tenantdb.Config{
		Migrations: migrationfs.FS, MigrationsDir: ".",
		AdminURL: testdb.AdminDSN(t), IdleTimeout: -1, Log: logger.New("test"),
	})
	t.Cleanup(router.Close)
	b, err := router.Provision(ctx, "Dedicated B", "enterprise")
	if err != nil {
		t.Fatal(err)
	}
	bPool, err := router.Pool(ctx, b.Tenant.CustomerID)
	if err != nil {
		t.Fatal(err)
	}
	bctx := tenantdb.WithScope(ctx, tenantdb.Scope{CustomerID: b.Tenant.CustomerID, Pool: bPool})
	broker := newFakeBroker(t)
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(control, router), devMode: true, kc: broker.client()}

	row := func(ctx context.Context, sub string) string {
		t.Helper()
		var id string
		if err := h.db.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1, $1 || '@elsewhere.test', $1) RETURNING id::text`, sub).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	gone := func(sub string) bool {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		return slices.Contains(broker.deleted, sub)
	}
	for _, sub := range []string{"kc-both", "kc-b-only", "kc-two-dbs"} {
		broker.users[sub] = &fakeKCUser{Email: sub + "@elsewhere.test", Enabled: true}
	}

	// In the control plane and in B's database: removed from B, kept.
	inControl, inB := row(ctx, "kc-both"), row(bctx, "kc-both")
	h.noteUser(bctx, "kc-both", "kc-both@elsewhere.test")
	if err := h.removeUserAccount(bctx, inB, "kc-both"); err != nil {
		t.Fatal(err)
	}
	if gone("kc-both") {
		t.Errorf("removing kc-both from tenant B deleted its identity-provider account; the control plane still holds it")
	}
	// Removed from the control plane too — its last row: deleted.
	if err := h.removeUserAccount(ctx, inControl, "kc-both"); err != nil {
		t.Fatal(err)
	}
	if !gone("kc-both") {
		t.Errorf("kc-both's last row is gone but its identity-provider account stays")
	}

	// In B's database and listed there only: deleted with that row.
	onlyB := row(bctx, "kc-b-only")
	h.noteUser(bctx, "kc-b-only", "kc-b-only@elsewhere.test")
	if err := h.removeUserAccount(bctx, onlyB, "kc-b-only"); err != nil {
		t.Fatal(err)
	}
	if !gone("kc-b-only") {
		t.Errorf("kc-b-only's only row is gone but its identity-provider account stays")
	}

	// In the control plane, and in B's database as the directory says:
	// removed from the control plane, kept.
	twoCtl := row(ctx, "kc-two-dbs")
	row(bctx, "kc-two-dbs")
	h.noteUser(bctx, "kc-two-dbs", "kc-two-dbs@elsewhere.test")
	if err := h.removeUserAccount(ctx, twoCtl, "kc-two-dbs"); err != nil {
		t.Fatal(err)
	}
	if gone("kc-two-dbs") {
		t.Errorf("removing kc-two-dbs from the control plane deleted its identity-provider account; tenant B still holds it")
	}
}
