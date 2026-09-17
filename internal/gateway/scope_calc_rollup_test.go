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

	cells, totals := scopeCalcCells(context.Background(), rollupDims, metricDims, map[string]string{geoID: "geography"}, universe, scoped)

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
