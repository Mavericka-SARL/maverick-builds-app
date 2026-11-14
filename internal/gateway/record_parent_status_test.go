package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Records belonging to an approved record follow it (owner decision
// 2026-11-13): a journal's lines, once the journal is approved, are not
// their creator's to change, add or delete, the records served say so, and
// an administrator's change sends the journal back to draft. A line changed
// after approval used to keep the journal "approved" while it posted out of
// balance.
func TestRecordsBelongingToADecidedRecordFollowIt(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	const dev, creator = "rollup-test-approver", "rollup-test-manager"
	call := func(method, path, persona string, body any) map[string]any {
		t.Helper()
		status, out := f.do(t, method, path, persona, body)
		if status != http.StatusOK {
			t.Fatalf("%s %s as %s: %d %v", method, path, persona, status, out)
		}
		return out
	}
	dim := call("POST", "/api/developer/dimensions", dev, map[string]any{"name": "journal_t", "revision_id": f.workingRevID})["id"].(string)
	journals := call("POST", "/api/forms?revision_id="+f.workingRevID, dev, map[string]any{
		"name": "journals_t", "label": "Journals",
		"fields": []map[string]any{
			{"name": "code", "label": "JE", "type": "autocode", "code_prefix": "M", "code_digits": 2},
			{"name": "title", "label": "Memo", "type": "text", "required": true},
			{"name": "status", "label": "Status", "type": "status"},
		},
		"keeps_dimension_id": dim, "member_label_field": "title",
	})["id"].(string)
	lines := call("POST", "/api/forms?revision_id="+f.workingRevID, dev, map[string]any{
		"name": "journal_lines_t", "label": "Journal lines",
		"fields": []map[string]any{
			{"name": "journal", "label": "Journal", "type": "dimension", "dimension_id": dim, "required": true, "on_delete": "cascade"},
			{"name": "amount", "label": "Amount", "type": "number"},
		},
	})["id"].(string)

	head := call("POST", "/api/forms/"+journals+"/records", creator, map[string]any{"data": map[string]any{"title": "Accrual"}})
	headID := head["id"].(string)
	code := head["data"].(map[string]any)["code"].(string)
	line := call("POST", "/api/forms/"+lines+"/records", creator, map[string]any{"data": map[string]any{"journal": code, "amount": 7}})
	lineID := line["id"].(string)

	headStatus := func() string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, `SELECT COALESCE(status::text,'') FROM runtime.form_record WHERE id=$1::uuid`, headID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// A draft journal's lines are their creator's.
	call("PUT", "/api/records/"+lineID, creator, map[string]any{"data": map[string]any{"journal": code, "amount": 8}})

	call("PUT", "/api/records/"+headID, dev, map[string]any{"status": "approved"})
	if s := headStatus(); s != "approved" {
		t.Fatalf("journal status %q; want approved", s)
	}

	got := call("GET", "/api/records/"+lineID, creator, nil)
	if perms, _ := got["permissions"].(map[string]any); perms == nil || perms["edit"] != false || perms["delete"] != false {
		t.Fatalf("a line of an approved journal served to its creator with permissions %v; want no edit, no delete", got["permissions"])
	}
	refused := func(what, method, path string, body any) {
		t.Helper()
		status, out := f.do(t, method, path, creator, body)
		msg, _ := out["error"].(string)
		if status != http.StatusForbidden || !strings.Contains(msg, "belongs to") || !strings.Contains(msg, code) {
			t.Fatalf("%s under the approved journal: %d %v; want 403 naming %s", what, status, out, code)
		}
	}
	refused("changing a line", "PUT", "/api/records/"+lineID, map[string]any{"data": map[string]any{"journal": code, "amount": 70}})
	refused("adding a line", "POST", "/api/forms/"+lines+"/records", map[string]any{"data": map[string]any{"journal": code, "amount": 1}})
	refused("deleting a line", "DELETE", "/api/records/"+lineID, nil)
	if s := headStatus(); s != "approved" {
		t.Fatalf("refused changes moved the journal to %q", s)
	}

	// An administrator may; the journal then needs approval again.
	call("PUT", "/api/records/"+lineID, dev, map[string]any{"data": map[string]any{"journal": code, "amount": 70}})
	if s := headStatus(); s != "draft" {
		t.Fatalf("after an administrator changed a line, the journal is %q; want draft", s)
	}
	// Back to draft, the lines are the creator's again.
	call("PUT", "/api/records/"+lineID, creator, map[string]any{"data": map[string]any{"journal": code, "amount": 7}})
}
