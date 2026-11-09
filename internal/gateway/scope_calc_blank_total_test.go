package gateway

import (
	"context"
	"math"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// The grid's scoped calculation reads a metric across a dimension as the
// scheduler does (calculation TestReadAcrossADimensionWithNothingRecordedIsBlank):
// where nothing was entered under the cell it is blank to ISBLANK, and 0 in
// arithmetic.
func TestScopeCalcCellsReadAcrossADimensionWithNothingRecordedIsBlank(t *testing.T) {
	glID, monthID := "dim-gl", "dim-month"
	rollupDims := map[string]*rollup.Dimension{
		glID: {ID: glID, Members: []rollup.Member{
			{ID: "g-all", Code: "ALLGL"}, {ID: "g-1", Code: "G1", ParentCode: "ALLGL"}, {ID: "g-2", Code: "G2", ParentCode: "ALLGL"},
		}},
		monthID: {ID: monthID, IsTime: true, TimeGranularity: "month", Members: []rollup.Member{
			{ID: "m-1", Code: "2026-01", TimeIndex: 0}, {ID: "m-2", Code: "2026-02", TimeIndex: 1}, {ID: "m-3", Code: "2026-03", TimeIndex: 2},
		}},
	}
	flag, plus := "IF(ISBLANK(actual), -1, 1)", "actual + 5"
	universe := []metricRow{
		{ID: "a", Name: "actual", IsInput: true, AggRule: "sum", TimeSummary: "sum"},
		{ID: "f", Name: "flag", AggRule: "sum", TimeSummary: "sum", Formula: &flag},
		{ID: "p", Name: "plus", AggRule: "sum", TimeSummary: "sum", Formula: &plus},
	}
	metricDims := map[string][]string{"a": {glID, monthID}, "f": {monthID}, "p": {monthID}}
	scoped := map[string]float64{"a:G1:2026-01": 10, "a:G2:2026-02": 20}

	cells, _, _ := scopeCalcCells(context.Background(), rollupDims, metricDims,
		map[string]string{glID: "gl", monthID: "month"}, universe, scoped, nil)

	for code, want := range map[string]float64{"2026-01": 1, "2026-02": 1, "2026-03": -1} {
		if got := cells["f:"+code]; math.Abs(got-want) > 1e-9 {
			t.Errorf("flag at %s = %v, want %v", code, got, want)
		}
	}
	if got := cells["p:2026-03"]; math.Abs(got-5) > 1e-9 {
		t.Errorf("plus at 2026-03 = %v, want 5 (blank is 0 in arithmetic)", got)
	}
}
