package main

import "fmt"

// runDimensionalTests covers what the pure-literal formula matrix in
// formula_coverage.go can't: dimension-conditional formulas (a formula that
// branches on a bare dimension-name identifier, e.g. =IF(region="A",...)),
// a real cross-metric arithmetic formula over two input metrics, and
// agg_rule=average's per-combo-then-average semantics (as opposed to the
// default sum-of-combos).
func runDimensionalTests(dev *api) {
	step("creating dimension \"qa_region\" (2 members: A, B)")
	regionDimID := createDimension(dev, "qa_region", "")
	memA := createMember(dev, regionDimID, "A", "Region A", "")
	memB := createMember(dev, regionDimID, "B", "Region B", "")
	_ = memA
	_ = memB

	step("creating grid \"QA Dim Grid\" (dims: qa_region)")
	gridID := createGrid(dev, "QA Dim Grid")
	assignDimension(dev, gridID, regionDimID)

	step("creating input metric \"units\" and dimension-conditional calc \"region_bonus\"")
	unitsID := createInputMetric(dev, "units")
	// =IF(qa_region="A", units*10, units*5) — bare "qa_region" identifier
	// makes this dimension-conditional (formulaReferencesDims in
	// scheduler.go), so it must be evaluated once per member, not once
	// globally with a flat broadcast.
	bonusID := createCalcMetric(dev, "region_bonus", `IF(qa_region="A",units*10,units*5)`, "")

	step("creating agg_rule=average calc \"avg_check\" (also dimension-conditional, trivially)")
	// Formula mentions qa_region (even though both branches are identical)
	// specifically to force the per-combo-then-average code path rather
	// than the total-level shortcut — see scheduler.go's executePartition.
	avgID := createCalcMetric(dev, "avg_check", `IF(qa_region="A",units,units)`, "average")

	assignMetric(dev, gridID, unitsID)
	assignMetric(dev, gridID, bonusID)
	assignMetric(dev, gridID, avgID)

	step("creating input metrics \"revenue\"/\"cost_amt\" and cross-metric calc \"margin_pct\"")
	revenueID := createInputMetric(dev, "revenue")
	costID := createInputMetric(dev, "cost_amt")
	marginID := createCalcMetric(dev, "margin_pct", `ROUND((revenue-cost_amt)/revenue*100,1)`, "")

	step("writing units A=3, B=4; revenue=1000, cost_amt=650")
	writeCell(dev, unitsID, map[string]string{regionDimID: "A"}, 3)
	writeCell(dev, unitsID, map[string]string{regionDimID: "B"}, 4)
	writeCell(dev, revenueID, nil, 1000)
	writeCell(dev, costID, nil, 650)

	grid := dev.call("GET", fmt.Sprintf("/api/grid?grid_def_id=%s&revision_id=%s", gridID, dev.revisionID), nil)
	totals, _ := grid["totals"].(map[string]any)

	// NOTE: calc metrics never get a per-member entry in the grid's `cells`
	// map — the backend calculation engine (internal/calculation) only ever
	// writes ONE aggregated scalar per calc metric (dim_members='{}') to
	// runtime.calc_result; per-cell calc values (what the grid UI actually
	// renders per column/row) are computed client-side in BusinessConsole.tsx.
	// So the only thing checkable through this API is the aggregate: A(30)+B(20).
	checkTotal := func(name, metricID string, expected float64) {
		v, ok := totals[metricID]
		if !ok {
			record(name, false, "missing from totals")
			return
		}
		f, _ := v.(float64)
		record(name, f == expected, fmt.Sprintf("expected %v got %v", expected, v))
	}
	checkTotal("region_bonus total = A(units3*10=30)+B(units4*5=20) = 50", bonusID, 50)
	checkTotal("avg_check = average(3,4) = 3.5 (not sum=7)", avgID, 3.5)

	// margin_pct is scalar (no grid dims) so its total isn't in this grid's
	// scoped totals map (grid() only returns per-metric totals for metrics
	// with no dims OR reachable via this call's whole-model semantics) —
	// re-fetch whole-model to check it, same as phase 1.
	wholeModel := dev.call("GET", fmt.Sprintf("/api/grid?revision_id=%s", dev.revisionID), nil)
	wmTotals, _ := wholeModel["totals"].(map[string]any)
	if v, ok := wmTotals[marginID]; ok {
		f, _ := v.(float64)
		record("margin_pct = ROUND((1000-650)/1000*100,1) = 35", f == 35, fmt.Sprintf("expected 35 got %v", v))
	} else {
		record("margin_pct = ROUND((1000-650)/1000*100,1) = 35", false, "missing from totals")
	}
}
