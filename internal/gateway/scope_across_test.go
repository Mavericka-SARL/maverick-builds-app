package gateway

import (
	"encoding/json"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// A filter chooses which cells are shown; it never changes what a cell is.
// period_revenue on [period] is the total over every geography, so it reads
// the same filtered to the UK, and so does everything built on it: a capped
// total, a share of it. Until 2026-11-07 the scoped read recomputed it from
// the UK's inputs alone, and a leaf's share read 1 instead of 0.5 at unchanged
// coordinates. filled_revenue is the other half of the same symptom: ISBLANK
// answered FALSE on an empty cell in every filtered read.
func TestFilteredReadLeavesEveryCellAsItIs(t *testing.T) {
	d := setupSalesDemo(t)

	add := func(name, formula string) {
		d.metric[name] = idOf(d.call("POST", "/api/developer/metrics", d.dev, map[string]any{
			"name": name, "formula": formula, "revision_id": d.revID, "agg_rule": "sum", "format": "number",
		}))
	}
	add("period_revenue", "={revenue}")
	add("capped_revenue", "=MIN({period_revenue}, 1000000)")
	add("revenue_share", "=IFERROR({revenue} / {period_revenue}, 0)")
	add("filled_revenue", "=IF(ISBLANK({revenue}), -1, {revenue})")

	totalsGrid := idOf(d.call("POST", "/api/developer/grids", d.dev, map[string]any{"name": "Period totals", "revision_id": d.revID}))
	d.call("POST", "/api/developer/grids/"+totalsGrid+"/dimensions/"+d.periodDim, d.dev, nil)
	for _, name := range []string{"period_revenue", "capped_revenue"} {
		d.call("POST", "/api/developer/grids/"+totalsGrid+"/metrics/"+d.metric[name], d.dev, nil)
	}
	for _, name := range []string{"revenue_share", "filled_revenue"} {
		d.call("POST", "/api/developer/grids/"+d.gridID+"/metrics/"+d.metric[name], d.dev, nil)
	}
	d.seedFacts()
	q1 := map[string]string{d.periodDim: "Q1"}
	d.awaitCalc("period_revenue", q1, 1_800_000)
	d.awaitCalc("capped_revenue", q1, 1_000_000)
	ukLaptop := map[string]string{d.geoDim: "UK", d.prodDim: "LAPTOP", d.periodDim: "Q1"}
	d.awaitCalc("revenue_share", ukLaptop, 0.5)

	type gridRead struct {
		Cells  map[string]float64 `json:"cells"`
		Totals map[string]float64 `json:"totals"`
	}
	read := func(persona, gridID string, scope map[string]string) gridRead {
		t.Helper()
		q := url.Values{"grid_id": {gridID}}
		if scope != nil {
			raw, _ := json.Marshal(scope)
			q.Set("scope", string(raw))
		}
		status, raw := d.req("GET", "/api/grid?"+q.Encode(), persona, nil)
		if status != 200 {
			t.Fatalf("grid read: %d %s", status, raw)
		}
		var g gridRead
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return g
	}
	periodCell := func(name, period string) string { return d.metric[name] + ":" + period }
	// A plan cell: its key lists the members in the metric's dimension order.
	planValue := func(g gridRead, name string, codes ...string) float64 {
		t.Helper()
		for key, v := range g.Cells {
			parts := strings.Split(key, ":")
			sort.Strings(parts[1:])
			want := slices.Clone(codes)
			sort.Strings(want)
			if parts[0] == d.metric[name] && slices.Equal(parts[1:], want) {
				return v
			}
		}
		t.Fatalf("no cell of %s at %v", name, codes)
		return 0
	}

	pins := []map[string]string{
		{d.geoDim: "UK"},
		{d.geoDim: "EMEA"},
		{d.prodDim: "LAPTOP"},
		{d.geoDim: "UK", d.prodDim: "LAPTOP"},
		{d.geoDim: "UK", d.prodDim: "LAPTOP", d.periodDim: "Q1"},
	}
	for _, grid := range []string{totalsGrid, d.gridID} {
		whole := read(d.dev, grid, nil)
		for _, pin := range pins {
			shown := read(d.dev, grid, pin)
			if len(shown.Cells) == 0 {
				t.Errorf("grid %s filtered to %v shows no cells", grid, pin)
			}
			for key, v := range shown.Cells {
				if want, ok := whole.Cells[key]; ok && !nearly(v, want) {
					t.Errorf("grid %s filtered to %v: cell %s = %v, unfiltered %v", grid, pin, key, v, want)
				}
			}
		}
	}

	uk := read(d.dev, totalsGrid, map[string]string{d.geoDim: "UK"})
	if got := uk.Cells[periodCell("period_revenue", "Q1")]; !nearly(got, 1_800_000) {
		t.Errorf("period_revenue at Q1 filtered to the UK = %v, want the total 1,800,000", got)
	}
	if got := uk.Cells[periodCell("capped_revenue", "Q1")]; !nearly(got, 1_000_000) {
		t.Errorf("capped_revenue at Q1 filtered to the UK = %v, want 1,000,000", got)
	}
	if got := uk.Totals[d.metric["period_revenue"]]; !nearly(got, 1_900_000) {
		t.Errorf("period_revenue total filtered to the UK = %v, want 1,900,000", got)
	}

	ukPlan := read(d.dev, d.gridID, map[string]string{d.geoDim: "UK"})
	if got := planValue(ukPlan, "revenue_share", "UK", "LAPTOP", "Q1"); !nearly(got, 0.5) {
		t.Errorf("revenue_share at UK × Laptop × Q1 filtered to the UK = %v, want 0.5", got)
	}
	if got := planValue(ukPlan, "filled_revenue", "UK", "MONITOR", "Q1"); !nearly(got, -1) {
		t.Errorf("filled_revenue on an empty cell filtered to the UK = %v, want -1", got)
	}

	// A restricted viewer: unfiltered, the total over what they may see. Filtered,
	// the stored total holds the hidden Americas and the slice would pass for
	// it, so the cell is withheld, and so is what reads it.
	d.setAccessRules(d.roID, member(d.geo["AMER"], "hidden"))
	if got := read(d.ro, totalsGrid, nil).Cells[periodCell("period_revenue", "Q1")]; !nearly(got, 1_400_000) {
		t.Errorf("period_revenue at Q1 with the Americas hidden = %v, want 1,400,000", got)
	}
	status, raw := d.req("GET", "/api/grid?"+url.Values{"grid_id": {totalsGrid}, "scope": {`{"` + d.geoDim + `":"UK"}`}}.Encode(), d.ro, nil)
	if status != 200 {
		t.Fatalf("restricted filtered read: %d %s", status, raw)
	}
	var restricted struct {
		Cells    map[string]float64 `json:"cells"`
		Withheld []string           `json:"withheld"`
	}
	if err := json.Unmarshal(raw, &restricted); err != nil {
		t.Fatal(err)
	}
	key := periodCell("period_revenue", "Q1")
	if v, ok := restricted.Cells[key]; ok {
		t.Errorf("period_revenue at Q1 with the Americas hidden, filtered to the UK = %v, want it withheld", v)
	}
	if !slices.Contains(restricted.Withheld, key) {
		t.Errorf("withheld = %v, want %s among them", restricted.Withheld, key)
	}
	if v, ok := restricted.Cells[periodCell("capped_revenue", "Q1")]; ok {
		t.Errorf("capped_revenue reads a withheld cell, so it is withheld too; it read %v", v)
	}
}

// Across the scope means a total over a restricted dimension the metric has
// no relation to: a pin reaching a metric through a parent dimension is a
// drill-down, and stays recomputed.
func TestReadsAcrossScope(t *testing.T) {
	dims := map[string]*rollup.Dimension{
		"dept":  {ID: "dept"},
		"staff": {ID: "staff", ParentDimensionID: "dept"},
		"month": {ID: "month"},
		"scen":  {ID: "scen"},
	}
	formula := "={amount} + 1"
	byName := map[string]metricRow{"amount": {ID: "amount", Name: "amount"}}
	metric := metricRow{ID: "m", Name: "m", Formula: &formula}
	amountDims := []string{"staff", "month"}

	for _, c := range []struct {
		name    string
		own     []string
		trimmed []string
		want    bool
	}{
		{"no scope", []string{"month"}, nil, false},
		{"pin on the dimension it totals", []string{"month"}, []string{"staff"}, true},
		{"pin on a parent of what it totals", []string{"month"}, []string{"dept", "staff"}, true},
		{"dimensionless", nil, []string{"month"}, true},
		{"pin on its own dimension", []string{"staff", "month"}, []string{"staff"}, false},
		{"pin on a dimension nothing reads", []string{"month"}, []string{"scen"}, false},
		{"a drill-down into its own parent", []string{"dept", "month"}, []string{"dept", "staff"}, false},
		{"pin below its own dimension", []string{"dept", "month"}, []string{"staff"}, false},
	} {
		sr := &scopedReads{Trimmed: map[string]bool{}, Dims: dims}
		for _, d := range c.trimmed {
			sr.Trimmed[d] = true
		}
		metricDims := map[string][]string{"m": c.own, "amount": amountDims}
		if got := sr.readsAcrossScope(metric, byName, metricDims); got != c.want {
			t.Errorf("%s: readsAcrossScope = %v, want %v", c.name, got, c.want)
		}
	}
}
