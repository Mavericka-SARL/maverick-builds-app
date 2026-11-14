package gateway

import (
	"context"
	"net/http"
	"testing"
)

// A table that keeps a dimension may name each member with its code too
// (2026-11-13): two intercompany matches documented "Intragroup loan
// principal" read alike in every grid. Switching the setting relabels the
// members there already, and a record saved after follows it.
func TestKeptMembersNamedWithTheirCode(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	const dev = "rollup-test-approver"
	call := func(method, path string, body any) map[string]any {
		t.Helper()
		status, out := f.do(t, method, path, dev, body)
		if status != http.StatusOK {
			t.Fatalf("%s %s: %d %v", method, path, status, out)
		}
		return out
	}
	dim := call("POST", "/api/developer/dimensions", map[string]any{"name": "ic_match_t", "revision_id": f.workingRevID})["id"].(string)
	table := call("POST", "/api/forms?revision_id="+f.workingRevID, map[string]any{
		"name": "ic_matches_t", "label": "IC matches",
		"fields": []map[string]any{
			{"name": "code", "label": "Match", "type": "autocode", "code_prefix": "IC", "code_digits": 2},
			{"name": "title", "label": "Scope", "type": "text", "required": true},
		},
		"keeps_dimension_id": dim, "member_label_field": "title",
	})["id"].(string)
	for _, title := range []string{"Intragroup loan principal", "Intragroup loan principal"} {
		call("POST", "/api/forms/"+table+"/records", map[string]any{"data": map[string]any{"title": title}})
	}
	labels := func() map[string]string {
		t.Helper()
		rows, err := f.pool.Query(ctx, `SELECT code, label FROM model.dimension_member WHERE dimension_id = $1::uuid`, dim)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var c, l string
			if err := rows.Scan(&c, &l); err != nil {
				t.Fatal(err)
			}
			out[c] = l
		}
		return out
	}
	if got := labels(); got["IC01"] != "Intragroup loan principal" || got["IC02"] != got["IC01"] {
		t.Fatalf("before the setting: %v", got)
	}
	call("PATCH", "/api/forms/"+table, map[string]any{"member_label_with_code": true})
	if got := labels(); got["IC01"] != "IC01 · Intragroup loan principal" || got["IC02"] != "IC02 · Intragroup loan principal" {
		t.Fatalf("after the setting: %v; want each label led by its code", got)
	}
	call("POST", "/api/forms/"+table+"/records", map[string]any{"data": map[string]any{"title": "Dividend"}})
	if got := labels()["IC03"]; got != "IC03 · Dividend" {
		t.Fatalf("a record saved after the setting: %q; want IC03 · Dividend", got)
	}
	call("PATCH", "/api/forms/"+table, map[string]any{"member_label_with_code": false})
	if got := labels(); got["IC01"] != "Intragroup loan principal" || got["IC03"] != "Dividend" {
		t.Fatalf("after the setting is off again: %v", got)
	}
}
