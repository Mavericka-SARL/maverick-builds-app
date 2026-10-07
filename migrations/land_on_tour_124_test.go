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

// TestLanding124MovesDefaultBackToTour applies migrations 001–123, sets up
// four self-service tenants and applies 124: only the one whose application
// still defaults to the Developer guide moves to its tour. A default changed
// by hand, a deleted tour and a tour moved to another application all stay
// as they were, and 124 run again changes nothing.
func TestLanding124MovesDefaultBackToTour(t *testing.T) {
	ctx := context.Background()
	adminDSN := testdb.AdminDSN(t)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("landing124_%d", time.Now().UnixNano())
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
		if f >= "124" {
			break
		}
		apply(f)
	}
	// Foreign keys are not what this test is about.
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	id := func(sql string, args ...any) string {
		t.Helper()
		var v string
		if err := conn.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return v
	}
	type tenant struct{ app, tour, guide, other string }
	// setup: a tenant with the tour and the Developer guide in one
	// application, its default the guide; starter_model records both.
	setup := func() tenant {
		t.Helper()
		cust := uuid.NewString()
		app := id(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Getting started', 'planning') RETURNING id::text`, cust, uuid.NewString())
		tour := id(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Learn the platform') RETURNING id::text`, app)
		guide := id(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Developer guide') RETURNING id::text`, app)
		other := id(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Budget') RETURNING id::text`, app)
		id(`UPDATE core.application SET default_model_id = $2::uuid WHERE id = $1::uuid RETURNING id::text`, app, guide)
		for key, model := range map[string]string{"tour": tour, "developer_guide": guide} {
			id(`INSERT INTO core.starter_model (customer_id, starter_key, model_id, content_hash) VALUES ($1::uuid, $2, $3::uuid, 'h') RETURNING starter_key`, cust, key, model)
		}
		return tenant{app, tour, guide, other}
	}
	still := setup()
	changed := setup()
	id(`UPDATE core.application SET default_model_id = $2::uuid WHERE id = $1::uuid RETURNING id::text`, changed.app, changed.other)
	deleted := setup()
	id(`UPDATE core.starter_model SET model_id = NULL WHERE model_id = $1::uuid RETURNING starter_key`, deleted.tour)
	moved := setup()
	id(`UPDATE core.model SET application_id = $2::uuid WHERE id = $1::uuid RETURNING id::text`, moved.tour, uuid.NewString())

	defaultOf := func(app string) string {
		t.Helper()
		return id(`SELECT COALESCE(default_model_id::text, '') FROM core.application WHERE id = $1::uuid`, app)
	}
	for run := 1; run <= 2; run++ {
		apply("124_land_on_tour.sql")
		for _, c := range []struct {
			what      string
			app, want string
		}{
			{"default still the guide", still.app, still.tour},
			{"default changed by hand", changed.app, changed.other},
			{"tour deleted", deleted.app, deleted.guide},
			{"tour in another application", moved.app, moved.guide},
		} {
			if got := defaultOf(c.app); got != c.want {
				t.Errorf("run %d, %s: default = %s, want %s", run, c.what, got, c.want)
			}
		}
	}
}
