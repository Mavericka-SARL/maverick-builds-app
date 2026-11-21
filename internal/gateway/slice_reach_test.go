package gateway

import (
	"testing"

	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// pinReach decides which metrics a one-dimension pin can change; a metric it
// leaves out is served its whole-model total by the slice fast path, so a
// metric wrongly left out would show an unpinned figure under the pin.
func TestPinReach(t *testing.T) {
	f := func(s string) *string { return &s }
	dims := map[string]*rollup.Dimension{
		"p": {ID: "p"},
		"s": {ID: "s"},
		"g": {ID: "g", SourceDimensionID: "p", SourceProperty: "segment"}, // groups products by a property
	}
	names := map[string]string{"p": "product", "s": "scenario", "g": "segment"}
	metrics := []metricRow{
		{ID: "x", Name: "x", IsInput: true},
		{ID: "y", Name: "y", Formula: f("x * 2")},
		{ID: "sin", Name: "s_in", IsInput: true},
		{ID: "t", Name: "t", Formula: f("s_in + 1")},
		{ID: "u", Name: "u", Formula: f("t * 2")},                         // reads across the scenario
		{ID: "v", Name: "v", Formula: f("u + y")},                         // transitively
		{ID: "w", Name: "w", Formula: f(`SUMIFS(x, scenario, "BASE")`)},   // names the dimension
		{ID: "k", Name: "k", Formula: f(`IF(scenario.kind = "A", x, 0)`)}, // a property of it
		{ID: "z", Name: "z", Formula: f("x + LEN(PARENT(product))")},
		{ID: "bad", Name: "bad", Formula: f("x +")},
		{ID: "c1", Name: "c1", Formula: f("PREVIOUS(c2) + x")}, // a recurrence reaching t
		{ID: "c2", Name: "c2", Formula: f("c1 + t")},
		{ID: "d1", Name: "d1", Formula: f("PREVIOUS(d2) + x")}, // a recurrence that does not
		{ID: "d2", Name: "d2", Formula: f("d1 + y")},
	}
	metricDims := map[string][]string{
		"x": {"p"}, "y": {"p"}, "sin": {"p", "s"}, "t": {"p", "s"}, "u": {"p"}, "v": {"p"}, "w": {"p"}, "k": {"p"},
		"z": {"p"}, "bad": {"p"}, "c1": {"p"}, "c2": {"p"}, "d1": {"p"}, "d2": {"p"},
	}
	got := pinReach(metrics, metricDims, dims, names, "s")
	for id, want := range map[string]bool{
		"x": false, "y": false, "sin": true, "t": true, "u": true, "v": true, "w": true, "k": true, "z": false,
		"bad": true, "c1": true, "c2": true, "d1": false, "d2": false,
	} {
		if got[id] != want {
			t.Errorf("pin on scenario: %s reached = %v, want %v", id, got[id], want)
		}
	}
	// A pin on a grouping of products reaches everything on products.
	got = pinReach(metrics, metricDims, dims, names, "g")
	for _, id := range []string{"x", "y", "z", "d1"} {
		if !got[id] {
			t.Errorf("pin on segment (groups product): %s not reached", id)
		}
	}
}
