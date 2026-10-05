// scopeCalcCells serves hidden-member-restricted callers; its rollup cells
// must aggregate only what the caller may see. Direct unit-style test: no
// DB, in-memory dims/metrics/cells — the same shape grid() feeds it.
package gateway

import (
	"context"
	"math"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/rollup"
)

func TestScopeCalcCellsEmitsScopedRollupCells(t *testing.T) {
	geoID := "dim-geo"
	rollupDims := map[string]*rollup.Dimension{
		geoID: {ID: geoID, Members: []rollup.Member{
			{ID: "m-all", Code: "ALL"},
			{ID: "m-a", Code: "A", ParentCode: "ALL"},
			{ID: "m-b", Code: "B", ParentCode: "ALL"},
		}},
	}
	f := "=({revenue} - {cost}) / {revenue} * 100"
	universe := []metricRow{
		{ID: "rev", Name: "revenue", IsInput: true, AggRule: "sum"},
		{ID: "cost", Name: "cost", IsInput: true, AggRule: "sum"},
		{ID: "pct", Name: "margin_pct", IsInput: false, AggRule: "formula", Formula: &f},
	}
	metricDims := map[string][]string{"rev": {geoID}, "cost": {geoID}, "pct": {geoID}}
	// The caller's scoped input cells: they may see only member A (B's cells
	// were already filtered out upstream, exactly as grid() does).
	scoped := map[string]float64{
		"rev:A": 100, "cost:A": 80,
	}

	cells, totals, _ := scopeCalcCells(context.Background(), rollupDims, metricDims, map[string]string{geoID: "geography"}, universe, scoped, nil)

	if got := cells["pct:A"]; math.Abs(got-20) > 1e-6 {
		t.Errorf("leaf A = %v, want 20", got)
	}
	// The ALL rollup must aggregate ONLY the visible member: (100-80)/100.
	got, ok := cells["pct:ALL"]
	if !ok {
		t.Fatalf("no rollup cell for ALL — rollup enrichment missing; cells: %v", cells)
	}
	if math.Abs(got-20) > 1e-6 {
		t.Errorf("scoped ALL rollup = %v, want 20 (member B is hidden and must not contribute)", got)
	}
	if tot := totals["pct"]; math.Abs(tot-20) > 1e-6 {
		t.Errorf("scoped total = %v, want 20", tot)
	}
}

// A formula-rule total is evaluated at the scope's pins, as the scheduler's
// slice rows are. At {} a rule-none dependency read nothing even where the
// pin left it one leaf: the HR model's "Effective Global Note" (=drv_global,
// rule formula, on [department, cost_type]) showed 0 on the Cost Type FY
// Summary while its slice row held 2.5.
func TestScopeCalcCellsFormulaTotalReadsRuleNoneDependencyAtThePin(t *testing.T) {
	costID, deptID := "dim-cost", "dim-dept"
	rollupDims := map[string]*rollup.Dimension{
		// the scope's cost_type=BASE pin has trimmed the dimension to that member
		costID: {ID: costID, Members: []rollup.Member{{ID: "c-base", Code: "BASE"}}},
		deptID: {ID: deptID, Members: []rollup.Member{
			{ID: "d-all", Code: "ALL"},
			{ID: "d-x", Code: "X", ParentCode: "ALL"},
			{ID: "d-y", Code: "Y", ParentCode: "ALL"},
		}},
	}
	note := "drv_global"
	pct := "IF(cost_ly = 0, 0, (plan - cost_ly) / cost_ly * 100)"
	universe := []metricRow{
		{ID: "drv", Name: "drv_global", IsInput: true, AggRule: "none"},
		{ID: "ly", Name: "cost_ly", IsInput: true, AggRule: "sum"},
		{ID: "plan", Name: "plan", IsInput: true, AggRule: "sum"},
		{ID: "note", Name: "cost_global_pct", AggRule: "formula", Formula: &note},
		{ID: "pct", Name: "cost_var_pct", AggRule: "formula", Formula: &pct},
	}
	metricDims := map[string][]string{"drv": {costID}, "ly": {deptID, costID}, "plan": {deptID, costID},
		"note": {deptID, costID}, "pct": {deptID, costID}}
	scoped := map[string]float64{"drv:BASE": 2.5, "ly:X:BASE": 100, "ly:Y:BASE": 100, "plan:X:BASE": 110, "plan:Y:BASE": 120}
	sr := &scopedReads{Pinned: map[string]string{costID: "BASE"}}

	cells, totals, _ := scopeCalcCells(context.Background(), rollupDims, metricDims,
		map[string]string{costID: "cost_type", deptID: "department"}, universe, scoped, sr)

	// The All Departments rollup cell reads the one rate, not the rate once
	// per department (the browser showed 2.5 x 7, then x 12 months = 210%).
	if got := cells["note:ALL:BASE"]; math.Abs(got-2.5) > 1e-9 {
		t.Errorf("rollup cell at All Departments = %v, want 2.5", got)
	}

	if got := totals["note"]; math.Abs(got-2.5) > 1e-9 {
		t.Errorf("formula total of a rule-none dependency = %v, want 2.5 (its value at the pinned leaf)", got)
	}
	if got := totals["pct"]; math.Abs(got-15) > 1e-9 {
		t.Errorf("formula total = %v, want 15 ((230 - 200) / 200)", got)
	}
}
