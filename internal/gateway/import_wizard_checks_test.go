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

// What the Import Wizard's Episode 4 try-out found (2026-10-21), on the
// server: a Validate step that only the commit could correct, a refused
// import that left no line in Recent Imports, a revision the list never
// named, a run that imported the rows that passed, and a workbook's empty
// row sent to the Shape step as null.

type importAnswer struct {
	DryRun    bool `json:"dry_run"`
	Rows      int  `json:"rows"`
	Values    int  `json:"values"`
	ValidRows int  `json:"valid_rows"`
	ErrorRows int  `json:"error_rows"`
	Imported  int  `json:"rows_imported"`
	Errors    []struct {
		Row     int    `json:"row"`
		Column  string `json:"column"`
		Message string `json:"message"`
	} `json:"errors"`
}

func decodeAnswer(t *testing.T, body []byte) importAnswer {
	t.Helper()
	var a importAnswer
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return a
}

// A file's line 3 names a country the model does not have.
const marsFile = "geography,period,revenue,cost\nCA,Q1,10,4\nMARS,Q1,5,1\nUK,Q2,7,2\n"

// The upload's dry run lists each row the import would refuse, by its line
// in the file, and imports nothing; the import itself refuses the same row
// and keeps the refusal in Recent Imports with its revision and reason.
func TestTheUploadChecksAFileBeforeItImports(t *testing.T) {
	f := newSalesFileFixture(t)
	_, do := f.serve(t, &multiScriptProvider{})

	status, body := do(f.devSub, "POST", "/api/import/upload", map[string]any{"csv": marsFile, "revision_id": f.revID, "dry_run": true})
	a := decodeAnswer(t, body)
	if status != http.StatusOK || !a.DryRun || a.Rows != 2 || a.Values != 4 || a.ErrorRows != 1 ||
		len(a.Errors) != 1 || a.Errors[0].Row != 3 || a.Errors[0].Column != "geography" {
		t.Fatalf("dry run: %d %s", status, body)
	}
	if n := f.count(t, `SELECT count(*) FROM import.import_job WHERE model_id=$1::uuid`, f.modelID); n != 0 {
		t.Fatalf("a dry run left %d import job(s)", n)
	}
	if n := f.count(t, `SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, f.modelID); n != 0 {
		t.Fatalf("a dry run wrote %d value(s)", n)
	}

	status, body = do(f.devSub, "POST", "/api/import/upload", map[string]any{"csv": marsFile, "revision_id": f.revID})
	if a := decodeAnswer(t, body); status != http.StatusUnprocessableEntity || a.ErrorRows != 1 || a.Errors[0].Row != 3 {
		t.Fatalf("refused import: %d %s", status, body)
	}
	status, body = do(f.devSub, "POST", "/api/import/upload", map[string]any{"csv": strings.Replace(marsFile, "MARS", "US", 1), "revision_id": f.revID})
	if a := decodeAnswer(t, body); status != http.StatusOK || a.Rows != 3 || a.Values != 6 || a.ValidRows != 6 {
		t.Fatalf("import: %d %s", status, body)
	}

	status, body = do(f.devSub, "GET", "/api/import/jobs", nil)
	var jobs []struct {
		Status       int    `json:"status"`
		RevisionName string `json:"revision_name"`
		Error        string `json:"error"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &jobs) != nil || len(jobs) != 2 {
		t.Fatalf("jobs: %d %s", status, body)
	}
	committed, refused := jobs[0], jobs[1] // newest first
	if committed.Status != 3 || committed.RevisionName != "Working" || committed.Error != "" {
		t.Errorf("committed import listed as %+v", committed)
	}
	if refused.Status != 4 || refused.RevisionName != "Working" || !strings.Contains(refused.Error, "row 3, geography") || !strings.Contains(refused.Error, "MARS") {
		t.Errorf("refused import listed as %+v", refused)
	}

	// The wizard sends the rows it shaped as a CSV of its own, with each
	// row's line in the person's file: a title and a blank row above the
	// header put the CSV's line 3 on the file's line 6. The answer and the
	// history name that line.
	lines := []int{5, 6, 7}
	status, body = do(f.devSub, "POST", "/api/import/upload", map[string]any{"csv": marsFile, "revision_id": f.revID, "dry_run": true, "row_lines": lines})
	if a := decodeAnswer(t, body); status != http.StatusOK || len(a.Errors) != 1 || a.Errors[0].Row != 6 {
		t.Fatalf("dry run with the file's lines: %d %s", status, body)
	}
	status, body = do(f.devSub, "POST", "/api/import/upload", map[string]any{"csv": marsFile, "revision_id": f.revID, "row_lines": lines})
	if a := decodeAnswer(t, body); status != http.StatusUnprocessableEntity || a.Errors[0].Row != 6 {
		t.Fatalf("refused import with the file's lines: %d %s", status, body)
	}
	_, body = do(f.devSub, "GET", "/api/import/jobs", nil)
	if json.Unmarshal(body, &jobs) != nil || len(jobs) != 3 || !strings.HasPrefix(jobs[0].Error, "row 6, geography") {
		t.Errorf("the history names %+v; want the file's line 6", jobs)
	}
}

// A saved integration's run imports a grid file whole or not at all, as the
// wizard does (the owner's decision of 2026-10-22): it used to import the
// rows that passed. Its dry run checks the file the same way and records
// nothing.
func TestAGridFileRunImportsWholeOrNothing(t *testing.T) {
	f := newSalesFileFixture(t)
	_, do := f.serve(t, &multiScriptProvider{})
	status, body := do(f.devSub, "POST", "/api/developer/integrations?revision_id="+f.revID,
		map[string]any{"name": "Actuals", "type": "csv_import", "target_type": "grid", "target_id": f.grid})
	var created struct {
		ID string `json:"id"`
	}
	if status != http.StatusOK && status != http.StatusCreated || json.Unmarshal(body, &created) != nil || created.ID == "" {
		t.Fatalf("create integration: %d %s", status, body)
	}
	run := "/api/integrations/" + created.ID + "/run"

	status, body = do(f.devSub, "POST", run+"?dry_run=true", map[string]any{"csv": marsFile})
	if a := decodeAnswer(t, body); status != http.StatusOK || !a.DryRun || a.Rows != 2 || a.ErrorRows != 1 || a.Errors[0].Row != 3 {
		t.Fatalf("dry run: %d %s", status, body)
	}
	status, body = do(f.devSub, "POST", run+"?dry_run=true", map[string]any{"csv": marsFile, "row_lines": []int{10, 12, 14}})
	if a := decodeAnswer(t, body); status != http.StatusOK || a.Errors[0].Row != 12 {
		t.Fatalf("dry run with the file's lines: %d %s", status, body)
	}
	status, body = do(f.devSub, "POST", run, map[string]any{"csv": marsFile})
	if a := decodeAnswer(t, body); status != http.StatusUnprocessableEntity || a.ErrorRows != 1 || a.Errors[0].Row != 3 {
		t.Fatalf("run with a bad row: %d %s", status, body)
	}
	if n := f.count(t, `SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, f.modelID); n != 0 {
		t.Fatalf("a refused run wrote %d value(s); want none", n)
	}
	var runs []string
	rows, err := f.pool.Query(f.ctx, `SELECT status || ': ' || COALESCE(message, '') FROM model.integration_run WHERE integration_id=$1::uuid ORDER BY created_at`, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		runs = append(runs, s)
	}
	rows.Close()
	if len(runs) != 1 || !strings.Contains(runs[0], "error: 1 row(s) failed validation, nothing was imported — row 3, geography") {
		t.Fatalf("run history = %q; want the refused run only (the dry run records none)", runs)
	}

	status, body = do(f.devSub, "POST", run, map[string]any{"csv": strings.Replace(marsFile, "MARS", "US", 1)})
	if a := decodeAnswer(t, body); status != http.StatusOK || a.Imported != 3 || a.ErrorRows != 0 {
		t.Fatalf("run: %d %s", status, body)
	}
}

// A workbook's empty row comes back from the Shape step's preview as a row
// with no cells, not null, and each shaped row names its line in the file.
func TestThePreviewKeepsAWorkbooksEmptyRowAndEachRowsLine(t *testing.T) {
	f := newSalesFileFixture(t)
	_, do := f.serve(t, &multiScriptProvider{})
	x := excelize.NewFile()
	for i, r := range [][]any{{"Actuals FY26"}, {}, {"geography", "period", "revenue"}, {"CA", "Q1", 10}, {"UK", "Q2", 7}} {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		_ = x.SetSheetRow("Sheet1", cell, &r)
	}
	var buf bytes.Buffer
	if err := x.Write(&buf); err != nil {
		t.Fatal(err)
	}
	status, body := do(f.devSub, "POST", "/api/import/reshape-preview", map[string]any{
		"xlsx_base64": base64.StdEncoding.EncodeToString(buf.Bytes()), "reshape": map[string]any{"header_row": 3},
	})
	if status != http.StatusOK || !strings.Contains(string(body), `"raw":[["Actuals FY26"],[],`) {
		t.Fatalf("preview: %d %s", status, body)
	}
	var out struct {
		Lines []int `json:"lines"`
	}
	if json.Unmarshal(body, &out) != nil || len(out.Lines) != 2 || out.Lines[0] != 4 || out.Lines[1] != 5 {
		t.Fatalf("lines = %v, want [4 5] (%s)", out.Lines, body)
	}
}

// A table's import has the same dry run: every row checked as the import
// would check it, its problems listed by line, nothing created.
func TestATableImportsDryRunCreatesNothing(t *testing.T) {
	f := setupTransferFixture(t)
	count := func() int {
		var n int
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, f.formID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	status, body := f.jsonRequest(t, "POST", "/api/forms/"+f.formID+"/import", f.exporterSub,
		map[string]any{"csv": "Employee Name,Hours\nBob,5\n,3\n", "dry_run": true})
	errs, _ := body["errors"].([]any)
	if status != http.StatusOK || body["dry_run"] != true || body["error_rows"] != 1.0 || len(errs) != 1 {
		t.Fatalf("dry run: %d %v", status, body)
	}
	if first, _ := errs[0].(map[string]any); first["row"] != 3.0 {
		t.Errorf("the empty name is on line 3; the dry run says %v", first["row"])
	}
	if n := count(); n != before {
		t.Fatalf("a dry run created %d record(s)", n-before)
	}
}

// A loader saved from a workbook's second sheet keeps reading that sheet
// when a run's workbook names none — a dashboard's Integration button sends
// none. It used to read the first sheet: the education model's KPI and
// balance loaders read its GL sheet and refused the month (2026-10-28). A
// sheet the run names still wins.
func TestAFileLoaderReadsTheSheetItWasSavedWith(t *testing.T) {
	f := newSalesFileFixture(t)
	_, do := f.serve(t, &multiScriptProvider{})
	status, body := do(f.devSub, "POST", "/api/developer/integrations?revision_id="+f.revID,
		map[string]any{"name": "Actuals", "type": "csv_import", "target_type": "grid", "target_id": f.grid})
	var created struct {
		ID string `json:"id"`
	}
	if status != http.StatusOK && status != http.StatusCreated || json.Unmarshal(body, &created) != nil || created.ID == "" {
		t.Fatalf("create integration: %d %s", status, body)
	}
	if status, body := do(f.devSub, "PATCH", "/api/developer/integrations/"+created.ID+"/config",
		map[string]any{"config": map[string]any{"sheet": "Actuals", "import_mode": "replace"}}); status != http.StatusOK {
		t.Fatalf("save the loader's sheet: %d %s", status, body)
	}
	x := excelize.NewFile()
	_ = x.SetSheetName("Sheet1", "Notes")
	_ = x.SetSheetRow("Notes", "A1", &[]any{"Read me: the actuals are on the next sheet"})
	_, _ = x.NewSheet("Actuals")
	_ = x.SetSheetRow("Actuals", "A1", &[]any{"geography", "period", "revenue"})
	_ = x.SetSheetRow("Actuals", "A2", &[]any{"CA", "Q1", 11})
	_, _ = x.NewSheet("Other")
	_ = x.SetSheetRow("Other", "A1", &[]any{"geography", "period", "revenue"})
	_ = x.SetSheetRow("Other", "A2", &[]any{"UK", "Q2", 22})
	var buf bytes.Buffer
	if err := x.Write(&buf); err != nil {
		t.Fatal(err)
	}
	book := base64.StdEncoding.EncodeToString(buf.Bytes())
	run := "/api/integrations/" + created.ID + "/run"
	value := func(geo, period string) float64 {
		t.Helper()
		var v float64
		_ = f.pool.QueryRow(f.ctx, `
			SELECT COALESCE(sum(fi.value),0)::float8 FROM runtime.fact_input fi
			JOIN model.metric_def md ON md.id = fi.metric_id AND md.name = 'revenue'
			WHERE fi.model_id = $1::uuid AND fi.dim_members->>(SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND name='geography') = $2
			  AND fi.dim_members->>(SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND name='period') = $3`,
			f.modelID, geo, period).Scan(&v)
		return v
	}

	status, body = do(f.devSub, "POST", run, map[string]any{"xlsx_base64": book})
	if a := decodeAnswer(t, body); status != http.StatusOK || a.Imported != 1 || value("CA", "Q1") != 11 {
		t.Fatalf("a workbook naming no sheet: %d %s; CA Q1 = %v, want the saved sheet's 11", status, body, value("CA", "Q1"))
	}
	status, body = do(f.devSub, "POST", run, map[string]any{"xlsx_base64": book, "sheet": "Other"})
	if a := decodeAnswer(t, body); status != http.StatusOK || a.Imported != 1 || value("UK", "Q2") != 22 {
		t.Fatalf("a workbook naming its sheet: %d %s; UK Q2 = %v, want 22", status, body, value("UK", "Q2"))
	}
	// A workbook without the saved sheet is refused, naming the sheets it has.
	other := excelize.NewFile()
	_ = other.SetSheetRow("Sheet1", "A1", &[]any{"geography", "period", "revenue"})
	_ = other.SetSheetRow("Sheet1", "A2", &[]any{"CA", "Q3", 33})
	buf.Reset()
	if err := other.Write(&buf); err != nil {
		t.Fatal(err)
	}
	status, body = do(f.devSub, "POST", run, map[string]any{"xlsx_base64": base64.StdEncoding.EncodeToString(buf.Bytes())})
	if status == http.StatusOK || !strings.Contains(string(body), `no sheet \"Actuals\" — its sheets are: Sheet1`) || value("CA", "Q3") != 0 {
		t.Fatalf("a workbook without the saved sheet: %d %s; CA Q3 = %v, want a refusal and nothing written", status, body, value("CA", "Q3"))
	}
}
