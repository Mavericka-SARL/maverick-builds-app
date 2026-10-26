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

// A live read does not enumerate a metric with more visible cells than a
// pass may calculate (calculation.MaxMetricCells) — enumerating 2.5
// million took a gateway down — nor read a metric that reads it as if its
// values were zero; a smaller metric is read as before.
func TestScopeCalcCellsSkipsAMetricWithTooManyCells(t *testing.T) {
	t.Setenv("CALC_MAX_METRIC_CELLS", "3")
	geoID, skuID := "dim-geo", "dim-sku"
	rollupDims := map[string]*rollup.Dimension{
		geoID: {ID: geoID, Members: []rollup.Member{{ID: "g-a", Code: "A"}, {ID: "g-b", Code: "B"}}},
		skuID: {ID: skuID, Members: []rollup.Member{{ID: "s-x", Code: "X"}, {ID: "s-y", Code: "Y"}}},
	}
	big, reader, small := "{revenue} * 2", "{big} + 1", "{price} * 2"
	universe := []metricRow{
		{ID: "rev", Name: "revenue", IsInput: true, AggRule: "sum"},
		{ID: "price", Name: "price", IsInput: true, AggRule: "sum"},
		{ID: "big", Name: "big", AggRule: "sum", Formula: &big},
		{ID: "reader", Name: "reader", AggRule: "sum", Formula: &reader},
		{ID: "small", Name: "small", AggRule: "sum", Formula: &small},
	}
	metricDims := map[string][]string{"rev": {geoID, skuID}, "big": {geoID, skuID}, "price": {geoID}, "reader": {geoID}, "small": {geoID}}
	scoped := map[string]float64{"rev:A:X": 1, "price:A": 5}

	cells, totals, _ := scopeCalcCells(context.Background(), rollupDims, metricDims, map[string]string{geoID: "geography", skuID: "sku"}, universe, scoped, nil)

	for k := range cells {
		if len(k) >= 4 && (k[:4] == "big:" || k[:7] == "reader:") {
			t.Errorf("cell %s read for a metric over the limit or reading one", k)
		}
	}
	if _, ok := totals["big"]; ok {
		t.Error("big has a total")
	}
	if _, ok := totals["reader"]; ok {
		t.Error("reader has a total though big was not read")
	}
	if got := cells["small:A"]; got != 10 {
		t.Errorf("small @ A = %v, want 10", got)
	}
}

// A formula-aggregated ratio read by another formula above its leaves — at
// the pinned year, a quarter, the total — is the ratio evaluated there, as
// the scheduler reads it, never its months' ratios summed by its time
// summary: the CPG dashboard's "EBITDA margin vs LY" read 63.8 points where
// the change was 5.9 (2026-10-18).
func TestScopeCalcCellsReadsARatioAtAnAggregatePeriodByItsFormula(t *testing.T) {
	timeID := "t"
	rollupDims := map[string]*rollup.Dimension{timeID: {ID: timeID, IsTime: true, TimeGranularity: "month", Members: []rollup.Member{
		{Code: "2026-01", ParentCode: "Q1", TimeIndex: 0}, {Code: "2026-02", ParentCode: "Q1", TimeIndex: 1},
		{Code: "2026-03", ParentCode: "Q1", TimeIndex: 2}, {Code: "Q1", ParentCode: "FY26", TimeIndex: -1}, {Code: "FY26", TimeIndex: -1},
	}}}
	margin, ly, change := "ebitda / sales", "ly_ebitda / ly_sales", "margin - ly_margin"
	universe := []metricRow{
		{ID: "e", Name: "ebitda", IsInput: true, AggRule: "sum", TimeSummary: "sum"},
		{ID: "s", Name: "sales", IsInput: true, AggRule: "sum", TimeSummary: "sum"},
		{ID: "le", Name: "ly_ebitda", IsInput: true, AggRule: "sum", TimeSummary: "sum"},
		{ID: "ls", Name: "ly_sales", IsInput: true, AggRule: "sum", TimeSummary: "sum"},
		{ID: "m", Name: "margin", AggRule: "formula", TimeSummary: "sum", Formula: &margin},
		{ID: "l", Name: "ly_margin", AggRule: "formula", TimeSummary: "sum", Formula: &ly},
		{ID: "v", Name: "var_margin", AggRule: "formula", TimeSummary: "sum", Formula: &change},
	}
	metricDims := map[string][]string{}
	for _, m := range universe {
		metricDims[m.ID] = []string{timeID}
	}
	scoped := map[string]float64{}
	for i, mo := range []string{"2026-01", "2026-02", "2026-03"} {
		scoped["e:"+mo], scoped["s:"+mo] = float64(10*(i+1)), 100
		scoped["le:"+mo], scoped["ls:"+mo] = 5, 100
	}
	sr := &scopedReads{Pinned: map[string]string{timeID: "FY26"}}

	cells, totals, _ := scopeCalcCells(context.Background(), rollupDims, metricDims, map[string]string{timeID: "month"}, universe, scoped, sr)

	want := 60.0/300 - 15.0/300
	if got := totals["v"]; math.Abs(got-want) > 1e-9 {
		t.Errorf("change at the pinned year = %v, want %v (not its months' margins summed, 0.45)", got, want)
	}
	if got := cells["v:Q1"]; math.Abs(got-want) > 1e-9 {
		t.Errorf("change at Q1 = %v, want %v", got, want)
	}
	if got := cells["v:2026-02"]; math.Abs(got-(0.2-0.05)) > 1e-9 {
		t.Errorf("change in February = %v, want 0.15", got)
	}
}
