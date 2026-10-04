package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A chart plots only its own grid's metrics. One reading a metric of another
// grid used to save, and every chart-data read then answered 403 — found
// only when the dashboard rendered. The save now says so.
func TestChartWidgetRefusesAnotherGridsMetric(t *testing.T) {
	f := newSalesFileFixture(t)
	_, do := f.serve(t, nil)
	id := func(sql string, args ...any) string {
		t.Helper()
		var v string
		if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return v
	}
	revenue := id(`SELECT id::text FROM model.metric_def WHERE revision_id=$1::uuid AND name='revenue'`, f.revID)
	geo := id(`SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name='geography'`, f.revID)
	other := id(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid,$2::uuid,'headcount',true,'sum') RETURNING id::text`, f.modelID, f.revID)
	otherGrid := id(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'People') RETURNING id::text`, f.modelID, f.revID)
	id(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid,$2::uuid,0) RETURNING grid_id::text`, otherGrid, other)

	status, body := do(f.devSub, "POST", "/api/developer/dashboards", map[string]any{"name": "Board", "revision_id": f.revID})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("create dashboard: %d %s", status, body)
	}
	var dash struct{ ID string }
	_ = json.Unmarshal(body, &dash)
	chart := func(metrics ...string) map[string]any {
		return map[string]any{"chart": map[string]any{"chart_type": "line", "dimension_id": geo, "metric_ids": metrics}}
	}
	add := func(metrics ...string) (int, string) {
		status, body := do(f.devSub, "POST", "/api/developer/dashboards/"+dash.ID+"/widgets", map[string]any{
			"widget_type": "chart", "ref_id": f.grid, "pos_x": 0, "pos_y": 0, "size_w": 400, "size_h": 300, "widget_props": chart(metrics...)})
		return status, string(body)
	}

	if status, body := add(revenue, other); status != http.StatusBadRequest || !strings.Contains(body, "headcount") {
		t.Errorf("a chart reading another grid's metric: %d %s, want 400 naming headcount", status, body)
	}
	status, raw := add(revenue)
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("a chart of its grid's metric: %d %s", status, raw)
	}
	var widget struct{ ID string }
	_ = json.Unmarshal([]byte(raw), &widget)
	if status, body := do(f.devSub, "PATCH", "/api/developer/dashboards/"+dash.ID+"/widgets/"+widget.ID,
		map[string]any{"widget_props": chart(revenue, other)}); status != http.StatusBadRequest {
		t.Errorf("an update adding another grid's metric: %d %s, want 400", status, body)
	}
}
