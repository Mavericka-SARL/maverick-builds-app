package calculation_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// The lean path (metrics of very many cells: member indices instead of a
// map per cell, rows streamed into COPY, run alone, not kept in the pass
// cache) writes exactly the rows the ordinary path writes: leaves, rollup
// cells, one-dimension slices, aggregate periods and totals, for sums,
// averages, a formula-rule share, a closing balance and a metric reading a
// lean one. Test models are far below the threshold, so it is lowered.
func TestTheLeanPathWritesWhatTheOrdinaryPathWrites(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000e8"
	revID := "00000000-0000-0000-0001-0000000000e8"

	regionID := regionFixture(t, store, ctx, modelID, revID)
	productID := insertDimRev(t, store, ctx, modelID, revID, "product")
	addMember(t, store, ctx, productID, "P1", "", nil)
	addMember(t, store, ctx, productID, "P2", "", nil)
	monthID := insertTimeDim(t, store, ctx, modelID, revID, "month")
	months := insertMonths(t, store, ctx, monthID, 4)

	revenueID := insertMetric(t, store, ctx, modelID, revID, "revenue", "", true)
	balID := insertMetric(t, store, ctx, modelID, revID, "bal", "", true)
	setMetricField(t, store, ctx, balID, "time_summary", "last")
	calcs := []struct{ name, f, agg, ts string }{
		{"margin", "revenue * 0.3", "sum", ""},
		{"avg_rev", "revenue", "average", ""},
		{"share", `revenue / LOOKUP(revenue, region, "World")`, "formula", ""},
		{"bal2", "bal * 2", "sum", "last"},
	}
	ids := []string{revenueID, balID}
	for _, c := range calcs {
		id := insertCalc(t, store, ctx, modelID, revID, c.name, c.f)
		setMetricField(t, store, ctx, id, "agg_rule", c.agg)
		if c.ts != "" {
			setMetricField(t, store, ctx, id, "time_summary", c.ts)
		}
		ids = append(ids, id)
	}
	insertGridSetup(t, store, ctx, modelID, []string{regionID, productID, monthID}, ids)
	readerID := insertCalc(t, store, ctx, modelID, revID, "reader", "margin + 1")
	insertGridSetup(t, store, ctx, modelID, []string{regionID}, []string{readerID})

	v := 1.0
	for _, region := range []string{"DE", "FR", "US"} {
		for _, product := range []string{"P1", "P2"} {
			for _, month := range months {
				v += 3.25
				insertFact(t, store, ctx, modelID, revID, revenueID, factJSON(regionID, region, productID, product, monthID, month), v)
				insertFact(t, store, ctx, modelID, revID, balID, factJSON(regionID, region, productID, product, monthID, month), v/2)
			}
		}
	}
	runRecalc(t, store, ctx, modelID, revID, []string{revenueID, balID})
	ordinary := resultSnapshot(t, store, ctx, revID)

	defer calculation.SetLeanCellsAt(1)()
	var calcIDs []string
	if err := store.Pool().QueryRow(ctx, `SELECT array_agg(id::text) FROM model.metric_def WHERE revision_id = $1::uuid AND NOT is_input`, revID).Scan(&calcIDs); err != nil {
		t.Fatal(err)
	}
	if err := calculation.NewScheduler(logger.New("calc-test"), store).RecalcSpecific(ctx, modelID, revID, calcIDs); err != nil {
		t.Fatal(err)
	}
	lean := resultSnapshot(t, store, ctx, revID)

	if len(ordinary) < 100 {
		t.Fatalf("only %d result rows: the model no longer covers the reductions", len(ordinary))
	}
	if strings.Join(lean, "\n") != strings.Join(ordinary, "\n") {
		missing, extra := diffLines(ordinary, lean)
		t.Fatalf("the lean path wrote other rows than the ordinary path (%d vs %d):\nmissing %v\nextra %v", len(lean), len(ordinary), missing, extra)
	}
}

// resultSnapshot is every calculated row of revID as "metric dims=value",
// sorted.
func resultSnapshot(t *testing.T, store *calculation.Store, ctx context.Context, revID string) []string {
	t.Helper()
	rows, err := store.Pool().Query(ctx, `
		SELECT md.name, c.dim_members::text, c.value::text FROM runtime.calc_result c JOIN model.metric_def md ON md.id = c.metric_id
		WHERE c.revision_id = $1::uuid`, revID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, dims, value string
		if err := rows.Scan(&name, &dims, &value); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s=%s", name, dims, value))
	}
	sort.Strings(out)
	return out
}

func diffLines(want, got []string) (missing, extra []string) {
	in := func(list []string) map[string]bool {
		m := map[string]bool{}
		for _, s := range list {
			m[s] = true
		}
		return m
	}
	w, g := in(want), in(got)
	for _, s := range want {
		if !g[s] && len(missing) < 5 {
			missing = append(missing, s)
		}
	}
	for _, s := range got {
		if !w[s] && len(extra) < 5 {
			extra = append(extra, s)
		}
	}
	return missing, extra
}
