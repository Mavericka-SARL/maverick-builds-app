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
