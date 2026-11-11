package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/migrations"
)

// TestCalcResultByRevision176 applies migrations 001–175, gives two
// revisions results and leaves results of a revision that no longer exists,
// then applies 176. Each revision's results are in its own part with their
// ids and values, the gone revision's are not copied, role_runtime keeps its
// grants; a new revision gets its part, a result of a revision without one
// is refused; deleting a revision empties its part (no row left, no disk
// held) and the emptied part is then dropped; a part another session holds
// is left to the row-by-row delete and its drop waits for the next try;
// deleting a model empties every revision's part.
func TestCalcResultByRevision176(t *testing.T) {
	ctx := context.Background()
	adminDSN := testdb.AdminDSN(t)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("calcbyrev176_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	cfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	var notices []string
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) { notices = append(notices, n.Message) }
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
		if f >= "176" {
			break
		}
		apply(f)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := conn.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		return n
	}

	customer, app, model := uuid.NewString(), uuid.NewString(), uuid.NewString()
	revA, revB, gone := uuid.NewString(), uuid.NewString(), uuid.NewString()
	metA, metB := uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO core.customer (id, name) VALUES ($1::uuid, 'Acme')`, customer)
	exec(`INSERT INTO core.application (id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Plan', 'planning')`, app, customer)
	exec(`INSERT INTO core.model (id, application_id, name) VALUES ($1::uuid, $2::uuid, 'Budget')`, model, app)
	exec(`INSERT INTO model.revision (id, model_id, name) VALUES ($1::uuid, $3::uuid, 'A'), ($2::uuid, $3::uuid, 'B')`, revA, revB, model)
	exec(`INSERT INTO model.metric_def (id, model_id, revision_id, name) VALUES ($1::uuid, $3::uuid, $4::uuid, 'sales'), ($2::uuid, $3::uuid, $5::uuid, 'sales')`,
		metA, metB, model, revA, revB)
	result := func(rev, metric, region string, value float64) {
		t.Helper()
		exec(`INSERT INTO runtime.calc_result (model_id, revision_id, metric_id, dim_members, value)
		      VALUES ($1::uuid, $2::uuid, $3::uuid, jsonb_build_object('region', $4::text), $5)`, model, rev, metric, region, value)
	}
	result(revA, metA, "north", 1)
	result(revA, metA, "south", 2)
	result(revA, metA, "east", 3)
	result(revB, metB, "north", 10)
	result(revB, metB, "south", 20)
	// A result whose revision was deleted before 176: nothing removed it.
	result(gone, metB, "west", 99)
	var idA string
	if err := conn.QueryRow(ctx, `SELECT id::text FROM runtime.calc_result WHERE revision_id = $1::uuid AND dim_members->>'region' = 'east'`, revA).Scan(&idA); err != nil {
		t.Fatal(err)
	}

	apply("176_calc_result_by_revision.sql")

	part := func(rev string) string { return "runtime.calc_result_" + strings.ReplaceAll(rev, "-", "") }
	partExists := func(rev string) bool {
		t.Helper()
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, part(rev)).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	var strategy string
	if err := conn.QueryRow(ctx, `SELECT pg_get_partkeydef('runtime.calc_result'::regclass)`).Scan(&strategy); err != nil {
		t.Fatal(err)
	}
	if strategy != "LIST (revision_id)" {
		t.Fatalf("calc_result is partitioned by %q, want LIST (revision_id)", strategy)
	}
	if n := count(`SELECT count(*) FROM ` + part(revA)); n != 3 {
		t.Errorf("revision A's part holds %d results, want 3", n)
	}
	if n := count(`SELECT count(*) FROM ` + part(revB)); n != 2 {
		t.Errorf("revision B's part holds %d results, want 2", n)
	}
	if n := count(`SELECT count(*) FROM runtime.calc_result`); n != 5 {
		t.Errorf("calc_result holds %d results, want 5 (the gone revision's not copied)", n)
	}
	if n := count(`SELECT count(*) FROM runtime.calc_result WHERE id = $1::uuid AND value = 3`, idA); n != 1 {
		t.Errorf("result %s kept %d times with its value, want once", idA, n)
	}
	if n := count(`SELECT count(*) FROM pg_inherits WHERE inhparent = 'runtime.calc_result'::regclass`); n != 2 {
		t.Errorf("calc_result has %d parts, want 2 (no default part)", n)
	}
	var canWrite bool
	if err := conn.QueryRow(ctx, `SELECT has_table_privilege('role_runtime', 'runtime.calc_result', 'INSERT,SELECT,DELETE')`).Scan(&canWrite); err != nil {
		t.Fatal(err)
	}
	if !canWrite {
		t.Error("role_runtime lost its grants on calc_result")
	}

	// A new revision gets its part; a result of a revision without one is refused.
	revC, metC := uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO model.revision (id, model_id, name) VALUES ($1::uuid, $2::uuid, 'C')`, revC, model)
	if !partExists(revC) {
		t.Fatal("a new revision has no part")
	}
	exec(`INSERT INTO model.metric_def (id, model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, $3::uuid, 'sales')`, metC, model, revC)
	result(revC, metC, "north", 5)
	_, err = conn.Exec(ctx, `INSERT INTO runtime.calc_result (model_id, revision_id, metric_id, dim_members, value)
	      VALUES ($1::uuid, $2::uuid, $3::uuid, '{}', 1)`, model, uuid.NewString(), metC)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Errorf("a result of a revision without a part: err %v, want 23514 (no partition)", err)
	}

	// Deleting a revision empties its part at once; the part is then dropped.
	exec(`DELETE FROM model.revision WHERE id = $1::uuid`, revB)
	if n := count(`SELECT pg_relation_size($1::regclass)::int`, part(revB)); n != 0 {
		t.Errorf("deleted revision B's part still holds %d bytes", n)
	}
	if n := count(`SELECT count(*) FROM runtime.gone_revision_parts()`); n != 1 {
		t.Errorf("%d parts of gone revisions, want 1 (B's)", n)
	}
	var dropped bool
	if err := conn.QueryRow(ctx, `SELECT runtime.drop_revision_part(to_regclass($1))`, part(revB)).Scan(&dropped); err != nil || !dropped {
		t.Fatalf("drop B's part: dropped %v, err %v", dropped, err)
	}
	if partExists(revB) {
		t.Error("B's part is still there after its drop")
	}
	// A live revision's part is never dropped.
	if err := conn.QueryRow(ctx, `SELECT runtime.drop_revision_part(to_regclass($1))`, part(revA)).Scan(&dropped); err != nil || dropped {
		t.Fatalf("drop live A's part: dropped %v, err %v", dropped, err)
	}

	// A part another session holds: the delete waits three seconds, then
	// deletes the rows one by one; the drop gives up after a second.
	other, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)
	hold, err := other.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `LOCK TABLE `+part(revC)+` IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	notices = nil
	started := time.Now()
	exec(`DELETE FROM model.revision WHERE id = $1::uuid`, revC)
	if waited := time.Since(started); waited < 2*time.Second || waited > 10*time.Second {
		t.Errorf("the delete of a held revision took %v, want about 3 s", waited)
	}
	if n := count(`SELECT count(*) FROM runtime.calc_result WHERE revision_id = $1::uuid`, revC); n != 0 {
		t.Errorf("deleted revision C still has %d results", n)
	}
	if len(notices) == 0 || !strings.Contains(notices[0], "is in use") {
		t.Errorf("notices %q, want one naming the part in use", notices)
	}
	started = time.Now()
	if err := conn.QueryRow(ctx, `SELECT runtime.drop_revision_part(to_regclass($1))`, part(revC)).Scan(&dropped); err != nil || dropped {
		t.Fatalf("drop a held part: dropped %v, err %v", dropped, err)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Errorf("the drop of a held part waited %v, want about 1 s", waited)
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT runtime.drop_revision_part(to_regclass($1))`, part(revC)).Scan(&dropped); err != nil || !dropped {
		t.Fatalf("drop C's part once free: dropped %v, err %v", dropped, err)
	}

	// Deleting the model empties every revision's part.
	exec(`DELETE FROM core.model WHERE id = $1::uuid`, model)
	if n := count(`SELECT pg_relation_size($1::regclass)::int`, part(revA)); n != 0 {
		t.Errorf("the deleted model's revision A part still holds %d bytes", n)
	}
	if n := count(`SELECT count(*) FROM runtime.gone_revision_parts()`); n != 1 {
		t.Errorf("%d parts of gone revisions after the model's delete, want 1 (A's)", n)
	}
}
