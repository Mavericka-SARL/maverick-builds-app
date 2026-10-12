package calculation_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// ReplaceResultsTx leaves a metric with exactly the given cells, one row
// each, and rewrites only the cells whose value changed: recalculating used
// to delete and re-insert every cell and to add a '{}' total per pass, so a
// busy workspace's calc_result was mostly dead rows.
func TestReplaceResultsWritesOnlyWhatChanged(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	const modelID, revisionID = "00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0001-000000000001"
	var metricID string
	if err := store.Pool().QueryRow(ctx, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input)
		VALUES ($1::uuid, $2::uuid, 'margin', false) RETURNING id::text`, modelID, revisionID).Scan(&metricID); err != nil {
		t.Fatal(err)
	}
	replace := func(rows ...calculation.CalcResultRow) {
		t.Helper()
		if err := store.InTx(ctx, func(tx pgx.Tx) error {
			return store.ReplaceResultsTx(ctx, tx, modelID, revisionID, metricID, "p", rows)
		}); err != nil {
			t.Fatal(err)
		}
	}
	// state is each cell's value and row id, as "dims=value", sorted.
	state := func() (cells []string, ids map[string]string) {
		t.Helper()
		rows, err := store.Pool().Query(ctx, `SELECT dim_members::text, value::text, id::text FROM runtime.calc_result WHERE metric_id = $1::uuid`, metricID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		ids = map[string]string{}
		for rows.Next() {
			var dims, value, id string
			if err := rows.Scan(&dims, &value, &id); err != nil {
				t.Fatal(err)
			}
			cells = append(cells, dims+"="+value)
			ids[dims] = id
		}
		sort.Strings(cells)
		return cells, ids
	}
	cell := func(region string, v float64) calculation.CalcResultRow {
		if region == "" {
			return calculation.CalcResultRow{DimMembers: map[string]string{}, Value: v}
		}
		return calculation.CalcResultRow{DimMembers: map[string]string{"region": region}, Value: v}
	}

	replace(cell("north", 1), cell("south", 2), cell("", 3))
	first, firstIDs := state()
	if strings.Join(first, " ") != `{"region": "north"}=1 {"region": "south"}=2 {}=3` {
		t.Fatalf("after the first pass: %v", first)
	}

	// The same values again: nothing is rewritten.
	replace(cell("north", 1), cell("south", 2), cell("", 3))
	again, againIDs := state()
	if strings.Join(again, " ") != strings.Join(first, " ") || againIDs[`{}`] != firstIDs[`{}`] || againIDs[`{"region": "north"}`] != firstIDs[`{"region": "north"}`] {
		t.Fatalf("an unchanged pass rewrote rows: %v %v, was %v", again, againIDs, firstIDs)
	}

	// South and the total change: only those two get new rows.
	replace(cell("north", 1), cell("south", 5), cell("", 6))
	changed, changedIDs := state()
	if strings.Join(changed, " ") != `{"region": "north"}=1 {"region": "south"}=5 {}=6` {
		t.Fatalf("after a change: %v", changed)
	}
	if changedIDs[`{"region": "north"}`] != firstIDs[`{"region": "north"}`] || changedIDs[`{"region": "south"}`] == firstIDs[`{"region": "south"}`] {
		t.Fatalf("north should keep its row and south get a new one: %v, was %v", changedIDs, firstIDs)
	}

	// South lost its data and the pass has no total: both rows go.
	replace(cell("north", 1))
	if left, _ := state(); strings.Join(left, " ") != `{"region": "north"}=1` {
		t.Fatalf("after south and the total went: %v", left)
	}

	// Totals piled up by the old write path are cleared to the one current.
	for _, v := range []float64{7, 8, 9} {
		if _, err := store.Pool().Exec(ctx, `INSERT INTO runtime.calc_result (model_id, revision_id, dim_members, metric_id, value, partition_key)
			VALUES ($1::uuid, $2::uuid, '{}', $3::uuid, $4, 'p')`, modelID, revisionID, metricID, v); err != nil {
			t.Fatal(err)
		}
	}
	replace(cell("north", 1), cell("", 8))
	if left, _ := state(); strings.Join(left, " ") != `{"region": "north"}=1 {}=8` {
		t.Fatalf("after clearing piled-up totals: %v", left)
	}

	// A text result compares by its text.
	text := "on track"
	replace(calculation.CalcResultRow{DimMembers: map[string]string{"region": "north"}, Text: &text})
	if left, _ := state(); strings.Join(left, " ") != `{"region": "north"}=0` {
		t.Fatalf("after a text result: %v", left)
	}
	var got string
	if err := store.Pool().QueryRow(ctx, `SELECT text_value FROM runtime.calc_result WHERE metric_id = $1::uuid`, metricID).Scan(&got); err != nil || got != text {
		t.Fatalf("text = %q err=%v", got, err)
	}
}

// Recalculating with nothing changed writes nothing: the formula's total
// keeps its one row instead of gaining one per pass.
func TestRecalculatingUnchangedDataWritesNoRows(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	const modelID, revisionID = "00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0001-000000000001"
	id := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := store.Pool().QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	revenue := id(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'revenue', true) RETURNING id::text`, modelID, revisionID)
	cogs := id(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'cogs', true) RETURNING id::text`, modelID, revisionID)
	profit := id(`INSERT INTO model.metric_def (model_id, revision_id, name, formula, is_input) VALUES ($1::uuid, $2::uuid, 'gross_profit', '{revenue} - {cogs}', false) RETURNING id::text`, modelID, revisionID)
	if _, err := store.Pool().Exec(ctx, `INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid), ($1::uuid, $3::uuid)`, profit, revenue, cogs); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
		VALUES ($1::uuid, $2::uuid, $3::uuid, '{}', 1000), ($1::uuid, $2::uuid, $4::uuid, '{}', 600)`, modelID, revisionID, revenue, cogs); err != nil {
		t.Fatal(err)
	}
	scheduler := calculation.NewScheduler(logger.New("calc-test"), store)
	rowsOf := func() (n int, ids string) {
		t.Helper()
		if err := store.Pool().QueryRow(ctx, `SELECT count(*), COALESCE(string_agg(id::text, ','), '') FROM runtime.calc_result WHERE metric_id = $1::uuid`, profit).Scan(&n, &ids); err != nil {
			t.Fatal(err)
		}
		return n, ids
	}
	if err := scheduler.RecalcAffected(ctx, modelID, revisionID, []string{revenue, cogs}); err != nil {
		t.Fatal(err)
	}
	n, ids := rowsOf()
	for pass := 2; pass <= 4; pass++ {
		if err := scheduler.RecalcSpecific(ctx, modelID, revisionID, []string{profit}); err != nil {
			t.Fatal(err)
		}
		if n2, ids2 := rowsOf(); n2 != n || ids2 != ids || n != 1 {
			t.Fatalf("pass %d: %d rows %s, first pass %d rows %s", pass, n2, ids2, n, ids)
		}
	}
	if got, err := store.GetCalcValue(ctx, modelID, revisionID, profit, map[string]string{}); err != nil || got != 400 {
		t.Fatalf("gross_profit = %v err=%v", got, err)
	}
}
