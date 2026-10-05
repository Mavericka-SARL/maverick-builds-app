package gateway

// A chart point is the metric as the grid shows it. Two ways it was not,
// both found charting a target-setting workbook: an input on a grid with no
// dimensions (a growth %, a company target) read as nothing, because the
// chart loaded only facts with members; and a dimensionless calculated
// metric (a company total) read at a plotted region evaluated its inputs at
// that region, where the scheduler computes it once for the company.

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
)

func TestChartReadsSettingsAndCompanyTotals(t *testing.T) {
	f := setupRoundTripFixture(t)
	dev := "rollup-test-approver"
	rev := f.workingRevID
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, dev, f.appID, body)
	}
	must := func(method, path string, body any) string {
		t.Helper()
		status, raw := call(method, path, body)
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("%s %s: %d %s", method, path, status, raw)
		}
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &out)
		return out.ID
	}
	region := must("POST", "/api/developer/dimensions", map[string]any{"name": "market", "revision_id": rev})
	for _, c := range []string{"NA", "EU"} {
		must("POST", "/api/developer/dimensions/"+region+"/members", map[string]any{"code": c, "label": c})
	}
	metric := func(body map[string]any) string { body["revision_id"] = rev; return must("POST", "/api/developer/metrics", body) }
	growth := metric(map[string]any{"name": "growth_pct", "is_input": true, "format": "percentage", "agg_rule": "none"})
	base := metric(map[string]any{"name": "base_sales", "is_input": true})
	company := metric(map[string]any{"name": "company_sales", "formula": "base_sales"})
	target := metric(map[string]any{"name": "market_target", "formula": "base_sales * (1 + growth_pct / 100)"})
	share := metric(map[string]any{"name": "market_share", "formula": "base_sales / company_sales * 100", "agg_rule": "formula"})
	setup := must("POST", "/api/developer/grids", map[string]any{"name": "Settings", "revision_id": rev})
	for _, m := range []string{growth, company} {
		must("POST", "/api/developer/grids/"+setup+"/metrics/"+m, nil)
	}
	markets := must("POST", "/api/developer/grids", map[string]any{"name": "Markets", "revision_id": rev})
	for _, p := range []string{"/dimensions/" + region, "/metrics/" + base, "/metrics/" + target, "/metrics/" + share} {
		must("POST", "/api/developer/grids/"+markets+p, nil)
	}
	write := func(metricID string, dims map[string]string, v float64) {
		if status, raw := call("POST", "/api/cells", map[string]any{"model_id": f.modelID, "revision_id": rev, "metric_id": metricID,
			"dim_codes": dims, "value": v}); status != http.StatusOK {
			t.Fatalf("write: %d %s", status, raw)
		}
	}
	write(growth, map[string]string{}, 10)
	write(base, map[string]string{region: "NA"}, 300)
	write(base, map[string]string{region: "EU"}, 100)

	dash := must("POST", "/api/developer/dashboards", map[string]any{"name": "Markets"})
	chart := must("POST", "/api/developer/dashboards/"+dash+"/widgets", map[string]any{"widget_type": "chart", "ref_id": markets,
		"pos_x": 0, "pos_y": 0, "size_w": 400, "size_h": 300,
		"widget_props": map[string]any{"chart": map[string]any{"chart_type": "bar", "dimension_id": region,
			"metric_ids": []string{target, share}, "context_defaults": map[string]string{}}}})
	status, raw := call("POST", "/api/dashboard-widgets/"+chart+"/chart-data", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("chart-data: %d %s", status, raw)
	}
	var cd struct {
		Categories []struct {
			Key string `json:"key"`
		} `json:"categories"`
		Series []struct {
			MetricID string     `json:"metric_id"`
			Values   []*float64 `json:"values"`
		} `json:"series"`
	}
	_ = json.Unmarshal([]byte(raw), &cd)
	point := func(metricID, code string) float64 {
		for _, s := range cd.Series {
			if s.MetricID != metricID {
				continue
			}
			for i, c := range cd.Categories {
				if c.Key == code && s.Values[i] != nil {
					return *s.Values[i]
				}
			}
		}
		return math.NaN()
	}
	if v := point(target, "NA"); math.Abs(v-330) > 1e-9 {
		t.Errorf("NA target = %v, want 330: the 10%% growth on the settings grid counts", v)
	}
	if v := point(share, "NA"); math.Abs(v-75) > 1e-9 {
		t.Errorf("NA share = %v, want 75: 300 of the company's 400", v)
	}
}
