package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
)

// A grid shows its metrics in an order the developer sets — a P&L reads in
// statement order. Metrics added in the console all had sort_order 0 and
// showed alphabetically, and nothing could reorder them.
func TestGridMetricOrderIsTheDevelopersToSet(t *testing.T) {
	f := newSalesFileFixture(t)
	_, do := f.serve(t, nil)
	id := func(name string) string {
		t.Helper()
		var v string
		if err := f.pool.QueryRow(f.ctx, `SELECT id::text FROM model.metric_def WHERE revision_id=$1::uuid AND name=$2`, f.revID, name).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	revenue, cost, margin := id("revenue"), id("cost"), id("margin")
	order := func() []string {
		t.Helper()
		status, body := do(f.devSub, "GET", "/api/grid?grid_def_id="+f.grid+"&revision_id="+f.revID, nil)
		if status != http.StatusOK {
			t.Fatalf("grid: %d %s", status, body)
		}
		var g struct {
			Metrics []struct{ Name string } `json:"metrics"`
		}
		_ = json.Unmarshal(body, &g)
		var names []string
		for _, m := range g.Metrics {
			names = append(names, m.Name)
		}
		return names
	}
	same := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	path := "/api/developer/grids/" + f.grid + "/metrics/order"
	if status, body := do(f.devSub, "PUT", path, map[string]any{"metric_ids": []string{margin, revenue, cost}}); status != http.StatusOK {
		t.Fatalf("reorder: %d %s", status, body)
	}
	if got := order(); !same(got, "margin", "revenue", "cost") {
		t.Errorf("after reorder the grid shows %v, want margin, revenue, cost", got)
	}
	for what, ids := range map[string][]string{
		"one missing":   {margin, revenue},
		"listed twice":  {margin, revenue, cost, cost},
		"not on a grid": {margin, revenue, cost, f.grid},
	} {
		if status, _ := do(f.devSub, "PUT", path, map[string]any{"metric_ids": ids}); status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", what, status)
		}
	}

	// A metric added later goes last, not first by name.
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid,$2::uuid,'aaa_units',true,'sum')`, f.modelID, f.revID); err != nil {
		t.Fatal(err)
	}
	if status, body := do(f.devSub, "POST", "/api/developer/grids/"+f.grid+"/metrics/"+id("aaa_units"), nil); status != http.StatusOK {
		t.Fatalf("add: %d %s", status, body)
	}
	if got := order(); !same(got, "margin", "revenue", "cost", "aaa_units") {
		t.Errorf("after adding, the grid shows %v, want aaa_units appended", got)
	}
}
