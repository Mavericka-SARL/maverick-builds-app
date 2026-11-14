package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A member PATCH changes only the fields it sends. It used to replace the
// whole member: {"label": …} alone emptied the code and dropped the parent
// while formulas still named the old code (2026-11-13, consolidation kit:
// an approved period's net profit moved 471.5 → −90.5). An empty code is
// refused; an explicit null parent still moves the member to the top.
func TestMemberPatchChangesOnlyWhatItSends(t *testing.T) {
	f := setupDimFormulaFixture(t)
	ctx := context.Background()
	world := f.call("POST", "/api/developer/dimensions/"+f.region+"/members", map[string]any{"code": "WORLD", "label": "World"})
	emea := "/api/developer/dimensions/" + f.region + "/members/" + f.members["EMEA"]
	f.call("PATCH", emea, map[string]any{"code": "EMEA", "label": "EMEA", "parent_member_id": world})
	f.metric["lk_emea"] = f.call("POST", "/api/developer/metrics", f.metricBody("lk_emea", `IF(PARENT(region) = "WORLD", 1, 0)`))

	member := func() (code, label string, parent *string) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, `SELECT code, label, parent_member_id::text FROM model.dimension_member WHERE id=$1::uuid`,
			f.members["EMEA"]).Scan(&code, &label, &parent); err != nil {
			t.Fatal(err)
		}
		return
	}

	if status, raw := f.req("PATCH", emea, map[string]any{"label": "Europe"}); status != http.StatusOK {
		t.Fatalf("PATCH with only a label: %d %s; want 200", status, raw)
	}
	code, label, parent := member()
	if code != "EMEA" || label != "Europe" || parent == nil || *parent != world {
		t.Fatalf("after a label-only PATCH: code %q label %q parent %v; want EMEA, Europe, under WORLD", code, label, parent)
	}
	var formulaText string
	if err := f.pool.QueryRow(ctx, `SELECT formula FROM model.metric_def WHERE id=$1::uuid`, f.metric["lk_emea"]).Scan(&formulaText); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(formulaText, `"WORLD"`) {
		t.Fatalf("the formula naming WORLD changed: %s", formulaText)
	}

	// Renaming WORLD with only a code keeps its label and rewrites the formula.
	worldURL := "/api/developer/dimensions/" + f.region + "/members/" + world
	if status, raw := f.req("PATCH", worldURL, map[string]any{"code": "GLOBAL"}); status != http.StatusOK {
		t.Fatalf("PATCH with only a code: %d %s; want 200", status, raw)
	}
	var wCode, wLabel string
	if err := f.pool.QueryRow(ctx, `SELECT code, label FROM model.dimension_member WHERE id=$1::uuid`, world).Scan(&wCode, &wLabel); err != nil {
		t.Fatal(err)
	}
	if wCode != "GLOBAL" || wLabel != "World" {
		t.Fatalf("after a code-only PATCH: %q %q; want GLOBAL, World", wCode, wLabel)
	}
	if err := f.pool.QueryRow(ctx, `SELECT formula FROM model.metric_def WHERE id=$1::uuid`, f.metric["lk_emea"]).Scan(&formulaText); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(formulaText, `"GLOBAL"`) {
		t.Fatalf("the rename did not reach the formula: %s", formulaText)
	}

	for _, body := range []map[string]any{{"code": ""}, {"code": "  ", "label": "x"}} {
		status, raw := f.req("PATCH", emea, body)
		if status != http.StatusBadRequest || !strings.Contains(string(raw), "MEMBER_CODE_REQUIRED") {
			t.Fatalf("PATCH %v: %d %s; want 400 MEMBER_CODE_REQUIRED", body, status, raw)
		}
	}
	if code, _, _ := member(); code != "EMEA" {
		t.Fatalf("a refused empty code changed the member: %q", code)
	}

	// The console sends the whole member; null moves it to the top.
	if status, raw := f.req("PATCH", emea, map[string]any{"code": "EMEA", "label": "Europe", "parent_member_id": nil}); status != http.StatusOK {
		t.Fatalf("PATCH as the console sends it: %d %s", status, raw)
	}
	if _, _, parent := member(); parent != nil {
		t.Fatalf("an explicit null parent kept %v; want the member at the top", *parent)
	}
}
