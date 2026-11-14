package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A table is created with its settings or not at all, and a field option the
// server does not know is refused, not dropped (2026-11-13: a member label
// field that was no text field answered "the table was created, but …" and
// left the table; an auto-code field's {"prefix": "IC"} was ignored and the
// codes came out 0001).
func TestTableCreateIsWholeAndRefusesUnknownOptions(t *testing.T) {
	f := setupDimFormulaFixture(t)
	ctx := context.Background()
	dim := f.call("POST", "/api/developer/dimensions", map[string]any{"name": "ic_match", "revision_id": f.revID})
	count := func(name string) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM model.form_def WHERE name=$1`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	status, raw := f.req("POST", "/api/forms?revision_id="+f.revID, map[string]any{
		"name": "ic_matches", "label": "IC matches",
		"fields": []map[string]any{{"name": "code", "label": "Match ID", "type": "autocode", "prefix": "IC", "digits": 2}},
	})
	if status != http.StatusBadRequest || !strings.Contains(string(raw), `prefix`) || !strings.Contains(string(raw), "code_prefix") {
		t.Fatalf("an unknown field option: %d %s; want 400 naming prefix and suggesting code_prefix", status, raw)
	}
	if n := count("ic_matches"); n != 0 {
		t.Fatalf("a refused definition made %d table(s)", n)
	}

	status, raw = f.req("POST", "/api/forms?revision_id="+f.revID, map[string]any{
		"name": "ic_matches", "label": "IC matches",
		"fields": []map[string]any{
			{"name": "code", "label": "Match ID", "type": "autocode", "code_prefix": "IC", "code_digits": 2},
			{"name": "title", "label": "Scope", "type": "text"},
		},
		"keeps_dimension_id": dim, "member_label_field": "code",
	})
	if status != http.StatusBadRequest || strings.Contains(string(raw), "was created") {
		t.Fatalf("a refused setting: %d %s; want 400 without a table", status, raw)
	}
	if n := count("ic_matches"); n != 0 {
		t.Fatalf("a refused setting left %d table(s) behind", n)
	}

	f.call("POST", "/api/forms?revision_id="+f.revID, map[string]any{
		"name": "ic_matches", "label": "IC matches",
		"fields": []map[string]any{
			{"name": "code", "label": "Match ID", "type": "autocode", "code_prefix": "IC", "code_digits": 2},
			{"name": "title", "label": "Scope", "type": "text"},
		},
		"keeps_dimension_id": dim, "member_label_field": "title",
	})
	var keeps string
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(keeps_dimension_id::text,'') FROM model.form_def WHERE name='ic_matches'`).Scan(&keeps); err != nil || keeps != dim {
		t.Fatalf("the valid table keeps %q (err %v); want %s", keeps, err, dim)
	}
}
