package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
)

// Recent Imports as a self-hosting tester found it (2026-10-23): the list
// was the application's default model's, not the model being built; a
// dimension's members and a table's records never appeared in it, and their
// audit events named no revision; and a line said nothing of what was
// imported into what, from which file, by whom or how long it took.

type historyLine struct {
	Status       int    `json:"status"`
	ValidRows    int    `json:"valid_rows"`
	ErrorRows    int    `json:"error_rows"`
	RevisionName string `json:"revision_name"`
	Error        string `json:"error"`
	Source       string `json:"source"`
	TargetType   string `json:"target_type"`
	TargetName   string `json:"target_name"`
	ImportMode   string `json:"import_mode"`
	FileName     string `json:"file_name"`
	By           string `json:"by"`
	DurationMS   *int64 `json:"duration_ms"`
}

func TestRecentImportsDescribesEveryFileImportOfTheWorkingModel(t *testing.T) {
	f := newSalesFileFixture(t)
	_, do := f.serve(t, &multiScriptProvider{})
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return s
	}
	// A newer model of the same application: a request that names no
	// revision resolves it, as the tester's console did for hers.
	otherModel := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Other') RETURNING id::text`, f.appID)
	otherRev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Other working') RETURNING id::text`, otherModel)
	otherGrid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Other grid') RETURNING id::text`, otherModel, otherRev)
	geography := q(`SELECT id::text FROM model.dimension_def WHERE revision_id = $1::uuid AND name = 'geography'`, f.revID)
	form, err := crudapp.NewStore(f.pool).CreateForm(f.ctx, f.modelID, f.revID, "expenses", "Expenses", []crudapp.FormField{
		{Name: "description", Label: "Description", Type: "text", Required: true},
		{Name: "amount", Label: "Amount", Type: "number"},
	})
	if err != nil {
		t.Fatal(err)
	}

	imports := []struct {
		path     string
		body     map[string]any
		wantCode int
	}{
		// A browser may name its file with a path.
		{"/api/import/dimension-members", map[string]any{"dimension_id": geography, "csv": "code,label\nMX,Mexico\n", "file_name": `C:\fakepath\countries.csv`}, http.StatusOK},
		{"/api/import/upload", map[string]any{"csv": "geography,period,revenue\nCA,Q1,10\n", "revision_id": f.revID, "import_mode": "replace", "grid_id": f.grid, "file_name": "sales.csv"}, http.StatusOK},
		// Another model's grid describes nothing.
		{"/api/import/upload", map[string]any{"csv": "geography,period,cost\nCA,Q1,4\n", "revision_id": f.revID, "grid_id": otherGrid, "file_name": "cost.csv"}, http.StatusOK},
		{"/api/forms/" + form.ID + "/import", map[string]any{"csv": "description,amount\nTaxi,12\n", "file_name": "expenses.csv"}, http.StatusOK},
		{"/api/forms/" + form.ID + "/import", map[string]any{"csv": "description,amount\n,12\n", "file_name": "bad.csv"}, http.StatusUnprocessableEntity},
	}
	for _, imp := range imports {
		if status, body := do(f.devSub, "POST", imp.path, imp.body); status != imp.wantCode {
			t.Fatalf("POST %s: %d %s", imp.path, status, body)
		}
	}

	history := func(persona, query string) []historyLine {
		t.Helper()
		status, body := do(persona, "GET", "/api/import/jobs"+query, nil)
		var lines []historyLine
		if status != http.StatusOK || json.Unmarshal(body, &lines) != nil {
			t.Fatalf("GET /api/import/jobs%s as %s: %d %s", query, persona, status, body)
		}
		return lines
	}

	if lines := history(f.devSub, ""); len(lines) != 0 {
		t.Errorf("with no revision named the list is the newest model's, which has no imports; got %+v", lines)
	}
	lines := history(f.devSub, "?revision_id="+f.revID)
	if len(lines) != len(imports) {
		t.Fatalf("the working model's history has %d line(s), want %d: %+v", len(lines), len(imports), lines)
	}
	// Newest first.
	refusedTable, table, foreignGrid, grid, dimension := lines[0], lines[1], lines[2], lines[3], lines[4]
	for _, l := range lines {
		if l.RevisionName != "Working" || l.By != f.devSub || l.Source != "upload" || l.DurationMS == nil || *l.DurationMS < 0 {
			t.Errorf("each line names the revision, who and how long: %+v", l)
		}
	}
	if dimension.TargetType != "dimension" || dimension.TargetName != "geography" || dimension.FileName != "countries.csv" ||
		dimension.Status != 3 || dimension.ValidRows != 1 {
		t.Errorf("dimension import listed as %+v", dimension)
	}
	if grid.TargetType != "grid" || grid.TargetName != "Sales" || grid.ImportMode != "replace" || grid.FileName != "sales.csv" || grid.Status != 3 {
		t.Errorf("grid import listed as %+v", grid)
	}
	if foreignGrid.TargetType != "grid" || foreignGrid.TargetName != "" || foreignGrid.ImportMode != "incremental" {
		t.Errorf("an import naming another model's grid listed as %+v", foreignGrid)
	}
	if table.TargetType != "table" || table.TargetName != "Expenses" || table.FileName != "expenses.csv" || table.Status != 3 || table.ValidRows != 1 {
		t.Errorf("table import listed as %+v", table)
	}
	if refusedTable.Status != 4 || refusedTable.FileName != "bad.csv" || refusedTable.ErrorRows != 1 || !strings.HasPrefix(refusedTable.Error, "row 2") {
		t.Errorf("refused table import listed as %+v", refusedTable)
	}

	// The dimension's and the table's audit events name the revision.
	if n := f.count(t, `SELECT count(*) FROM audit.audit_event WHERE resource_type IN ('dimension', 'form') AND revision_id = $1::uuid`, f.revID); n != 2 {
		t.Errorf("%d dimension/table import event(s) name the revision, want 2", n)
	}

	// A business user sees the model's imports, but not what someone else's
	// was: the grid or table, the file, who, or a value its problem quotes.
	for _, l := range history(f.bizSub, "?revision_id="+f.revID) {
		if l.TargetName != "" || l.FileName != "" || l.By != "" || l.Error != "" {
			t.Errorf("a business user reads the developer's import as %+v", l)
		}
	}
}

// Recent Imports lists the open revision's imports: a copy's imports were
// listed on the original's Integrations page, and once the copy was deleted
// they stayed there with Revision "—" (2026-10-23, a how-to video's
// rehearsal revision). Deleting a revision deletes its import jobs.
func TestRecentImportsListsTheOpenRevisionsImportsOnly(t *testing.T) {
	f := newSalesFileFixture(t)
	_, do := f.serve(t, &multiScriptProvider{})
	status, body := do(f.devSub, "POST", "/api/developer/revisions", map[string]any{"name": "Rehearsal", "source_revision_id": f.revID})
	var created struct {
		ID string `json:"id"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &created) != nil || created.ID == "" {
		t.Fatalf("copy the revision: %d %s", status, body)
	}
	rehearsal := created.ID
	var rehearsalGrid string
	if err := f.pool.QueryRow(f.ctx, `SELECT id::text FROM model.grid_def WHERE revision_id = $1::uuid AND name = 'Sales'`, rehearsal).Scan(&rehearsalGrid); err != nil {
		t.Fatal(err)
	}
	for _, imp := range []map[string]any{
		{"csv": "geography,period,revenue\nCA,Q1,10\n", "revision_id": f.revID, "grid_id": f.grid, "file_name": "working.csv"},
		{"csv": "geography,period,revenue\nCA,Q1,24\n", "revision_id": rehearsal, "grid_id": rehearsalGrid, "file_name": "rehearsal.csv"},
	} {
		if status, body := do(f.devSub, "POST", "/api/import/upload", imp); status != http.StatusOK {
			t.Fatalf("import %s: %d %s", imp["file_name"], status, body)
		}
	}

	files := func(revisionID string) []string {
		t.Helper()
		status, body := do(f.devSub, "GET", "/api/import/jobs?revision_id="+revisionID, nil)
		var lines []historyLine
		if status != http.StatusOK || json.Unmarshal(body, &lines) != nil {
			t.Fatalf("GET /api/import/jobs: %d %s", status, body)
		}
		var out []string
		for _, l := range lines {
			out = append(out, l.FileName)
		}
		return out
	}
	if got := files(f.revID); len(got) != 1 || got[0] != "working.csv" {
		t.Errorf("the working revision's Recent Imports list %v, want [working.csv]", got)
	}
	if got := files(rehearsal); len(got) != 1 || got[0] != "rehearsal.csv" {
		t.Errorf("the rehearsal's Recent Imports list %v, want [rehearsal.csv]", got)
	}

	if status, body := do(f.devSub, "DELETE", "/api/developer/revisions/"+rehearsal, nil); status != http.StatusOK {
		t.Fatalf("delete the rehearsal: %d %s", status, body)
	}
	if n := f.count(t, `SELECT count(*) FROM import.import_job WHERE revision_id = $1::uuid`, rehearsal); n != 0 {
		t.Errorf("%d import job(s) of the deleted revision are left, want 0", n)
	}
	if got := files(f.revID); len(got) != 1 || got[0] != "working.csv" {
		t.Errorf("after the delete the working revision's Recent Imports list %v, want [working.csv]", got)
	}
}
