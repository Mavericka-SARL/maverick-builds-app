package gateway

import (
	"context"
	"strings"
	"testing"
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
