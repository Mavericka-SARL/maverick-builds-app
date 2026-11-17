package dataexport

import (
	"runtime"
	"strings"
	"testing"
)

// An include_totals export over many members is refused before its points
// are made, not after. On 2026-11-17 the AI developer previewed a CRM
// Pipeline export over six dimensions (5, 7, 8, 12, 51 and 340 members):
// 35,196,840 combinations named a parent, each made as a map before the
// 600 cap was checked, and the gateway was killed at its 3 GiB limit.
func TestTotalsCapIsCheckedBeforeThePointsAreMade(t *testing.T) {
	sizes := []struct {
		name             string
		members, parents int
	}{
		{"Currency", 5, 0}, {"Service Category", 7, 1}, {"Manager", 8, 1},
		{"Deal Stage", 12, 3}, {"Close Month", 51, 15}, {"Account", 340, 1},
	}
	var g Grid
	g.Name = "Pipeline"
	var dimIDs []string
	for _, s := range sizes {
		d := Dimension{ID: "dim-" + s.name, Name: s.name}
		for i := 0; i < s.parents; i++ {
			d.Members = append(d.Members, Member{Code: "P" + itoa(i)})
		}
		for i := 0; i < s.members-s.parents; i++ {
			m := Member{Code: "L" + itoa(i)}
			if s.parents > 0 {
				m.ParentCode = "P" + itoa(i%s.parents)
			}
			d.Members = append(d.Members, m)
		}
		g.Dimensions = append(g.Dimensions, d)
		dimIDs = append(dimIDs, d.ID)
	}
	g.Metrics = []Metric{{ID: "m-value", Name: "deal_value", DimensionIDs: dimIDs}}

	asked := false
	snap := Snapshot{Grid: g, Cells: map[string]float64{}, TotalsAt: func(points []map[string]string) ([]map[string]float64, error) {
		asked = true
		return make([]map[string]float64, len(points)), nil
	}}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := Render(Spec{Layout: "wide", Metrics: []string{"deal_value"}, IncludeTotals: true}, snap)
	runtime.ReadMemStats(&after)

	if err == nil || !strings.Contains(err.Error(), "35196840 totals") || !strings.Contains(err.Error(), "selector bar") {
		t.Fatalf("an export asking for 35,196,840 totals: %v; want it refused with its count, saying how to narrow it", err)
	}
	if asked {
		t.Error("the engine was asked for totals the export refuses")
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Errorf("refusing the export allocated %d MB; want it refused before the points are made", grew>>20)
	}

	// Narrowed to one leaf of Account, Close Month and Deal Stage, the
	// same export stays under the cap and reads exactly the points the
	// count promised: 5×7×8 combinations less the 5×6×7 leaf-only ones.
	var got int
	snap.TotalsAt = func(points []map[string]string) ([]map[string]float64, error) {
		got += len(points)
		return make([]map[string]float64, len(points)), nil
	}
	_, err = Render(Spec{Layout: "wide", Metrics: []string{"deal_value"}, IncludeTotals: true,
		Filters: map[string][]string{"Account": {"L0"}, "Close Month": {"L0"}, "Deal Stage": {"L0"}}}, snap)
	if err != nil {
		t.Fatalf("a narrowed export: %v", err)
	}
	if want := 5*7*8 - 5*6*7; got != want {
		t.Errorf("a narrowed export read %d totals; want %d", got, want)
	}
}
