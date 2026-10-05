package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A date metric holds a date: typed as yyyy-mm-dd, stored as DATE()'s serial
// (formulas read the number), never added up by default, and refused a Sum.
func TestDateMetrics(t *testing.T) {
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
	if status, raw := call("POST", "/api/developer/metrics", map[string]any{"name": "bad_date", "is_input": true, "format": "date", "agg_rule": "sum", "revision_id": rev}); status != http.StatusBadRequest || !strings.Contains(raw, "never added up") {
		t.Errorf("a summed date: %d %s, want 400", status, raw)
	}
	hired := must("POST", "/api/developer/metrics", map[string]any{"name": "hire_date", "is_input": true, "format": "date", "revision_id": rev})
	tenure := must("POST", "/api/developer/metrics", map[string]any{"name": "tenure_days", "formula": "DAYS(DATE(2027, 1, 1), hire_date)", "revision_id": rev})
	grid := must("POST", "/api/developer/grids", map[string]any{"name": "Dates", "revision_id": rev})
	must("POST", "/api/developer/grids/"+grid+"/metrics/"+hired, nil)
	must("POST", "/api/developer/grids/"+grid+"/metrics/"+tenure, nil)
	if status, raw := call("POST", "/api/cells", map[string]any{"model_id": f.modelID, "revision_id": rev, "metric_id": hired, "text": "2026-12-01"}); status != http.StatusOK {
		t.Fatalf("write a date: %d %s", status, raw)
	}
	if status, raw := call("POST", "/api/cells", map[string]any{"model_id": f.modelID, "revision_id": rev, "metric_id": hired, "text": "1st of May"}); status != http.StatusBadRequest {
		t.Errorf("not a date: %d %s, want 400", status, raw)
	}
	_, raw := call("GET", "/api/grid?grid_def_id="+grid+"&model_id="+f.modelID+"&revision_id="+rev, nil)
	var g struct {
		Totals  map[string]float64 `json:"totals"`
		Metrics []struct {
			ID, AggRule string
		} `json:"metrics"`
	}
	_ = json.Unmarshal([]byte(raw), &g)
	if v := g.Totals[hired]; v != 46357 { // 2026-12-01
		t.Errorf("hire_date = %v, want the serial 46357", v)
	}
	if v := g.Totals[tenure]; v != 31 {
		t.Errorf("tenure_days = %v, want 31", v)
	}
	if !strings.Contains(raw, `"agg_rule":"none"`) {
		t.Errorf("a date metric's default rule is none:\n%.400s", raw)
	}
}
