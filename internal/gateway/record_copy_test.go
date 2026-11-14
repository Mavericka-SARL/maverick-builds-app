package gateway

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// Records that recur every period are copied into the next one with their
// date moved (2026-11-13): September's intercompany matches become
// October's, as new records with new codes — new members of the dimension
// the table keeps — and nothing else changes.
func TestRecordsCopyIntoAnotherPeriod(t *testing.T) {
	f := setupDimFormulaFixture(t)
	ctx := context.Background()
	dim := f.call("POST", "/api/developer/dimensions", map[string]any{"name": "ic_match", "revision_id": f.revID})
	table := f.call("POST", "/api/forms?revision_id="+f.revID, map[string]any{
		"name": "ic_matches", "label": "IC matches",
		"fields": []map[string]any{
			{"name": "code", "label": "Match ID", "type": "autocode", "code_prefix": "IC", "code_digits": 2},
			{"name": "period", "label": "Period end", "type": "date", "required": true},
			{"name": "title", "label": "Scope", "type": "text", "required": true},
			{"name": "seller_reported", "label": "Seller reported", "type": "number"},
		},
		"keeps_dimension_id": dim, "member_label_field": "title", "member_label_with_code": true,
	})
	var ids []string
	for _, tl := range []string{"Loan HOLD-PROD", "Loan HOLD-LOGI"} {
		ids = append(ids, f.call("POST", "/api/forms/"+table+"/records", map[string]any{"data": map[string]any{
			"period": "2026-09-30", "title": tl, "seller_reported": 150}}))
	}
	status, raw := f.req("POST", "/api/forms/"+table+"/records/copy", map[string]any{"record_ids": ids, "field": "title", "value": "2026-10-31"})
	if status != http.StatusBadRequest || !strings.Contains(string(raw), "no date field") {
		t.Fatalf("copy moving a text field: %d %s; want 400", status, raw)
	}
	status, raw = f.req("POST", "/api/forms/"+table+"/records/copy", map[string]any{"record_ids": ids, "field": "period", "value": "2026-10-31"})
	if status != http.StatusOK || !strings.Contains(string(raw), `"copied":2`) {
		t.Fatalf("copy to October: %d %s; want 2 copied", status, raw)
	}
	rows, err := f.pool.Query(ctx, `SELECT (data->>'code') || '|' || (data->>'period') || '|' || (data->>'seller_reported') FROM runtime.form_record WHERE form_id=$1::uuid`, table)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		got = append(got, s)
	}
	rows.Close()
	sort.Strings(got)
	want := "IC01|2026-09-30|150,IC02|2026-09-30|150,IC03|2026-10-31|150,IC04|2026-10-31|150"
	if strings.Join(got, ",") != want {
		t.Fatalf("records after the copy: %v; want %s", got, want)
	}
	// Each copy is a member, named code first.
	var label string
	if err := f.pool.QueryRow(ctx, `SELECT label FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='IC03'`, dim).Scan(&label); err != nil || label != "IC03 · Loan HOLD-PROD" {
		t.Fatalf("member IC03 label %q (err %v); want IC03 · Loan HOLD-PROD", label, err)
	}
}
