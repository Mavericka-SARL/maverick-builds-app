package plan

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// Storage counts the data a tenant holds, not the files it sits in: the
// figure is close to what the tables take once rewritten, and it falls as
// soon as a model is deleted, while the files keep their size until
// PostgreSQL reuses the space. Each model's share is in proportion to its
// rows, and deleting a model or revision leaves no cell history behind
// (migration 133).
func TestStorageCountsLiveDataAndFallsWhenAModelIsDeleted(t *testing.T) {
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Live', 'commercial') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, cust)
	app := q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, cust, ws)
	type model struct{ id, rev string }
	seed := func(name string, facts int) model {
		m := model{id: q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, app, name)}
		m.rev = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, m.id)
		metric := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, 'Amount', true, 'sum') RETURNING id::text`, m.id, m.rev)
		exec(`INSERT INTO runtime.fact_input (model_id, revision_id, dim_members, metric_id, value)
			SELECT $1::uuid, $2::uuid, jsonb_build_object('region', 'r' || (i % 50), 'i', i), $3::uuid, i FROM generate_series(1, $4::int) i`, m.id, m.rev, metric, facts)
		exec(`INSERT INTO runtime.calc_result (model_id, revision_id, dim_members, metric_id, value)
			SELECT $1::uuid, $2::uuid, jsonb_build_object('region', 'r' || (i % 50), 'i', i), $3::uuid, i * 2 FROM generate_series(1, $4::int) i`, m.id, m.rev, metric, facts)
		return m
	}
	big, small := seed("Big", 60000), seed("Small", 6000)
	exec(`ANALYZE`)
	flushStats(t, pool)

	// Compared with the data tables as VACUUM FULL would leave them (their
	// files right after a bulk load, nothing deleted yet).
	dataFiles := func() int64 {
		var b int64
		if err := pool.QueryRow(ctx, `SELECT sum(pg_total_relation_size(relid))::bigint FROM pg_stat_user_tables
			WHERE relname LIKE 'fact_input_default' OR relname LIKE 'calc_result_default'`).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	sizes, err := ModelStorage(ctx, pool, cust)
	if err != nil {
		t.Fatal(err)
	}
	files := dataFiles()
	modelsLive := sizes[big.id] + sizes[small.id]
	t.Logf("data tables: files %d, live estimate %d (Big %d, Small %d)", files, modelsLive, sizes[big.id], sizes[small.id])
	if modelsLive < files*7/10 || modelsLive > files*13/10 {
		t.Fatalf("live estimate %d is not within 30%% of the tables' files %d", modelsLive, files)
	}
	if sizes[big.id] < 9*sizes[small.id] || sizes[small.id] <= 0 {
		t.Fatalf("model sizes = %v, want Big ~10x Small", sizes)
	}

	before, err := dedicatedStorageBytes(ctx, pool, cust)
	if err != nil {
		t.Fatal(err)
	}
	filesBefore := dataFiles()

	// Deleting the big model: the figure falls at once — no vacuum, no
	// rewrite — while the files have not moved.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM core.model WHERE id = $1::uuid`, big.id); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := dedicatedStorageBytes(ctx, pool, cust)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("storage %d before deleting Big, %d after; data files %d -> %d", before, after, filesBefore, dataFiles())
	if before-after < sizes[big.id]*8/10 {
		t.Fatalf("storage fell by %d after deleting a model of %d", before-after, sizes[big.id])
	}
	if dataFiles() < filesBefore*9/10 {
		t.Fatalf("the files shrank by themselves; the test no longer shows the difference")
	}
	if n := q(`SELECT count(*)::text FROM runtime.fact_input_history`); n != "0" {
		t.Fatalf("cell history after deleting the model = %s, want 0", n)
	}

	// The sweep's compaction vacuums: it rewrites no table — a rewrite
	// locks the table, and a model in use would stand still for it every
	// five minutes — and leaves the rewrite to the forced one.
	node := func() string {
		return q(`SELECT string_agg(pg_relation_filenode(i.inhrelid)::text, ',' ORDER BY i.inhrelid)
			FROM pg_inherits i WHERE i.inhparent = 'runtime.fact_input'::regclass`)
	}
	nodeBefore := node()
	if _, err := Compact(ctx, pool, false); err != nil {
		t.Fatal(err)
	}
	if node() != nodeBefore {
		t.Fatal("the sweep's compaction rewrote runtime.fact_input's partitions")
	}

	// Compacting gives the deleted model's disk back: the files shrink to
	// about the data left, and a second pass finds nothing to do.
	freed, err := Compact(ctx, pool, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("compaction gave back %d bytes; data files %d -> %d", freed, filesBefore, dataFiles())
	if dataFiles() > filesBefore/4 || freed < filesBefore/2 {
		t.Fatalf("after compaction the data files are %d (were %d), %d given back", dataFiles(), filesBefore, freed)
	}
	if again, err := Compact(ctx, pool, true); err != nil || again != 0 {
		t.Fatalf("second compaction = %d err=%v, want nothing to do", again, err)
	}

	// An ordinary delete of cells is archived; deleting the revision takes
	// its cells and their history.
	exec(`DELETE FROM runtime.fact_input WHERE model_id = $1::uuid AND (dim_members->>'i')::int <= 10`, small.id)
	if n := q(`SELECT count(*)::text FROM runtime.fact_input_history WHERE model_id = $1::uuid`, small.id); n != "10" {
		t.Fatalf("history after deleting 10 cells = %s, want 10", n)
	}
	exec(`DELETE FROM model.revision WHERE id = $1::uuid`, small.rev)
	for _, table := range []string{"runtime.fact_input", "runtime.fact_input_history", "runtime.calc_result"} {
		if n := q(`SELECT count(*)::text FROM `+table+` WHERE model_id = $1::uuid`, small.id); n != "0" {
			t.Fatalf("%s rows of the deleted revision = %s, want 0", table, n)
		}
	}

	// The measurement is kept for the console.
	sizes, err = ModelStorage(ctx, pool, cust)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordModelStorage(ctx, pool, sizes); err != nil {
		t.Fatal(err)
	}
	if n := q(`SELECT count(*)::text FROM runtime.model_storage WHERE model_id = $1::uuid`, small.id); n != "1" {
		t.Fatalf("recorded sizes for Small = %s", n)
	}
}

// flushStats makes the statistics collector see this test's writes now.
func flushStats(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(context.Background(), `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatal(err)
	}
}
