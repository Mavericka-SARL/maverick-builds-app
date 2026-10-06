package migrations_test

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/migrations"
)

// TestThreePlans123 applies migrations 001–122, puts tenants on every old
// plan (one in each of the two places a tenant names its plan) and gives
// Standard a limit the platform admin set, then applies 123: the catalog is
// Community, Commercial and Enterprise; Community keeps only its 100 MB;
// Commercial keeps Standard's edited limit; every tenant is on a plan that
// exists; and a tenant created without a plan starts on Commercial.
func TestThreePlans123(t *testing.T) {
	ctx := context.Background()
	adminDSN := testdb.AdminDSN(t)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("plans123_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
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
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

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
		if f >= "123" {
			break
		}
		apply(f)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	one := func(sql string, args ...any) string {
		t.Helper()
		var v string
		if err := conn.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return v
	}
	exec(`UPDATE platform.plan SET limits = '{"max_models": 7}' WHERE key = 'standard'`)
	old := map[string]string{"test": "community", "trial": "community", "standard": "commercial", "starter": "commercial", "enterprise": "enterprise"}
	for from := range old {
		exec(`INSERT INTO core.customer (name, plan) VALUES ($1, $1)`, from)
		exec(`INSERT INTO platform.tenant_database (customer_id, name, plan, database, status) VALUES ($1::uuid, $2, $2, $3, 'ready')`,
			uuid.NewString(), from, "tenant_"+from)
	}

	apply("123_three_plans.sql")

	if got := one(`SELECT string_agg(key, ',' ORDER BY sort_order) FROM platform.plan`); got != "community,commercial,enterprise" {
		t.Errorf("plans = %s, want community,commercial,enterprise", got)
	}
	if got := one(`SELECT name || ' ' || limits::text || ' ' || self_service::text FROM platform.plan WHERE key = 'community'`); got != `Community {"max_storage_mb": 100} true` {
		t.Errorf("community = %s, want its 100 MB only, self-service", got)
	}
	if got := one(`SELECT left(limit_note, 40) FROM platform.plan WHERE key = 'community'`); !strings.HasPrefix(got, "A Community workspace holds") {
		t.Errorf("community limit note starts %q", got)
	}
	if got := one(`SELECT name || ' ' || limits::text FROM platform.plan WHERE key = 'commercial'`); got != `Commercial {"max_models": 7}` {
		t.Errorf("commercial = %s, want Standard's edited limit kept", got)
	}
	for from, want := range old {
		if got := one(`SELECT plan FROM core.customer WHERE name = $1`, from); got != want {
			t.Errorf("tenant on %s moved to %s, want %s", from, got, want)
		}
		if got := one(`SELECT plan FROM platform.tenant_database WHERE name = $1`, from); got != want {
			t.Errorf("directory entry on %s moved to %s, want %s", from, got, want)
		}
	}
	exec(`INSERT INTO core.customer (name) VALUES ('no plan given')`)
	if got := one(`SELECT plan FROM core.customer WHERE name = 'no plan given'`); got != "commercial" {
		t.Errorf("a tenant created without a plan starts on %s, want commercial", got)
	}
}
