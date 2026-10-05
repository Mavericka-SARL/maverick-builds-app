package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A metric changes between input and calculated in place, keeping its id and
// the formulas naming it: to input, its formula and results go and its
// dependents read the typed value; back to calculated, a metric holding
// values is refused unless they may be dropped.
func TestMetricSwitchesBetweenInputAndCalculated(t *testing.T) {
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
	parts := must("POST", "/api/developer/metrics", map[string]any{"name": "kind_parts", "is_input": true, "revision_id": rev})
	total := must("POST", "/api/developer/metrics", map[string]any{"name": "kind_total", "formula": "kind_parts * 2", "agg_rule": "formula", "revision_id": rev})
	expense := must("POST", "/api/developer/metrics", map[string]any{"name": "kind_expense", "formula": "kind_total + 1", "revision_id": rev})
	grid := must("POST", "/api/developer/grids", map[string]any{"name": "Kinds", "revision_id": rev})
	for _, m := range []string{parts, total, expense} {
		must("POST", "/api/developer/grids/"+grid+"/metrics/"+m, nil)
	}
	write := func(metricID string, v float64) {
		t.Helper()
		if status, raw := call("POST", "/api/cells", map[string]any{"model_id": f.modelID, "revision_id": rev, "metric_id": metricID, "value": v}); status != http.StatusOK {
			t.Fatalf("write: %d %s", status, raw)
		}
	}
	read := func(metricID string) float64 {
		t.Helper()
		_, raw := call("GET", "/api/grid?grid_def_id="+grid+"&model_id="+f.modelID+"&revision_id="+rev, nil)
		var g struct {
			Totals map[string]float64 `json:"totals"`
		}
		_ = json.Unmarshal([]byte(raw), &g)
		return g.Totals[metricID]
	}
	write(parts, 10)
	if v := read(expense); v != 21 {
		t.Fatalf("before: kind_expense = %v, want 21", v)
	}

	// The total, typed in the workbook: an input now, same id.
	if status, raw := call("PATCH", "/api/developer/metrics/"+total, map[string]any{"is_input": true}); status != http.StatusOK {
		t.Fatalf("to input: %d %s", status, raw)
	}
	write(total, 7)
	if v := read(expense); v != 8 {
		t.Errorf("after typing the now-input total: kind_expense = %v, want 8", v)
	}

	// Back to calculated: its typed value is in the way until dropped.
	status, raw := call("PATCH", "/api/developer/metrics/"+total, map[string]any{"is_input": false, "formula": "kind_parts * 3"})
	if status != http.StatusConflict || !strings.Contains(raw, "drop_values") {
		t.Errorf("to calculated while holding a value: %d %s, want 409 naming drop_values", status, raw)
	}
	if status, raw := call("PATCH", "/api/developer/metrics/"+total, map[string]any{"is_input": false}); status != http.StatusBadRequest {
		t.Errorf("to calculated without a formula: %d %s, want 400", status, raw)
	}
	if status, raw := call("PATCH", "/api/developer/metrics/"+total, map[string]any{"is_input": false, "formula": "kind_parts * 3", "drop_values": true}); status != http.StatusOK {
		t.Fatalf("to calculated, dropping its value: %d %s", status, raw)
	}
	if v := read(expense); v != 31 {
		t.Errorf("after it is calculated again: kind_expense = %v, want 31", v)
	}
}
