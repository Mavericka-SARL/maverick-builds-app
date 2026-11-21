package gateway

// A calculated pick-list saved without an aggregation totals by its formula
// only when everything the formula reads has a total; reading a metric with
// none, or the cell's own member, it gets none (2026-11-21: an inventory
// model's product totals read the action "Expedite supply" and XYZ "None").
// An aggregation the developer chooses is kept.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestCalculatedPicklistTotalDefault(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	rev := f.workingRevID
	must := func(method, path string, body any) string {
		t.Helper()
		status, raw := doAs(t, f.rollupFixture, method, path, dev, f.appID, body)
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("%s %s: %d %s", method, path, status, raw)
		}
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &out)
		return out.ID
	}
	statuses := must("POST", "/api/developer/dimensions", map[string]any{"name": "pick_statuses", "revision_id": rev})
	for _, c := range []string{"OK", "LOW"} {
		must("POST", "/api/developer/dimensions/"+statuses+"/members", map[string]any{"code": c, "label": c})
	}
	region := must("POST", "/api/developer/dimensions", map[string]any{"name": "pick_region", "revision_id": rev})
	must("POST", "/api/developer/dimensions/"+region+"/members", map[string]any{"code": "NA", "label": "NA"})
	metric := func(body map[string]any) string {
		body["revision_id"] = rev
		return must("POST", "/api/developer/metrics", body)
	}
	pick := func(name, formula string, extra map[string]any) string {
		body := map[string]any{"name": name, "format": "picklist", "picklist_dimension_id": statuses, "formula": formula}
		for k, v := range extra {
			body[k] = v
		}
		return metric(body)
	}
	metric(map[string]any{"name": "stock", "is_input": true, "format": "number"})
	metric(map[string]any{"name": "mean_stock", "formula": "stock", "agg_rule": "none"})
	cases := []struct {
		id, want, what string
	}{
		{pick("by_sum", `IF(stock > 10, "OK", "LOW")`, nil), "formula", "reads a metric that adds up"},
		{pick("by_mean", `IF(mean_stock > 10, "OK", "LOW")`, nil), "none", "reads a metric with no total"},
		{pick("by_member", `IF(pick_region = "NA", "OK", "LOW")`, nil), "none", "reads the cell's member"},
		{pick("by_parent", `IF(PARENT(pick_region) = "", "LOW", "OK")`, nil), "none", "reads the member's parent"},
		{pick("by_choice", `IF(mean_stock > 10, "OK", "LOW")`, map[string]any{"agg_rule": "formula"}), "formula", "chosen"},
		{metric(map[string]any{"name": "flag_in", "is_input": true, "format": "picklist", "picklist_dimension_id": statuses}), "none", "an input"},
	}
	for _, c := range cases {
		var got string
		if err := f.pool.QueryRow(ctx, `SELECT agg_rule FROM model.metric_def WHERE id = $1::uuid`, c.id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("a pick-list that %s: agg_rule %q, want %q", c.what, got, c.want)
		}
	}
}
