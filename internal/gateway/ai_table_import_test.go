package gateway

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// TestAIDeveloperLoadsAFileIntoATable: the AI Developer saves a file loader
// whose target is a table of records and runs it, as the console's Import
// Wizard does — previewed as a dry run that creates nothing, each row a
// record, the run recorded on the loader, and a file with a bad row refused
// by the plan check before the developer sees it. Its file tools used to
// know grids and dimensions only, so an actuals ledger kept as records had
// to be loaded into a grid instead.
func TestAIDeveloperLoadsAFileIntoATable(t *testing.T) {
	f := newSalesFileFixture(t)
	ctx, pool := f.ctx, f.pool
	var formID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'ledger', 'Ledger',
			jsonb_build_array(jsonb_build_object('name','account','label','Account','type','text','required',true),
			                  jsonb_build_object('name','amount','label','Amount','type','number')))
		RETURNING id::text`, f.modelID, f.revID).Scan(&formID); err != nil {
		t.Fatal(err)
	}

	columnMap := map[string]string{"GL account": "account", "Amount EUR": "amount", "Notes": "ignore"}
	tool := func(id, name string, args any) providers.ChatResponse {
		b, _ := json.Marshal(args)
		return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: "call_" + id, Name: name, Arguments: b}}}}
	}
	reply := providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Done."}}
	fake := &multiScriptProvider{resps: []providers.ChatResponse{
		tool("preview", "preview_file_import", map[string]any{"file": "gl.csv", "target_type": "table", "target_id": "ledger", "column_map": columnMap}),
		tool("plan", "propose_actions", map[string]any{"steps": []map[string]any{
			proposeStep("create_file_integration", "Save 'GL actuals' (CSV into Ledger)", map[string]any{
				"name": "GL actuals", "target_type": "table", "target_id": "ledger", "column_map": columnMap}),
			proposeStep("import_file_data", "Import gl.csv", map[string]any{"file": "gl.csv", "integration_id": "<created in step 1>"}),
		}}),
		// A proposal ends the turn.
		tool("bad", "propose_actions", map[string]any{"steps": []map[string]any{
			proposeStep("import_file_data", "Import bad.csv", map[string]any{"file": "bad.csv", "target_type": "table", "target_id": "ledger", "column_map": columnMap}),
		}}),
		reply,
	}}
	send, do := f.serve(t, fake)
	attach := func(sessionID, filename, data string) {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", filename)
		_, _ = fw.Write([]byte(data))
		_ = mw.Close()
		if status, out := send(f.devSub, "POST", "/api/ai/sessions/"+sessionID+"/documents", mw.FormDataContentType(), body.Bytes()); status != http.StatusOK {
			t.Fatalf("attach %s: %d %s", filename, status, out)
		}
	}
	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, f.modelID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled")
	toolResult := func(id string) string {
		t.Helper()
		msgs, err := chatStore.ListMessages(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "tool" && msgs[i].ToolCallID == "call_"+id {
				return msgs[i].Content
			}
		}
		t.Fatalf("no %s tool result", id)
		return ""
	}
	records := func() int {
		return f.count(t, `SELECT count(*) FROM runtime.form_record r JOIN model.form_def d ON d.id = r.form_id WHERE d.model_id=$1::uuid AND d.name='ledger'`, f.modelID)
	}

	attach(sess.ID, "gl.csv", "GL account,Amount EUR,Notes\n4000,100.5,subscriptions\n4010,250,\n")
	if status, body := do(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "load the GL file into the ledger"}); status != http.StatusOK {
		t.Fatalf("message: %d %s", status, body)
	}
	if got := toolResult("preview"); !strings.Contains(got, "No errors. Would add 2 record(s) to table 'ledger'") {
		t.Errorf("preview_file_import into a table:\n%s", got)
	}
	if n := records(); n != 0 {
		t.Fatalf("the preview created %d record(s)", n)
	}
	pStore := aiassistant.NewProposalStore(pool)
	pending, err := pStore.ListPendingProposals(ctx, sess.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending proposals: %v %+v (plan tool result: %s)", err, pending, toolResult("plan"))
	}
	if status, body := do(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+pending[0].ID+"/confirm", nil); status != http.StatusOK {
		t.Fatalf("confirm: %d %s", status, body)
	}
	p, err := pStore.GetProposal(ctx, pending[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Steps {
		if s.Status != "success" {
			t.Fatalf("step %s: %s — %s", s.Tool, s.Status, s.Result)
		}
	}
	if n := records(); n != 2 {
		t.Errorf("records after the import = %d, want 2", n)
	}
	var amount float64
	if err := pool.QueryRow(ctx, `SELECT (r.data->>'amount')::float8 FROM runtime.form_record r JOIN model.form_def d ON d.id = r.form_id
		WHERE d.model_id=$1::uuid AND d.name='ledger' AND r.data->>'account'='4000'`, f.modelID).Scan(&amount); err != nil || amount != 100.5 {
		t.Errorf("account 4000's amount = %v (%v), want 100.5", amount, err)
	}
	intID := p.Steps[0].CreatedID
	if n := f.count(t, `SELECT count(*) FROM model.integration_def WHERE id=$1::uuid AND type='csv_import' AND target_type='form' AND NOT config ? 'import_mode'`, intID); n != 1 {
		t.Error("the loader is not saved as a csv_import into the table")
	}
	if n := f.count(t, `SELECT count(*) FROM model.integration_run WHERE integration_id=$1::uuid AND status='success' AND rows_imported=2`, intID); n != 1 {
		t.Error("the run is not recorded on the loader")
	}

	// A row that fails the table's checks: refused by the plan check, nothing created.
	attach(sess.ID, "bad.csv", "GL account,Amount EUR,Notes\n4020,12,\n,13,no account\n")
	if status, body := do(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "and this one"}); status != http.StatusOK {
		t.Fatalf("message: %d %s", status, body)
	}
	if got := toolResult("bad"); !strings.HasPrefix(got, "Proposal NOT shown") || !strings.Contains(got, "fail validation") {
		t.Errorf("a bad table file must be refused by the plan check:\n%s", got)
	}
	if n := records(); n != 2 {
		t.Errorf("records after the refused file = %d, want 2", n)
	}
}
