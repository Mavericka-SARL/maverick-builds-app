package gateway

import (
	"context"
	"net/http"
	"testing"
)

// Freezing a calculated metric's values into an input metric: what the
// margin is now, kept as a typed value a later formula compares with
// (2026-11-20: a weekly cash forecast's capture was typed by hand).
func TestFreezeValuesIntoAnInput(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()
	path := "/api/business-admin/freeze-values"
	at := map[string]string{d.geoDim: "UK", d.prodDim: "LAPTOP", d.periodDim: "Q1"}
	body := map[string]any{"pairs": []map[string]string{{"from": "margin", "to": "target"}}, "at": at}

	if status, raw := d.req("POST", path, d.westRep, body); status != http.StatusForbidden {
		t.Errorf("business user: %d %s, want 403", status, raw)
	}
	if status, raw := d.req("POST", path, d.admin, map[string]any{"pairs": []map[string]string{{"from": "revenue", "to": "margin"}}, "at": at}); status != http.StatusBadRequest {
		t.Errorf("into a calculated metric: %d %s, want 400", status, raw)
	}
	latest := func() float64 {
		t.Helper()
		var v float64
		if err := d.pool.QueryRow(context.Background(), `
			SELECT value FROM runtime.fact_input
			WHERE revision_id = $1::uuid AND metric_id = $2::uuid AND dim_members->>$3 = 'UK' AND dim_members->>$4 = 'LAPTOP'
			  AND dim_members->>$5 = 'Q1'
			ORDER BY entered_at DESC LIMIT 1`, d.revID, d.metric["target"], d.geoDim, d.prodDim, d.periodDim).Scan(&v); err != nil {
			return -1
		}
		return v
	}
	before := latest()
	dry := d.call("POST", path, d.admin, map[string]any{"pairs": body["pairs"], "at": at, "dry_run": true})
	vals, _ := dry["values"].([]any)
	if dry["cells"] != float64(1) || len(vals) != 1 || vals[0].(map[string]any)["value"] != float64(200_000) {
		t.Fatalf("dry run: %v, want the UK laptops' Q1 margin, 200,000", dry)
	}
	if got := latest(); got != before {
		t.Fatalf("a dry run wrote the target: %v (was %v)", got, before)
	}
	if res := d.call("POST", path, d.admin, body); res["cells"] != float64(1) {
		t.Fatalf("freeze: %v", res)
	}
	if got := latest(); got != 200_000 {
		t.Errorf("UK laptop Q1 target after the freeze = %v, want the margin, 200,000", got)
	}
	// A dimension the target has and the source shares is frozen member by
	// member: every period of UK laptops.
	whole := d.call("POST", path, d.admin, map[string]any{"pairs": body["pairs"],
		"at": map[string]string{d.geoDim: "UK", d.prodDim: "LAPTOP"}, "dry_run": true})
	if n, _ := whole["cells"].(float64); n < 1 {
		t.Errorf("freezing every period: %v", whole)
	}
}
