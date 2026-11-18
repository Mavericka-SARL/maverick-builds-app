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

// The AI Developer updates a table's auto-coded records by a field naming
// each (a source system's id, or the auto-code itself) and deletes records
// it names. A CRM's November extract could neither update its deals — every
// row would have been added again — nor replace them, and the assistant
// asked the developer to correct each deal by hand (2026-11-17).
func TestAIDeveloperUpdatesByMatchAndDeletesRecords(t *testing.T) {
	fake := &multiScriptProvider{}
	f := setupLinkFixture(t, fake)
	ctx := context.Background()
	f.ok("POST", "/api/forms", linkDev, f.tgtApp, f.tgtRev, map[string]any{
		"name": "deals_t", "label": "Deals", "revision_id": f.tgtRev,
		"fields": []map[string]any{
			{"name": "code", "label": "Code", "type": "autocode"},
			{"name": "name", "label": "Deal name", "type": "text", "required": true},
			{"name": "source_id", "label": "Source ID", "type": "text"},
			{"name": "amount", "label": "Deal value", "type": "number"},
		},
	})
	chat := aiassistant.NewChatStore(f.pool)
	sess, err := chat.CreateSession(ctx, f.tgtApp, f.tgtModel, f.devID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	_ = chat.SetTitleIfEmpty(ctx, sess.ID, "deals")
	proposals := aiassistant.NewProposalStore(f.pool)
	stop := providers.ChatResponse{FinishReason: "stop", Message: providers.Message{Role: "assistant", Content: "Stopped."}}
	turn := func(content string, resps ...providers.ChatResponse) {
		t.Helper()
		fake.resps = append(fake.resps, resps...)
		if status, raw := f.req("POST", "/api/ai/sessions/"+sess.ID+"/messages", linkDev, f.tgtApp, "", map[string]string{"content": content}); status != http.StatusOK {
			t.Fatalf("turn %q: %d %s", content, status, raw)
		}
	}
	proposalCount := func() int {
		var n int
		_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM ai_assistant.proposal WHERE session_id=$1::uuid`, sess.ID).Scan(&n)
		return n
	}
	confirmed := 0
	var lastToolResult func() string
	confirmLatest := func() aiassistant.Proposal {
		t.Helper()
		if proposalCount() <= confirmed {
			t.Fatalf("no new proposal; the last tool result: %s", lastToolResult())
		}
		confirmed = proposalCount()
		var id string
		if err := f.pool.QueryRow(ctx, `SELECT id::text FROM ai_assistant.proposal WHERE session_id=$1::uuid ORDER BY created_at DESC LIMIT 1`, sess.ID).Scan(&id); err != nil {
			t.Fatalf("no proposal: %v", err)
		}
		f.ok("POST", "/api/ai/sessions/"+sess.ID+"/proposals/"+id+"/confirm", linkDev, f.tgtApp, "", nil)
		p, _ := proposals.GetProposal(ctx, id)
		return p
	}
	lastToolResult = func() string {
		t.Helper()
		msgs, err := chat.ListMessages(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "tool" {
				return msgs[i].Content
			}
		}
		return ""
	}
	// records are the draft's deals by name.
	records := func() map[string]map[string]any {
		t.Helper()
		rows, err := f.pool.Query(ctx, `SELECT r.data FROM runtime.form_record r JOIN model.form_def d ON d.id = r.form_id
			WHERE d.name = 'deals_t' AND d.revision_id = (SELECT draft_revision_id FROM ai_assistant.session WHERE id = $1::uuid)`, sess.ID)
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
			out[d["name"].(string)] = d
		}
		return out
	}
	save := func(id string, matchBy string, recs ...map[string]any) providers.ChatResponse {
		params := map[string]any{"table": "deals_t", "records": recs}
		if matchBy != "" {
			params["match_by"] = matchBy
		}
		return proposeResp(id, map[string]any{"tool": "save_records", "description": "Deals", "params": params})
	}
	del := func(id string, matchBy string, values ...string) providers.ChatResponse {
		params := map[string]any{"table": "deals_t", "records": values}
		if matchBy != "" {
			params["match_by"] = matchBy
		}
		return proposeResp(id, map[string]any{"tool": "delete_records", "description": "Delete deals", "params": params})
	}

	turn("load the deals", save("p1", "",
		map[string]any{"name": "Alpha", "source_id": "S1", "amount": 10},
		map[string]any{"name": "Beta", "source_id": "S2", "amount": 20},
		map[string]any{"name": "Beta copy", "source_id": "S2", "amount": 20}))
	if p := confirmLatest(); p.Status != "executed" || !strings.Contains(p.Steps[0].Result, "3 added") {
		t.Fatalf("load: %+v", p.Steps)
	}

	// By the source's id: S1 is updated (its name kept), S3 is added.
	turn("refresh from the extract", save("p2", "Source ID",
		map[string]any{"source_id": "S1", "amount": 15},
		map[string]any{"source_id": "S3", "name": "Gamma", "amount": 30}))
	if p := confirmLatest(); p.Status != "executed" || !strings.Contains(p.Steps[0].Result, "1 added, 1 updated by their Source ID") {
		t.Fatalf("refresh: %+v", p.Steps)
	}
	got := records()
	if len(got) != 4 || got["Alpha"]["amount"] != float64(15) || got["Gamma"]["source_id"] != "S3" {
		t.Fatalf("after the refresh: %v, want Alpha at 15 and Gamma added (4 deals)", got)
	}

	// S2 is held by two deals: the row cannot say which, so the plan check
	// refuses it and nothing changes.
	before := proposalCount()
	turn("Beta is 25", save("p3", "source_id", map[string]any{"source_id": "S2", "amount": 25}), stop)
	if res := lastToolResult(); proposalCount() != before || !strings.Contains(res, `2 records of Deals hold Source ID "S2"`) {
		t.Fatalf("ambiguous match: %d proposal(s), tool result %q", proposalCount()-before, res)
	}

	// The duplicate goes by its code; then S2 names one deal.
	turn("delete the copy", del("p4", "", got["Beta copy"]["code"].(string)))
	if p := confirmLatest(); p.Status != "executed" || !strings.Contains(p.Steps[0].Result, "1 record(s) deleted") {
		t.Fatalf("delete by code: %+v", p.Steps)
	}
	// By the auto-code itself.
	turn("Beta is 26", save("p5", "code", map[string]any{"code": got["Beta"]["code"], "amount": 26}))
	if p := confirmLatest(); p.Status != "executed" || !strings.Contains(p.Steps[0].Result, "1 updated by their Code") {
		t.Fatalf("update by auto-code: %+v", p.Steps)
	}
	if got := records(); len(got) != 3 || got["Beta"]["amount"] != float64(26) || got["Beta copy"] != nil {
		t.Fatalf("after the delete and the update: %v", got)
	}

	// A code no deal has: an auto-code is the server's, so a row naming one
	// does not add a deal, and a delete naming one deletes nothing.
	before = proposalCount()
	turn("add D-999", save("p6", "code", map[string]any{"code": "D-999", "name": "Ghost"}), stop)
	if res := lastToolResult(); proposalCount() != before || !strings.Contains(res, `no record of Deals has Code "D-999"`) {
		t.Fatalf("unknown auto-code saved: tool result %q", res)
	}
	turn("delete D-999 and Gamma", del("p7", "", "D-999", got["Gamma"]["code"].(string)), stop)
	if res := lastToolResult(); proposalCount() != before || !strings.Contains(res, "none was") {
		t.Fatalf("unknown code deleted: tool result %q", res)
	}
	if len(records()) != 3 {
		t.Fatal("a refused delete deleted a record")
	}

	// By the source's id.
	turn("S3 is gone upstream", del("p8", "source_id", "S3"))
	if p := confirmLatest(); p.Status != "executed" || !strings.Contains(p.Steps[0].Result, "1 record(s) deleted") {
		t.Fatalf("delete by source id: %+v", p.Steps)
	}
	if got := records(); len(got) != 2 || got["Gamma"] != nil {
		t.Fatalf("after deleting S3: %v", got)
	}
}

// A file matched by a field updates only records its loader may see: in a
// table showing each person their own records, a business user's row
// holding the source id of a developer's record adds the user's own record
// — it neither changes the developer's nor learns it is there. A match_by
// that is not a field to match by is refused before anything is read.
func TestImportMatchesOnlyRecordsTheLoaderSees(t *testing.T) {
	f := setupLinkFixture(t, &multiScriptProvider{})
	ctx := context.Background()
	formID := f.ok("POST", "/api/forms", linkDev, f.tgtApp, f.tgtRev, map[string]any{
		"name": "claims_t", "label": "Claims", "revision_id": f.tgtRev, "own_records_only": true,
		"fields": []map[string]any{
			{"name": "name", "label": "Name", "type": "text"},
			{"name": "source_id", "label": "Source ID", "type": "text"},
			{"name": "filed", "label": "Filed", "type": "date"},
		},
	})
	importCSV := func(persona, csv string, dryRun bool, matchBy string) (int, map[string]any) {
		t.Helper()
		status, raw := f.req("POST", "/api/forms/"+formID+"/import", persona, f.tgtApp, f.tgtRev,
			map[string]any{"csv": csv, "dry_run": dryRun, "match_by": matchBy})
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		if status != http.StatusOK {
			out = map[string]any{"error": string(raw)}
		}
		return status, out
	}
	if status, out := importCSV(linkDev, "name,source_id\nDev's,S1\n", false, ""); status != http.StatusOK {
		t.Fatalf("developer's record: %d %v", status, out)
	}
	// The developer refreshes their own record by its source id.
	if _, out := importCSV(linkDev, "name,source_id\nDev's v2,S1\n", true, "source_id"); out["rows_updated"] != float64(1) || out["rows_added"] != float64(0) {
		t.Fatalf("developer's dry run: %v, want 1 updated", out)
	}
	// The user's row with S1 adds their own record.
	if _, out := importCSV(linkUser, "name,source_id\nMine,S1\n", true, "source_id"); out["rows_added"] != float64(1) || out["rows_updated"] != float64(0) {
		t.Fatalf("user's dry run: %v, want 1 added and nothing updated", out)
	}
	if status, out := importCSV(linkUser, "name,source_id\nMine,S1\n", false, "source_id"); status != http.StatusOK {
		t.Fatalf("user's import: %d %v", status, out)
	}
	names := map[string]bool{}
	rows, err := f.pool.Query(ctx, `SELECT data->>'name' FROM runtime.form_record WHERE form_id = $1::uuid`, formID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names[n] = true
	}
	rows.Close()
	if len(names) != 2 || !names["Dev's"] || !names["Mine"] {
		t.Fatalf("records after the user's load: %v, want the developer's unchanged and the user's added", names)
	}
	status, raw := f.req("POST", "/api/forms/"+formID+"/import", linkDev, f.tgtApp, f.tgtRev,
		map[string]any{"csv": "name,filed\nX,2026-01-01\n", "dry_run": true, "match_by": "filed"})
	f.refused("match_by a date field", status, raw, http.StatusBadRequest, "match_by: Filed is a date field")
}
