package migrations_test

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/migrations"
)

// TestBackfill102AccountTenant applies migrations 001–101 to an empty
// database, applies 102 there (a no-op on an empty database, as on a
// dedicated tenant database just provisioned), writes accounts with no tenant
// the way a platform admin's invitation left them, and applies 102 again: an
// account whose every role — workspace role, business-role membership,
// application or model grant — lies in one tenant gets that tenant; a
// platform admin, an account with developer or tenant_admin without a
// workspace, one whose roles lie in two tenants or in none, one the directory
// lists in a tenant database, and one that has a tenant already keep theirs. It then re-applies 102, which must change
// nothing.
func TestBackfill102AccountTenant(t *testing.T) {
	ctx := context.Background()
	conn, apply := migratedTo101(t, fmt.Sprintf("backfill102_%d", time.Now().UnixNano()))
	// An empty database: nothing to do, nothing to fail on.
	apply(backfill)

	id := func(sql string, args ...any) string {
		t.Helper()
		var out string
		if err := conn.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
		return out
	}
	c1 := id(`INSERT INTO core.customer (name) VALUES ('Tenant one') RETURNING id::text`)
	c2 := id(`INSERT INTO core.customer (name) VALUES ('Tenant two') RETURNING id::text`)
	ws := func(c string) string {
		return id(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'W') RETURNING id::text`, c)
	}
	w1, w1b, w2 := ws(c1), ws(c1), ws(c2)
	app1 := id(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App one', 'planning') RETURNING id::text`, w1, c1)
	appT := id(`INSERT INTO core.application (customer_id, name, mode) VALUES ($1::uuid, 'Tenant-level app', 'planning') RETURNING id::text`, c1)
	app2 := id(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App two', 'planning') RETURNING id::text`, w2, c2)
	m2 := id(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Model two') RETURNING id::text`, app2)
	role1 := id(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Approvers') RETURNING id::text`, w1)

	account := func(sub, cust string) string {
		return id(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1||'@b102.test', $1, NULLIF($2,'')::uuid) RETURNING id::text`, sub, cust)
	}
	grant := func(user, role, ws string) {
		t.Helper()
		if _, err := conn.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2::identity.user_role, NULLIF($3,'')::uuid)`, user, role, ws); err != nil {
			t.Fatalf("grant %s@%s: %v", role, ws, err)
		}
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	want := map[string]string{} // account → its tenant after 102 ("" = none)
	u := account("ws-role", "")
	grant(u, "business_user", w1)
	want[u] = c1
	u = account("two-workspaces-one-tenant", "")
	grant(u, "business_user", w1)
	grant(u, "business_admin", w1b)
	want[u] = c1
	u = account("workspace-developer", "")
	grant(u, "developer", w1)
	want[u] = c1
	u = account("business-role-member", "")
	exec(`INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, role1, u)
	want[u] = c1
	u = account("app-grant", "")
	exec(`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, u, app1)
	want[u] = c1
	u = account("tenant-level-app-grant", "")
	exec(`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, u, appT)
	want[u] = c1
	u = account("model-grant", "")
	exec(`INSERT INTO identity.user_model_access (user_id, model_id) VALUES ($1::uuid, $2::uuid)`, u, m2)
	want[u] = c2
	// An unscoped business role grants nothing and places the account
	// nowhere; it does not stop the account taking its one tenant.
	u = account("inert-unscoped-business-role", "")
	grant(u, "business_user", "")
	grant(u, "business_user", w2)
	want[u] = c2

	u = account("two-tenants", "")
	grant(u, "business_user", w1)
	exec(`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, u, app2)
	want[u] = ""
	u = account("no-roles", "")
	want[u] = ""
	u = account("platform-admin", "")
	grant(u, "platform_admin", "")
	grant(u, "business_user", w1)
	want[u] = ""
	u = account("unscoped-developer-narrowed", "")
	grant(u, "developer", "")
	exec(`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, u, app1)
	want[u] = ""
	u = account("unscoped-tenant-admin", "")
	grant(u, "tenant_admin", "")
	grant(u, "business_admin", w1)
	want[u] = ""
	u = account("has-a-tenant", c2)
	grant(u, "business_user", w1)
	want[u] = c2
	// An identity the directory lists in a tenant database holds roles
	// there this database cannot see: its roles here are not all it has.
	u = account("also-in-a-tenant-database", "")
	grant(u, "business_user", w1)
	dedicated := id(`INSERT INTO platform.tenant_database (customer_id, name, database, status)
		VALUES (gen_random_uuid(), 'Dedicated', 'tenant_dedicated_b102', 'ready') RETURNING customer_id::text`)
	exec(`INSERT INTO platform.user_directory (keycloak_sub, customer_id, email)
		VALUES ('also-in-a-tenant-database', $1::uuid, 'also-in-a-tenant-database@b102.test')`, dedicated)
	want[u] = ""

	apply(backfill)
	tenants := func() map[string]string {
		t.Helper()
		rows, err := conn.Query(ctx, `SELECT id::text, COALESCE(customer_id::text, '') FROM identity.user`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var id, cust string
			if err := rows.Scan(&id, &cust); err != nil {
				t.Fatal(err)
			}
			out[id] = cust
		}
		return out
	}
	subs := map[string]string{}
	rows, err := conn.Query(ctx, `SELECT id::text, keycloak_sub FROM identity.user`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, sub string
		if err := rows.Scan(&id, &sub); err != nil {
			t.Fatal(err)
		}
		subs[id] = sub
	}
	rows.Close()
	after := tenants()
	for u, w := range want {
		if after[u] != w {
			t.Errorf("%s: tenant %q, want %q", subs[u], after[u], w)
		}
	}

	// Re-runnable: pkg/migrate applies a file and records it in two separate
	// statements, so a crash between them re-applies 102 on the next start.
	apply(backfill)
	again := tenants()
	for u, c := range after {
		if again[u] != c {
			t.Errorf("re-applying 102 changed %s's tenant: %q → %q", subs[u], c, again[u])
		}
	}
}

// In database-per-tenant mode each tenant database is migrated on its own and
// holds that tenant's workspaces only, so every account in it would look like
// one of that tenant's: 102 writes nothing there. The database is named as
// tenantdb.DatabaseName names a tenant's.
func TestBackfill102LeavesTenantDatabasesAlone(t *testing.T) {
	ctx := context.Background()
	var cust string
	{
		admin, err := pgx.Connect(ctx, testdb.AdminDSN(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&cust); err != nil {
			t.Fatal(err)
		}
		_ = admin.Close(ctx)
	}
	conn, apply := migratedTo101(t, "tenant_"+strings.ReplaceAll(cust, "-", ""))
	id := func(sql string, args ...any) string {
		t.Helper()
		var out string
		if err := conn.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
		return out
	}
	id(`INSERT INTO core.customer (id, name) VALUES ($1::uuid, 'Dedicated') RETURNING id::text`, cust)
	ws := id(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, cust)
	u := id(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('in-tenant-db', 'in@b102.test', 'In') RETURNING id::text`)
	id(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid) RETURNING id::text`, u, ws)

	apply(backfill)
	if got := id(`SELECT COALESCE(customer_id::text, '-') FROM identity.user WHERE id = $1::uuid`, u); got != "-" {
		t.Errorf("an account of a tenant database got tenant %s, want none", got)
	}
}

const backfill = "102_account_tenant_backfill.sql"

// migratedTo101 creates the database name, applies every migration before
// 102 to it, and returns a connection to it and a function that applies one
// migration file there. The database is dropped when the test ends.
func migratedTo101(t *testing.T, name string) (*pgx.Conn, func(string)) {
	t.Helper()
	ctx := context.Background()
	adminDSN := testdb.AdminDSN(t)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	cfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = conn.Close(ctx)
		if admin, err := pgx.Connect(ctx, adminDSN); err == nil {
			_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
			_ = admin.Close(ctx)
		}
	})

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	apply := func(f string) {
		t.Helper()
		sql, err := fs.ReadFile(migrations.FS, f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	for _, f := range files {
		if f >= "102" {
			break
		}
		apply(f)
	}
	return conn, apply
}
