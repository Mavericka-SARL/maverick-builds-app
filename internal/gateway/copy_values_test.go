package gateway

import (
	"context"
	"net/http"
	"testing"
)

// A business admin copies the values typed into one member to another — the
// plan into the budget — in one step, each checked as a typed value is
// (owner request, 2026-11-07: the budget was frozen by downloading the plan
// and uploading it again).
func TestBusinessAdminCopiesValues(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()
	path := "/api/business-admin/copy-values"
	body := map[string]any{"dimension_id": d.geoDim, "from": "UK", "to": "US"}

	if status, raw := d.req("POST", path, d.westRep, body); status != http.StatusForbidden {
		t.Errorf("business user: %d %s, want 403", status, raw)
	}
	if status, raw := d.req("POST", path, d.admin, map[string]any{"dimension_id": d.geoDim, "from": "UK", "to": "UK"}); status != http.StatusBadRequest {
		t.Errorf("the same member: %d %s, want 400", status, raw)
	}
	if status, raw := d.req("POST", path, d.admin, map[string]any{"dimension_id": d.geoDim, "from": "UK", "to": "AMER"}); status != http.StatusBadRequest {
		t.Errorf("a parent member: %d %s, want 400", status, raw)
	}

	dry := d.call("POST", path, d.admin, map[string]any{"dimension_id": d.geoDim, "from": "UK", "to": "US", "dry_run": true})
	if dry["cells"] != float64(4) || dry["dry_run"] != true {
		t.Fatalf("dry run: %v, want the UK's 4 values (units, revenue, cost, target)", dry)
	}
	latest := func(geo string) float64 {
		t.Helper()
		var v float64
		if err := d.pool.QueryRow(context.Background(), `
			SELECT value FROM runtime.fact_input
			WHERE revision_id = $1::uuid AND metric_id = $2::uuid AND dim_members->>$3 = $4 AND dim_members->>$5 = 'LAPTOP'
			ORDER BY entered_at DESC LIMIT 1`, d.revID, d.metric["revenue"], d.geoDim, geo, d.prodDim).Scan(&v); err != nil {
			return -1
		}
		return v
	}
	if got := latest("US"); got != -1 {
		t.Fatalf("a dry run wrote US laptops: %v", got)
	}
	res := d.call("POST", path, d.admin, body)
	if res["cells"] != float64(4) {
		t.Fatalf("copy: %v", res)
	}
	if got := latest("US"); got != 900_000 {
		t.Errorf("US laptop revenue after the copy = %v, want the UK's 900,000", got)
	}
	if got := latest("UK"); got != 900_000 {
		t.Errorf("the copy changed its source: UK = %v", got)
	}
}
