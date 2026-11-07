package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A table keeping a dimension takes codes given from outside: a text field
// marked as the code holds a ledger's account numbers, each record is the
// member of its number, a number already taken is refused, and a saved
// number stays. Before, a kept table only issued auto-codes, so the
// Controllers could not add a GL account under its ledger number
// (restaurant model, 2026-10-31).
func TestKeptTableTakesTypedCodes(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()
	glDim := idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": "gl_account", "revision_id": d.revID}))
	table := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{
		"name": "gl_accounts", "label": "GL accounts", "edit_access": "everyone",
		"fields": []map[string]any{
			{"name": "account", "label": "Account no.", "type": "text", "code": true, "core": true},
			{"name": "name", "label": "Account name", "type": "text", "required": true},
		},
		"keeps_dimension_id": glDim, "member_label_field": "name",
	}))
	rec := d.call("POST", "/api/forms/"+table+"/records", d.dev, map[string]any{"data": map[string]any{"account": "4000", "name": "Dine-in sales"}})
	var label string
	if err := d.pool.QueryRow(ctx, `SELECT label FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='4000'`, glDim).Scan(&label); err != nil || label != "Dine-in sales" {
		t.Fatalf("member 4000: %q, %v — want the record's member, labelled Dine-in sales", label, err)
	}
	for _, c := range []struct {
		data map[string]any
		want string
	}{
		{map[string]any{"account": "4000", "name": "Again"}, "is taken"},
		{map[string]any{"name": "No number"}, "is required"},
		{map[string]any{"account": "40:00", "name": "Colon"}, "which a code cannot"},
	} {
		status, raw := d.req("POST", "/api/forms/"+table+"/records", d.dev, map[string]any{"data": c.data})
		if status < 400 || !strings.Contains(string(raw), c.want) {
			t.Errorf("record %v: %d %s, want a refusal saying %q", c.data, status, raw, c.want)
		}
	}
	d.call("PUT", "/api/records/"+idOf(rec), d.dev, map[string]any{"data": map[string]any{"account": "4999", "name": "Dine-in revenue"}})
	var code string
	if err := d.pool.QueryRow(ctx, `SELECT data->>'account' FROM runtime.form_record WHERE id=$1::uuid`, idOf(rec)).Scan(&code); err != nil || code != "4000" {
		t.Errorf("after a save naming 4999 the record's code is %q (%v), want 4000 kept", code, err)
	}
	status, raw := d.req("POST", "/api/forms/"+table+"/import", d.dev, map[string]any{
		"csv": "Account no.,Account name\n6100,Rent\n6100,Rent again\n"})
	if status < 400 || !strings.Contains(string(raw), "also on row") {
		t.Errorf("a file giving 6100 twice: %d %s, want it refused", status, raw)
	}
	if status, raw := d.req("POST", "/api/forms", d.dev, map[string]any{"name": "bad", "label": "Bad", "fields": []map[string]any{
		{"name": "n", "label": "N", "type": "number", "code": true}}}); status < 400 || !strings.Contains(string(raw), "only a text field") {
		t.Errorf("a number field marked as the code: %d %s, want it refused", status, raw)
	}
}

// A new record of a typed-code table posts under the member it is about to
// become. The posting check ran before the save made that member, so a plant
// added with its nameplate was refused: "there is no member" (production
// model, 2026-11-06). An auto-code record has no code at that point and was
// never checked.
func TestTypedCodeRecordPostsIntoItsOwnMember(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()
	dim := idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": "plant", "revision_id": d.revID}))
	grid := idOf(d.call("POST", "/api/developer/grids", d.dev, map[string]any{"name": "Plant master", "revision_id": d.revID}))
	d.call("POST", "/api/developer/grids/"+grid+"/dimensions/"+dim, d.dev, map[string]any{})
	metric := idOf(d.call("POST", "/api/developer/metrics", d.dev, map[string]any{
		"name": "nameplate", "label": "Nameplate", "is_input": true, "revision_id": d.revID, "format": "number", "agg_rule": "sum"}))
	d.call("POST", "/api/developer/grids/"+grid+"/metrics/"+metric, d.dev, map[string]any{})
	table := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{
		"name": "plants", "label": "Plants",
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

	d.call("POST", "/api/forms/"+table+"/records", d.dev, map[string]any{
		"data": map[string]any{"site": "PLANT_D", "facility": "West Plant", "nameplate": 300}})

	deadline := time.Now().Add(10 * time.Second)
	for {
		var total float64
		if err := d.pool.QueryRow(ctx, `SELECT COALESCE(SUM(value), 0) FROM runtime.fact_input WHERE metric_id=$1::uuid`, metric).Scan(&total); err != nil {
			t.Fatal(err)
		}
		if total == 300 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the new plant's nameplate posted %v, want 300", total)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A dimension kept by a typed-code table keeps its members through a model
// file. The import's cleanup matched records through an autocode field only,
// so it deleted every plant and GL account and reported success (production
// model, 2026-11-06: 16,968 of 27,936 cells wrong).
func TestImportKeepsTypedCodeMembers(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	post := func(path string, body map[string]any) string {
		t.Helper()
		status, out := doAs(t, f.rollupFixture, "POST", path, dev, f.appID, body)
		if status != 200 {
			t.Fatalf("POST %s: %d %s", path, status, out)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(out), &m)
		id, _ := m["id"].(string)
		return id
	}
	dim := post("/api/developer/dimensions", map[string]any{"name": "gl_account", "revision_id": f.workingRevID})
	table := post("/api/forms?revision_id="+f.workingRevID, map[string]any{
		"name": "gl_accounts", "label": "GL accounts",
		"fields": []map[string]any{
			{"name": "account", "label": "Account no.", "type": "text", "code": true, "core": true},
			{"name": "name", "label": "Account name", "type": "text", "required": true},
		},
		"keeps_dimension_id": dim, "member_label_field": "name", "member_root_label": "All accounts",
	})
	post("/api/forms/"+table+"/records", map[string]any{"data": map[string]any{"account": "4000", "name": "Sales"}})

	status, pkg := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s&include_data=true", f.modelID, f.workingRevID), f.taPersona, nil)
	if status != 200 {
		t.Fatalf("export: %d %v", status, pkg)
	}
	status, res := f.do(t, "POST", "/api/admin/models/import", f.taPersona,
		map[string]any{"application_id": f.appID, "model_name": "Imported accounts", "package": pkg})
	if status != 200 {
		t.Fatalf("import: %d %v", status, res)
	}
	newRev, _ := res["revision_id"].(string)
	var n int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id
		WHERE d.revision_id = $1::uuid AND d.name = 'gl_account' AND m.code = '4000'`, newRev).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("imported gl_account holds member 4000 %d time(s), want 1", n)
	}
}
