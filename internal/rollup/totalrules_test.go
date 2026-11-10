package rollup

import (
	"context"
	"testing"
)

// A dimension's total rule (migration 174) rewrites the coordinate a read
// totals at: first and last read one member, none reads nothing above the
// members, and a metric's own rule wins over the dimension's. Resolve
// totals by it; a time dimension keeps its time summary.
func TestTotalRulesRewriteTheRead(t *testing.T) {
	scen := &Dimension{ID: "scen", TotalRule: TotalFirst, Members: []Member{
		{Code: "ALL"}, {Code: "BASE", ParentCode: "ALL"}, {Code: "DOWN", ParentCode: "ALL"}, {Code: "UP", ParentCode: "ALL"}}}
	region := &Dimension{ID: "region", Members: []Member{{Code: "EU"}, {Code: "US"}}}
	month := &Dimension{ID: "month", IsTime: true, TotalRule: TotalNone, Members: []Member{{Code: "JAN", TimeIndex: 0}, {Code: "FEB", TimeIndex: 1}}}
	dims := map[string]*Dimension{"scen": scen, "region": region, "month": month}
	own := []string{"region", "scen"}

	for name, c := range map[string]struct {
		metric string
		combo  map[string]string
		want   map[string]string
		ok     bool
	}{
		"unpinned reads the first":     {"m", map[string]string{"region": "EU"}, map[string]string{"region": "EU", "scen": "BASE"}, true},
		"a parent reads its first":     {"m", map[string]string{"scen": "ALL"}, map[string]string{"scen": "BASE"}, true},
		"a leaf is read as it is":      {"m", map[string]string{"scen": "DOWN"}, map[string]string{"scen": "DOWN"}, true},
		"the metric's own rule: last":  {"last", map[string]string{}, map[string]string{"scen": "UP"}, true},
		"the metric's own rule: sum":   {"sum", map[string]string{}, map[string]string{}, true},
		"the metric's own rule: none":  {"none", map[string]string{"region": "EU"}, nil, false},
		"none at a leaf has its value": {"none", map[string]string{"scen": "UP"}, map[string]string{"scen": "UP"}, true},
	} {
		scen.MetricRules = map[string]string{"last": TotalLast, "sum": TotalSum, "none": TotalNone}
		got, ok := RuleCombo(dims, c.metric, own, c.combo)
		if ok != c.ok || len(got) != len(c.want) {
			t.Errorf("%s: %v %v, want %v %v", name, got, ok, c.want, c.ok)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: %v, want %v", name, got, c.want)
			}
		}
	}
	// A time dimension's rule is its metrics' time summary, never this.
	if got, ok := RuleCombo(dims, "m", []string{"month"}, map[string]string{}); !ok || len(got) != 0 {
		t.Errorf("a time dimension's rule rewrote the read: %v %v", got, ok)
	}

	// Resolve totals at the rule's member: the total over every scenario
	// and region is the first scenario's, the regions added up.
	values := map[string]float64{"EU|BASE": 10, "US|BASE": 20, "EU|DOWN": 1, "US|DOWN": 2, "EU|UP": 100, "US|UP": 200}
	fetch := func(_ context.Context, _ string, c map[string]string) (float64, bool, error) {
		v, ok := values[c["region"]+"|"+c["scen"]]
		return v, ok, nil
	}
	for metric, want := range map[string]float64{"m": 30, "last": 300, "sum": 333} {
		if v, ok, err := Resolve(context.Background(), dims, metric, own, AggSum, map[string]string{}, fetch); err != nil || !ok || v != want {
			t.Errorf("%s total: %v %v %v, want %v", metric, v, ok, err, want)
		}
	}
	if _, ok, _ := Resolve(context.Background(), dims, "none", own, AggSum, map[string]string{}, fetch); ok {
		t.Error("none: a total over the scenarios")
	}
	if !NoValueByRule(dims, "none", own, AggSum, "", map[string]string{"region": "EU"}) {
		t.Error("NoValueByRule: none gives no value above the scenarios")
	}
}
