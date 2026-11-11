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
// soon as a model is deleted, while the inputs' files keep their size until
// PostgreSQL reuses the space. The results' files go at the delete: they are
// kept one part per revision, emptied when the revision goes and dropped at
// the next compaction (migration 176). Each model's share is in proportion
// to its rows, and deleting a model or revision leaves no cell history
// behind (migration 133).
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
		// The calculation makes a revision's parts before it writes its
		// results; until then they would sit in the default part (migration 177).
		if missing, err := EnsureRevisionParts(ctx, pool); err != nil || missing != 0 {
			t.Fatalf("revision parts: %d missing, err %v", missing, err)
		}
		exec(`INSERT INTO runtime.calc_result (model_id, revision_id, dim_members, metric_id, value)
			SELECT $1::uuid, $2::uuid, jsonb_build_object('region', 'r' || (i % 50), 'i', i), $3::uuid, i * 2 FROM generate_series(1, $4::int) i`, m.id, m.rev, metric, facts)
		return m
	}
	big, small := seed("Big", 60000), seed("Small", 6000)
	exec(`ANALYZE`)
	flushStats(t, pool)

	// Compared with the data tables as VACUUM FULL would leave them (their
	// files right after a bulk load, nothing deleted yet).
	sizeOf := func(where string) int64 {
		var b int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(pg_total_relation_size(relid)), 0)::bigint FROM pg_stat_user_tables WHERE `+where).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	inputFiles := func() int64 { return sizeOf(`relname = 'fact_input_default'`) }
	// Results are kept one part per revision (migration 176).
	resultFiles := func() int64 {
		return sizeOf(`relid IN (SELECT inhrelid FROM pg_inherits WHERE inhparent = 'runtime.calc_result'::regclass)`)
	}
	dataFiles := func() int64 { return inputFiles() + resultFiles() }
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
	filesBefore, inputsBefore, resultsBefore := dataFiles(), inputFiles(), resultFiles()

	// Deleting the big model: the figure falls at once — no vacuum, no
	// rewrite — while the inputs' files have not moved, and its results'
	// part is emptied with it.
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
	if inputFiles() < inputsBefore*9/10 {
		t.Fatalf("the inputs' files shrank by themselves; the test no longer shows the difference")
	}
	if resultFiles() > resultsBefore/5 {
		t.Fatalf("the results' files went from %d to %d: deleting the model kept its results' disk", resultsBefore, resultFiles())
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

	// The forced compaction after a delete vacuums too (owner's decision
	// I9, 2026-11-10): rewriting a large table locked it for minutes, so the
	// files go back to the data left with the weekly pg_repack job
	// (deploy/docker/pg-backup/pg-repack.sh), online. Storage, which counts
	// live data, fell at the delete already; a second pass finds nothing to
	// do.
	freed, err := Compact(ctx, pool, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("forced compaction gave back %d bytes; data files %d -> %d", freed, filesBefore, dataFiles())
	if node() != nodeBefore {
		t.Fatal("the forced compaction rewrote runtime.fact_input's partitions")
	}
	if n := q(`SELECT count(*)::text FROM runtime.gone_revision_parts()`); n != "0" {
		t.Fatalf("%s parts of deleted revisions left after the forced compaction, want 0", n)
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
