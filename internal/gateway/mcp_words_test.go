package gateway

import (
	"testing"
)

// query_grid serves words as words: a pick-list cell as the member it holds
// (it is stored as the member's key, a large number) and a text metric as
// its text where every dimension is pinned. Both were served as numbers —
// the key, and 0 for a text with state "ok" (2026-11-13).
func TestMCPConnectorServesPicklistsAndTexts(t *testing.T) {
	f := setupConnector(t)
	d := f.salesDemo
	mode := idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": "funding_mode", "revision_id": d.revID}))
	for _, m := range []map[string]any{{"code": "ENABLED", "label": "Enabled"}, {"code": "DISABLED", "label": "Disabled"}} {
		d.call("POST", "/api/developer/dimensions/"+mode+"/members", d.dev, m)
	}
	grid := idOf(d.call("POST", "/api/developer/grids", d.dev, map[string]any{"name": "Settings", "revision_id": d.revID}))
	choice := idOf(d.call("POST", "/api/developer/metrics", d.dev, map[string]any{
		"name": "sched_funding", "label": "Scheduled funding", "is_input": true, "revision_id": d.revID,
		"agg_rule": "none", "time_summary": "none", "format": "picklist", "picklist_dimension_id": mode}))
	check := idOf(d.call("POST", "/api/developer/metrics", d.dev, map[string]any{
		"name": "setup_check", "label": "Setup check", "is_input": false, "revision_id": d.revID,
		"agg_rule": "none", "time_summary": "none", "format": "text", "formula": `IF(sched_funding = "ENABLED", "OK", "OFF")`}))
	for _, m := range []string{choice, check} {
		d.call("POST", "/api/developer/grids/"+grid+"/metrics/"+m, d.dev, nil)
	}
	member := "ENABLED"
	d.call("POST", "/api/cells", d.dev, map[string]any{"model_id": d.modelID, "metric_id": choice, "dim_codes": map[string]string{}, "member": member})

	res := f.as(d.dev).must("query_grid", f.ctxArgs(map[string]any{"grid_id": grid}))
	got := map[string]map[string]any{}
	for _, r := range res["rows"].([]any) {
		row := r.(map[string]any)
		got[row["metric_id"].(string)] = row["value"].(map[string]any)
	}
	if v := got[choice]; v["text"] != "Enabled (ENABLED)" || v["value"] != nil || v["state"] != "ok" {
		t.Errorf("pick-list = %v, want text Enabled (ENABLED) and no number", v)
	}
	if v := got[check]; v["text"] != "OK" || v["value"] != nil || v["state"] != "ok" {
		t.Errorf("text metric = %v, want text OK and no number", v)
	}
}
