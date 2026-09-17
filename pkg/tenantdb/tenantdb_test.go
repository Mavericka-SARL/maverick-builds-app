package tenantdb_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

func newRouter(t *testing.T) (*tenantdb.Router, *pgxpool.Pool) {
	t.Helper()
	control := testdb.New(t, migrationfs.FS, ".")
	r := tenantdb.New(control, tenantdb.Config{
		Migrations:    migrationfs.FS,
		MigrationsDir: ".",
		AdminURL:      testdb.AdminDSN(t),
		IdleTimeout:   -1,
	})
	t.Cleanup(r.Close)
	return r, control
}

// The whole point: a provisioned tenant has its own database with the full
// schema and exactly one customer, and routing through the handle reaches it
// while the control plane stays untouched.
func TestProvisionRoutesAndIsolates(t *testing.T) {
	ctx := context.Background()
	r, control := newRouter(t)

	p, err := r.Provision(ctx, "Acme Corp", "enterprise")
	if err != nil {
		t.Fatal(err)
	}
	if p.Tenant.Status != tenantdb.StatusReady || p.WorkspaceID == "" || p.Tenant.Database != tenantdb.DatabaseName(p.Tenant.CustomerID) {
		t.Fatalf("unexpected provision result %+v", p)
	}

	pool, err := r.Pool(ctx, p.Tenant.CustomerID)
	if err != nil {
		t.Fatal(err)
	}
	var name, dbName string
	if err := pool.QueryRow(ctx, `SELECT name, current_database() FROM core.customer WHERE id = $1::uuid`, p.Tenant.CustomerID).Scan(&name, &dbName); err != nil {
		t.Fatalf("tenant database has no customer row: %v", err)
	}
	if name != "Acme Corp" || dbName != p.Tenant.Database {
		t.Fatalf("customer %q in %q", name, dbName)
	}
	var inControl int
	_ = control.QueryRow(ctx, `SELECT count(*) FROM core.customer WHERE id = $1::uuid`, p.Tenant.CustomerID).Scan(&inControl)
	if inControl != 0 {
		t.Fatal("the tenant's customer row leaked into the control plane")
	}

	// The handle dispatches on the context.
	h := tenantdb.NewHandle(control, r)
	routed := tenantdb.WithScope(ctx, tenantdb.Scope{CustomerID: p.Tenant.CustomerID, Pool: pool})
	if _, err := h.Exec(routed, `INSERT INTO core.application (customer_id, name, mode) VALUES ($1::uuid, 'Planning', 'planning')`, p.Tenant.CustomerID); err != nil {
		t.Fatal(err)
	}
	var inTenant int
	_ = h.QueryRow(routed, `SELECT count(*) FROM core.application`).Scan(&inTenant)
	_ = h.QueryRow(ctx, `SELECT count(*) FROM core.application`).Scan(&inControl)
	if inTenant != 1 || inControl != 0 {
		t.Fatalf("application rows: tenant=%d control=%d", inTenant, inControl)
	}
	if h.For(ctx) != control || h.For(routed) != pool || tenantdb.TenantFrom(routed) != p.Tenant.CustomerID || tenantdb.TenantFrom(ctx) != "" {
		t.Fatal("handle routing is wrong")
	}
	// A scope survives WithoutCancel, which is how background work is spawned.
	if h.For(context.WithoutCancel(routed)) != pool {
		t.Fatal("scope lost across WithoutCancel")
	}

	// Without a router nothing routes, because nothing sets a scope: an
	// unrouted context always reaches the control plane.
	plain := tenantdb.NewHandle(control, nil)
	if plain.Dedicated() || plain.For(ctx) != control {
		t.Fatal("a router-less handle must use the control pool")
	}
}

func TestDirectory(t *testing.T) {
	ctx := context.Background()
	r, _ := newRouter(t)
	cat := r.Catalog()
	a, err := r.Provision(ctx, "A", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Provision(ctx, "B", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Tenant.Plan != "starter" {
		t.Fatalf("default plan = %q", a.Tenant.Plan)
	}
	if err := cat.AddUser(ctx, "sub-1", a.Tenant.CustomerID, "one@a.test"); err != nil {
		t.Fatal(err)
	}
	if err := cat.AddUser(ctx, "sub-1", a.Tenant.CustomerID, "one@a.test"); err != nil {
		t.Fatalf("AddUser must be idempotent: %v", err)
	}
	if err := cat.AddUser(ctx, "sub-2", b.Tenant.CustomerID, "two@b.test"); err != nil {
		t.Fatal(err)
	}
	got, _ := cat.TenantsForUser(ctx, "sub-1")
	if len(got) != 1 || got[0] != a.Tenant.CustomerID {
		t.Fatalf("sub-1 tenants = %v", got)
	}
	if got, _ := cat.TenantsForUser(ctx, "nobody"); len(got) != 0 {
		t.Fatalf("unknown sub tenants = %v", got)
	}
	if err := cat.AddApplication(ctx, "0b1e1b5e-0000-4000-8000-000000000001", b.Tenant.CustomerID); err != nil {
		t.Fatal(err)
	}
	if id, err := cat.TenantForApplication(ctx, "0b1e1b5e-0000-4000-8000-000000000001"); err != nil || id != b.Tenant.CustomerID {
		t.Fatalf("app → tenant = %q, %v", id, err)
	}
	if _, err := cat.TenantForApplication(ctx, "0b1e1b5e-0000-4000-8000-000000000002"); !errors.Is(err, tenantdb.ErrNotFound) {
		t.Fatalf("unknown app: %v", err)
	}
	list, err := cat.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %v, %v", list, err)
	}

	// Deprovision drops the database and, by cascade, the directory rows.
	if err := r.Deprovision(ctx, b.Tenant.CustomerID); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Get(ctx, b.Tenant.CustomerID); !errors.Is(err, tenantdb.ErrNotFound) {
		t.Fatalf("deprovisioned tenant still in catalog: %v", err)
	}
	if _, err := cat.TenantForApplication(ctx, "0b1e1b5e-0000-4000-8000-000000000001"); !errors.Is(err, tenantdb.ErrNotFound) {
		t.Fatal("application directory row survived deprovisioning")
	}
	if _, err := r.Pool(ctx, b.Tenant.CustomerID); err == nil {
		t.Fatal("pool for a dropped tenant should fail")
	}
	var exists bool
	_ = r.Control().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, b.Tenant.Database).Scan(&exists)
	if exists {
		t.Fatal("database still exists after Deprovision")
	}
}

func TestMigrateAllAndWatch(t *testing.T) {
	ctx := context.Background()
	r, _ := newRouter(t)
	a, err := r.Provision(ctx, "A", "")
	if err != nil {
		t.Fatal(err)
	}
	// MigrateAll on an up-to-date tenant is a no-op that keeps it ready.
	if err := r.MigrateAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Catalog().Get(ctx, a.Tenant.CustomerID); got.Status != tenantdb.StatusReady {
		t.Fatalf("status after MigrateAll = %s", got.Status)
	}
	// A failed tenant is skipped by Each and Pool refuses it.
	if err := r.Catalog().SetStatus(ctx, a.Tenant.CustomerID, tenantdb.StatusFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	r.Forget(a.Tenant.CustomerID)
	visited := 0
	_ = r.Each(ctx, func(tenantdb.Tenant, *pgxpool.Pool) error { visited++; return nil })
	if visited != 0 {
		t.Fatal("Each visited a failed tenant")
	}
	if _, err := r.Pool(ctx, a.Tenant.CustomerID); err == nil {
		t.Fatal("Pool must refuse a failed tenant")
	}
	_ = r.Catalog().SetStatus(ctx, a.Tenant.CustomerID, tenantdb.StatusReady, "")

	// Watch starts once per ready tenant, including ones that appear later.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	started := map[string]int{}
	r.Watch(wctx, 50*time.Millisecond, func(t tenantdb.Tenant, _ *pgxpool.Pool) {
		mu.Lock()
		started[t.CustomerID]++
		mu.Unlock()
	})
	b, err := r.Provision(ctx, "B", "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		ok := started[a.Tenant.CustomerID] == 1 && started[b.Tenant.CustomerID] == 1
		mu.Unlock()
		if ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if started[a.Tenant.CustomerID] != 1 || started[b.Tenant.CustomerID] != 1 {
		t.Fatalf("watch starts = %v", started)
	}
}
