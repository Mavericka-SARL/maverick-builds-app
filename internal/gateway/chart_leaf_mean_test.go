package gateway

import "testing"

// TestChartPropertyAverageTwoLevelsIsLeafMean: a metric reading a member
// property (evaluated per leaf) with agg_rule average plots, at a member
// two levels up, the mean of the LEAVES under it — the scheduler's rule
// for its total and slice rows — never the mean of its children's means.
// World > {EMEA > {UK 2, DE 3}, US 4}: World is 3, not mean(2.5, 4) = 3.25
// (the live chart showed 3.25 while the grid showed 3).
func TestChartPropertyAverageTwoLevelsIsLeafMean(t *testing.T) {
	f := setupRestrictedFixture(t)
	dims := "/api/developer/dimensions/" + f.region + "/members"
	f.members["World"] = f.dev_("POST", dims, map[string]any{"code": "World", "label": "World"})
	f.dev_("PATCH", dims+"/"+f.members["EMEA"], map[string]any{"code": "EMEA", "label": "EMEA",
		"parent_member_id": f.members["World"]})
	f.dev_("PATCH", dims+"/"+f.members["US"], map[string]any{"code": "US", "label": "US",
		"parent_member_id": f.members["World"],
		"properties":       map[string]string{"segment": "ENT", "currency": "USD", "factor": "4"}})
	f.addMetric("wavg", "region.factor", "average")
	f.recalc()
	f.await("wavg", map[string]string{f.region: "US", f.period: "2026-02"}, 4)

	// The grid's total: per period the mean of UK, DE and US (3), summed
	// over the six months by time_summary sum.
	g := f.grid(f.dev, f.plan, "")
	if v, ok := g.Totals[f.metric["wavg"]]; !ok || !nearly(v, 18) {
		t.Errorf("grid wavg total = %v (present=%v), want 18", v, ok)
	}

	dash := f.dev_("POST", "/api/developer/dashboards", map[string]any{"name": "L", "revision_id": f.revID})
	v := func(x float64) *float64 { return &x }
	one := f.chartPoints(f.dev, f.chartWidget(dash, f.region, []string{"wavg"}, map[string]string{f.period: "2026-02"}))
	f.point("one period", one, "wavg", "World", v(3))
	f.point("one period", one, "wavg", "EMEA", v(2.5))
	f.point("one period", one, "wavg", "US", v(4))
	f.point("one period", one, "wavg", "DE", v(3))

	// No period pinned: per period the leaf mean, then time_summary sum —
	// equal to the grid's total at World.
	all := f.chartPoints(f.dev, f.chartWidget(dash, f.region, []string{"wavg"}, map[string]string{}))
	f.point("all periods", all, "wavg", "World", v(18))
	f.point("all periods", all, "wavg", "EMEA", v(15))
}
