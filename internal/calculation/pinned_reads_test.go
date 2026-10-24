package calculation

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// A read that pins every dimension of its source is made once per pass and
// answers the same from every cell — a SUMIFS's criteria metric at each
// candidate row, re-read for every cell, was half of a DDMRP model's
// recalculation (2026-10-23). A read that leaves a dimension to the cell is
// the cell's own, and with the memo off (a recurrence, whose values appear
// during the pass) nothing is kept.
func TestPinnedReadsAreReadOncePerPass(t *testing.T) {
	dims := map[string]*rollup.Dimension{
		"d-row":  {ID: "d-row", Members: []rollup.Member{{Code: "R1"}, {Code: "R2"}}},
		"d-node": {ID: "d-node", Members: []rollup.Member{{Code: "N1"}, {Code: "N2"}}},
	}
	meta := NewDimMetadata(dims, map[string]string{"d-row": "row", "d-node": "node"}, nil)
	values := map[string]float64{ // dimKey -> value
		dimKey(map[string]string{"d-row": "R1"}):                 7,
		dimKey(map[string]string{"d-row": "R1", "d-node": "N1"}): 1,
		dimKey(map[string]string{"d-row": "R1", "d-node": "N2"}): 2,
	}
	reads := map[string]int{}
	fetch := func(_ context.Context, metricID string, combo map[string]string) (float64, bool, error) {
		reads[metricID]++
		v, ok := values[dimKey(combo)]
		return v, ok, nil
	}
	defs := map[string]*MetricDef{
		"m-demand-node": {ID: "m-demand-node", Name: "demand_node", AggRule: "sum"},
		"m-qty":         {ID: "m-qty", Name: "qty", AggRule: "sum"},
	}
	def := &MetricDef{ID: "m-calc", Name: "calc", DependsOnID: []string{"m-demand-node", "m-qty"}}
	metricDims := map[string][]string{"m-demand-node": {"d-row"}, "m-qty": {"d-row", "d-node"}}
	pass := func(memo bool) *dimReads {
		return newDimReads(context.Background(), meta, def, defs, metricDims,
			map[string]rollup.RawValue{"m-demand-node": fetch, "m-qty": fetch}, nil, memo)
	}

	r := pass(true)
	for _, node := range []string{"N1", "N2", "N1"} {
		r.data = false
		v, found, ferr := r.resolve(map[string]string{"d-node": node}, "demand_node", map[string]string{"row": "R1"})
		if ferr != nil || !found || v != 7 {
			t.Fatalf("demand_node at R1 from %s: %v %v %v, want 7", node, v, found, ferr)
		}
		if !r.data {
			t.Errorf("a kept non-zero read from %s does not count as data", node)
		}
	}
	if reads["m-demand-node"] != 1 {
		t.Errorf("demand_node at R1 was read %d times in one pass, want 1", reads["m-demand-node"])
	}

	// qty is also dimensioned by node: pinning the row leaves the node to
	// the cell.
	for node, want := range map[string]float64{"N1": 1, "N2": 2} {
		if v, _, ferr := r.resolve(map[string]string{"d-node": node}, "qty", map[string]string{"row": "R1"}); ferr != nil || v != want {
			t.Errorf("qty at R1 from %s: %v %v, want %v", node, v, ferr, want)
		}
	}

	reads = map[string]int{}
	r = pass(false)
	for _, node := range []string{"N1", "N2"} {
		if _, _, ferr := r.resolve(map[string]string{"d-node": node}, "demand_node", map[string]string{"row": "R1"}); ferr != nil {
			t.Fatal(ferr)
		}
	}
	if reads["m-demand-node"] != 2 {
		t.Errorf("with the memo off demand_node was read %d times, want each time (2)", reads["m-demand-node"])
	}
}
