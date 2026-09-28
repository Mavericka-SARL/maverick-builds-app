package gateway

import (
	"fmt"
	"testing"
)

// TestPropertyMetricBlankLeavesAndParents: a metric reading a member
// property has no value at a member without one (FR here), and its value
// at a parent combines its leaves.
//
//   - The scoped grid read (a restricted viewer) leaves FR out and keeps it
//     out of the average total; it used to show FR = 0 and average it in.
//   - chart-data plots nothing at FR, and at EMEA combines the leaves by
//     agg_rule — it used to evaluate the formula AT EMEA, reading EMEA's own
//     blank factor, and plot 0 for both metrics.
func TestPropertyMetricBlankLeavesAndParents(t *testing.T) {
	f := setupRestrictedFixture(t)
	f.members["FR"] = f.dev_("POST", "/api/developer/dimensions/"+f.region+"/members",
		map[string]any{"code": "FR", "label": "FR", "parent_member_id": f.members["EMEA"]})
	f.addMetric("wavg", "region.factor", "average")
	f.addMetric("wsum", "revenue * region.factor", "sum")
	f.recalc()
	f.await("wsum", map[string]string{f.region: "DE", f.period: "2026-02"}, 606)
	f.await("wavg", map[string]string{f.region: "DE", f.period: "2026-02"}, 3)

	// Per period the average of UK 2 and DE 3 (FR has no factor), summed
	// over the six months by time_summary sum.
	const wantTotal = 2.5 * 6
	f.hide("US")
	for _, scope := range []string{"", fmt.Sprintf(`{%q:"EMEA"}`, f.region)} {
		g := f.grid(f.viewer, f.plan, scope)
		label := "US hidden, scope " + scope
		if v, ok := g.Cells[f.at("wavg", "FR", "2026-02")]; ok {
			t.Errorf("%s: wavg at FR = %v, want no cell (blank)", label, v)
		}
		if v, ok := g.Cells[f.at("wsum", "FR", "2026-02")]; ok && v != 0 {
			t.Errorf("%s: wsum at FR = %v", label, v)
		}
		f.served(g, label, f.at("wavg", "DE", "2026-02"), 3)
		if v, ok := g.Totals[f.metric["wavg"]]; !ok || !nearly(v, wantTotal) {
			t.Errorf("%s: wavg total = %v (present=%v), want %v (FR left out)", label, v, ok, wantTotal)
		}
	}

	f.hide()
	dash := f.dev_("POST", "/api/developer/dashboards", map[string]any{"name": "P", "revision_id": f.revID})
	w := f.chartWidget(dash, f.region, []string{"wavg", "wsum"}, map[string]string{f.period: "2026-02"})
	v := func(x float64) *float64 { return &x }
	c := f.chartPoints(f.dev, w)
	f.point("chart", c, "wavg", "FR", nil)
	f.point("chart", c, "wavg", "UK", v(2))
	f.point("chart", c, "wavg", "EMEA", v(2.5))
	f.point("chart", c, "wsum", "EMEA", v(102*2+202*3))
	f.point("chart", c, "wsum", "US", v(302*4))
}
