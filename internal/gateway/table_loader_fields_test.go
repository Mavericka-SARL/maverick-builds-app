package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A table loader maps columns to the table's fields: a field named Metric is
// loaded (it was turned into the grid import's long-file keyword and dropped
// while the run reported success — the AI build's KPI table, 2026-11-08), and
// a column mapped to no field is refused when the map is saved and when a
// run applies it, naming the fields.
func TestTableLoaderMapsColumnsToItsFields(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()
	form := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{"name": "kpi_import", "label": "Import KPIs", "fields": []map[string]any{
		{"name": "Metric", "label": "Metric (exact label)", "type": "text", "required": true},
		{"name": "Value", "label": "Value", "type": "number"},
	}, "edit_access": "everyone"}))
	intg := idOf(d.call("POST", "/api/developer/integrations?revision_id="+d.revID, d.dev, map[string]any{
		"name": "Load KPIs", "type": "csv_import", "target_type": "form", "target_id": form}))

	if status, raw := d.req("PATCH", "/api/developer/integrations/"+intg+"/config", d.dev, map[string]any{"config": map[string]any{
		"column_map": map[string]string{"KPI": "Metric", "Amount": "Amount"}}}); status != http.StatusBadRequest ||
		!strings.Contains(string(raw), `\"Amount\" to \"Amount\"`) || !strings.Contains(string(raw), "its fields are: Metric, Value") {
		t.Fatalf("a column mapped to no field: %d %s, want 400 naming it and the fields", status, raw)
	}
	d.call("PATCH", "/api/developer/integrations/"+intg+"/config", d.dev, map[string]any{"config": map[string]any{
		"column_map": map[string]string{"KPI": "Metric", "Amount": "Value"}}})
	if status, raw := d.req("POST", "/api/integrations/"+intg+"/run", d.dev, map[string]any{"csv": "KPI,Amount\nEnd followers,1200\nPaid members,85\n"}); status != http.StatusOK {
		t.Fatalf("run: %d %s", status, raw)
	}
	var withMetric int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid AND COALESCE(data->>'Metric','') <> ''`, form).Scan(&withMetric); err != nil || withMetric != 2 {
		t.Errorf("records with their Metric: %d (%v), want 2", withMetric, err)
	}
}
