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

// TestIntegrationName151 applies migrations 001–150, gives one revision three
// connectors of one name (and one already called "Nightly (2)"), another
// revision a connector of that name too, and a revision-less pair, then
// applies 151: the oldest keeps its name, the others take the first free
// "Name (n)", other revisions are untouched, and a second duplicate is
// refused by the new index.
func TestIntegrationName151(t *testing.T) {
	ctx := context.Background()
	adminDSN := testdb.AdminDSN(t)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("intname151_%d", time.Now().UnixNano())
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
		if f >= "151" {
			break
		}
		apply(f)
	}
	// Foreign keys are not what this test is about.
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	modelID, revA, revB := uuid.NewString(), uuid.NewString(), uuid.NewString()
	add := func(rev *string, name string, age int) string {
		t.Helper()
		id := uuid.NewString()
		if _, err := conn.Exec(ctx, `
			INSERT INTO model.integration_def (id, model_id, revision_id, name, type, created_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, 'rest_api', now() - make_interval(mins => $5))`,
			id, modelID, rev, name, age); err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldest, second, third := add(&revA, "Nightly", 30), add(&revA, "Nightly", 20), add(&revA, "Nightly", 10)
	taken := add(&revA, "Nightly (2)", 40)
	otherRev := add(&revB, "Nightly", 5)
	loose1, loose2 := add(nil, "Loose", 2), add(nil, "Loose", 1)

	apply("151_integration_name_unique.sql")

	for id, want := range map[string]string{
		oldest: "Nightly", taken: "Nightly (2)", second: "Nightly (3)", third: "Nightly (4)",
		otherRev: "Nightly", loose1: "Loose", loose2: "Loose (2)",
	} {
		var got string
		if err := conn.QueryRow(ctx, `SELECT name FROM model.integration_def WHERE id = $1::uuid`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("connector %s is named %q, want %q", id, got, want)
		}
	}
	if _, err := conn.Exec(ctx, `INSERT INTO model.integration_def (model_id, revision_id, name, type) VALUES ($1::uuid, $2::uuid, 'Nightly', 'rest_api')`, modelID, revA); err == nil {
		t.Error("a second connector named Nightly in one revision was accepted after 151")
	}
	if _, err := conn.Exec(ctx, `INSERT INTO model.integration_def (model_id, name, type) VALUES ($1::uuid, 'Loose', 'rest_api')`, modelID); err == nil {
		t.Error("a second revision-less connector named Loose was accepted after 151")
	}
}
