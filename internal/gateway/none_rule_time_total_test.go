package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// TestNoneRuleStillTotalsOverTime: agg_rule none gives no total across
// members, but "time still totals by the time summary" — at one leaf of the
// non-time dimensions an H1 or FY value is the periods reduced by
// time_summary, exactly as the grid's own period columns show it. The
// scheduler's {region: UK} slice row, the scoped read of a calculated metric
// and the scoped read of an input metric all used to give none.
//
// revenue at (region, month m = 1..6): UK 100+m, DE 200+m, US 300+m;
// idx = revenue * 10 with time_summary last, so UK over H1 is its June value.
func TestNoneRuleStillTotalsOverTime(t *testing.T) {
	f := setupRestrictedFixture(t)
	f.metric["idx"] = f.dev_("POST", "/api/developer/metrics", map[string]any{"name": "idx", "formula": "revenue * 10",
		"revision_id": f.revID, "agg_rule": "none", "format": "number", "time_summary": "last"})
	f.metric["pct_in"] = f.dev_("POST", "/api/developer/metrics", map[string]any{"name": "pct_in", "is_input": true,
		"revision_id": f.revID, "agg_rule": "none", "format": "number", "time_summary": "last"})
	for _, name := range []string{"idx", "pct_in"} {
		f.dev_("POST", "/api/developer/grids/"+f.plan+"/metrics/"+f.metric[name], nil)
	}
	f.loadMetricDims()
	for _, c := range []struct {
		region, period string
		v              float64
	}{{"UK", "2026-01", 5}, {"UK", "2026-02", 7}, {"DE", "2026-01", 9}} {
		f.dev_("POST", "/api/cells", map[string]any{"model_id": f.modelID, "metric_id": f.metric["pct_in"], "revision_id": f.revID,
			"dim_codes": map[string]string{f.region: c.region, f.period: c.period}, "value": c.v})
	}
	f.recalc()

	// The scheduler's slice row at a leaf region; none at a parent, none at '{}'.
	f.await("idx", map[string]string{f.region: "UK"}, 1060)
	f.await("idx", map[string]string{f.region: "US"}, 3060)
	for _, combo := range []map[string]string{{f.region: "EMEA"}, {}, {f.period: "2026-03"}} {
		raw, _ := json.Marshal(combo)
		var n int
		if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM runtime.calc_result
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid AND dim_members=$4::jsonb`,
			f.modelID, f.revID, f.metric["idx"], string(raw)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("idx has a row at %s, a total across members", raw)
		}
	}

	totals := func(scope map[string]string) map[string]float64 {
		t.Helper()
		raw, _ := json.Marshal(scope)
		path := "/api/grid?totals_only=1&grid_def_id=" + f.plan + "&scope=" + url.QueryEscape(string(raw))
		status, body := f.req("GET", path, f.dev, nil)
		if status != http.StatusOK {
			t.Fatalf("%s: %d %s", path, status, body)
		}
		var g gridRead
		if err := json.Unmarshal(body, &g); err != nil {
			t.Fatal(err)
		}
		return g.Totals
	}
	for _, c := range []struct {
		scope   map[string]string
		metric  string
		want    float64
		present bool
	}{
		{map[string]string{f.region: "UK", f.period: "H1"}, "idx", 1060, true},
		{map[string]string{f.region: "UK", f.period: "Q1"}, "idx", 1030, true},
		{map[string]string{f.region: "UK"}, "idx", 1060, true},
		{map[string]string{f.region: "EMEA", f.period: "H1"}, "idx", 0, false},
		{map[string]string{f.period: "H1"}, "idx", 0, false},
		{map[string]string{f.region: "UK", f.period: "H1"}, "pct_in", 7, true},
		{map[string]string{f.region: "DE", f.period: "Q1"}, "pct_in", 9, true},
		{map[string]string{f.region: "EMEA", f.period: "H1"}, "pct_in", 0, false},
	} {
		label := fmt.Sprintf("%s at %v", c.metric, c.scope)
		v, ok := totals(c.scope)[f.metric[c.metric]]
		switch {
		case ok != c.present:
			t.Errorf("%s: total present=%v (%v), want present=%v", label, ok, v, c.present)
		case ok && !nearly(v, c.want):
			t.Errorf("%s: total = %v, want %v", label, v, c.want)
		}
	}
}
