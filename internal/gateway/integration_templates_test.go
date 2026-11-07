package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// An integration saved as a template is made again with its target found by
// name (migration 158): a loader could not be reused in another model.
func TestIntegrationTemplates(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()
	loader := idOf(d.call("POST", "/api/developer/integrations", d.dev, map[string]any{
		"name": "Load plan", "type": "csv_import", "target_type": "grid", "target_id": d.gridID,
	}))
	d.call("PATCH", "/api/developer/integrations/"+loader+"/config", d.dev, map[string]any{
		"config": map[string]any{"column_map": map[string]string{"Revenue": "revenue", "Geo": "geo"}, "target_id": d.gridID},
	})
	if status, raw := d.req("POST", "/api/developer/integration-templates", d.westRep, map[string]any{"integration_id": loader}); status != http.StatusForbidden {
		t.Errorf("a business user saving a template: %d %s, want 403", status, raw)
	}
	tpl := idOf(d.call("POST", "/api/developer/integration-templates", d.dev, map[string]any{"integration_id": loader, "name": "Plan loader"}))
	if status, _ := d.req("POST", "/api/developer/integration-templates", d.dev, map[string]any{"integration_id": loader, "name": "Plan loader"}); status != http.StatusConflict {
		t.Errorf("a second template of the same name: %d, want 409", status)
	}
	var list []integrationTemplate
	_, raw := d.req("GET", "/api/developer/integration-templates", d.dev, nil)
	if err := json.Unmarshal(raw, &list); err != nil || len(list) != 1 || list[0].TargetName != "Sales Plan" || string(list[0].Config) == "" {
		t.Fatalf("templates: %s", raw)
	}
	var cfg map[string]any
	_ = json.Unmarshal(list[0].Config, &cfg)
	if _, ok := cfg["target_id"]; ok {
		t.Error("a template keeps its model's target id")
	}

	made := idOf(d.call("POST", "/api/developer/integration-templates/"+tpl+"/use", d.dev, map[string]any{"name": "Load plan (2)"}))
	var target, column string
	if err := d.pool.QueryRow(ctx, `SELECT target_id::text, config->'column_map'->>'Revenue' FROM model.integration_def WHERE id = $1::uuid`, made).Scan(&target, &column); err != nil {
		t.Fatal(err)
	}
	if target != d.gridID || column != "revenue" {
		t.Errorf("made from the template: target %s, column_map Revenue → %s", target, column)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE core.integration_template SET target_name = 'No such grid' WHERE id = $1::uuid`, tpl); err != nil {
		t.Fatal(err)
	}
	if status, raw := d.req("POST", "/api/developer/integration-templates/"+tpl+"/use", d.dev, map[string]any{"name": "Load plan (3)"}); status != http.StatusBadRequest {
		t.Errorf("a template whose grid this model lacks: %d %s, want 400", status, raw)
	}
	d.call("DELETE", "/api/developer/integration-templates/"+tpl, d.dev, nil)
	if status, _ := d.req("DELETE", "/api/developer/integration-templates/"+tpl, d.dev, nil); status != http.StatusNotFound {
		t.Errorf("deleting it again: %d, want 404", status)
	}
}
