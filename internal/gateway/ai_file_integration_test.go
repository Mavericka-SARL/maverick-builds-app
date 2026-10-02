package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// The AI Developer imports a spreadsheet attached to the chat and defines a
// data export, end to end through the real chat, confirm and download
// endpoints: the whole file (not the LLM's text sample) lands in the
// session's draft through the Import Wizard's pipeline, the saved
// integration records the run, a file with one bad row imports nothing, and
// the export a business user downloads after the draft is promoted leaves
// out the member hidden from them while showing calculated values.
// salesFileFixture is a small sales model — geography (AMER: CA, US; EMEA:
// UK) × period (Q1, Q2), input revenue and cost, calculated margin, one
// "Sales" grid — with a developer and a business user US is hidden from.
type salesFileFixture struct {
	ctx                                       context.Context
	pool                                      *pgxpool.Pool
	appID, modelID, revID, grid, devID, bizID string
	devSub, bizSub                            string
}

func newSalesFileFixture(t *testing.T) *salesFileFixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")

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
	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('FileCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Sales', 'planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Sales model') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID)

	geo := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type) VALUES ($1::uuid,$2::uuid,'geography','standard') RETURNING id::text`, modelID, revID)
	member := func(dim, code, label, parent string, sort int) string {
		return q(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, sort_order)
			VALUES ($1::uuid,$2,$3,NULLIF($4,'')::uuid,$5) RETURNING id::text`, dim, code, label, parent, sort)
	}
	amer := member(geo, "AMER", "Americas", "", 1)
	member(geo, "CA", "Canada", amer, 2)
	usID := member(geo, "US", "United States", amer, 3)
	emea := member(geo, "EMEA", "EMEA", "", 4)
	member(geo, "UK", "United Kingdom", emea, 5)
	period := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type) VALUES ($1::uuid,$2::uuid,'period','standard') RETURNING id::text`, modelID, revID)
	member(period, "Q1", "Quarter 1", "", 1)
	member(period, "Q2", "Quarter 2", "", 2)
	revenue := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid,$2::uuid,'revenue',true,'sum') RETURNING id::text`, modelID, revID)
	cost := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid,$2::uuid,'cost',true,'sum') RETURNING id::text`, modelID, revID)
	margin := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula, agg_rule) VALUES ($1::uuid,$2::uuid,'margin',false,'revenue - cost','sum') RETURNING id::text`, modelID, revID)
	// The edges the developer's metric endpoint stores for a formula.
	for _, dep := range []string{revenue, cost} {
		exec(`INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid,$2::uuid)`, margin, dep)
	}
	grid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'Sales') RETURNING id::text`, modelID, revID)
	for _, d := range []string{geo, period} {
		exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid,$2::uuid)`, grid, d)
	}
	for i, m := range []string{revenue, cost, margin} {
		exec(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid,$2::uuid,$3)`, grid, m, i)
	}

	user := func(sub, email, role string) string {
		id := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1,$2,$3,$4::uuid) RETURNING id::text`, sub, email, sub, custID)
		exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,$2,$3::uuid)`, id, role, wsID)
		return id
	}
	devSub, bizSub := "file-dev", "file-biz"
	devID := user(devSub, "dev@file.co", "developer")
	bizID := user(bizSub, "biz@file.co", "business_user")
	exec(`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid,'dimension_member',$2::uuid,'hidden')`, bizID, usID)

	return &salesFileFixture{ctx: ctx, pool: pool, appID: appID, modelID: modelID, revID: revID, grid: grid,
		devID: devID, bizID: bizID, devSub: devSub, bizSub: bizSub}
}

func (f *salesFileFixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// serve starts the gateway on the fixture's database and returns a raw and
// a JSON request helper, as persona, scoped to the fixture's application.
func (f *salesFileFixture) serve(t *testing.T, provider providers.Provider) (
	send func(persona, method, path, contentType string, body []byte) (int, []byte),
	do func(persona, method, path string, body any) (int, []byte),
) {
	t.Helper()
	ctx, pool, appID := f.ctx, f.pool, f.appID
	t.Setenv("DEV_MODE", "true")
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true, testProvider: provider}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)

	send = func(persona, method, path, contentType string, body []byte) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Dev-User", persona)
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", contentType)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.Bytes()
	}
	do = func(persona, method, path string, body any) (int, []byte) {
		t.Helper()
		var buf []byte
		if body != nil {
			buf, _ = json.Marshal(body)
		}
		return send(persona, method, path, "application/json", buf)
	}
	return send, do
}

func TestAIDeveloperImportsAttachedFileAndDefinesAnExport(t *testing.T) {
	f := newSalesFileFixture(t)
	ctx, pool, modelID, revID, appID := f.ctx, f.pool, f.modelID, f.revID, f.appID
	devSub, bizSub, devID, bizID := f.devSub, f.bizSub, f.devID, f.bizID
	count := func(sql string, args ...any) int { t.Helper(); return f.count(t, sql, args...) }

	// The attachment: a formatted workbook whose data sheet is not the
	// first, with a column to ignore and a value shown as "1,234.50".
	columnMap := map[string]string{"Country": "geography", "Quarter": "period", "Revenue USD": "revenue", "Cost": "cost", "Notes": "ignore"}
	xlsx := func(rows [][]any) []byte {
		f := excelize.NewFile()
		defer func() { _ = f.Close() }()
		_, _ = f.NewSheet("Actuals")
		_ = f.SetSheetRow("Sheet1", "A1", &[]any{"cover sheet"})
		_ = f.SetSheetRow("Actuals", "A1", &[]any{"Country", "Quarter", "Revenue USD", "Cost", "Notes"})
		for i, r := range rows {
			_ = f.SetSheetRow("Actuals", "A"+string(rune('2'+i)), &r)
		}
		style, _ := f.NewStyle(&excelize.Style{NumFmt: 4})
		_ = f.SetCellStyle("Actuals", "C2", "D9", style)
		var buf bytes.Buffer
		if err := f.Write(&buf); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	good := xlsx([][]any{
		{"CA", "Q1", 1234.5, 200, "first"},
		{"US", "Q1", 1000, 400, ""},
		{"UK", "Q2", 300, 100, "late"},
	})

	tool := func(name string, args any) providers.ChatResponse {
		b, _ := json.Marshal(args)
		return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: "call_" + name, Name: name, Arguments: b}}}}
	}
	reply := providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Here is the plan."}}
	importArgs := map[string]any{"file": "actuals.xlsx", "sheet": "Actuals", "target_type": "grid", "target_id": "Sales", "column_map": columnMap}
	spec := map[string]any{
		"format": "csv", "layout": "pivot", "pivot_dimension": "period", "metrics": []string{"revenue", "margin"},
		"member_display": "label", "delimiter": ";", "decimal_separator": ",", "decimals": 1,
		"column_names": map[string]string{"geography": "Country"},
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		// Turn 1: dry-run the attachment, then propose saving + importing.
		tool("preview_file_import", importArgs),
		tool("propose_actions", map[string]any{"steps": []map[string]any{
			proposeStep("create_file_integration", "Save 'Monthly actuals' (Excel into Sales)", map[string]any{
				"name": "Monthly actuals", "target_type": "grid", "target_id": "Sales", "column_map": columnMap}),
			proposeStep("import_file_data", "Import actuals.xlsx", map[string]any{
				"file": "actuals.xlsx", "sheet": "Actuals", "integration_id": "<created in step 1>"}),
		}}),
		// Turn 2: a file with an unknown member — the import must fail whole.
		tool("propose_actions", map[string]any{"steps": []map[string]any{
			proposeStep("import_file_data", "Import bad.csv", map[string]any{
				"file": "bad.csv", "target_type": "grid", "target_id": "Sales", "column_map": columnMap}),
		}}),
		// Turn 3: preview an export, then propose it.
		tool("preview_export", map[string]any{"grid_id": "Sales", "name": "Sales to ERP", "spec": spec}),
		tool("propose_actions", map[string]any{"steps": []map[string]any{
			proposeStep("create_export_integration", "Create export 'Sales to ERP'", map[string]any{
				"name": "Sales to ERP", "grid_id": "Sales", "spec": spec}),
		}}),
		reply,
	}}

	send, do := f.serve(t, fake)
	attach := func(sessionID, filename string, data []byte) map[string]any {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", filename)
		_, _ = fw.Write(data)
		_ = mw.Close()
		status, out := send(devSub, "POST", "/api/ai/sessions/"+sessionID+"/documents", mw.FormDataContentType(), body.Bytes())
		if status != http.StatusOK {
			t.Fatalf("attach %s: %d %s", filename, status, out)
		}
		var doc map[string]any
		_ = json.Unmarshal(out, &doc)
		return doc
	}

	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, appID, devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled")
	pStore := aiassistant.NewProposalStore(pool)
	lastToolResult := func(name string) string {
		t.Helper()
		msgs, err := chatStore.ListMessages(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "tool" && msgs[i].ToolCallID == "call_"+name { // ListMessages loads no tool_name
				return msgs[i].Content
			}
		}
		t.Fatalf("no %s tool result", name)
		return ""
	}
	confirmLatest := func() aiassistant.Proposal {
		t.Helper()
		proposals, err := pStore.ListPendingProposals(ctx, sess.ID)
		if err != nil || len(proposals) != 1 {
			t.Fatalf("pending proposals: %v %+v", err, proposals)
		}
		if status, body := do(devSub, "POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+proposals[0].ID+"/confirm", nil); status != http.StatusOK {
			t.Fatalf("confirm: %d %s", status, body)
		}
		p, err := pStore.GetProposal(ctx, proposals[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	facts := func(rev string) int {
		return count(`SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id=$2::uuid`, modelID, rev)
	}

	// ── 1. Attach, preview, confirm the import ───────────────────────────
	doc := attach(sess.ID, "actuals.xlsx", good)
	if doc["importable"] != true {
		t.Fatalf("an attached workbook must be kept whole: %v", doc)
	}
	if status, body := do(devSub, "POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "import the attached actuals"}); status != http.StatusOK {
		t.Fatalf("message: %d %s", status, body)
	}
	preview := lastToolResult("preview_file_import")
	for _, want := range []string{"3 data row(s)", "Workbook sheets: Sheet1, Actuals", "No errors. Would import 6 value(s): cost (3), revenue (3)"} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview_file_import result lacks %q:\n%s", want, preview)
		}
	}
	if n := facts(revID); n != 0 {
		t.Fatalf("a preview wrote %d fact(s)", n)
	}
	p := confirmLatest()
	for _, s := range p.Steps {
		if s.Status != "success" {
			t.Fatalf("step %s: %s — %s", s.Tool, s.Status, s.Result)
		}
	}
	sess, _ = chatStore.GetSession(ctx, sess.ID)
	draft := sess.DraftRevisionID
	if draft == "" || draft == revID {
		t.Fatalf("the import must land in a draft revision, got %q", draft)
	}
	if n := facts(draft); n != 6 {
		t.Errorf("draft facts = %d, want 6", n)
	}
	if n := facts(revID); n != 0 {
		t.Errorf("the active revision gained %d fact(s); AI writes stay in the draft", n)
	}
	var caRevenue float64
	if err := pool.QueryRow(ctx, `
		SELECT fi.value::float8 FROM runtime.fact_input fi JOIN model.metric_def m ON m.id = fi.metric_id
		WHERE fi.revision_id=$1::uuid AND m.name='revenue' AND fi.dim_members->>(SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name='geography') = 'CA'`,
		draft).Scan(&caRevenue); err != nil || caRevenue != 1234.5 {
		t.Errorf("CA revenue = %v (err %v), want the stored 1234.5, not the displayed \"1,234.50\"", caRevenue, err)
	}
	intID := p.Steps[0].CreatedID
	if n := count(`SELECT count(*) FROM model.integration_def WHERE id=$1::uuid AND type='csv_import' AND revision_id=$2::uuid AND config->>'import_mode'='replace'`, intID, draft); n != 1 {
		t.Errorf("saved integration not found as a csv_import in the draft with mode replace")
	}
	if n := count(`SELECT count(*) FROM model.integration_run WHERE integration_id=$1::uuid AND status='success' AND rows_imported=6`, intID); n != 1 {
		t.Errorf("the import was not recorded in the integration's run history")
	}
	if n := count(`SELECT count(*) FROM audit.audit_event WHERE event_type='import.uploaded' AND metadata->>'source'='ai_assistant'`); n != 1 {
		t.Errorf("audit rows for the AI import = %d, want 1", n)
	}

	// ── 2. A file with an unknown member imports nothing ─────────────────
	attach(sess.ID, "bad.csv", []byte("Country,Quarter,Revenue USD,Cost,Notes\nCA,Q2,10,1,\nFR,Q2,20,2,\n"))
	if status, body := do(devSub, "POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "import bad.csv"}); status != http.StatusOK {
		t.Fatalf("message: %d %s", status, body)
	}
	p = confirmLatest()
	if p.Steps[0].Status != "failed" || !strings.Contains(p.Steps[0].Result, `"FR" is not a member of dimension "geography"`) {
		t.Errorf("bad file step = %s: %s", p.Steps[0].Status, p.Steps[0].Result)
	}
	if n := facts(draft); n != 6 {
		t.Errorf("draft facts after a rejected file = %d, want 6 (all-or-nothing)", n)
	}

	// ── 3. Preview and create an export; download it ────────────────────
	if status, body := do(devSub, "POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "export sales for the ERP"}); status != http.StatusOK {
		t.Fatalf("message: %d %s", status, body)
	}
	if got := lastToolResult("preview_export"); !strings.Contains(got, "Columns: Country | metric | Quarter 1 | Quarter 2") {
		t.Errorf("preview_export result:\n%s", got)
	}
	p = confirmLatest()
	if p.Steps[0].Status != "success" {
		t.Fatalf("create export: %s", p.Steps[0].Result)
	}
	exportID := p.Steps[0].CreatedID

	// Calculated values are written by the scheduler after the import.
	// A calculated metric has a value at every leaf combination — margin
	// is 0 - 0 where nothing was entered — exactly as the grid shows it.
	wantDev := "Country;metric;Quarter 1;Quarter 2\n" +
		"Canada;revenue;1234,5;\nCanada;margin;1034,5;0,0\n" +
		"United States;revenue;1000,0;\nUnited States;margin;600,0;0,0\n" +
		"United Kingdom;revenue;;300,0\nUnited Kingdom;margin;0,0;200,0\n"
	var got string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		status, body := do(devSub, "GET", "/api/integrations/"+exportID+"/export", nil)
		if status != http.StatusOK {
			t.Fatalf("download: %d %s", status, body)
		}
		if got = string(body); got == wantDev {
			break
		}
	}
	if got != wantDev {
		t.Errorf("developer's export:\n%s\nwant:\n%s", got, wantDev)
	}
	if status, body := do(devSub, "POST", "/api/integrations/"+exportID+"/run", nil); status != http.StatusBadRequest || !strings.Contains(string(body), "is an export") {
		t.Errorf("running an export: %d %s", status, body)
	}

	// ── 4. Promoted, a business user's download leaves out what they cannot see
	if status, body := do(devSub, "POST", "/api/ai/sessions/"+sess.ID+"/promote-draft", nil); status != http.StatusOK {
		t.Fatalf("promote: %d %s", status, body)
	}
	wantBiz := "Country;metric;Quarter 1;Quarter 2\n" +
		"Canada;revenue;1234,5;\nCanada;margin;1034,5;0,0\n" +
		"United Kingdom;revenue;;300,0\nUnited Kingdom;margin;0,0;200,0\n"
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		status, body := do(bizSub, "GET", "/api/integrations/"+exportID+"/export", nil)
		if status != http.StatusOK {
			t.Fatalf("business download: %d %s", status, body)
		}
		if got = string(body); got == wantBiz {
			break
		}
	}
	if got != wantBiz {
		t.Errorf("business user's export (US hidden):\n%s\nwant:\n%s", got, wantBiz)
	}
	if n := count(`SELECT count(*) FROM model.integration_run WHERE integration_id=$1::uuid AND run_by=$2::uuid`, exportID, bizID); n == 0 {
		t.Error("the business user's download is not in the export's history")
	}
}

// The developer role reaches every export capability the AI Developer has,
// through the console's own endpoints: a spec is validated with every
// problem listed, previewed without saving, saved, re-specified, drafted
// and deleted; a business user downloads it scoped to what they may see and
// cannot reach the developer's preview.
func TestDeveloperBuildsAndDownloadsExportsInTheConsole(t *testing.T) {
	f := newSalesFileFixture(t)
	_, do := f.serve(t, nil)
	dev, biz := f.devSub, f.bizSub

	// Data entered the developer's way: the import endpoint.
	status, body := do(dev, "POST", "/api/import/upload", map[string]any{
		"revision_id": f.revID,
		"csv":         "geography,period,revenue,cost\nCA,Q1,100,40\nUS,Q1,50,10\nUK,Q2,30,5\n",
	})
	if status != http.StatusOK {
		t.Fatalf("import: %d %s", status, body)
	}

	path := "/api/developer/integrations?revision_id=" + f.revID
	status, body = do(dev, "POST", path, map[string]any{
		"type": "file_export", "name": "Bad", "target_id": f.grid,
		"config": map[string]any{"metrics": []string{"revenu"}, "dimensions": []string{"geography"}},
	})
	if status != http.StatusBadRequest || !strings.Contains(string(body), `metric \"revenu\" is not in grid`) ||
		!strings.Contains(string(body), `dimension \"period\" is not a column`) {
		t.Fatalf("an invalid spec must be refused with every problem: %d %s", status, body)
	}

	spec := map[string]any{"format": "json", "layout": "long", "metrics": []string{"revenue", "margin"},
		"filters": map[string][]string{"geography": {"AMER"}}, "member_display": "code_and_label"}
	status, body = do(dev, "POST", "/api/developer/integrations/export-preview", map[string]any{"target_id": f.grid, "config": spec})
	if status != http.StatusOK {
		t.Fatalf("preview: %d %s", status, body)
	}
	var preview struct {
		Header        []string   `json:"header"`
		DefaultHeader []string   `json:"default_header"`
		Rows          [][]string `json:"rows"`
		TotalRows     int        `json:"total_rows"`
		FileName      string     `json:"file_name"`
	}
	_ = json.Unmarshal(body, &preview)
	if strings.Join(preview.Header, ",") != "geography,geography label,period,period label,metric,value" || preview.FileName != "Sales.json" {
		t.Errorf("preview header %v file %q", preview.Header, preview.FileName)
	}
	if n := f.count(t, `SELECT count(*) FROM model.integration_def WHERE type='file_export'`); n != 0 {
		t.Fatalf("a preview saved %d export(s)", n)
	}
	if status, _ := do(biz, "POST", "/api/developer/integrations/export-preview", map[string]any{"target_id": f.grid, "config": spec}); status != http.StatusForbidden {
		t.Errorf("a business user reached the developer's preview: %d", status)
	}

	status, body = do(dev, "POST", path, map[string]any{"type": "file_export", "name": "Americas revenue", "target_id": f.grid, "config": spec})
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	var created struct{ ID string }
	_ = json.Unmarshal(body, &created)
	id := created.ID

	// Margin is calculated after the import; wait for the scheduler.
	// margin = revenue - cost, 0 at a combination nothing was entered for.
	want := `[
  {"geography": "CA", "geography label": "Canada", "period": "Q1", "period label": "Quarter 1", "metric": "revenue", "value": 100},
  {"geography": "CA", "geography label": "Canada", "period": "Q1", "period label": "Quarter 1", "metric": "margin", "value": 60},
  {"geography": "CA", "geography label": "Canada", "period": "Q2", "period label": "Quarter 2", "metric": "margin", "value": 0},
  {"geography": "US", "geography label": "United States", "period": "Q1", "period label": "Quarter 1", "metric": "revenue", "value": 50},
  {"geography": "US", "geography label": "United States", "period": "Q1", "period label": "Quarter 1", "metric": "margin", "value": 40},
  {"geography": "US", "geography label": "United States", "period": "Q2", "period label": "Quarter 2", "metric": "margin", "value": 0}
]
`
	var got string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		status, body = do(dev, "GET", "/api/integrations/"+id+"/export", nil)
		if status != http.StatusOK {
			t.Fatalf("download: %d %s", status, body)
		}
		if got = string(body); got == want {
			break
		}
	}
	if got != want {
		t.Errorf("developer download:\n%s\nwant:\n%s", got, want)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("download is not JSON: %v", err)
	}
	for _, r := range rows {
		if r["geography"] == "UK" {
			t.Errorf("the AMER filter let UK through: %v", r)
		}
	}

	// The business user cannot see US.
	status, body = do(biz, "GET", "/api/integrations/"+id+"/export", nil)
	if status != http.StatusOK || strings.Contains(string(body), `"US"`) || !strings.Contains(string(body), `"CA"`) {
		t.Errorf("business download: %d %s", status, body)
	}

	// Re-specify: an invalid spec is refused, a valid one saved.
	if status, body := do(dev, "PATCH", "/api/developer/integrations/"+id+"/config", map[string]any{"config": map[string]any{"layout": "pivot"}}); status != http.StatusBadRequest {
		t.Errorf("an invalid config patch: %d %s", status, body)
	}
	if status, body := do(dev, "PATCH", "/api/developer/integrations/"+id+"/config", map[string]any{"config": map[string]any{"format": "xlsx", "sheet_name": "Sales"}}); status != http.StatusOK {
		t.Fatalf("config patch: %d %s", status, body)
	}
	status, body = do(dev, "GET", "/api/integrations/"+id+"/export", nil)
	if status != http.StatusOK || !bytes.HasPrefix(body, []byte("PK")) {
		t.Errorf("xlsx download: %d, %d bytes", status, len(body))
	} else if wb, err := excelize.OpenReader(bytes.NewReader(body)); err != nil {
		t.Errorf("xlsx: %v", err)
	} else {
		if v, _ := wb.GetCellValue("Sales", "C2"); v == "" {
			t.Errorf("xlsx Sales!C2 is empty")
		}
		_ = wb.Close()
	}

	// A draft is not downloadable; a non-grid target is refused.
	if status, body := do(dev, "PATCH", "/api/developer/integrations/"+id, map[string]any{"name": "Americas revenue", "target_type": "dimension", "target_id": f.grid}); status != http.StatusBadRequest {
		t.Errorf("retarget to a dimension: %d %s", status, body)
	}
	if status, body := do(dev, "PATCH", "/api/developer/integrations/"+id, map[string]any{"name": "Americas revenue", "target_type": "grid", "target_id": f.grid, "status": "draft"}); status != http.StatusOK {
		t.Fatalf("to draft: %d %s", status, body)
	}
	if status, body := do(biz, "GET", "/api/integrations/"+id+"/export", nil); status != http.StatusBadRequest || !strings.Contains(string(body), "draft") {
		t.Errorf("draft download: %d %s", status, body)
	}

	if status, body := do(dev, "DELETE", "/api/developer/integrations/"+id, nil); status != http.StatusOK {
		t.Fatalf("delete: %d %s", status, body)
	}
	if status, _ := do(dev, "GET", "/api/integrations/"+id+"/export", nil); status != http.StatusNotFound {
		t.Errorf("a deleted export downloaded: %d", status)
	}
}
