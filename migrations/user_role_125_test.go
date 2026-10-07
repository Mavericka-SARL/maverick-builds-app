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

// TestUserRole125BesideBusinessAdmin applies migrations 001–124, assigns
// roles, and applies 125: every business_admin assignment gets a user
// (business_user) one in the same workspace or none, a user assignment
// already there is not doubled, other roles get nothing, and 125 run again
// changes nothing.
func TestUserRole125BesideBusinessAdmin(t *testing.T) {
	ctx := context.Background()
	adminDSN := testdb.AdminDSN(t)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("userrole125_%d", time.Now().UnixNano())
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
		if f >= "125" {
			break
		}
		apply(f)
	}
	// Foreign keys are not what this test is about.
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	assign := func(user, role, ws string) {
		t.Helper()
		if _, err := conn.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2::identity.user_role, NULLIF($3, '')::uuid)`, user, role, ws); err != nil {
			t.Fatalf("assign %s: %v", role, err)
		}
	}
	w1, w2 := uuid.NewString(), uuid.NewString()
	owner, platformBA, both, developer, elsewhere := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	assign(owner, "tenant_admin", "")
	assign(owner, "developer", "")
	assign(owner, "business_admin", w1)
	assign(platformBA, "business_admin", "")
	assign(both, "business_admin", w1)
	assign(both, "business_user", w1)
	assign(developer, "developer", "")
	assign(elsewhere, "business_admin", w1)
	assign(elsewhere, "business_user", w2)

	userRoles := func(user string) string {
		t.Helper()
		var got string
		if err := conn.QueryRow(ctx, `
			SELECT COALESCE(string_agg(COALESCE(workspace_id::text, 'none'), ',' ORDER BY workspace_id::text NULLS FIRST), '')
			FROM identity.role_assignment WHERE user_id = $1::uuid AND role = 'business_user'`, user).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	sorted := func(ws ...string) string { sort.Strings(ws); return strings.Join(ws, ",") }
	for run := 1; run <= 2; run++ {
		apply("125_user_role_beside_business_admin.sql")
		for _, c := range []struct{ what, user, want string }{
			{"a sign-up owner", owner, w1},
			{"a business admin with no workspace", platformBA, "none"},
			{"already a user there", both, w1},
			{"a developer", developer, ""},
			{"a user in another workspace", elsewhere, sorted(w1, w2)},
		} {
			if got := userRoles(c.user); got != c.want {
				t.Errorf("run %d, %s: user role in %q, want %q", run, c.what, got, c.want)
			}
		}
	}
}
