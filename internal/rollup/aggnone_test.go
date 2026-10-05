package rollup

import (
	"context"
	"testing"
)

// agg_rule none: a value at the metric's own leaves only — an index or a
// correction % entered per member, a pick-list — and nothing at a total, or
// where a dimension the metric carries is left open. A dimension the metric
// does not carry is ignored, as for every rule.
func TestAggNoneHasNoTotal(t *testing.T) {
	dims := map[string]*Dimension{"dept": deptDim(), "other": {ID: "other", Members: []Member{{ID: "x", Code: "x"}}}}
	fetch := memFetch(t, map[string]float64{
		fetchKey("index", map[string]string{"dept": "ga"}):    1.1,
		fetchKey("index", map[string]string{"dept": "sales"}): 0.9,
		fetchKey("setting", map[string]string{}):              6,
	})
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		metric string
		dims   []string
		combo  map[string]string
		want   float64
		ok     bool
	}{
		{"a leaf", "index", []string{"dept"}, map[string]string{"dept": "ga"}, 1.1, true},
		{"a leaf, with a dimension it does not carry", "index", []string{"dept"}, map[string]string{"dept": "sales", "other": "x"}, 0.9, true},
		{"a parent", "index", []string{"dept"}, map[string]string{"dept": "region-a"}, 0, false},
		{"its dimension left open", "index", []string{"dept"}, map[string]string{}, 0, false},
		{"a dimensionless setting anywhere", "setting", nil, map[string]string{"dept": "region-a"}, 6, true},
	} {
		for _, resolve := range []func() (float64, bool, error){
			func() (float64, bool, error) { return Resolve(ctx, dims, c.metric, c.dims, AggNone, c.combo, fetch) },
			func() (float64, bool, error) {
				return ResolveTime(ctx, dims, c.metric, c.dims, AggNone, "sum", c.combo, fetch)
			},
		} {
			v, ok, err := resolve()
			if err != nil || ok != c.ok || v != c.want {
				t.Errorf("%s: %v %v %v, want %v %v", c.name, v, ok, err, c.want, c.ok)
			}
		}
	}
	if Aggregates(AggNone) || !Aggregates(AggSum) {
		t.Error("Aggregates: none gives no totals, sum does")
	}
}
