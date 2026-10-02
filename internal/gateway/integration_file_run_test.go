package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// A saved Excel/CSV integration mapped by NAME — the kind the Import Wizard
// and the AI Developer save — is re-run by a business user from a dashboard
// button with the file they have: a CSV in the long layout or a formatted
// workbook in the wide one. Valid rows commit, bad ones are named, negative
// values are values, the write guard still applies, and every run is in the
// integration's history. The run endpoint used to take CSV only and stage
// the raw metric cell as a metric id, so none of these files ran.
func TestBusinessUserRunsANameMappedFileIntegration(t *testing.T) {
	f := newSalesFileFixture(t)
	_, do := f.serve(t, nil)
	dev, biz := f.devSub, f.bizSub

	status, body := do(dev, "POST", "/api/developer/integrations?revision_id="+f.revID, map[string]any{
		"name": "Monthly actuals", "type": "csv_import", "target_type": "grid", "target_id": f.grid,
	})
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)
	id := created.ID
	if status, body := do(dev, "PATCH", "/api/developer/integrations/"+id+"/config", map[string]any{"config": map[string]any{
		"column_map":  map[string]string{"Country": "geography", "Quarter": "period", "Account": "metric", "Amount": "value", "Notes": "ignore"},
		"import_mode": "replace",
	}}); status != http.StatusOK {
		t.Fatalf("config: %d %s", status, body)
	}
	value := func(geo, period, metric string) (float64, bool) {
		t.Helper()
		var v float64
		err := f.pool.QueryRow(f.ctx, `
			SELECT fi.value::float8 FROM runtime.fact_input fi
			JOIN model.metric_def m ON m.id = fi.metric_id
			JOIN model.dimension_def g ON g.revision_id = fi.revision_id AND g.name = 'geography'
			JOIN model.dimension_def p ON p.revision_id = fi.revision_id AND p.name = 'period'
			WHERE fi.revision_id=$1::uuid AND m.name=$2 AND fi.dim_members->>g.id::text = $3 AND fi.dim_members->>p.id::text = $4
			ORDER BY fi.entered_at DESC, fi.id DESC LIMIT 1`, f.revID, metric, geo, period).Scan(&v)
		return v, err == nil
	}
	type runResult struct {
		RowsImported   int              `json:"rows_imported"`
		ValuesImported int              `json:"values_imported"`
		ErrorRows      int              `json:"error_rows"`
		Errors         []map[string]any `json:"errors"`
	}
	run := func(payload map[string]any) (int, runResult, string) {
		t.Helper()
		status, body := do(biz, "POST", "/api/integrations/"+id+"/run", payload)
		var res runResult
		_ = json.Unmarshal(body, &res)
		return status, res, string(body)
	}

	// 1. A long CSV: metric names per row, a credit, an unknown member.
	status, res, raw := run(map[string]any{"csv": "Country,Quarter,Account,Amount,Notes\n" +
		"CA,Q1,revenue,100,\nCA,Q1,cost,-20,credit note\nUK,Q2,revenue,50,\nFR,Q1,revenue,5,\n"})
	if status != http.StatusOK || res.RowsImported != 3 || res.ErrorRows != 1 {
		t.Fatalf("long csv run: %d %s", status, raw)
	}
	if msg, _ := res.Errors[0]["message"].(string); !strings.Contains(msg, `"FR" is not a member of dimension "geography"`) {
		t.Errorf("the bad row must be named: %v", res.Errors)
	}
	if v, ok := value("CA", "Q1", "cost"); !ok || v != -20 {
		t.Errorf("CA Q1 cost = %v (found %v), want the credit -20", v, ok)
	}
	if v, _ := value("UK", "Q2", "revenue"); v != 50 {
		t.Errorf("UK Q2 revenue = %v, want 50", v)
	}

	// 2. A workbook in the wide layout, number-formatted, on its second sheet.
	wb := excelize.NewFile()
	_, _ = wb.NewSheet("Actuals")
	_ = wb.SetSheetRow("Actuals", "A1", &[]any{"Country", "Quarter", "revenue", "cost"})
	_ = wb.SetSheetRow("Actuals", "A2", &[]any{"CA", "Q1", 1234.5, 200})
	style, _ := wb.NewStyle(&excelize.Style{NumFmt: 4})
	_ = wb.SetCellStyle("Actuals", "C2", "D2", style)
	var buf bytes.Buffer
	_ = wb.Write(&buf)
	_ = wb.Close()
	status, res, raw = run(map[string]any{"xlsx_base64": base64.StdEncoding.EncodeToString(buf.Bytes()), "sheet": "Actuals"})
	if status != http.StatusOK || res.RowsImported != 1 || res.ValuesImported != 2 || res.ErrorRows != 0 {
		t.Fatalf("xlsx run: %d %s", status, raw)
	}
	// The saved mode is "replace": the workbook's value is the cell's, not
	// added to the CSV's 100.
	if v, _ := value("CA", "Q1", "revenue"); v != 1234.5 {
		t.Errorf("CA Q1 revenue = %v, want 1234.5 (the stored value, replacing 100)", v)
	}

	// 3. The write guard applies to the business user's run: US is hidden
	// from them, so a file touching it writes nothing.
	status, _, raw = run(map[string]any{"csv": "Country,Quarter,Account,Amount\nUS,Q1,revenue,7\n"})
	if status != http.StatusForbidden {
		t.Errorf("a run touching a hidden member: %d %s", status, raw)
	}
	if _, ok := value("US", "Q1", "revenue"); ok {
		t.Error("the hidden member's value was written")
	}

	// Every run is in the history the console shows.
	status, body = do(dev, "GET", "/api/developer/integrations/"+id+"/runs", nil)
	var runs []struct {
		Status       string `json:"status"`
		RowsImported int    `json:"rows_imported"`
		ErrorRows    int    `json:"error_rows"`
	}
	_ = json.Unmarshal(body, &runs)
	if status != http.StatusOK || len(runs) != 3 {
		t.Fatalf("run history: %d %s", status, body)
	}
	statuses := map[string]int{}
	for _, r := range runs {
		statuses[r.Status]++
	}
	if statuses["success"] != 2 || statuses["error"] != 1 {
		t.Errorf("run statuses = %v, want 2 success and 1 error", statuses)
	}

	// A run without a file says what it needs.
	if status, _, raw := run(map[string]any{}); status != http.StatusBadRequest || !strings.Contains(raw, "xlsx_base64") {
		t.Errorf("empty run: %d %s", status, raw)
	}
}

// A dimension integration runs from a workbook too: the saved map renames
// the file's columns to member fields.
func TestDimensionIntegrationRunsFromAWorkbook(t *testing.T) {
	f := newSalesFileFixture(t)
	_, do := f.serve(t, nil)
	var geo string
	if err := f.pool.QueryRow(f.ctx, `SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name='geography'`, f.revID).Scan(&geo); err != nil {
		t.Fatal(err)
	}
	status, body := do(f.devSub, "POST", "/api/developer/integrations?revision_id="+f.revID, map[string]any{
		"name": "Countries", "type": "csv_import", "target_type": "dimension", "target_id": geo,
	})
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)
	if status, body := do(f.devSub, "PATCH", "/api/developer/integrations/"+created.ID+"/config", map[string]any{"config": map[string]any{
		"column_map": map[string]string{"ISO": "code", "Country name": "label", "Region": "parent_code"},
	}}); status != http.StatusOK {
		t.Fatalf("config: %d %s", status, body)
	}
	wb := excelize.NewFile()
	_ = wb.SetSheetRow("Sheet1", "A1", &[]any{"ISO", "Country name", "Region"})
	_ = wb.SetSheetRow("Sheet1", "A2", &[]any{"DE", "Germany", "EMEA"})
	var buf bytes.Buffer
	_ = wb.Write(&buf)
	_ = wb.Close()
	status, body = do(f.devSub, "POST", "/api/integrations/"+created.ID+"/run", map[string]any{"xlsx_base64": base64.StdEncoding.EncodeToString(buf.Bytes())})
	if status != http.StatusOK || !strings.Contains(string(body), `"rows_imported":1`) {
		t.Fatalf("run: %d %s", status, body)
	}
	if n := f.count(t, `SELECT count(*) FROM model.dimension_member m JOIN model.dimension_member p ON p.id = m.parent_member_id
		WHERE m.dimension_id=$1::uuid AND m.code='DE' AND m.label='Germany' AND p.code='EMEA'`, geo); n != 1 {
		t.Error("DE was not added under EMEA")
	}
}
