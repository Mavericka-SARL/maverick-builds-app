package gateway

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// The AI Developer reaches what the Import wizard does (2026-11-13): a
// loader replacing by entity AND period, asking at each upload for the
// period its file does not say, and a run of it given that period.
func TestAIDeveloperLoaderReplacesByFieldsAndAsksAtUpload(t *testing.T) {
	f := newSalesFileFixture(t)
	ctx, pool := f.ctx, f.pool
	if _, err := pool.Exec(ctx, `
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'tb', 'Trial balance',
			jsonb_build_array(jsonb_build_object('name','entity','label','Entity','type','text','required',true),
			                  jsonb_build_object('name','period','label','Period end','type','date','required',true),
			                  jsonb_build_object('name','amount','label','Amount','type','number')))`, f.modelID, f.revID); err != nil {
		t.Fatal(err)
	}
	columnMap := map[string]string{"Entity": "entity", "Amount": "amount", "Period end": "period"}
	tool := func(id, name string, args any) providers.ChatResponse {
		b, _ := json.Marshal(args)
		return providers.ChatResponse{FinishReason: "tool_calls", Message: providers.Message{Role: "assistant",
			ToolCalls: []providers.ToolCall{{ID: "call_" + id, Name: name, Arguments: b}}}}
	}
	plan := func(id, file, period string, create bool) providers.ChatResponse {
		steps := []map[string]any{}
		ref := "<created in step 1>"
		if create {
			steps = append(steps, proposeStep("create_file_integration", "Save 'TB upload'", map[string]any{
				"name": "TB upload", "target_type": "table", "target_id": "tb", "column_map": columnMap,
				"replace_by": []string{"entity", "period"},
				"parameters": []map[string]any{{"column": "Period end", "type": "date", "required": true}},
			}))
		} else {
			var id string
			_ = pool.QueryRow(ctx, `SELECT id::text FROM model.integration_def WHERE name='TB upload'`).Scan(&id)
			ref = id
		}
		steps = append(steps, proposeStep("import_file_data", "Import "+file, map[string]any{
			"file": file, "integration_id": ref, "parameters": map[string]string{"Period end": period}}))
		return tool(id, "propose_actions", map[string]any{"steps": steps})
	}
	reply := providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Done."}}
	// A proposal ends the turn: each turn is one scripted plan.
	fake := &multiScriptProvider{resps: []providers.ChatResponse{plan("p1", "prod-sep.csv", "2026-09-30", true)}}
	send, do := f.serve(t, fake)
	chatStore := aiassistant.NewChatStore(pool)
	sess, err := chatStore.CreateSession(ctx, f.appID, f.modelID, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chatStore.SetTitleIfEmpty(ctx, sess.ID, "pre-titled")
	pStore := aiassistant.NewProposalStore(pool)
	turn := func(file, data string) {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", file)
		_, _ = fw.Write([]byte(data))
		_ = mw.Close()
		if status, out := send(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/documents", mw.FormDataContentType(), body.Bytes()); status != http.StatusOK {
			t.Fatalf("attach %s: %d %s", file, status, out)
		}
		if status, out := do(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/messages", map[string]string{"content": "load " + file}); status != http.StatusOK {
			t.Fatalf("message: %d %s", status, out)
		}
		pending, err := pStore.ListPendingProposals(ctx, sess.ID)
		if err != nil || len(pending) != 1 {
			msgs, _ := chatStore.ListMessages(ctx, sess.ID)
			last := ""
			for _, m := range msgs {
				if m.Role == "tool" {
					last = m.Content
				}
			}
			t.Fatalf("pending proposals for %s: %v %+v; last tool result: %s", file, err, pending, last)
		}
		if status, out := do(f.devSub, "POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+pending[0].ID+"/confirm", nil); status != http.StatusOK {
			t.Fatalf("confirm: %d %s", status, out)
		}
		p, err := pStore.GetProposal(ctx, pending[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range p.Steps {
			if s.Status != "success" {
				t.Fatalf("%s step %s: %s — %s", file, s.Tool, s.Status, s.Result)
			}
		}
	}
	sum := func(entity string) (n int, total float64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum((r.data->>'amount')::float8),0) FROM runtime.form_record r
			JOIN model.form_def d ON d.id = r.form_id WHERE d.model_id=$1::uuid AND d.name='tb' AND r.data->>'entity'=$2 AND r.data->>'period'='2026-09-30'`,
			f.modelID, entity).Scan(&n, &total); err != nil {
			t.Fatal(err)
		}
		return
	}

	turn("prod-sep.csv", "Entity,Amount\nPROD,100\nPROD,50\n")
	var cfg map[string]any
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT config FROM model.integration_def WHERE name='TB upload'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &cfg)
	if cfg["replace_by"] != "entity,period" {
		t.Errorf("saved replace_by %v; want entity,period", cfg["replace_by"])
	}
	if ps, _ := cfg["parameters"].([]any); len(ps) != 1 || ps[0].(map[string]any)["column"] != "Period end" {
		t.Errorf("saved parameters %v; want Period end", cfg["parameters"])
	}
	// Later turns run the saved loader by its id.
	fake.resps = append(fake.resps, plan("p2", "hold-sep.csv", "2026-09-30", false),
		plan("p3", "prod-sep-fixed.csv", "2026-09-30", false), reply)
	turn("hold-sep.csv", "Entity,Amount\nHOLD,7\n")
	turn("prod-sep-fixed.csv", "Entity,Amount\nPROD,120\n")
	if n, total := sum("PROD"); n != 1 || total != 120 {
		t.Errorf("PROD September after its corrected file: %d record(s), %v; want 1, 120 (replaced, not added)", n, total)
	}
	if n, total := sum("HOLD"); n != 1 || total != 7 {
		t.Errorf("HOLD September after PROD's reload: %d record(s), %v; want 1, 7 (another entity's stays)", n, total)
	}
}
