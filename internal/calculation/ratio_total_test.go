package calculation_test

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// TestRatioReadAtATotalPeriodIsItsFormula: a formula-aggregated ratio with
// time summary sum, read by another formula at an aggregate period, is the
// ratio evaluated there — 60/300 at FY26 — as its own cell is, never the sum
// of its months' ratios (2026-10-18: the CPG dashboard's "EBITDA margin vs
// LY" showed 63.8 points where the change was 5.9).
func TestRatioReadAtATotalPeriodIsItsFormula(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000f7"
	revID := "00000000-0000-0000-0001-0000000000f7"

	regionID := regionFixture(t, store, ctx, modelID, revID)
	monthID, _ := monthHierarchy(t, store, ctx, modelID, revID, 1)
	inputs := map[string]string{}
	for _, n := range []string{"ebitda", "sales", "ly_ebitda", "ly_sales"} {
		inputs[n] = insertMetric(t, store, ctx, modelID, revID, n, "", true)
	}
	calc := func(name, f string) string {
		id := insertCalc(t, store, ctx, modelID, revID, name, f)
		setMetricField(t, store, ctx, id, "agg_rule", "formula")
		setMetricField(t, store, ctx, id, "time_summary", "sum")
		return id
	}
	marginID := calc("margin", "ebitda / sales")
	lyID := calc("ly_margin", "ly_ebitda / ly_sales")
	varID := calc("var_margin", "margin - ly_margin")
	all := []string{inputs["ebitda"], inputs["sales"], inputs["ly_ebitda"], inputs["ly_sales"], marginID, lyID, varID}
	insertGridSetup(t, store, ctx, modelID, []string{regionID, monthID}, all)
	for i, m := range []string{"2026-01", "2026-02", "2026-03"} {
		at := factJSON(regionID, "DE", monthID, m)
		insertFact(t, store, ctx, modelID, revID, inputs["ebitda"], at, float64(10*(i+1)))
		insertFact(t, store, ctx, modelID, revID, inputs["sales"], at, 100)
		insertFact(t, store, ctx, modelID, revID, inputs["ly_ebitda"], at, 5)
		insertFact(t, store, ctx, modelID, revID, inputs["ly_sales"], at, 100)
	}
	runRecalc(t, store, ctx, modelID, revID, []string{inputs["ebitda"], inputs["sales"], inputs["ly_ebitda"], inputs["ly_sales"]})

	c := rowChecker{t, store, ctx, modelID, revID, map[string]string{marginID: "margin", lyID: "ly_margin", varID: "var_margin"}}
	fy := func(region string) map[string]string { return pins(regionID, region, monthID, "FY26") }
	c.want(marginID, fy("DE"), 60.0/300)
	c.want(lyID, fy("DE"), 15.0/300)
	c.want(varID, fy("DE"), 60.0/300-15.0/300)
	c.want(varID, fy("EMEA"), 60.0/300-15.0/300)
	c.want(varID, pins(regionID, "DE", monthID, "2026-02"), 0.2-0.05)
}

// TestRecalcFromAllInputsReachesDependentsOfDependencyFreeMetrics: a
// revision recalculated from all its inputs (a copy, an import) computes a
// metric that reads only a dependency-free metric; it is reached neither
// from an input nor as dependency-free itself, and stayed blank.
func TestRecalcFromAllInputsReachesDependentsOfDependencyFreeMetrics(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000f8"
	revID := "00000000-0000-0000-0001-0000000000f8"

	regionID := regionFixture(t, store, ctx, modelID, revID)
	salesID := insertMetric(t, store, ctx, modelID, revID, "sales", "", true)
	countID := insertCalc(t, store, ctx, modelID, revID, "regions", `COUNTIFS(region, "*")`)
	twiceID := insertCalc(t, store, ctx, modelID, revID, "regions_x2", "regions * 2")
	insertGridSetup(t, store, ctx, modelID, []string{regionID}, []string{salesID, countID, twiceID})
	insertFact(t, store, ctx, modelID, revID, salesID, factJSON(regionID, "DE"), 1)

	sched := calculation.NewScheduler(logger.New("calc-test"), store)
	if err := sched.RecalcFromAllInputs(ctx, modelID, revID, []string{salesID}); err != nil {
		t.Fatalf("RecalcFromAllInputs: %v", err)
	}
	c := rowChecker{t, store, ctx, modelID, revID, map[string]string{countID: "regions", twiceID: "regions_x2"}}
	n, ok := calcRow(t, store, ctx, modelID, revID, countID, pins(regionID, "DE"))
	if !ok {
		t.Fatal("the dependency-free metric itself has no row")
	}
	c.want(twiceID, pins(regionID, "DE"), 2*n)
}
