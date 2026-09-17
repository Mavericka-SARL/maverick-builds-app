// Tests for the grid/form CSV+XLSX export and form CSV+XLSX import added in
// grid_export.go and form_transfer.go. Covers: export respects the same
// hidden-dimension-member access rules /api/grid enforces, an exported grid
// file round-trips through the existing /api/import/upload, and form export/
// import round-trip through each other (including label-based column
// matching and whole-file validation rejection).
package gateway

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type transferFixture struct {
	pool *pgxpool.Pool
	srv  *httptest.Server

	appID, modelID, revID string
	deptsDimID            string
	deptAID, deptBID      string
	amountMetricID        string
	gridID                string
	formID                string

	exporterSub, restrictedSub string
}

func setupTransferFixture(t *testing.T) *transferFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

	f := &transferFixture{pool: pool, exporterSub: "test-exporter", restrictedSub: "test-restricted"}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('T', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	f.appID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, wsID, custID)
	f.modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, f.appID)
	f.revID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, f.modelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, f.revID, f.modelID)

	f.deptsDimID = q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'departments') RETURNING id::text`, f.modelID, f.revID)
	f.deptAID = q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'DEPT_A', 'Dept A') RETURNING id::text`, f.deptsDimID)
	f.deptBID = q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'DEPT_B', 'Dept B') RETURNING id::text`, f.deptsDimID)

	f.amountMetricID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format) VALUES ($1::uuid, $2::uuid, 'amount', true, 'currency') RETURNING id::text`, f.modelID, f.revID)

	f.gridID = q(`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Ops Grid', $2::uuid) RETURNING id::text`, f.modelID, f.revID)
	exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, f.gridID, f.deptsDimID)
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 0)`, f.gridID, f.amountMetricID)

	exporterID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'exporter@t.com', 'Exporter', $2::uuid) RETURNING id::text`, f.exporterSub, custID)
	restrictedID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'restricted@t.com', 'Restricted', $2::uuid) RETURNING id::text`, f.restrictedSub, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, exporterID, wsID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, restrictedID, wsID)
	exec(`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'hidden')`, restrictedID, f.deptBID)

	exec(`INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, $4::jsonb, 100, $5::uuid)`,
		f.modelID, f.revID, f.amountMetricID, fmt.Sprintf(`{"%s":"DEPT_A"}`, f.deptsDimID), exporterID)
	exec(`INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, $4::jsonb, 200, $5::uuid)`,
		f.modelID, f.revID, f.amountMetricID, fmt.Sprintf(`{"%s":"DEPT_B"}`, f.deptsDimID), exporterID)

	form, err := crudapp.NewStore(pool).CreateForm(ctx, f.modelID, f.revID, "timesheet", "Timesheet", []crudapp.FormField{
		{Name: "employee_name", Label: "Employee Name", Type: "text", Required: true},
		{Name: "hours", Label: "Hours", Type: "number"},
	})
	if err != nil {
		t.Fatalf("create form: %v", err)
	}
	f.formID = form.ID
	if _, err := crudapp.NewStore(pool).CreateRecord(ctx, f.formID, exporterID, map[string]any{"employee_name": "Alice", "hours": 8.0}); err != nil {
		t.Fatalf("create record: %v", err)
	}

	t.Setenv("DEV_MODE", "true")
	f.srv = httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(f.srv.Close)

	return f
}

func (f *transferFixture) request(t *testing.T, method, path, persona string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", persona)
	req.Header.Set("X-App-Id", f.appID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func (f *transferFixture) jsonRequest(t *testing.T, method, path, persona string, body any) (int, map[string]any) {
	t.Helper()
	resp := f.request(t, method, path, persona, body)
	defer resp.Body.Close() //nolint:errcheck
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

func readCSV(t *testing.T, resp *http.Response) [][]string {
	t.Helper()
	defer resp.Body.Close() //nolint:errcheck
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	recs, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v (body: %s)", err, b)
	}
	return recs
}

// ── grid export ─────────────────────────────────────────────────────────────

func TestGridExport_FiltersHiddenMembers(t *testing.T) {
	f := setupTransferFixture(t)

	resp := f.request(t, "GET", fmt.Sprintf("/api/grid/export?grid_def_id=%s&revision_id=%s", f.gridID, f.revID), f.exporterSub, nil)
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exporter export status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/csv" {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	rows := readCSV(t, resp)
	if len(rows) != 3 { // header + 2 data rows
		t.Fatalf("exporter rows = %v, want header + 2 data rows", rows)
	}
	if rows[0][0] != "departments" || rows[0][1] != "amount" {
		t.Errorf("header = %v, want [departments amount]", rows[0])
	}
	want := map[string]string{"DEPT_A": "100", "DEPT_B": "200"}
	got := map[string]string{}
	for _, r := range rows[1:] {
		got[r[0]] = r[1]
	}
	if got["DEPT_A"] != want["DEPT_A"] || got["DEPT_B"] != want["DEPT_B"] {
		t.Errorf("exporter data rows = %v, want %v", got, want)
	}

	// "restricted" has DEPT_B hidden — export must never surface it, mirroring
	// what /api/grid itself would hide from this user.
	resp2 := f.request(t, "GET", fmt.Sprintf("/api/grid/export?grid_def_id=%s&revision_id=%s", f.gridID, f.revID), f.restrictedSub, nil)
	defer resp2.Body.Close() //nolint:errcheck
	rows2 := readCSV(t, resp2)
	if len(rows2) != 2 { // header + 1 data row
		t.Fatalf("restricted rows = %v, want header + 1 data row", rows2)
	}
	if rows2[1][0] != "DEPT_A" {
		t.Errorf("restricted's only visible row = %v, want DEPT_A", rows2[1])
	}
}

// TestGridExport_FiltersHiddenMetric is a regression test for
// hiddenMemberFilter used to only ever load rule_type='dimension_member'
// rows, ignoring rule_type='metric' entirely — a hidden metric's whole
// column, header and all, was exported unfiltered.
func TestGridExport_FiltersHiddenMetric(t *testing.T) {
	f := setupTransferFixture(t)
	ctx := context.Background()

	var headcountID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'headcount', true) RETURNING id::text`,
		f.modelID, f.revID,
	).Scan(&headcountID); err != nil {
		t.Fatalf("seed headcount metric: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 1)`,
		f.gridID, headcountID,
	); err != nil {
		t.Fatalf("attach headcount to grid: %v", err)
	}
	var exporterID, restrictedID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM identity.user WHERE keycloak_sub=$1`, f.exporterSub).Scan(&exporterID); err != nil {
		t.Fatalf("find exporter user: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM identity.user WHERE keycloak_sub=$1`, f.restrictedSub).Scan(&restrictedID); err != nil {
		t.Fatalf("find restricted user: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by)
		VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, $4::jsonb, 5, $5::uuid)
	`, f.modelID, f.revID, headcountID, fmt.Sprintf(`{"%s":"DEPT_A"}`, f.deptsDimID), exporterID); err != nil {
		t.Fatalf("seed headcount fact: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'metric', $2, 'hidden')`,
		restrictedID, headcountID,
	); err != nil {
		t.Fatalf("seed hidden metric rule: %v", err)
	}

	// The exporter (no restriction) sees both metric columns.
	resp := f.request(t, "GET", fmt.Sprintf("/api/grid/export?grid_def_id=%s&revision_id=%s", f.gridID, f.revID), f.exporterSub, nil)
	defer resp.Body.Close() //nolint:errcheck
	rows := readCSV(t, resp)
	if len(rows[0]) != 3 || rows[0][2] != "headcount" {
		t.Fatalf("exporter header = %v, want departments/amount/headcount", rows[0])
	}

	// "restricted" has headcount hidden — its whole column must be absent,
	// not just its values.
	resp2 := f.request(t, "GET", fmt.Sprintf("/api/grid/export?grid_def_id=%s&revision_id=%s", f.gridID, f.revID), f.restrictedSub, nil)
	defer resp2.Body.Close() //nolint:errcheck
	rows2 := readCSV(t, resp2)
	for _, col := range rows2[0] {
		if col == "headcount" {
			t.Errorf("restricted export header = %v, still contains the hidden metric's column", rows2[0])
		}
	}
	if len(rows2[0]) != 2 {
		t.Errorf("restricted export header = %v, want 2 columns (departments, amount)", rows2[0])
	}
}

func TestGridExport_XLSXParsesAndRoundTripsThroughImport(t *testing.T) {
	f := setupTransferFixture(t)

	resp := f.request(t, "GET", fmt.Sprintf("/api/grid/export?grid_def_id=%s&revision_id=%s&format=xlsx", f.gridID, f.revID), f.exporterSub, nil)
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("xlsx export status = %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	wb, err := excelize.OpenReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer wb.Close() //nolint:errcheck
	rows, err := wb.GetRows(wb.GetSheetList()[0])
	if err != nil {
		t.Fatalf("get rows: %v", err)
	}
	if len(rows) != 3 || rows[0][0] != "departments" || rows[0][1] != "amount" {
		t.Fatalf("xlsx rows = %v", rows)
	}

	// Round-trip: export CSV, bump DEPT_A's value, re-import as "replace" —
	// the same file shape /api/import/upload already accepts (name-based
	// columns), proving the two endpoints speak a compatible format.
	csvResp := f.request(t, "GET", fmt.Sprintf("/api/grid/export?grid_def_id=%s&revision_id=%s", f.gridID, f.revID), f.exporterSub, nil)
	defer csvResp.Body.Close() //nolint:errcheck
	csvRows := readCSV(t, csvResp)
	var out bytes.Buffer
	cw := csv.NewWriter(&out)
	_ = cw.Write(csvRows[0])
	for _, r := range csvRows[1:] {
		if r[0] == "DEPT_A" {
			r = []string{"DEPT_A", "150"}
		}
		_ = cw.Write(r)
	}
	cw.Flush()

	status, body := f.jsonRequest(t, "POST", "/api/import/upload", f.exporterSub, map[string]any{
		"csv": out.String(), "revision_id": f.revID, "import_mode": "replace",
	})
	if status != http.StatusOK {
		t.Fatalf("import status = %d, body = %v", status, body)
	}

	_, gridBody := f.jsonRequest(t, "GET", fmt.Sprintf("/api/grid?grid_def_id=%s&revision_id=%s", f.gridID, f.revID), f.exporterSub, nil)
	cells, _ := gridBody["cells"].(map[string]any)
	cellKey := f.amountMetricID + ":DEPT_A"
	if v, ok := cells[cellKey].(float64); !ok || v != 150 {
		t.Errorf("post-import DEPT_A cell (%s) = %v, want 150 (re-imported value)", cellKey, cells[cellKey])
	}
	// DEPT_B was untouched by the re-import — the grid total must reflect
	// both cells (150 + 200), not just the one that changed.
	totals, _ := gridBody["totals"].(map[string]any)
	if v, ok := totals[f.amountMetricID].(float64); !ok || v != 350 {
		t.Errorf("post-import total = %v, want 350 (150 + unchanged DEPT_B 200)", totals[f.amountMetricID])
	}
}

// ── form export/import ──────────────────────────────────────────────────────

func TestFormExport_CSV(t *testing.T) {
	f := setupTransferFixture(t)

	resp := f.request(t, "GET", fmt.Sprintf("/api/forms/%s/export", f.formID), f.exporterSub, nil)
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d", resp.StatusCode)
	}
	rows := readCSV(t, resp)
	if len(rows) != 2 { // header + 1 record
		t.Fatalf("rows = %v, want header + 1 record", rows)
	}
	header := rows[0]
	col := func(name string) int {
		for i, h := range header {
			if h == name {
				return i
			}
		}
		t.Fatalf("column %q not found in header %v", name, header)
		return -1
	}
	if rows[1][col("employee_name")] != "Alice" || rows[1][col("hours")] != "8" {
		t.Errorf("record row = %v", rows[1])
	}
}

// TestFormRecords_FiltersHiddenDimensionMember is a regression test for a
// real gap: crudapp.Store.ListRecords took no access parameter at all, so
// GET /api/forms/{id}/records and .../export returned every record
// verbatim — the read-side counterpart to applyFormMappings' write-side
// check (fixed earlier this session). A form with a "dimension"-type field
// bound to the departments dimension; one record touches DEPT_A (visible),
// one touches DEPT_B (hidden for the restricted user) — the restricted
// user must see only the DEPT_A record via both endpoints.
func TestFormRecords_FiltersHiddenDimensionMember(t *testing.T) {
	f := setupTransferFixture(t)
	ctx := context.Background()

	form, err := crudapp.NewStore(f.pool).CreateForm(ctx, f.modelID, f.revID, "expense", "Expense", []crudapp.FormField{
		{Name: "department", Label: "Department", Type: "dimension", DimensionID: f.deptsDimID},
		{Name: "amount", Label: "Amount", Type: "number"},
	})
	if err != nil {
		t.Fatalf("create form: %v", err)
	}
	var exporterID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM identity.user WHERE keycloak_sub=$1`, f.exporterSub).Scan(&exporterID); err != nil {
		t.Fatalf("find exporter user: %v", err)
	}
	if _, err := crudapp.NewStore(f.pool).CreateRecord(ctx, form.ID, exporterID, map[string]any{"department": "DEPT_A", "amount": 10.0}); err != nil {
		t.Fatalf("create DEPT_A record: %v", err)
	}
	if _, err := crudapp.NewStore(f.pool).CreateRecord(ctx, form.ID, exporterID, map[string]any{"department": "DEPT_B", "amount": 20.0}); err != nil {
		t.Fatalf("create DEPT_B record: %v", err)
	}

	// Unrestricted user sees both records.
	status, body := f.jsonRequest(t, "GET", fmt.Sprintf("/api/forms/%s/records", form.ID), f.exporterSub, nil)
	if status != http.StatusOK {
		t.Fatalf("exporter GET records: status=%d, body=%v", status, body)
	}

	// "restricted" has DEPT_B hidden — only the DEPT_A record must be
	// visible via records listing...
	resp := f.request(t, "GET", fmt.Sprintf("/api/forms/%s/records", form.ID), f.restrictedSub, nil)
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restricted GET records: status=%d", resp.StatusCode)
	}
	var records []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&records); err != nil {
		t.Fatalf("decode records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("restricted records = %d, want 1 (DEPT_B record excluded)", len(records))
	}
	if data, _ := records[0]["data"].(map[string]any); data["department"] != "DEPT_A" {
		t.Errorf("restricted's only visible record = %v, want department=DEPT_A", data)
	}

	// ...and via export.
	exportResp := f.request(t, "GET", fmt.Sprintf("/api/forms/%s/export", form.ID), f.restrictedSub, nil)
	defer exportResp.Body.Close() //nolint:errcheck
	rows := readCSV(t, exportResp)
	if len(rows) != 2 { // header + 1 record
		t.Fatalf("restricted export rows = %v, want header + 1 record", rows)
	}
}

func TestFormImport_RoundTripAndValidation(t *testing.T) {
	f := setupTransferFixture(t)

	// Import matching by LABEL ("Employee Name"/"Hours"), not field name —
	// exercises the case-insensitive name-or-label header match.
	csvBody := "Employee Name,Hours\nBob,5\n"
	status, body := f.jsonRequest(t, "POST", fmt.Sprintf("/api/forms/%s/import", f.formID), f.exporterSub, map[string]any{"csv": csvBody})
	if status != http.StatusOK {
		t.Fatalf("import status = %d, body = %v", status, body)
	}
	if body["records_created"].(float64) != 1 {
		t.Fatalf("records_created = %v, want 1", body["records_created"])
	}

	_, recBody := f.jsonRequest(t, "GET", fmt.Sprintf("/api/forms/%s/records", f.formID), f.exporterSub, nil)
	// jsonRequest expects a map; records is a top-level array, so re-fetch raw.
	resp := f.request(t, "GET", fmt.Sprintf("/api/forms/%s/records", f.formID), f.exporterSub, nil)
	defer resp.Body.Close() //nolint:errcheck
	var records []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&records); err != nil {
		t.Fatalf("decode records: %v (recBody=%v)", err, recBody)
	}
	if len(records) != 2 { // Alice (fixture) + Bob (import)
		t.Fatalf("records = %v, want 2", records)
	}
	var bob map[string]any
	for _, r := range records {
		if data, _ := r["data"].(map[string]any); data["employee_name"] == "Bob" {
			bob = data
		}
	}
	if bob == nil {
		t.Fatal("Bob record not found")
	}
	if bob["hours"] != 5.0 {
		t.Errorf("Bob's hours = %v, want 5", bob["hours"])
	}

	// Whole-file validation: a row missing the required "employee_name" must
	// reject the entire file, creating nothing.
	badCSV := "Employee Name,Hours\n,3\n"
	status2, body2 := f.jsonRequest(t, "POST", fmt.Sprintf("/api/forms/%s/import", f.formID), f.exporterSub, map[string]any{"csv": badCSV})
	if status2 != http.StatusUnprocessableEntity {
		t.Fatalf("bad import status = %d, body = %v", status2, body2)
	}
	if n, _ := body2["error_rows"].(float64); n != 1 {
		t.Errorf("error_rows = %v, want 1", body2["error_rows"])
	}

	resp2 := f.request(t, "GET", fmt.Sprintf("/api/forms/%s/records", f.formID), f.exporterSub, nil)
	defer resp2.Body.Close() //nolint:errcheck
	var records2 []map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&records2)
	if len(records2) != 2 {
		t.Errorf("records after rejected import = %d, want still 2", len(records2))
	}
}
