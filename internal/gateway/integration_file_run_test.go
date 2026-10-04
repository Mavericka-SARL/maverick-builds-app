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

// Sheets laid out for people name members by label ("Canada", not CA) and
// periods by month ("Jan", not 2026-01). An import resolves a value that is
// no member's code by the one member whose label it is, and on a monthly
// time dimension by the one leaf period starting in that month; a label two
// members share, or a month two periods start in, stays unknown.
func TestFileImportResolvesLabelsAndMonthNames(t *testing.T) {
	f := newSalesFileFixture(t)
	_, do := f.serve(t, nil)
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	month := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type, time_granularity, fiscal_year_start_month)
		VALUES ($1::uuid,$2::uuid,'month','time','month',1) RETURNING id::text`, f.modelID, f.revID)
	for i, m := range []string{"2026-01", "2026-02"} {
		q(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, sort_order)
			VALUES ($1::uuid,$2,$2,$3::date,($3::date + interval '1 month - 1 day')::date,$4,$4) RETURNING id::text`, month, m, m+"-01", i)
	}
	geo := q(`SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name='geography'`, f.revID)
	units := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid,$2::uuid,'units',true,'sum') RETURNING id::text`, f.modelID, f.revID)
	grid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'Monthly') RETURNING id::text`, f.modelID, f.revID)
	for _, d := range []string{geo, month} {
		q(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid,$2::uuid) RETURNING grid_id::text`, grid, d)
	}
	q(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid,$2::uuid,0) RETURNING grid_id::text`, grid, units)

	status, body := do(f.devSub, "POST", "/api/developer/integrations?revision_id="+f.revID, map[string]any{
		"name": "Units by month", "type": "csv_import", "target_type": "grid", "target_id": grid})
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)
	if status, body := do(f.devSub, "PATCH", "/api/developer/integrations/"+created.ID+"/config", map[string]any{"config": map[string]any{
		"column_map": map[string]string{"Country": "geography", "Month": "month", "Units": "units"}, "import_mode": "replace"}}); status != http.StatusOK {
		t.Fatalf("config: %d %s", status, body)
	}
	run := func(csv string) (int, string) {
		t.Helper()
		status, body := do(f.devSub, "POST", "/api/integrations/"+created.ID+"/run", map[string]any{"csv": csv})
		return status, string(body)
	}
	value := func(geoCode, monthCode string) float64 {
		t.Helper()
		var v float64
		if err := f.pool.QueryRow(f.ctx, `
			SELECT fi.value::float8 FROM runtime.fact_input fi
			WHERE fi.metric_id=$1::uuid AND fi.dim_members->>$2 = $3 AND fi.dim_members->>$4 = $5
			ORDER BY fi.entered_at DESC LIMIT 1`, units, geo, geoCode, month, monthCode).Scan(&v); err != nil {
			t.Fatalf("value %s/%s: %v", geoCode, monthCode, err)
		}
		return v
	}

	if status, body := run("Country,Month,Units\nCanada,Jan,5\nunited kingdom,February,7\nUS,2026-01,9\n"); status != http.StatusOK || !strings.Contains(body, `"error_rows":0`) {
		t.Fatalf("labels and month names: %d %s", status, body)
	}
	if v := value("CA", "2026-01"); v != 5 {
		t.Errorf("Canada/Jan = %v, want 5 at CA/2026-01", v)
	}
	if v := value("UK", "2026-02"); v != 7 {
		t.Errorf("united kingdom/February = %v, want 7 at UK/2026-02", v)
	}

	// A label two members share is not a guess.
	q(`INSERT INTO model.dimension_member (dimension_id, code, label, sort_order) VALUES ($1::uuid,'CA2','Canada',9) RETURNING id::text`, geo)
	if _, body := run("Country,Month,Units\nCanada,Jan,6\n"); !strings.Contains(body, "UNKNOWN_MEMBER") {
		t.Errorf("an ambiguous label must stay unknown: %s", body)
	}
	// Nor is a month two periods start in.
	q(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, sort_order)
		VALUES ($1::uuid,'2027-01','2027-01','2027-01-01','2027-01-31',2,2) RETURNING id::text`, month)
	if _, body := run("Country,Month,Units\nUS,Jan,6\n"); !strings.Contains(body, "UNKNOWN_MEMBER") {
		t.Errorf("an ambiguous month must stay unknown: %s", body)
	}
}

// A column mapped to a dimension the target grid does not have is refused
// with the fix, instead of being resolved as members and written as an
// extra key on the grid's facts.
func TestFileImportRefusesADimensionTheGridLacks(t *testing.T) {
	f := newSalesFileFixture(t)
	_, do := f.serve(t, nil)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type) VALUES ($1::uuid,$2::uuid,'segment','standard')`, f.modelID, f.revID); err != nil {
		t.Fatal(err)
	}
	status, body := do(f.devSub, "POST", "/api/developer/integrations?revision_id="+f.revID, map[string]any{
		"name": "With segment", "type": "csv_import", "target_type": "grid", "target_id": f.grid})
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)
	if status, body := do(f.devSub, "PATCH", "/api/developer/integrations/"+created.ID+"/config", map[string]any{"config": map[string]any{
		"column_map": map[string]string{"Country": "geography", "Quarter": "period", "Segment": "segment", "Amount": "revenue"}}}); status != http.StatusOK {
		t.Fatalf("config: %d %s", status, body)
	}
	status, body = do(f.devSub, "POST", "/api/integrations/"+created.ID+"/run", map[string]any{"csv": "Country,Quarter,Segment,Amount\nCA,Q1,SMB,5\n"})
	if status != http.StatusBadRequest || !strings.Contains(string(body), `which grid \"Sales\" does not have`) {
		t.Errorf("run: %d %s — want 400 naming the dimension the grid lacks", status, body)
	}

	// Nor may a grid dimension go without a column: the value has nowhere to go.
	if status, body := do(f.devSub, "PATCH", "/api/developer/integrations/"+created.ID+"/config", map[string]any{"config": map[string]any{
		"column_map": map[string]string{"Country": "geography", "Quarter": "ignore", "Amount": "revenue"}}}); status != http.StatusOK {
		t.Fatalf("config: %d %s", status, body)
	}
	status, body = do(f.devSub, "POST", "/api/integrations/"+created.ID+"/run", map[string]any{"csv": "Country,Quarter,Amount\nCA,Q1,5\n"})
	if status != http.StatusBadRequest || !strings.Contains(string(body), `no column maps to it`) {
		t.Errorf("run without a period column: %d %s — want 400 naming the unmapped grid dimension", status, body)
	}
}
