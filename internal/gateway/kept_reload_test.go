package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A file loaded again into a table whose records have a typed code updates
// the records holding its codes and adds the others: a CRM extract or an
// HRIS roster refreshed every month. Each such load was refused whole —
// "Account ID "ACC-0001" is taken" for every row — although a kept
// dimension's records are documented as added and updated by their code
// (Territory & Quota build, 2026-11-16).
func TestFileLoadedAgainUpdatesRecordsByTheirCode(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()
	dim := idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": "plant", "revision_id": d.revID}))
	grid := idOf(d.call("POST", "/api/developer/grids", d.dev, map[string]any{"name": "Plant master", "revision_id": d.revID}))
	d.call("POST", "/api/developer/grids/"+grid+"/dimensions/"+dim, d.dev, map[string]any{})
	metric := idOf(d.call("POST", "/api/developer/metrics", d.dev, map[string]any{
		"name": "nameplate", "label": "Nameplate", "is_input": true, "revision_id": d.revID, "format": "number", "agg_rule": "sum"}))
	d.call("POST", "/api/developer/grids/"+grid+"/metrics/"+metric, d.dev, map[string]any{})
	table := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{
		"name": "plants", "label": "Plants", "edit_access": "creator",
		"fields": []map[string]any{
			{"name": "site", "label": "Site code", "type": "text", "code": true, "core": true},
			{"name": "facility", "label": "Facility", "type": "text", "required": true},
			{"name": "nameplate", "label": "Nameplate", "type": "number"},
		},
		"keeps_dimension_id": dim, "member_label_field": "facility",
	}))
	d.call("POST", "/api/developer/form-integrations", d.dev, map[string]any{
		"form_id": table, "grid_id": grid, "name": "Plants to nameplate", "source_field": "nameplate", "target_metric_id": metric,
		"aggregation": "sum", "dimension_mappings": map[string]string{dim: "site"}, "live_posting": true})

	load := func(csv string, dry bool) map[string]any {
		t.Helper()
		return d.call("POST", "/api/forms/"+table+"/import", d.dev, map[string]any{"csv": csv, "dry_run": dry})
	}
	num := func(m map[string]any, k string) float64 { v, _ := m[k].(float64); return v }
	load("Site code,Facility,Nameplate\nP1,West,300\nP2,East,200\n", false)

	again := "Site code,Facility,Nameplate\nP1,West Plant,350\nP2,East,200\nP3,North,100\n"
	check := load(again, true)
	if num(check, "rows_added") != 1 || num(check, "rows_updated") != 1 || num(check, "rows_unchanged") != 1 {
		t.Fatalf("validate of the file loaded again: %v, want 1 added, 1 updated, 1 unchanged", check)
	}
	res := load(again, false)
	if num(res, "records_created") != 1 || num(res, "records_updated") != 1 || num(res, "records_unchanged") != 1 {
		t.Fatalf("the file loaded again: %v, want 1 created, 1 updated, 1 unchanged", res)
	}
	var n int
	var raw []byte
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, table).Scan(&n); err != nil || n != 3 {
		t.Fatalf("records after the second load: %d (%v), want 3", n, err)
	}
	if err := d.pool.QueryRow(ctx, `SELECT data FROM runtime.form_record WHERE form_id=$1::uuid AND data->>'site'='P1'`, table).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var p1 map[string]any
	_ = json.Unmarshal(raw, &p1)
	if v, ok := p1["nameplate"].(float64); !ok || v != 350 {
		t.Errorf("P1's nameplate after the load: %#v, want the number 350", p1["nameplate"])
	}
	var label string
	if err := d.pool.QueryRow(ctx, `SELECT label FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='P1'`, dim).Scan(&label); err != nil || label != "West Plant" {
		t.Errorf("member P1 after the load: %q (%v), want relabelled West Plant", label, err)
	}
	waitTotal := func(want float64) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var total float64
			if err := d.pool.QueryRow(ctx, `SELECT COALESCE(SUM(value), 0) FROM runtime.fact_input WHERE metric_id=$1::uuid`, metric).Scan(&total); err != nil {
				t.Fatal(err)
			}
			if total == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("nameplate posted %v in all, want %v", total, want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitTotal(650) // 350 + 200 + 100: P1's posting moved, not added again

	// A file without a column leaves that field as it is.
	load("Site code,Facility\nP2,East Works\n", false)
	if err := d.pool.QueryRow(ctx, `SELECT data FROM runtime.form_record WHERE form_id=$1::uuid AND data->>'site'='P2'`, table).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var p2 map[string]any
	_ = json.Unmarshal(raw, &p2)
	if p2["facility"] != "East Works" || p2["nameplate"] != float64(200) {
		t.Errorf("P2 after a file without the nameplate column: %v, want East Works, 200 kept", p2)
	}

	// Someone who may not change the records is refused, whole.
	status, body := d.req("POST", "/api/forms/"+table+"/import", d.westRep, map[string]any{"csv": "Site code,Facility,Nameplate\nP1,West,1\nP9,New,5\n"})
	if status < 400 || !strings.Contains(string(body), "would update") {
		t.Errorf("a load updating records its loader may not change: %d %s, want it refused", status, body)
	}
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, table).Scan(&n); err != nil || n != 3 {
		t.Errorf("records after the refused load: %d (%v), want 3", n, err)
	}
	waitTotal(650)
}
