package gateway

import (
	"encoding/json"
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

// TestChartFirstRequestTakesThePageSelectors: the dashboard's selector values
// sent as "shared" apply on the chart's own non-plotted dimensions; a plotted
// or foreign one is ignored, and the chart's context wins. The first request
// of a chart used to go out without them and drew every member added
// together until the second (2026-11-08).
func TestChartFirstRequestTakesThePageSelectors(t *testing.T) {
	f := setupRestrictedFixture(t)
	dash := f.dev_("POST", "/api/developer/dashboards", map[string]any{"name": "S", "revision_id": f.revID})
	w := f.chartWidget(dash, f.region, []string{"revenue"}, nil)
	// The series drawn (as_of, the answer's time, differs between calls).
	data := func(body map[string]any) string {
		status, raw := f.req("POST", "/api/dashboard-widgets/"+w+"/chart-data", f.dev, body)
		if status != http.StatusOK {
			t.Fatalf("chart-data %v: %d %s", body, status, raw)
		}
		var answer struct {
			Series json.RawMessage `json:"series"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil {
			t.Fatalf("chart-data answer: %v", err)
		}
		return string(answer.Series)
	}
	pinned := data(map[string]any{"context": map[string]string{f.period: "2026-02"}})
	shared := data(map[string]any{"context": map[string]string{},
		"shared": map[string]string{f.period: "2026-02", f.region: "UK", f.currency: "EUR"}})
	if shared != pinned {
		t.Errorf("shared period 2026-02 drew %s, want what the context 2026-02 draws: %s", shared, pinned)
	}
	if total := data(map[string]any{"context": map[string]string{}}); total == pinned {
		t.Fatalf("the fixture's February equals its total: the test proves nothing")
	}
	if own := data(map[string]any{"context": map[string]string{f.period: "2026-02"},
		"shared": map[string]string{f.period: "2026-01"}}); own != pinned {
		t.Errorf("the chart's own context must win over the shared value: %s", own)
	}
}
