package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A loader replaces by a combination of fields and asks for what its file
// does not say (2026-11-13): one entity's trial balance for one month is
// reloaded without touching the other entities or months, and the month is
// given at upload, not written into the file or the loader.
func TestTableLoaderReplacesByFieldsAndAsksForParameters(t *testing.T) {
	f := setupRoundTripFixture(t)
	dev := "rollup-test-approver"
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, dev, f.appID, body)
	}
	status, raw := call("POST", "/api/forms?revision_id="+f.workingRevID, map[string]any{
		"name": "tb_lines_t", "label": "Trial balance lines",
		"fields": []map[string]any{
			{"name": "entity", "label": "Legal entity", "type": "text", "required": true},
			{"name": "period", "label": "Period end", "type": "date", "required": true},
			{"name": "amount", "label": "Amount", "type": "number"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("create table: %d %s", status, raw)
	}
	var table struct{ ID string }
	_ = json.Unmarshal([]byte(raw), &table)
	status, raw = call("POST", "/api/developer/integrations?revision_id="+f.workingRevID, map[string]any{
		"name": "Upload trial balances", "type": "csv_import", "target_type": "form", "target_id": table.ID,
	})
	if status != http.StatusOK {
		t.Fatalf("create integration: %d %s", status, raw)
	}
	var intg struct{ ID string }
	_ = json.Unmarshal([]byte(raw), &intg)
	if status, raw := call("PATCH", "/api/developer/integrations/"+intg.ID+"/config", map[string]any{"config": map[string]any{
		"replace_by": []string{"entity", "amount"}}}); status != http.StatusBadRequest || !strings.Contains(raw, "Amount is a number field") {
		t.Fatalf("replace_by naming a number field among others: %d %s, want 400", status, raw)
	}
	if status, raw := call("PATCH", "/api/developer/integrations/"+intg.ID+"/config", map[string]any{"config": map[string]any{
		"replace_by": []string{"entity", "period"},
		"parameters": []map[string]any{{"column": "period", "label": "Period end", "type": "date", "required": true}},
	}}); status != http.StatusOK {
		t.Fatalf("replace_by entity and period: %d %s", status, raw)
	}
	run := func(csv string, params map[string]string) (int, string) {
		t.Helper()
		body := map[string]any{"csv": csv}
		if params != nil {
			body["parameters"] = params
		}
		return call("POST", "/api/integrations/"+intg.ID+"/run", body)
	}
	records := func() map[string]string {
		t.Helper()
		rows, err := f.pool.Query(t.Context(), `SELECT data->>'entity' || '/' || left(data->>'period', 7), data->>'amount'
			FROM runtime.form_record WHERE form_id=$1::uuid`, table.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				t.Fatal(err)
			}
			out[k] = v
		}
		return out
	}

	if status, raw := run("entity,amount\nHOLD,1\n", nil); status != http.StatusBadRequest || !strings.Contains(raw, "asks for Period end") {
		t.Fatalf("a run without its period: %d %s, want 400 asking for it", status, raw)
	}
	if status, raw := run("entity,amount\nHOLD,1\n", map[string]string{"period": "30/09/2026"}); status != http.StatusBadRequest || !strings.Contains(raw, "is a date") {
		t.Fatalf("a period that is no date: %d %s, want 400", status, raw)
	}
	if status, raw := run("entity,amount\nHOLD,1\nPROD,2\n", map[string]string{"period": "2026-09-30"}); status != http.StatusOK {
		t.Fatalf("September: %d %s", status, raw)
	}
	if status, raw := run("entity,amount\nHOLD,3\n", map[string]string{"period": "2026-10-31"}); status != http.StatusOK {
		t.Fatalf("October: %d %s", status, raw)
	}
	// HOLD reloads September: PROD's September and HOLD's October stay.
	if status, raw := run("entity,amount\nHOLD,10\n", map[string]string{"period": "2026-09-30"}); status != http.StatusOK || !strings.Contains(raw, `"rows_replaced":1`) {
		t.Fatalf("HOLD reloads September: %d %s, want one record replaced", status, raw)
	}
	got := records()
	want := map[string]string{"HOLD/2026-09": "10", "PROD/2026-09": "2", "HOLD/2026-10": "3"}
	if len(got) != len(want) {
		t.Fatalf("records %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("records %v, want %v", got, want)
		}
	}
}
