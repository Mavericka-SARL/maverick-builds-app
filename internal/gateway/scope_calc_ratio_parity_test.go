package gateway

import (
	"context"
	"math"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// A pure ratio with agg_rule average (it totals as its formula,
// calculation.EffectiveAggRule) read above its leaves by another formula is
// the ratio of the sums there — in the scoped read as in the scheduler
// (calculation TestPlainReadOfAPureRatioAboveItsLeaves): x = margin * 2 at
// EMEA is 2 × 40 / 200, never 2 × the mean of the products' margins.
func TestScopeCalcCellsReadsAnAveragePureRatioByItsFormula(t *testing.T) {
	regionID, productID := "dim-region", "dim-product"
	rollupDims := map[string]*rollup.Dimension{
		regionID: {ID: regionID, Members: []rollup.Member{
			{ID: "r-w", Code: "World"}, {ID: "r-e", Code: "EMEA", ParentCode: "World"}, {ID: "r-u", Code: "US", ParentCode: "World"},
		}},
		productID: {ID: productID, Members: []rollup.Member{{ID: "p1", Code: "P1"}, {ID: "p2", Code: "P2"}, {ID: "p3", Code: "P3"}}},
	}
	margin, x := "profit / revenue", "margin * 2"
	universe := []metricRow{
		{ID: "p", Name: "profit", IsInput: true, AggRule: "sum"},
		{ID: "r", Name: "revenue", IsInput: true, AggRule: "sum"},
		{ID: "m", Name: "margin", AggRule: "average", Formula: &margin},
		{ID: "x", Name: "x", AggRule: "sum", Formula: &x},
	}
	metricDims := map[string][]string{"p": {regionID, productID}, "r": {regionID, productID}, "m": {regionID, productID}, "x": {regionID}}
	scoped := map[string]float64{
		"p:EMEA:P1": 10, "r:EMEA:P1": 100, "p:EMEA:P2": 30, "r:EMEA:P2": 50, "p:EMEA:P3": 0, "r:EMEA:P3": 50,
		"p:US:P1": 5, "r:US:P1": 10,
	}

	cells, _, _ := scopeCalcCells(context.Background(), rollupDims, metricDims,
		map[string]string{regionID: "region", productID: "product"}, universe, scoped, nil)

	if got := cells["x:EMEA"]; math.Abs(got-0.4) > 1e-9 {
		t.Errorf("x at EMEA = %v, want 0.4 (2 × 40 / 200, not 2 × the mean of the products' margins)", got)
	}
	if got := cells["x:US"]; math.Abs(got-1) > 1e-9 {
		t.Errorf("x at US = %v, want 1", got)
	}
}
