package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// TestParentRowsCombineLeavesFlat: every server reader answers a parent two
// levels above the data with its LEAVES combined once — an average is the
// mean of the leaves with a value, a count the number of leaves with a
// (non-zero) value, a sum unchanged — so the World row (World is the only
// root) equals the grand total, and a chart's World bar equals both.
//
// World > {EMEA > {UK, DE, FR}, US}; iavg (average) and icnt (count) record
// UK 2, DE 3, US 4 in February and UK 6 in March, FR nothing. Level by level
// World's February average would be mean(EMEA 2.5, US 4) = 3.25 and its
// count the 2 children; flat they are 3 and 3.
func TestParentRowsCombineLeavesFlat(t *testing.T) {
	f := setupRestrictedFixture(t)
	dims := "/api/developer/dimensions/" + f.region + "/members"
	f.members["World"] = f.dev_("POST", dims, map[string]any{"code": "World", "label": "World"})
	f.dev_("PATCH", dims+"/"+f.members["EMEA"], map[string]any{"code": "EMEA", "label": "EMEA",
		"parent_member_id": f.members["World"]})
	f.dev_("PATCH", dims+"/"+f.members["US"], map[string]any{"code": "US", "label": "US",
		"parent_member_id": f.members["World"],
		"properties":       map[string]string{"segment": "ENT", "currency": "USD", "factor": "4"}})
	f.members["FR"] = f.dev_("POST", dims, map[string]any{"code": "FR", "label": "FR", "parent_member_id": f.members["EMEA"]})

	for name, rule := range map[string]string{"iavg": "average", "icnt": "count"} {
		f.metric[name] = f.dev_("POST", "/api/developer/metrics", map[string]any{"name": name, "is_input": true, "formula": "",
			"revision_id": f.revID, "agg_rule": rule, "format": "number", "time_summary": "sum"})
		f.dev_("POST", "/api/developer/grids/"+f.plan+"/metrics/"+f.metric[name], nil)
	}
	f.addMetric("csum", "iavg * 2", "sum")
	f.addMetric("ccnt", "iavg * 2", "count")
	// Dimension-conditional, so evaluated per leaf and averaged over the
	// leaf rows (FR's row is 0: a missing input reads as 0 in a formula).
	f.addMetric("cavg", `IF(region = "", 0, iavg * 2)`, "average")
	for _, c := range []struct {
		region, period string
		v              float64
	}{{"UK", "2026-02", 2}, {"DE", "2026-02", 3}, {"US", "2026-02", 4}, {"UK", "2026-03", 6}} {
		for _, name := range []string{"iavg", "icnt"} {
			f.dev_("POST", "/api/cells", map[string]any{"model_id": f.modelID, "metric_id": f.metric[name], "revision_id": f.revID,
				"dim_codes": map[string]string{f.region: c.region, f.period: c.period}, "value": c.v})
		}
	}
	f.await("csum", map[string]string{f.region: "UK", f.period: "2026-03"}, 12)
	f.await("ccnt", map[string]string{f.region: "UK", f.period: "2026-03"}, 12)
	f.await("cavg", map[string]string{f.region: "UK", f.period: "2026-03"}, 12)
	f.await("cavg", map[string]string{}, 7.5)

	metrics := []string{"revenue", "iavg", "icnt", "csum", "ccnt", "cavg"}
	// The grand totals: per month the leaves combined, the months summed.
	want := map[string]float64{
		"iavg": 3 + 6,           // Feb mean(2,3,4), Mar 6
		"icnt": 3 + 1,           // leaves with a value
		"csum": 18 + 12,         // (4+6+8) + 12
		"ccnt": 3 + 1,           // non-zero leaf rows
		"cavg": 18.0/4 + 12.0/4, // leaf rows UK, DE, FR (0), US
	}
	wantEMEA := map[string]float64{
		"iavg": 2.5 + 6,
		"icnt": 2 + 1,
		"csum": 10 + 12,
		"ccnt": 2 + 1,
		"cavg": 10.0/3 + 12.0/3,
	}
	scope := func(code string) string { return fmt.Sprintf(`{%q:%q}`, f.region, code) }
	totalsOnly := func(scope string) map[string]float64 {
		t.Helper()
		path := "/api/grid?totals_only=1"
		if scope != "" {
			path += "&scope=" + url.QueryEscape(scope)
		}
		status, raw := f.req("GET", path, f.dev, nil)
		if status != http.StatusOK {
			t.Fatalf("%s: %d %s", path, status, raw)
		}
		var g gridRead
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatal(err)
		}
		return g.Totals
	}

	grand := f.grid(f.dev, f.plan, "").Totals
	reads := map[string]map[string]float64{
		"unscoped totals_only":    totalsOnly(""),
		"scope World":             f.grid(f.dev, f.plan, scope("World")).Totals,
		"scope World totals_only": totalsOnly(scope("World")),
	}
	for name, w := range want {
		if v, ok := grand[f.metric[name]]; !ok || !nearly(v, w) {
			t.Errorf("grand total %s = %v (present=%v), want %v", name, v, ok, w)
		}
	}
	for label, totals := range reads {
		for _, name := range metrics {
			g, ok := totals[f.metric[name]]
			if !ok || !nearly(g, grand[f.metric[name]]) {
				t.Errorf("%s: %s = %v (present=%v), want the grand total %v", label, name, g, ok, grand[f.metric[name]])
			}
		}
	}
	for label, totals := range map[string]map[string]float64{
		"scope EMEA":             f.grid(f.dev, f.plan, scope("EMEA")).Totals,
		"scope EMEA totals_only": totalsOnly(scope("EMEA")),
	} {
		for name, w := range wantEMEA {
			if v, ok := totals[f.metric[name]]; !ok || !nearly(v, w) {
				t.Errorf("%s: %s = %v (present=%v), want %v", label, name, v, ok, w)
			}
		}
	}

	// Chart-data by region: World equals the grand total; EMEA, a leaf
	// over time (each month's one leaf counted) and FR with no value.
	dash := f.dev_("POST", "/api/developer/dashboards", map[string]any{"name": "F", "revision_id": f.revID})
	v := func(x float64) *float64 { return &x }
	// A bar chart takes at most five metrics: two widgets, points merged.
	chart := func(ctx map[string]string) map[string]map[string]*float64 {
		out := f.chartPoints(f.dev, f.chartWidget(dash, f.region, metrics[:3], ctx))
		for k, pts := range f.chartPoints(f.dev, f.chartWidget(dash, f.region, metrics[3:], ctx)) {
			out[k] = pts
		}
		return out
	}
	all := chart(map[string]string{})
	for _, name := range metrics {
		f.point("all periods", all, name, "World", v(grand[f.metric[name]]))
	}
	for name, w := range wantEMEA {
		f.point("all periods", all, name, "EMEA", v(w))
	}
	for _, p := range []struct {
		metric, member string
		want           *float64
	}{
		{"iavg", "UK", v(8)}, {"iavg", "US", v(4)}, {"iavg", "FR", nil},
		{"icnt", "UK", v(2)}, {"icnt", "US", v(1)}, {"icnt", "FR", nil},
		{"csum", "UK", v(16)}, {"csum", "FR", v(0)},
		{"ccnt", "UK", v(2)}, {"ccnt", "FR", v(0)},
		{"cavg", "UK", v(16)}, {"cavg", "FR", v(0)},
	} {
		f.point("all periods", all, p.metric, p.member, p.want)
	}

	feb := chart(map[string]string{f.period: "2026-02"})
	for _, p := range []struct {
		metric, member string
		want           float64
	}{
		{"iavg", "World", 3}, {"iavg", "EMEA", 2.5}, {"iavg", "UK", 2},
		{"icnt", "World", 3}, {"icnt", "EMEA", 2}, {"icnt", "UK", 2}, // a leaf cell is its value
		{"csum", "World", 18}, {"ccnt", "World", 3}, {"cavg", "World", 4.5}, {"cavg", "EMEA", 10.0 / 3},
	} {
		f.point("February", feb, p.metric, p.member, v(p.want))
	}
}
