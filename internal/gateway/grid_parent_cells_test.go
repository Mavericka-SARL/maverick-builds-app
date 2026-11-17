package gateway

import "testing"

// A grid of territories grouped by their regions: a region's row holds a
// ratio's value worked out by the engine's rule (the formula over its
// inputs totalled), not "—" and not the territories' ratios added up
// (Territory & Quota build, 2026-11-16).
func TestParentDimensionCellsWorkOutRatios(t *testing.T) {
	region := "dim-region"
	dims := []gridDimension{
		{ID: "dim-sc", Name: "scenario", Members: []gridDimMember{{Code: "Base"}}},
		{ID: "dim-terr", Name: "territory", ParentDimensionID: &region, Members: []gridDimMember{
			{Code: "T1", ParentCode: "EU"}, {Code: "T2", ParentCode: "EU"}, {Code: "T3", ParentCode: "US"}}},
	}
	allDims := append(append([]gridDimension{}, dims...), gridDimension{ID: region, Name: "region", Members: []gridDimMember{
		{Code: "ALL"}, {Code: "EU", ParentCode: "ALL"}, {Code: "US", ParentCode: "ALL"}}})
	f := "IF(hours = 0, 0, 100 * work / hours)"
	metrics := []metricRow{
		{ID: "m-hours", Name: "hours", AggRule: "sum"},
		{ID: "m-work", Name: "work", AggRule: "sum"},
		{ID: "m-util", Name: "util", AggRule: "formula", Formula: &f, AggregateEvaluated: true},
	}
	md := map[string][]string{"m-hours": {"dim-sc", "dim-terr"}, "m-work": {"dim-sc", "dim-terr"}, "m-util": {"dim-sc", "dim-terr"}}
	cells := map[string]float64{
		"m-hours:Base:T1": 10, "m-hours:Base:T2": 30, "m-hours:Base:T3": 50,
		"m-work:Base:T1": 5, "m-work:Base:T2": 15, "m-work:Base:T3": 60,
		"m-util:Base:T1": 50, "m-util:Base:T2": 50, "m-util:Base:T3": 120,
	}
	addParentDimensionCells(dims, allDims, metrics, metrics, md, nil, nil, cells)
	for key, want := range map[string]float64{"m-util:Base:EU": 50, "m-util:Base:US": 120, "m-util:Base:ALL": 100 * 80.0 / 90} {
		if got, ok := cells[key]; !ok || got != want {
			t.Errorf("%s = %v (%v), want %v", key, got, ok, want)
		}
	}
	if _, ok := cells["m-hours:Base:EU"]; ok {
		t.Error("a sum's region row is the grid's to add up: none expected from the read")
	}
	// A reader with a territory hidden gets no region ratios.
	hiddenCells := map[string]float64{"m-hours:Base:T1": 10}
	addParentDimensionCells(dims, allDims, metrics, metrics, md, map[string]map[string]bool{"dim-terr": {"T2": true}}, nil, hiddenCells)
	if len(hiddenCells) != 1 {
		t.Errorf("with a hidden territory the read added %v", hiddenCells)
	}
}
