package gateway

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// A developer clears a grid's typed values, or a metric's, across the
// revision or under chosen members: what they may not write stays, a dry
// run changes nothing, and calculated values follow.
func TestDeveloperClearsValues(t *testing.T) {
	f := setupDevAuthoringFixture(t)
	ctx := context.Background()
	post := func(path string, body any) (int, map[string]any) {
		t.Helper()
		return f.do(t, http.MethodPost, path, body)
	}
	mk := func(path string, body map[string]any) string {
		t.Helper()
		status, out := post(path, body)
		if status != http.StatusOK {
			t.Fatalf("POST %s: %d %v", path, status, out)
		}
		id, _ := out["id"].(string)
		return id
	}
	for _, m := range []struct{ code, parent string }{{"EU", ""}, {"DE", "EU"}, {"FR", "EU"}, {"US", ""}} {
		body := map[string]any{"code": m.code, "label": m.code}
		if m.parent != "" {
			var pid string
			_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, f.dimID, m.parent).Scan(&pid)
			body["parent_member_id"] = pid
		}
		mk(fmt.Sprintf("/api/developer/dimensions/%s/members", f.dimID), body)
	}
	cost := mk("/api/developer/metrics", map[string]any{"name": "Cost", "is_input": true, "format": "number", "revision_id": f.revID})
	double := mk("/api/developer/metrics", map[string]any{"name": "DoubleRevenue", "is_input": false, "formula": "FixtureRevenue * 2", "format": "number", "revision_id": f.revID})
	grid := mk("/api/developer/grids", map[string]any{"name": "Plan", "revision_id": f.revID})
	for _, p := range []string{"/dimensions/" + f.dimID, "/metrics/" + f.metricID, "/metrics/" + cost, "/metrics/" + double} {
		if status, out := post("/api/developer/grids/"+grid+p, nil); status != http.StatusOK {
			t.Fatalf("assign %s: %d %v", p, status, out)
		}
	}
	if status, out := post("/api/import/upload", map[string]string{
		"csv": "FixtureRegion,FixtureRevenue,Cost\nDE,10,1\nFR,20,2\nUS,40,4\n", "revision_id": f.revID, "import_mode": "replace",
	}); status != http.StatusOK {
		t.Fatalf("import: %d %v", status, out)
	}
	value := func(metric, code string) (float64, bool) {
		var v float64
		err := f.pool.QueryRow(ctx, `SELECT value::float8 FROM runtime.fact_input WHERE metric_id=$1::uuid AND dim_members->>$2 = $3 ORDER BY entered_at DESC LIMIT 1`, metric, f.dimID, code).Scan(&v)
		return v, err == nil
	}

	// A dry run counts and clears nothing.
	status, out := post("/api/developer/grids/"+grid+"/clear-values", map[string]any{
		"members": map[string][]string{f.dimID: {"EU"}}, "dry_run": true,
	})
	if status != http.StatusOK || out["cells"] != 4.0 || out["dry_run"] != true {
		t.Fatalf("dry run under EU: %d %v, want 4 cells (two metrics × DE, FR)", status, out)
	}
	if _, ok := value(f.metricID, "DE"); !ok {
		t.Fatal("a dry run cleared a value")
	}

	// Under EU: DE and FR go (EU's children), US stays.
	status, out = post("/api/developer/grids/"+grid+"/clear-values", map[string]any{"members": map[string][]string{f.dimID: {"EU"}}})
	if status != http.StatusOK || out["cells"] != 4.0 {
		t.Fatalf("clear under EU: %d %v", status, out)
	}
	for _, c := range []string{"DE", "FR"} {
		if _, ok := value(f.metricID, c); ok {
			t.Errorf("%s still holds a revenue value", c)
		}
	}
	if v, ok := value(f.metricID, "US"); !ok || v != 40 {
		t.Errorf("US revenue = %v (%v), want 40 kept", v, ok)
	}
	var reason string
	_ = f.pool.QueryRow(ctx, `SELECT delete_reason FROM runtime.fact_input_history WHERE metric_id=$1::uuid ORDER BY deleted_at DESC LIMIT 1`, f.metricID).Scan(&reason)
	if reason != "cleared" {
		t.Errorf("the cleared value's history says %q, want cleared", reason)
	}
	if row := f.latestAuditEvent(t, "values.cleared"); row.resourceID != grid {
		t.Errorf("values.cleared resource_id = %s, want the grid", row.resourceID)
	}

	// One metric: Cost at US goes, revenue at US stays.
	if status, out := post("/api/developer/metrics/"+cost+"/clear-values", nil); status != http.StatusOK || out["cells"] != 1.0 {
		t.Fatalf("clear Cost: %d %v", status, out)
	}
	if _, ok := value(cost, "US"); ok {
		t.Error("Cost at US was not cleared")
	}
	if _, ok := value(f.metricID, "US"); !ok {
		t.Error("clearing Cost cleared revenue")
	}

	// A calculated metric holds no typed values; an unknown member is named.
	if status, _ := post("/api/developer/metrics/"+double+"/clear-values", nil); status != http.StatusBadRequest {
		t.Errorf("clearing a calculated metric: %d, want 400", status)
	}
	if status, out := post("/api/developer/grids/"+grid+"/clear-values", map[string]any{"members": map[string][]string{f.dimID: {"XX"}}}); status != http.StatusBadRequest {
		t.Errorf("an unknown member: %d %v, want 400", status, out)
	}

	// The calculated metric follows the cleared input: US alone, 2 × 40.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var total float64
		_ = f.pool.QueryRow(ctx, `SELECT value::float8 FROM runtime.calc_result WHERE metric_id=$1::uuid AND dim_members='{}'::jsonb ORDER BY calc_at DESC LIMIT 1`, double).Scan(&total)
		if total == 80 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("DoubleRevenue's total = %v after the clear, want 80", total)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A member the developer may only read stays.
	var userID string
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM identity.user WHERE keycloak_sub='dev-audit-pa'`).Scan(&userID)
	var us string
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='US'`, f.dimID).Scan(&us)
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'read')`, userID, us); err != nil {
		t.Fatal(err)
	}
	status, out = post("/api/developer/grids/"+grid+"/clear-values", nil)
	if status != http.StatusOK || out["cells"] != 0.0 || out["withheld"] != 1.0 {
		t.Fatalf("clear with US read-only: %d %v, want nothing cleared and 1 withheld", status, out)
	}
	if _, ok := value(f.metricID, "US"); !ok {
		t.Error("a read-only member's value was cleared")
	}
}
