package rollup

import "testing"

// NoValueByRule answers where a metric has no value by its own rules — time
// summary none over several periods, aggregation none above its members —
// as ResolveTime's no-value exits do; "nothing recorded" is not its
// business.
func TestNoValueByRule(t *testing.T) {
	month := &Dimension{ID: "t", IsTime: true, TimeGranularity: "month", Members: []Member{
		{Code: "FY26", TimeIndex: -1}, {Code: "Q1", ParentCode: "FY26", TimeIndex: -1},
		{Code: "2026-01", ParentCode: "Q1", TimeIndex: 0}, {Code: "2026-02", ParentCode: "Q1", TimeIndex: 1},
	}}
	region := &Dimension{ID: "r", Members: []Member{{Code: "World"}, {Code: "EMEA", ParentCode: "World"}, {Code: "US", ParentCode: "World"}}}
	dims := map[string]*Dimension{"t": month, "r": region}
	both := []string{"r", "t"}
	for _, tc := range []struct {
		name   string
		agg    AggRule
		ts     TimeSummaryRule
		combo  map[string]string
		noneOK bool
	}{
		{"time none at the year", AggSum, "none", map[string]string{"r": "US", "t": "FY26"}, true},
		{"time none at a quarter", AggSum, "none", map[string]string{"r": "US", "t": "Q1"}, true},
		{"time none over every period", AggSum, "none", map[string]string{"r": "US"}, true},
		{"time none at a month", AggSum, "none", map[string]string{"r": "US", "t": "2026-01"}, false},
		{"time none at a parent region and a month", AggSum, "none", map[string]string{"r": "World", "t": "2026-01"}, false},
		{"time sum at the year", AggSum, "sum", map[string]string{"r": "US", "t": "FY26"}, false},
		{"aggregation none at a parent", AggNone, "sum", map[string]string{"r": "World", "t": "2026-01"}, true},
		{"aggregation none unpinned", AggNone, "sum", map[string]string{"t": "2026-01"}, true},
		{"aggregation none at a leaf", AggNone, "sum", map[string]string{"r": "US", "t": "2026-01"}, false},
		{"formula", AggFormula, "none", map[string]string{"r": "US", "t": "FY26"}, false},
		{"rate", AggRate, "none", map[string]string{"r": "World"}, false},
	} {
		if got := NoValueByRule(dims, both, tc.agg, tc.ts, tc.combo); got != tc.noneOK {
			t.Errorf("%s: NoValueByRule = %v, want %v", tc.name, got, tc.noneOK)
		}
	}
	// Without a time dimension, time summary none says nothing.
	if NoValueByRule(dims, []string{"r"}, AggSum, "none", map[string]string{}) {
		t.Error("no time dimension: time summary none must not mean no value")
	}
}
