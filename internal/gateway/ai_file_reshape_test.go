// The AI Developer turns a spreadsheet laid out for people into the rows an
// import reads — end to end through the real chat, confirm, conversion and
// integration-run endpoints. Reported live: asked to "convert it to the
// necessary format", the assistant answered it could not transform the
// workbook, because an import only renamed columns.
package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

func TestAIDeveloperReshapesAPeopleShapedWorkbook(t *testing.T) {
	f := newSalesFileFixture(t)
	ctx, pool := f.ctx, f.pool

	// A plan as finance sends it: a title above the header, countries by
	// LABEL written once per group, quarters across, a total row, amounts
	// in thousands with a negative in brackets.
	workbook := func(rows [][]any) []byte {
		x := excelize.NewFile()
		defer func() { _ = x.Close() }()
		all := append([][]any{{"Plan FY26 (USD thousands)"}, {}, {"Country", "Line", "Quarter 1", "Quarter 2"}}, rows...)
		for i, r := range all {
			cell, _ := excelize.CoordinatesToCellName(1, i+1)
			_ = x.SetSheetRow("Sheet1", cell, &r)
		}
		var buf bytes.Buffer
		if err := x.Write(&buf); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	plan := workbook([][]any{
		{"Canada", "Revenue", 10, 12},
		{"", "Cost", "(3)", 4},
		{"United Kingdom", "Revenue", 5, ""},
		{"Total", "", 15, 16},
	})
	reshape := map[string]any{
		"header_row": 3,
		"fill_down":  []string{"Country"},
		"skip_rows":  []map[string]any{{"column": "Country", "equals": "Total"}},
		"unpivot":    map[string]any{"from": "Quarter 1", "to": "Quarter 2", "name_column": "Quarter", "value_column": "value"},
		"value_map": map[string]any{
			"Country": map[string]string{"Canada": "CA", "United Kingdom": "UK"},
			"Quarter": map[string]string{"Quarter 1": "Q1", "Quarter 2": "Q2"},
		},
		"scale": map[string]float64{"value": 1000},
	}
	columnMap := map[string]string{"Country": "geography", "Quarter": "period", "Line": "metric"}
	args := map[string]any{"file": "plan.xlsx", "target_type": "grid", "target_id": "Sales", "reshape": reshape, "column_map": columnMap}

	tool := func(name string, a any) providers.ChatResponse {
		b, _ := json.Marshal(a)
		return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: "call_" + name, Name: name, Arguments: b}}}}
	}
	reply := providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Done."}}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		tool("preview_file_import", args),
		tool("prepare_converted_file", map[string]any{"file": "plan.xlsx", "reshape": reshape, "column_map": columnMap}),
		reply,
		tool("propose_actions", map[string]any{"steps": []map[string]any{
			proposeStep("create_file_integration", "Save 'Plan upload'", map[string]any{
				"name": "Plan upload", "target_type": "grid", "target_id": "Sales", "reshape": reshape, "column_map": columnMap}),
			proposeStep("import_file_data", "Import plan.xlsx", map[string]any{
				"file": "plan.xlsx", "integration_id": "<created in step 1>"}),
		}}),
	}}
	send, do := f.serve(t, fake)

	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, f.modelID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "plan.xlsx")
	_, _ = fw.Write(plan)
	_ = mw.Close()
	if status, out := send(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/documents", mw.FormDataContentType(), body.Bytes()); status != http.StatusOK {
		t.Fatalf("attach: %d %s", status, out)
	}
	toolResult := func(name string) string {
		t.Helper()
		msgs, _ := chatStore.ListMessages(ctx, sess.ID)
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "tool" && msgs[i].ToolCallID == "call_"+name {
				return msgs[i].Content
			}
		}
		t.Fatalf("no %s result", name)
		return ""
	}

	// ── 1. Preview and convert ────────────────────────────────────────────
	if status, out := do(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "convert it to the necessary format"}); status != http.StatusOK {
		t.Fatalf("message: %d %s", status, out)
	}
	preview := toolResult("preview_file_import")
	for _, want := range []string{
		`1: "Plan FY26 (USD thousands)"`, // the sheet as read
		"After the reshape: 5 row(s)",    // CA ×2 lines ×2 quarters + UK Q1; total and blank cells gone
		"Columns after column_map: geography, metric_id, period, value",
		`"CA", "Revenue", "Q1", "10000"`, // as the import reads it
		"No errors. Would import 5 value(s): cost (2), revenue (3)",
	} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview lacks %q:\n%s", want, preview)
		}
	}
	if converted := toolResult("prepare_converted_file"); !strings.Contains(converted, `Converted file "plan (converted)" is ready: 5 row(s)`) {
		t.Fatalf("prepare_converted_file: %s", converted)
	}

	status, out := do(f.devSub, "GET", "/api/ai/sessions/"+sess.ID+"/conversions", nil)
	var list []aiassistant.Conversion
	if status != http.StatusOK || json.Unmarshal(out, &list) != nil || len(list) != 1 || list[0].RowCount != 5 {
		t.Fatalf("conversions: %d %s", status, out)
	}
	conv := "/api/ai/sessions/" + sess.ID + "/conversions/" + list[0].ID
	status, csvBody := do(f.devSub, "GET", conv+"?format=csv", nil)
	wantCSV := "geography,metric_id,period,value\nCA,Revenue,Q1,10000\nCA,Revenue,Q2,12000\nCA,Cost,Q1,-3000\nCA,Cost,Q2,4000\nUK,Revenue,Q1,5000\n"
	if status != http.StatusOK || string(csvBody) != wantCSV {
		t.Fatalf("CSV download: %d\n%s\nwant\n%s", status, csvBody, wantCSV)
	}
	status, xlsxBody := do(f.devSub, "GET", conv+"?format=xlsx", nil)
	if status != http.StatusOK {
		t.Fatalf("Excel download: %d %s", status, xlsxBody)
	}
	x, err := excelize.OpenReader(bytes.NewReader(xlsxBody))
	if err != nil {
		t.Fatal(err)
	}
	// A number is written untyped (OOXML's default); text is a string cell.
	if typ, _ := x.GetCellType("Data", "D4"); typ == excelize.CellTypeSharedString || typ == excelize.CellTypeInlineString {
		t.Errorf("the value column should be numbers in Excel, D4 is text")
	}
	if typ, _ := x.GetCellType("Data", "A4"); typ != excelize.CellTypeSharedString && typ != excelize.CellTypeInlineString {
		t.Errorf("a member code should stay text in Excel, A4 is %v", typ)
	}
	if v, _ := x.GetCellValue("Data", "D4"); v != "-3000" {
		t.Errorf("D4 = %q, want -3000", v)
	}
	// The developer's own session only.
	other := "file-dev2"
	var wsID string
	_ = pool.QueryRow(ctx, `SELECT workspace_id::text FROM identity.role_assignment WHERE user_id=$1::uuid`, f.devID).Scan(&wsID)
	var otherID string
	_ = pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		SELECT $1, 'dev2@file.co', 'dev2', customer_id FROM identity.user WHERE id=$2::uuid RETURNING id::text`, other, f.devID).Scan(&otherID)
	_, _ = pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,'developer',$2::uuid)`, otherID, wsID)
	if status, _ := do(other, "GET", conv+"?format=csv", nil); status != http.StatusNotFound {
		t.Errorf("another developer downloading this session's conversion: %d, want 404", status)
	}

	// ── 2. Save it as an integration and import ───────────────────────────
	if status, out := do(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "import it, and keep the setup for next month"}); status != http.StatusOK {
		t.Fatalf("message: %d %s", status, out)
	}
	proposals, err := aiassistant.NewProposalStore(pool).ListPendingProposals(ctx, sess.ID)
	if err != nil || len(proposals) != 1 {
		t.Fatalf("pending proposals: %v %+v", err, proposals)
	}
	if status, out := do(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+proposals[0].ID+"/confirm", nil); status != http.StatusOK {
		t.Fatalf("confirm: %d %s", status, out)
	}
	s2, _ := chatStore.GetSession(ctx, sess.ID)
	value := func(geo, period, metric string) (v float64) {
		_ = pool.QueryRow(ctx, `
			SELECT fi.value FROM runtime.fact_input fi JOIN model.metric_def m ON m.id = fi.metric_id
			WHERE fi.revision_id=$1::uuid AND m.name=$2
			  AND fi.dim_members->>(SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name='geography')=$3
			  AND fi.dim_members->>(SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name='period')=$4`,
			s2.DraftRevisionID, metric, geo, period).Scan(&v)
		return v
	}
	if n := f.count(t, `SELECT count(*) FROM runtime.fact_input WHERE revision_id=$1::uuid`, s2.DraftRevisionID); n != 5 {
		t.Fatalf("imported %d value(s) into the draft, want 5", n)
	}
	if v := value("CA", "Q1", "cost"); v != -3000 {
		t.Errorf("CA Q1 cost = %v, want -3000", v)
	}

	// ── 3. Next month's file, uploaded to the saved integration ───────────
	var intID, cfg string
	if err := pool.QueryRow(ctx, `SELECT id::text, config::text FROM model.integration_def WHERE name='Plan upload' AND revision_id=$1::uuid`, s2.DraftRevisionID).Scan(&intID, &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, `"header_row": 3`) {
		t.Fatalf("the saved integration lost its reshape: %s", cfg)
	}
	next := workbook([][]any{{"United Kingdom", "Revenue", 7, 8}, {"Total", "", 7, 8}})
	status, out = do(f.devSub, "POST", "/api/integrations/"+intID+"/run", map[string]string{"xlsx_base64": base64.StdEncoding.EncodeToString(next)})
	if status != http.StatusOK {
		t.Fatalf("run with next month's file: %d %s", status, out)
	}
	if v := value("UK", "Q2", "revenue"); v != 8000 {
		t.Errorf("UK Q2 revenue after the run = %v, want 8000 (%s)", v, out)
	}
}

// The Import Wizard's Shape step previews a reshape by the server's code —
// what a developer sets up there is what every run applies.
func TestImportReshapePreview(t *testing.T) {
	f := newSalesFileFixture(t)
	_, do := f.serve(t, &multiScriptProvider{})
	csv := "Plan FY26\nCountry;Q1;Q2\nCanada;1.234,5;(2)\nTotal;1;1\n"
	reshape := map[string]any{
		"delimiter": ";", "header_row": 2, "decimal_comma": true,
		"skip_rows": []map[string]any{{"column": "Country", "equals": "Total"}},
		"unpivot":   map[string]any{"from": "Q1", "to": "Q2", "name_column": "period", "value_column": "revenue"},
	}
	status, body := do(f.devSub, "POST", "/api/import/reshape-preview", map[string]any{"csv": csv, "reshape": reshape})
	var out struct {
		Raw      [][]string `json:"raw"`
		Header   []string   `json:"header"`
		Rows     [][]string `json:"rows"`
		RowCount int        `json:"row_count"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &out) != nil {
		t.Fatalf("preview: %d %s", status, body)
	}
	if strings.Join(out.Header, ",") != "Country,period,revenue" || out.RowCount != 2 ||
		strings.Join(out.Rows[0], ",") != "Canada,Q1,1234.5" || strings.Join(out.Rows[1], ",") != "Canada,Q2,-2" || len(out.Raw) != 4 {
		t.Fatalf("preview = %+v", out)
	}

	// A reshape naming a column the file lacks: the sheet as read comes back
	// with the error, to correct it by.
	reshape["fill_down"] = []string{"Region"}
	status, body = do(f.devSub, "POST", "/api/import/reshape-preview", map[string]any{"csv": csv, "reshape": reshape})
	if status != http.StatusUnprocessableEntity || !strings.Contains(string(body), `names column \"Region\"`) || !strings.Contains(string(body), "Plan FY26") {
		t.Fatalf("bad reshape: %d %s", status, body)
	}
	// A developer's tool, as the Import Wizard is.
	if status, _ := do(f.bizSub, "POST", "/api/import/reshape-preview", map[string]any{"csv": csv}); status != http.StatusForbidden {
		t.Fatalf("a business user previewing a reshape: %d, want 403", status)
	}
}

// The preview hands the assistant what it could not work out live: the
// reshape a sheet laid out for people needs (title rows above the header,
// months across), and a warning when fractions are headed for a Percentage
// metric, which stores percent units.
func TestPreviewSuggestsReshapeAndWarnsOnFractions(t *testing.T) {
	f := newSalesFileFixture(t)
	ctx, pool := f.ctx, f.pool
	if _, err := pool.Exec(ctx, `
		WITH m AS (INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule, format)
		           VALUES ($1::uuid,$2::uuid,'growth_pct',true,'average','percentage') RETURNING id)
		INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) SELECT $3::uuid, id, 9 FROM m`, f.modelID, f.revID, f.grid); err != nil {
		t.Fatal(err)
	}
	x := excelize.NewFile()
	for i, r := range [][]any{{"Plan by month"}, {}, {"Country", "Jan", "Feb", "Mar", "FY"}, {"Canada", 1, 2, 3, 6}} {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		_ = x.SetSheetRow("Sheet1", cell, &r)
	}
	var wb bytes.Buffer
	_ = x.Write(&wb)
	_ = x.Close()

	tool := func(id string, a any) providers.ChatResponse {
		b, _ := json.Marshal(a)
		return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: id, Name: "preview_file_import", Arguments: b}}}}
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		tool("call_layout", map[string]any{"file": "plan.xlsx", "target_id": "Sales"}),
		tool("call_pct", map[string]any{"file": "rates.csv", "target_id": "Sales"}),
		{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Done."}},
	}}
	send, do := f.serve(t, fake)
	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, f.modelID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled")
	attach := func(name string, data []byte) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", name)
		_, _ = fw.Write(data)
		_ = mw.Close()
		if status, out := send(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/documents", mw.FormDataContentType(), body.Bytes()); status != http.StatusOK {
			t.Fatalf("attach %s: %d %s", name, status, out)
		}
	}
	attach("plan.xlsx", wb.Bytes())
	attach("rates.csv", []byte("geography,period,growth_pct\nCA,Q1,0.05\nUK,Q1,0.07\n"))
	if status, out := do(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "preview both"}); status != http.StatusOK {
		t.Fatalf("message: %d %s", status, out)
	}
	results := map[string]string{}
	msgs, _ := chatStore.ListMessages(ctx, sess.ID)
	for _, m := range msgs {
		if m.Role == "tool" {
			results[m.ToolCallID] = m.Content
		}
	}
	if r := results["call_layout"]; !strings.Contains(r, `Suggested "reshape": {"header_row":3,"unpivot":{"columns":["Jan","Feb","Mar"]`) {
		t.Errorf("layout preview should suggest header_row 3 and the months into rows; got:\n%s", r)
	}
	// The unmapped title header fails the preview, which then suggests a map.
	if r := results["call_layout"]; !strings.Contains(r, `Suggested "column_map" for this grid: {"Plan by month":"ignore"}`) {
		t.Errorf("a failed preview should suggest a column_map; got:\n%s", r)
	}
	if r := results["call_pct"]; !strings.Contains(r, "WARNING: every value for growth_pct is a fraction") {
		t.Errorf("fractions into a Percentage metric should warn; got:\n%s", r)
	}
}

// Live, the assistant reached a clean preview and then proposed the import
// without the target, reshape and map it had just proven. An import naming
// only the file (and sheet) repeats the session's last clean preview of it.
func TestImportRepeatsTheLastCleanPreview(t *testing.T) {
	f := newSalesFileFixture(t)
	ctx, pool := f.ctx, f.pool
	args := map[string]any{"file": "plan.csv", "target_id": "Sales",
		"reshape":    map[string]any{"value_map": map[string]any{"Country": map[string]string{"Canada": "CA"}}},
		"column_map": map[string]string{"Country": "geography", "Quarter": "period", "Amount": "revenue"}}
	tool := func(id, name string, a any) providers.ChatResponse {
		b, _ := json.Marshal(a)
		return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: id, Name: name, Arguments: b}}}}
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		tool("call_preview", "preview_file_import", args),
		tool("call_propose", "propose_actions", map[string]any{"steps": []map[string]any{
			// As live: the target type and one map entry, the reshape left out.
			proposeStep("import_file_data", "Import plan.csv", map[string]any{"file": "plan.csv", "target_type": "grid",
				"column_map": map[string]string{"Amount": "revenue"}}),
		}}),
	}}
	send, do := f.serve(t, fake)
	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, f.modelID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "plan.csv")
	_, _ = fw.Write([]byte("Country,Quarter,Amount\nCanada,Q1,41\n"))
	_ = mw.Close()
	if status, out := send(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/documents", mw.FormDataContentType(), body.Bytes()); status != http.StatusOK {
		t.Fatalf("attach: %d %s", status, out)
	}
	if status, out := do(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "import it"}); status != http.StatusOK {
		t.Fatalf("message: %d %s", status, out)
	}
	pending, err := aiassistant.NewProposalStore(pool).ListProposals(ctx, sess.ID)
	if err != nil || len(pending) != 1 {
		msgs, _ := chatStore.ListMessages(ctx, sess.ID)
		for _, m := range msgs {
			t.Logf("%s: %.300s", m.Role, m.Content)
		}
		t.Fatalf("want the plan shown (its import resolved from the clean preview): %d proposal(s), %v", len(pending), err)
	}
	if status, out := do(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+pending[0].ID+"/confirm", nil); status != http.StatusOK {
		t.Fatalf("confirm: %d %s", status, out)
	}
	if n := f.count(t, `SELECT count(*) FROM runtime.fact_input fi JOIN model.metric_def m ON m.id = fi.metric_id
		WHERE m.name = 'revenue' AND fi.value = 41`); n == 0 {
		t.Error("the import did not land: the recalled preview's target, reshape and map were not used")
	}
}

// Fractions headed for a Percentage metric pass a preview with a warning the
// assistant ignored live; the plan check refuses them unless the step says
// they really are percents under 1%.
func TestPlanCheckRefusesFractionsIntoAPercentage(t *testing.T) {
	f := newSalesFileFixture(t)
	ctx, pool := f.ctx, f.pool
	if _, err := pool.Exec(ctx, `
		WITH m AS (INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule, format)
		           VALUES ($1::uuid,$2::uuid,'growth_pct',true,'average','percentage') RETURNING id)
		INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) SELECT $3::uuid, id, 9 FROM m`, f.modelID, f.revID, f.grid); err != nil {
		t.Fatal(err)
	}
	step := func(confirmUnits bool) map[string]any {
		p := map[string]any{"file": "rates.csv", "target_id": "Sales"}
		if confirmUnits {
			p["values_are_percent_units"] = true
		}
		return proposeStep("import_file_data", "Import rates", p)
	}
	propose := func(id string, s map[string]any) providers.ChatResponse {
		b, _ := json.Marshal(map[string]any{"steps": []map[string]any{s}})
		return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: id, Name: "propose_actions", Arguments: b}}}}
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		propose("call_fractions", step(false)),
		{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Refused."}},
		propose("call_units", step(true)),
	}}
	send, do := f.serve(t, fake)
	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, f.modelID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "rates.csv")
	_, _ = fw.Write([]byte("geography,period,growth_pct\nCA,Q1,0.05\nUK,Q1,0.07\n"))
	_ = mw.Close()
	if status, out := send(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/documents", mw.FormDataContentType(), body.Bytes()); status != http.StatusOK {
		t.Fatalf("attach: %d %s", status, out)
	}
	for _, msg := range []string{"import the rates", "they really are under 1%"} {
		if status, out := do(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": msg}); status != http.StatusOK {
			t.Fatalf("message: %d %s", status, out)
		}
	}
	results := map[string]string{}
	msgs, _ := chatStore.ListMessages(ctx, sess.ID)
	for _, m := range msgs {
		if m.Role == "tool" {
			results[m.ToolCallID] = m.Content
		}
	}
	if r := results["call_fractions"]; !strings.Contains(r, "Proposal NOT shown") || !strings.Contains(r, "values_are_percent_units") {
		t.Errorf("fractions into growth_pct should be refused with the way out; got %q", r)
	}
	if r := results["call_units"]; !strings.Contains(r, "Proposal created") {
		t.Errorf("with values_are_percent_units the plan should be shown; got %q", r)
	}
}

// A file previews into a grid the same proposal creates: preview_file_import
// runs after_steps in a dry run first, and the target may be one of their
// creations, by placeholder or by name. Nothing they make is kept. Without it,
// loading data took a proposal of its own after the grid was confirmed.
func TestPreviewIntoAGridThePlanCreates(t *testing.T) {
	f := newSalesFileFixture(t)
	ctx, pool := f.ctx, f.pool
	steps := []map[string]any{
		{"tool": "create_metric", "description": "Units", "params": map[string]any{"name": "units", "is_input": true}},
		{"tool": "create_grid", "description": "Units grid", "params": map[string]any{"name": "Units Grid", "metrics": []string{"units"}, "dimensions": []string{"geography"}}},
	}
	tool := func(id string, a any) providers.ChatResponse {
		b, _ := json.Marshal(a)
		return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: id, Name: "preview_file_import", Arguments: b}}}}
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		tool("call_ref", map[string]any{"file": "units.csv", "target_id": "<created in step 2>", "after_steps": steps}),
		tool("call_name", map[string]any{"file": "units.csv", "target_id": "Units Grid", "after_steps": steps}),
		tool("call_bad", map[string]any{"file": "units.csv", "target_id": "Units Grid", "after_steps": []map[string]any{
			{"tool": "create_grid", "description": "bad", "params": map[string]any{"name": "Units Grid", "metrics": []string{"no_such_metric"}}}}}),
		{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Done."}},
	}}
	send, do := f.serve(t, fake)
	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, f.modelID, f.devID, "openai", "gpt-5-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "units.csv")
	_, _ = fw.Write([]byte("geography,units\nCA,5\nUK,7\n"))
	_ = mw.Close()
	if status, out := send(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/documents", mw.FormDataContentType(), body.Bytes()); status != http.StatusOK {
		t.Fatalf("attach: %d %s", status, out)
	}
	if status, out := do(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "preview"}); status != http.StatusOK {
		t.Fatalf("message: %d %s", status, out)
	}
	results := map[string]string{}
	msgs, _ := chatStore.ListMessages(ctx, sess.ID)
	for _, m := range msgs {
		if m.Role == "tool" {
			results[m.ToolCallID] = m.Content
		}
	}
	for _, id := range []string{"call_ref", "call_name"} {
		if r := results[id]; !strings.Contains(r, "No errors. Would import 2 value(s): units (2)") || !strings.Contains(r, "Previewed after the 2 step(s)") {
			t.Errorf("%s: want a clean preview into the planned grid; got:\n%s", id, r)
		}
	}
	if r := results["call_bad"]; !strings.Contains(r, "Nothing previewed: the after_steps would fail") {
		t.Errorf("failing after_steps: got:\n%s", r)
	}
	var kept int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.grid_def WHERE name='Units Grid'`).Scan(&kept)
	if kept != 0 {
		t.Errorf("the dry run kept %d grid(s)", kept)
	}
}

// write_input_values is held to the same guard: 0.03 for a 3% threshold is
// refused at the plan check with the way out, not warned of after the write.
func TestPlanCheckRefusesFractionsWrittenIntoAPercentage(t *testing.T) {
	f := newSalesFileFixture(t)
	ctx, pool := f.ctx, f.pool
	if _, err := pool.Exec(ctx, `
		WITH m AS (INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule, format)
		           VALUES ($1::uuid,$2::uuid,'growth_pct',true,'average','percentage') RETURNING id)
		INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) SELECT $3::uuid, id, 9 FROM m`, f.modelID, f.revID, f.grid); err != nil {
		t.Fatal(err)
	}
	step := func(confirmUnits bool) map[string]any {
		p := map[string]any{"metric_id": "growth_pct", "values": []map[string]any{{"members": map[string]string{"geography": "CA", "period": "Q1"}, "value": 0.03}}}
		if confirmUnits {
			p["values_are_percent_units"] = true
		}
		return proposeStep("write_input_values", "Growth", p)
	}
	propose := func(id string, s map[string]any) providers.ChatResponse {
		b, _ := json.Marshal(map[string]any{"steps": []map[string]any{s}})
		return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: id, Name: "propose_actions", Arguments: b}}}}
	}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{propose("call_fraction", step(false)), propose("call_units", step(true))}}
	_, do := f.serve(t, fake)
	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, f.modelID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled") // no titling call to use up the script
	if status, out := do(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "set the growth"}); status != http.StatusOK {
		t.Fatalf("message: %d %s", status, out)
	}
	results := map[string]string{}
	msgs, _ := chatStore.ListMessages(ctx, sess.ID)
	for _, m := range msgs {
		if m.Role == "tool" {
			results[m.ToolCallID] = m.Content
		}
	}
	if r := results["call_fraction"]; !strings.Contains(r, "Proposal NOT shown") || !strings.Contains(r, "values_are_percent_units") {
		t.Errorf("0.03 into growth_pct should be refused with the way out; got %q (all: %q)", r, results)
	}
	if r := results["call_units"]; !strings.Contains(r, "Proposal created") {
		t.Errorf("with values_are_percent_units the plan should be shown; got %q", r)
	}
}
