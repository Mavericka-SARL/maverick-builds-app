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

// TestCleanup121OrphanMemberFacts applies migrations 001–120, stores facts
// at known and unknown member codes, applies 121 and checks that only a
// fact naming an existing dimension with a code it does not have goes — into
// the history with reason 'orphan_member_code' — and that 121 runs again
// without removing anything more.
func TestCleanup121OrphanMemberFacts(t *testing.T) {
	ctx := context.Background()
	adminDSN := testdb.AdminDSN(t)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("cleanup121_%d", time.Now().UnixNano())
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
		if f >= "121" {
			break
		}
		apply(f)
	}

	// Foreign keys are not what this test is about (replica role: FK
	// triggers off). The replica role also silences the archive trigger on
	// fact_input, which this test reads, so it is made to fire ALWAYS.
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `ALTER TABLE runtime.fact_input ENABLE ALWAYS TRIGGER fact_input_archive_on_delete`); err != nil {
		t.Fatal(err)
	}
	id := func(sql string, args ...any) string {
		t.Helper()
		var out string
		if err := conn.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
		return out
	}
	model := id(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, uuid.NewString())
	rev := id(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'R') RETURNING id::text`, model)
	region := id(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'region') RETURNING id::text`, model, rev)
	id(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'EU', 'EU') RETURNING id::text`, region)
	metric := id(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'sales', true) RETURNING id::text`, model, rev)
	fact := func(dims string) string {
		return id(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::jsonb, 1, $5::uuid) RETURNING id::text`, model, rev, metric, dims, uuid.NewString())
	}
	known := fact(fmt.Sprintf(`{"%s": "EU"}`, region))
	orphan := fact(fmt.Sprintf(`{"%s": "Snacks"}`, region))
	goneDim := fact(fmt.Sprintf(`{"%s": "EU"}`, uuid.NewString())) // a dimension no longer there
	byName := fact(`{"region": "Snacks"}`)                         // a key that is no dimension id
	none := fact(`{}`)

	apply("121_orphan_member_facts.sql")
	left := func() string {
		return id(`SELECT COALESCE(string_agg(id::text, ',' ORDER BY id), '') FROM runtime.fact_input`)
	}
	want := []string{known, goneDim, byName, none}
	sort.Strings(want)
	if got := left(); got != strings.Join(want, ",") {
		t.Errorf("facts after 121: %s, want %v (all but the orphan)", got, want)
	}
	if reason := id(`SELECT COALESCE((SELECT delete_reason FROM runtime.fact_input_history WHERE id=$1::uuid), 'not archived')`, orphan); reason != "orphan_member_code" {
		t.Errorf("the orphan's history row says %q, want orphan_member_code", reason)
	}
	apply("121_orphan_member_facts.sql")
	if got := left(); got != strings.Join(want, ",") {
		t.Errorf("facts after 121 ran again: %s", got)
	}
}
