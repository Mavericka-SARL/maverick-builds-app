package migrations_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/migrations"
)

// TestEveryValueTableIsWatched: every table of the schemas a grid read
// consults counts its changes in the data generation (migration 198), or is
// named in runtime.unwatched_data_tables' list of tables left out on
// purpose. A table a later migration adds fails here until it is watched
// (runtime.watch_data_changes) or left out with a reason — unwatched, a
// change to it would leave dashboards showing the answer from before it
// (read_etag.go).
func TestEveryValueTableIsWatched(t *testing.T) {
	pool := testdb.New(t, migrations.FS, ".")
	rows, err := pool.Query(context.Background(), `SELECT * FROM runtime.unwatched_data_tables()`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		t.Errorf("%s is neither watched for data changes nor left out on purpose (migration 198)", name)
	}
}

// TestDataChangeModelLookupsRun: how each watched table names a changed
// row's model works — the column a trigger reads is the table's, and every
// parent lookup runs. A lookup that failed would fail every write to its
// table.
func TestDataChangeModelLookupsRun(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrations.FS, ".")
	rows, err := pool.Query(ctx, `
		SELECT t.tgrelid::regclass::text, t.tgargs,
		       EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = t.tgrelid AND a.attname = 'model_id' AND NOT a.attisdropped),
		       EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = t.tgrelid AND a.attname = 'revision_id' AND NOT a.attisdropped)
		FROM pg_trigger t WHERE t.tgname = 'data_change' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	type lookup struct{ table, column, sql string }
	var lookups []lookup
	tables := 0
	for rows.Next() {
		var table string
		var raw []byte
		var hasModel, hasRevision bool
		if err := rows.Scan(&table, &raw, &hasModel, &hasRevision); err != nil {
			t.Fatal(err)
		}
		tables++
		args := bytes.Split(bytes.TrimSuffix(raw, []byte{0}), []byte{0})
		switch string(args[0]) {
		case "model":
			if !hasModel {
				t.Errorf("%s is watched by its model_id, which it has not", table)
			}
		case "revision":
			if !hasRevision {
				t.Errorf("%s is watched by its revision_id, which it has not", table)
			}
		case "parent":
			if len(args) != 3 {
				t.Errorf("%s: a parent lookup with %d arguments", table, len(args))
				continue
			}
			lookups = append(lookups, lookup{table, string(args[1]), string(args[2])})
		case "self", "database":
		default:
			t.Errorf("%s: unknown model_of %q", table, args[0])
		}
	}
	rows.Close()
	if tables < 40 || len(lookups) < 10 {
		t.Fatalf("only %d tables watched, %d of them through a parent", tables, len(lookups))
	}
	for _, l := range lookups {
		var column bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = $1::regclass AND attname = $2 AND NOT attisdropped)`,
			l.table, l.column).Scan(&column); err != nil || !column {
			t.Errorf("%s: its parent lookup reads %s, which it has not (%v)", l.table, l.column, err)
		}
		var model *string
		if err := pool.QueryRow(ctx, l.sql, uuid.NewString()).Scan(&model); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("%s: its parent lookup fails: %v", l.table, err)
		}
	}
}

// TestDataGenerationCountsCommittedChanges: a model's generation grows by
// one per committed transaction that changed something of that model, or
// something every model's reads depend on — and only once it has
// committed. Another model's changes, a rolled-back one, a statement or an
// UPDATE that changes nothing (or only a bookkeeping column), and a prune
// leave it as it is.
func TestDataGenerationCountsCommittedChanges(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrations.FS, ".")
	run := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	one := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	customer := one(`INSERT INTO core.customer (name) VALUES ('Gen') RETURNING id::text`)
	app := one(`INSERT INTO core.application (name, mode, customer_id) VALUES ('Gen', 'planning', $1) RETURNING id::text`, customer)
	modelA := one(`INSERT INTO core.model (application_id, name) VALUES ($1, 'A') RETURNING id::text`, app)
	modelB := one(`INSERT INTO core.model (application_id, name) VALUES ($1, 'B') RETURNING id::text`, app)
	revA := one(`INSERT INTO model.revision (model_id, name) VALUES ($1, 'Working') RETURNING id::text`, modelA)
	metricA := one(`INSERT INTO model.metric_def (model_id, name, is_input, revision_id) VALUES ($1, 'sales', true, $2) RETURNING id::text`, modelA, revA)
	dimB := one(`INSERT INTO model.dimension_def (model_id, name) VALUES ($1, 'region') RETURNING id::text`, modelB)
	memberB := one(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1, 'EU', 'Europe') RETURNING id::text`, dimB)
	user := one(`INSERT INTO identity."user" (keycloak_sub, email, display_name, customer_id) VALUES ('gen', 'gen@example.com', 'Gen', $1) RETURNING id::text`, customer)

	gen := func(model string) int64 {
		t.Helper()
		var g int64
		if err := pool.QueryRow(ctx, `SELECT runtime.data_generation($1::uuid)`, model).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	a, b := gen(modelA), gen(modelB)
	expect := func(label string, da, db int64) {
		t.Helper()
		if got := gen(modelA); got != a+da {
			t.Fatalf("%s: model A's generation %d, want %d", label, got, a+da)
		}
		if got := gen(modelB); got != b+db {
			t.Fatalf("%s: model B's generation %d, want %d", label, got, b+db)
		}
		a, b = a+da, b+db
	}
	inTx := func(label string, stmts ...[]any) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s[0].(string), s[1:]...); err != nil {
				t.Fatalf("%s: %s: %v", label, s[0], err)
			}
		}
		expect(label+", not committed yet", 0, 0)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	run(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value) VALUES ($1, $2, $3, '{}', 10)`, modelA, revA, metricA)
	expect("a fact of A", 1, 0)
	run(`UPDATE model.dimension_member SET label = 'EMEA' WHERE id = $1::uuid`, memberB)
	expect("a member of B (through its dimension)", 0, 1)
	run(`DELETE FROM runtime.fact_input WHERE false`)
	expect("a statement changing no row", 0, 0)
	run(`UPDATE model.metric_def SET name = name WHERE id = $1::uuid`, metricA)
	expect("an update changing nothing (updated_at stamped)", 0, 0)
	run(`UPDATE identity."user" SET last_seen_at = now() WHERE id = $1::uuid`, user)
	run(`UPDATE core.customer SET usage_checked_at = now(), storage_bytes = 1 WHERE id = $1::uuid`, customer)
	expect("bookkeeping columns", 0, 0)
	run(`UPDATE identity."user" SET display_name = 'Gen 2' WHERE id = $1::uuid`, user)
	expect("a user (every model's reads depend on users)", 1, 1)

	inTx("two changes to A and one to B rolled back to a savepoint",
		[]any{`UPDATE runtime.fact_input SET value = 11 WHERE metric_id = $1::uuid`, metricA},
		[]any{`UPDATE runtime.fact_input SET value = 12 WHERE metric_id = $1::uuid`, metricA},
		[]any{`SAVEPOINT s`},
		[]any{`UPDATE model.dimension_member SET label = 'Gone' WHERE id = $1::uuid`, memberB},
		[]any{`ROLLBACK TO SAVEPOINT s`})
	expect("two changes to A and one to B rolled back to a savepoint", 1, 0)

	inTx("one transaction changing both",
		[]any{`UPDATE runtime.fact_input SET value = 13 WHERE metric_id = $1::uuid`, metricA},
		[]any{`UPDATE model.dimension_member SET label = 'Europe' WHERE id = $1::uuid`, memberB})
	expect("one transaction changing both", 1, 1)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE runtime.fact_input SET value = 14 WHERE metric_id = $1::uuid`, metricA); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	expect("a rolled-back change", 0, 0)

	run(`SELECT runtime.prune_data_changes()`)
	expect("a prune", 0, 0)
	var left int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime.data_change`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("after a prune %d rows are left (%v)", left, err)
	}
	run(`UPDATE runtime.fact_input SET value = 15 WHERE metric_id = $1::uuid`, metricA)
	expect("a change after a prune", 1, 0)

	// A member deleted together with its dimension: the dimension row counts
	// for B; the member's lookup may find no dimension any more, which counts
	// for every model — never for none.
	run(`DELETE FROM model.dimension_def WHERE id = $1::uuid`, dimB)
	if gen(modelB) <= b {
		t.Fatalf("a dimension of B deleted with its members left B's generation at %d", gen(modelB))
	}
	a, b = gen(modelA), gen(modelB)

	// A deleted model leaves no row of it behind, pruned or not, and the
	// other model's generation is as it was.
	run(`SELECT runtime.prune_data_changes()`)
	run(`UPDATE core.model SET description = 'last change' WHERE id = $1::uuid`, modelB)
	run(`DELETE FROM core.model WHERE id = $1::uuid`, modelB)
	var leftOfB int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM runtime.data_change WHERE model_id = $1::uuid)
		+ (SELECT count(*) FROM runtime.data_generation WHERE model_id = $1::uuid)`, modelB).Scan(&leftOfB); err != nil || leftOfB != 0 {
		t.Fatalf("a deleted model left %d rows of it (%v)", leftOfB, err)
	}
	if got := gen(modelA); got != a {
		t.Fatalf("deleting model B moved model A's generation %d -> %d", a, got)
	}
}
