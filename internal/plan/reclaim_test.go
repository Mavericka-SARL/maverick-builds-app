package plan

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// Deleting a model gives its space back: its facts are not archived on the
// way out (migration 133), each model's size is measured, and reclaim
// compacts what the deleted rows held, so the database's size falls.
func TestDeletedModelSpaceIsReclaimed(t *testing.T) {
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
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Reclaim', 'community') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, cust)
	app := q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, cust, ws)
	type model struct{ id, rev, metric string }
	seed := func(name string, facts int) model {
		m := model{id: q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, app, name)}
		m.rev = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, m.id)
		m.metric = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, 'Amount', true, 'sum') RETURNING id::text`, m.id, m.rev)
		exec(`INSERT INTO runtime.fact_input (model_id, revision_id, dim_members, metric_id, value)
			SELECT $1::uuid, $2::uuid, jsonb_build_object('i', i), $3::uuid, i FROM generate_series(1, $4::int) i`, m.id, m.rev, m.metric, facts)
		return m
	}
	big, small := seed("Big", 60000), seed("Small", 6000)
	exec(`ANALYZE`)

	// Each model's size is its share of the data, in proportion to its rows.
	sizes, err := ModelStorage(ctx, pool, cust)
	if err != nil {
		t.Fatal(err)
	}
	if sizes[big.id] < 9*sizes[small.id] || sizes[small.id] <= 0 {
		t.Fatalf("model sizes = %v, want Big ~10x Small", sizes)
	}
	if err := recordModelStorage(ctx, pool, sizes); err != nil {
		t.Fatal(err)
	}
	if got := q(`SELECT bytes::text FROM runtime.model_storage WHERE model_id = $1::uuid`, small.id); got == "0" {
		t.Fatalf("recorded Small = %s", got)
	}

	// An ordinary delete of a cell is still archived; deleting a metric or a
	// revision archives nothing and drops the history they had.
	exec(`DELETE FROM runtime.fact_input WHERE model_id = $1::uuid AND (dim_members->>'i')::int <= 10`, small.id)
	if n := q(`SELECT count(*)::text FROM runtime.fact_input_history WHERE model_id = $1::uuid`, small.id); n != "10" {
		t.Fatalf("history after deleting 10 cells = %s, want 10", n)
	}
	exec(`DELETE FROM model.revision WHERE id = $1::uuid`, small.rev)
	if n := q(`SELECT count(*)::text FROM runtime.fact_input_history WHERE model_id = $1::uuid`, small.id); n != "0" {
		t.Fatalf("history after deleting the revision = %s, want 0", n)
	}
	if n := q(`SELECT count(*)::text FROM runtime.fact_input WHERE model_id = $1::uuid`, small.id); n != "0" {
		t.Fatalf("facts after deleting the revision = %s, want 0", n)
	}

	before, err := dedicatedStorageBytes(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	exec(`DELETE FROM core.model WHERE id = $1::uuid`, big.id)
	if n := q(`SELECT count(*)::text FROM runtime.fact_input_history`); n != "0" {
		t.Fatalf("history after deleting the model = %s, want 0", n)
	}
	waitForDeleteStats(t, pool, 60000)
	if after, _ := dedicatedStorageBytes(ctx, pool); after < before*9/10 {
		t.Fatalf("size fell from %d to %d without a compaction; the test no longer shows the problem", before, after)
	}

	e := NewEnforcer(pool)
	rewrote, err := e.reclaim(ctx, pool, cust, false)
	if err != nil || !rewrote {
		t.Fatalf("reclaim after deleting most rows = %v err=%v", rewrote, err)
	}
	after, err := dedicatedStorageBytes(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("storage before the delete %d, after reclaim %d", before, after)
	if after > before/2 {
		t.Fatalf("size after reclaim = %d, before the delete %d: the deleted model's space was not given back", after, before)
	}
	// Nothing deleted since: the next sweep rewrites nothing.
	if rewrote, err := e.reclaim(ctx, pool, cust, true); err != nil || rewrote {
		t.Fatalf("second reclaim = %v err=%v, want nothing to do", rewrote, err)
	}
}

// waitForDeleteStats waits until the statistics show the deleted rows: a
// backend reports them shortly after its transaction ends, not at once.
func waitForDeleteStats(t *testing.T, pool *pgxpool.Pool, atLeast int64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var n int64
		if err := pool.QueryRow(context.Background(), `SELECT COALESCE(sum(n_tup_del), 0)::bigint FROM pg_stat_user_tables WHERE schemaname = 'runtime'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= atLeast {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("deleted rows in statistics = %d, want %d", n, atLeast)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
