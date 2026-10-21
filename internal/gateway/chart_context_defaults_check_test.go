package gateway

import (
	"net/http"
	"strings"
	"testing"
)

// TestChartContextDefaultsAreCheckedAtSave: a chart's context_defaults that
// chart-data would refuse on every load — a dimension its grid lacks, the
// plotted dimension, a code that is no member — are refused when the chart
// is saved, on add and on change. Live, such a chart saved and answered 400
// "context dimension … not in grid" every time the dashboard opened.
func TestChartContextDefaultsAreCheckedAtSave(t *testing.T) {
	f := setupRestrictedFixture(t)
	dash := f.dev_("POST", "/api/developer/dashboards", map[string]any{"name": "C", "revision_id": f.revID})
	body := func(ctx map[string]string) map[string]any {
		return map[string]any{"widget_type": "chart", "ref_id": f.plan, "size_w": 400, "size_h": 300,
			"widget_props": map[string]any{"chart": map[string]any{"chart_type": "bar", "dimension_id": f.region,
				"metric_ids": []string{f.metric["revenue"]}, "context_defaults": ctx}}}
	}
	for _, c := range []struct {
		label string
		ctx   map[string]string
		want  string
	}{
		{"a dimension the grid lacks", map[string]string{f.currency: "EUR"}, "does not have"},
		{"the plotted dimension", map[string]string{f.region: "UK"}, "plots along"},
		{"a code that is no member", map[string]string{f.period: "2027-01"}, "not a member code"},
	} {
		status, raw := f.req("POST", "/api/developer/dashboards/"+dash+"/widgets", f.dev, body(c.ctx))
		if status != http.StatusBadRequest || !strings.Contains(string(raw), c.want) {
			t.Errorf("add with %s: %d %s, want 400 naming %q", c.label, status, raw, c.want)
		}
	}
	w := f.chartWidget(dash, f.region, []string{"revenue"}, map[string]string{f.period: "2026-02"})
	status, raw := f.req("PATCH", "/api/developer/dashboards/"+dash+"/widgets/"+w, f.dev, map[string]any{
		"widget_props": body(map[string]string{f.currency: "EUR"})["widget_props"]})
	if status != http.StatusBadRequest || !strings.Contains(string(raw), "does not have") {
		t.Errorf("change to a dimension the grid lacks: %d %s, want 400", status, raw)
	}
	if pts := f.chartPoints(f.dev, w); len(pts["revenue"]) == 0 {
		t.Errorf("the valid chart draws nothing: %v", pts)
	}
}
