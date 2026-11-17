package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// The AI Developer saves records: a placeholder account a later import
// names, and a correction of one record by its code — through the table
// import a person's file goes through, into the working revision's table
// (the session's draft). It had no such step and asked the developer to
// type the record on Records (2026-11-16).
func TestAIDeveloperSavesRecords(t *testing.T) {
	fake := &multiScriptProvider{}
	f := setupLinkFixture(t, fake)
	ctx := context.Background()
	f.ok("POST", "/api/forms", linkDev, f.tgtApp, f.tgtRev, map[string]any{
		"name": "accounts_t", "label": "Accounts", "revision_id": f.tgtRev,
		"fields": []map[string]any{
			{"name": "code", "label": "Account ID", "type": "text", "code": true},
			{"name": "name", "label": "Account name", "type": "text", "required": true},
			{"name": "score", "label": "Score", "type": "number", "min": 0, "max": 10},
		},
	})
	chat := aiassistant.NewChatStore(f.pool)
	sess, err := chat.CreateSession(ctx, f.tgtApp, f.tgtModel, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chat.SetTitleIfEmpty(ctx, sess.ID, "records")
	proposals := aiassistant.NewProposalStore(f.pool)
	turn := func(content string, resps ...providers.ChatResponse) {
		t.Helper()
		fake.resps = append(fake.resps, resps...)
		if status, raw := f.req("POST", "/api/ai/sessions/"+sess.ID+"/messages", linkDev, f.tgtApp, "", map[string]string{"content": content}); status != http.StatusOK {
			t.Fatalf("turn %q: %d %s", content, status, raw)
		}
	}
	confirmLatest := func() aiassistant.Proposal {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, `SELECT id::text FROM ai_assistant.proposal WHERE session_id=$1::uuid ORDER BY created_at DESC LIMIT 1`, sess.ID).Scan(&id); err != nil {
			t.Fatalf("no proposal: %v", err)
		}
		f.ok("POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+id+"/confirm", linkDev, f.tgtApp, "", nil)
		p, _ := proposals.GetProposal(ctx, id)
		return p
	}
	records := func() map[string]map[string]any {
		t.Helper()
		rows, err := f.pool.Query(ctx, `SELECT r.data FROM runtime.form_record r JOIN model.form_def d ON d.id = r.form_id
			WHERE d.name = 'accounts_t' AND d.revision_id = (SELECT draft_revision_id FROM ai_assistant.session WHERE id = $1::uuid)`, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]map[string]any{}
		for rows.Next() {
			var raw []byte
			_ = rows.Scan(&raw)
			var d map[string]any
			_ = json.Unmarshal(raw, &d)
			out[d["code"].(string)] = d
		}
		return out
	}

	turn("add a No account placeholder and Acme", proposeResp("p1", map[string]any{"tool": "save_records", "description": "Placeholder account",
		"params": map[string]any{"table": "accounts_t", "records": []map[string]any{
			{"Account ID": "NOACC", "Account name": "No account"}, {"code": "A-1", "name": "Acme", "score": 4}}}}))
	if p := confirmLatest(); p.Status != "executed" || !strings.Contains(p.Steps[0].Result, "Table 'accounts_t': 2 added") {
		t.Fatalf("save: %+v", p.Steps)
	}
	if got := records(); got["NOACC"]["name"] != "No account" || got["A-1"]["score"] != float64(4) {
		t.Fatalf("records in the draft: %v", got)
	}
	turn("Acme's score is 7", proposeResp("p2", map[string]any{"tool": "save_records", "description": "Correct Acme",
		"params": map[string]any{"table": "accounts_t", "records": []map[string]any{{"code": "A-1", "score": 7}}}}))
	if p := confirmLatest(); p.Status != "executed" || !strings.Contains(p.Steps[0].Result, "1 updated by their code") {
		t.Fatalf("update: %+v", p.Steps)
	}
	got := records()
	if len(got) != 2 || got["A-1"]["score"] != float64(7) || got["A-1"]["name"] != "Acme" {
		t.Errorf("after the correction: %v, want A-1 at 7, its name kept, nothing added", got)
	}
}
