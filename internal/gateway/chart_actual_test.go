package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestChartMarksActualPeriods: a chart naming the metric that is 1 in the
// periods holding actuals answers which of its categories those are, so the
// console draws them solid and the rest as forecast (IBCS); a flag the
// chart's grid does not hold is refused when the chart is saved.
func TestChartMarksActualPeriods(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()
	isActual := idOf(d.call("POST", "/api/developer/metrics", d.dev, map[string]any{
		"name": "is_actual", "is_input": false, "formula": `=IF(OR(period = "Q1", period = "Q2"), 1, 0)`, "agg_rule": "formula", "format": "number", "revision_id": d.revID}))
	d.call("POST", "/api/developer/grids/"+d.gridID+"/metrics/"+isActual, d.dev, nil)
	d.metric["is_actual"] = isActual
	offGrid := idOf(d.call("POST", "/api/developer/metrics", d.dev, map[string]any{
		"name": "flag_elsewhere", "is_input": true, "agg_rule": "sum", "format": "number", "revision_id": d.revID}))
	d.awaitCalc("is_actual", map[string]string{d.geoDim: "UK", d.prodDim: "LAPTOP", d.periodDim: "Q1"}, 1)

	dash := idOf(d.call("POST", "/api/developer/dashboards", d.dev, map[string]any{"name": "Board", "revision_id": d.revID}))
	chartProps := func(flag string) map[string]any {
		return map[string]any{"chart": map[string]any{"chart_type": "bar", "dimension_id": d.periodDim, "metric_ids": []string{d.metric["revenue"]},
			"context_defaults": map[string]string{d.geoDim: "UK", d.prodDim: "LAPTOP"}, "hide_rollup_members": true, "stacked": true, "actual_metric_id": flag}}
	}
	if status, body := d.req("POST", "/api/developer/dashboards/"+dash+"/widgets", d.dev, map[string]any{
		"widget_type": "chart", "ref_id": d.gridID, "widget_props": chartProps(offGrid)}); status < 400 || !strings.Contains(string(body), "flag_elsewhere") {
		t.Errorf("a flag off the chart's grid: %d %s, want it refused naming the flag", status, body)
	}
	chart := idOf(d.call("POST", "/api/developer/dashboards/"+dash+"/widgets", d.dev, map[string]any{
		"widget_type": "chart", "ref_id": d.gridID, "widget_props": chartProps(isActual)}))
	status, raw := d.req("POST", "/api/dashboard-widgets/"+chart+"/chart-data", d.dev, map[string]any{"context": map[string]string{}})
	if status != 200 {
		t.Fatalf("chart data: %d %s", status, raw)
	}
	var data struct {
		Categories []struct{ Key string } `json:"categories"`
		Actual     []bool                 `json:"actual"`
	}
	_ = json.Unmarshal(raw, &data)
	got := map[string]bool{}
	for i, c := range data.Categories {
		if i < len(data.Actual) {
			got[c.Key] = data.Actual[i]
		}
	}
	if len(data.Actual) != len(data.Categories) || !got["Q1"] || !got["Q2"] || got["Q3"] || got["Q4"] {
		t.Errorf("actual periods %v over %v, want Q1 and Q2", data.Actual, data.Categories)
	}
}
