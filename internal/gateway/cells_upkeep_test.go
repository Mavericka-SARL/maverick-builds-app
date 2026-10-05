package gateway

// Text cells, cleared cells, highlight rules, business-maintained dimensions
// and the developer API's widget_props check (migration 111) — found
// rebuilding a sales target-setting workbook: its per-row comments had no
// cell, a planner could not name a new strategic activity, a cell could only
// go back to 0, its five conditional formats had no counterpart, and a
// button widget's misspelt key saved without a word.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

func TestTextCellsClearsHighlightsAndBusinessMembers(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	planner := "rollup-test-manager"
	rev := f.workingRevID
	callAs := func(who, method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, who, f.appID, body)
	}
	call := func(method, path string, body any) (int, string) { return callAs(dev, method, path, body) }
	must := func(method, path string, body any) string {
		t.Helper()
		status, raw := call(method, path, body)
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("%s %s: %d %s", method, path, status, raw)
		}
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &out)
		return out.ID
	}
	refused := func(what string, status int, raw string, want int, text string) {
		t.Helper()
		if status != want || !strings.Contains(raw, text) {
			t.Errorf("%s: %d %s, want %d mentioning %q", what, status, raw, want, text)
		}
	}

	// Activities, business-maintained, numbered INIT-001 …
	activity := must("POST", "/api/developer/dimensions", map[string]any{"name": "plan_initiative", "revision_id": rev, "business_maintained": true})
	all := must("POST", "/api/developer/dimensions/"+activity+"/members", map[string]any{"code": "ALL", "label": "All activities"})
	for _, c := range []string{"INIT-001", "INIT-002"} {
		must("POST", "/api/developer/dimensions/"+activity+"/members", map[string]any{"code": c, "label": c, "parent_member_id": all})
	}
	metric := func(body map[string]any) string {
		body["revision_id"] = rev
		return must("POST", "/api/developer/metrics", body)
	}
	comment := metric(map[string]any{"name": "act_comment", "is_input": true, "format": "text"})
	amount := metric(map[string]any{"name": "act_amount", "is_input": true, "format": "number"})
	threshold := metric(map[string]any{"name": "act_threshold", "is_input": true, "format": "number"})
	share := metric(map[string]any{"name": "act_share", "formula": "act_amount / 2", "format": "number", "agg_rule": "formula",
		"highlight_rules": []map[string]any{{"abs": true, "op": ">", "than": "ACT_THRESHOLD", "tone": "negative"}, {"op": "blank", "tone": "info"}}})
	grid := must("POST", "/api/developer/grids", map[string]any{"name": "Strategic activities", "revision_id": rev})
	for _, p := range []string{"/dimensions/" + activity, "/metrics/" + comment, "/metrics/" + amount, "/metrics/" + share} {
		must("POST", "/api/developer/grids/"+grid+p, nil)
	}
	setup := must("POST", "/api/developer/grids", map[string]any{"name": "Setup", "revision_id": rev})
	must("POST", "/api/developer/grids/"+setup+"/metrics/"+threshold, nil)

	write := func(who, metricID, code string, body map[string]any) (int, string) {
		body["model_id"], body["revision_id"], body["metric_id"] = f.modelID, rev, metricID
		if code != "" {
			body["dim_codes"] = map[string]string{activity: code}
		}
		return callAs(who, "POST", "/api/cells", body)
	}
	type gridResp struct {
		Metrics []struct {
			ID             string          `json:"id"`
			HighlightRules json.RawMessage `json:"highlight_rules"`
		} `json:"metrics"`
		Dimensions []struct {
			ID                 string `json:"id"`
			BusinessMaintained bool   `json:"business_maintained"`
			Members            []struct {
				ID    string `json:"id"`
				Code  string `json:"code"`
				Label string `json:"label"`
			} `json:"members"`
		} `json:"dimensions"`
		Cells  map[string]float64 `json:"cells"`
		Texts  map[string]string  `json:"texts"`
		Totals map[string]float64 `json:"totals"`
	}
	read := func(who, gridID, revision string) gridResp {
		t.Helper()
		status, raw := callAs(who, "GET", "/api/grid?grid_def_id="+gridID+"&model_id="+f.modelID+"&revision_id="+revision, nil)
		if status != http.StatusOK {
			t.Fatalf("grid: %d %s", status, raw)
		}
		var g gridResp
		_ = json.Unmarshal([]byte(raw), &g)
		return g
	}

	// ── Text cells ──
	for code, note := range map[string]string{"INIT-001": "Retailer confirmed the listing", "INIT-002": "Owner: K. Weber"} {
		if status, raw := write(dev, comment, code, map[string]any{"text": note}); status != http.StatusOK {
			t.Fatalf("write a note: %d %s", status, raw)
		}
	}
	if status, raw := write(dev, amount, "INIT-001", map[string]any{"value": 40}); status != http.StatusOK {
		t.Fatalf("write amount: %d %s", status, raw)
	}
	g := read(dev, grid, rev)
	if got := g.Texts[comment+":INIT-002"]; got != "Owner: K. Weber" {
		t.Errorf("the note reads %q, want the text written", got)
	}
	if _, ok := g.Totals[comment]; ok && g.Totals[comment] != 0 {
		t.Errorf("a text metric has a total %v", g.Totals[comment])
	}
	st, raw := write(dev, comment, "INIT-001", map[string]any{"value": 3})
	refused("a number into a text cell", st, raw, http.StatusBadRequest, "text metric")
	st, raw = write(dev, amount, "INIT-001", map[string]any{"text": "x"})
	refused("a text into a number cell", st, raw, http.StatusBadRequest, "is for a text metric")
	st, raw = call("POST", "/api/developer/metrics", map[string]any{"name": "comment_len", "formula": "LEN(act_comment)", "revision_id": rev})
	refused("a formula reading a text metric", st, raw, http.StatusBadRequest, "TEXT_METRIC_IN_FORMULA")
	st, raw = call("POST", "/api/developer/metrics", map[string]any{"name": "summed_notes", "is_input": true, "format": "text", "agg_rule": "sum", "revision_id": rev})
	refused("a text metric that sums", st, raw, http.StatusBadRequest, "never added up")

	// A file's text column resolves to notes, not numbers.
	staged, importErrs, err := importpkg.ResolveRows(ctx, f.pool, f.modelID, rev, []string{"plan_initiative", "act_comment", "act_amount"},
		[]importpkg.RawRow{{RowNumber: 2, Cells: map[string]string{"plan_initiative": "INIT-002", "act_comment": "From the file", "act_amount": "7"}}})
	if err != nil || len(importErrs) > 0 || len(staged) != 2 {
		t.Fatalf("resolve a text column: %v %v %d rows", err, importErrs, len(staged))
	}
	for _, r := range staged {
		if r.MetricID == comment && (r.Text == nil || *r.Text != "From the file") {
			t.Errorf("the note resolves to %v", r.Text)
		}
		if r.MetricID == amount && (r.Text != nil || r.Value != 7) {
			t.Errorf("the amount resolves to %v / %v", r.Value, r.Text)
		}
	}

	// ── Clearing: blank, not 0, and kept in the history ──
	if status, raw := write(dev, amount, "INIT-001", map[string]any{"clear": true}); status != http.StatusOK || !strings.Contains(raw, "cleared") {
		t.Fatalf("clear: %d %s", status, raw)
	}
	if status, raw := write(dev, comment, "INIT-001", map[string]any{"text": ""}); status != http.StatusOK {
		t.Fatalf("clear a note: %d %s", status, raw)
	}
	g = read(dev, grid, rev)
	if v, ok := g.Cells[amount+":INIT-001"]; ok {
		t.Errorf("a cleared cell still holds %v", v)
	}
	if _, ok := g.Texts[comment+":INIT-001"]; ok {
		t.Errorf("a cleared note is still there: %q", g.Texts[comment+":INIT-001"])
	}
	var archived int
	_ = f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.fact_input_history WHERE metric_id IN ($1::uuid, $2::uuid) AND delete_reason='cleared'`, amount, comment).Scan(&archived)
	if archived != 2 {
		t.Errorf("%d cleared rows archived, want 2 (the amount and the note)", archived)
	}
	var keptNote string
	_ = f.pool.QueryRow(ctx, `SELECT COALESCE(text_value,'') FROM runtime.fact_input_history WHERE metric_id=$1::uuid AND delete_reason='cleared'`, comment).Scan(&keptNote)
	if keptNote != "Retailer confirmed the listing" {
		t.Errorf("the history keeps %q of the cleared note", keptNote)
	}

	// ── Highlight rules ──
	for _, m := range read(dev, grid, rev).Metrics {
		if m.ID == share && !strings.Contains(string(m.HighlightRules), `"than":"act_threshold"`) {
			t.Errorf("the rules come back as %s, want the metric named as the revision spells it", m.HighlightRules)
		}
	}
	for what, rules := range map[string]any{
		"an unknown op":             []map[string]any{{"op": "~", "value": 1, "tone": "negative"}},
		"an unknown metric":         []map[string]any{{"op": ">", "than": "no_such_metric", "tone": "negative"}},
		"an unknown tone":           []map[string]any{{"op": ">", "value": 1, "tone": "purple"}},
		"between without its bound": []map[string]any{{"op": "between", "value": 1, "tone": "warning"}},
		"an unknown key":            []map[string]any{{"op": ">", "value": 1, "tone": "warning", "colour": "red"}},
	} {
		st, raw := call("PATCH", "/api/developer/metrics/"+share, map[string]any{"name": "act_share", "highlight_rules": rules})
		refused(what, st, raw, http.StatusBadRequest, "highlight")
	}

	// ── Business-maintained members ──
	g = read(planner, grid, rev)
	maintained := false
	for _, d := range g.Dimensions {
		if d.ID == activity {
			maintained = d.BusinessMaintained
		}
	}
	if !maintained {
		t.Errorf("the grid does not say plan_initiative is business-maintained")
	}
	st, raw = callAs(planner, "POST", "/api/dimensions/"+activity+"/members", map[string]any{"label": "Retail expansion", "parent_member_id": all})
	if st != http.StatusOK || !strings.Contains(raw, `"code":"INIT-003"`) {
		t.Fatalf("a planner adds an activity: %d %s, want code INIT-003 (the dimension's sequence)", st, raw)
	}
	var added struct{ ID string }
	_ = json.Unmarshal([]byte(raw), &added)
	if status, raw := write(planner, amount, "INIT-003", map[string]any{"value": 12}); status != http.StatusOK {
		t.Fatalf("the planner fills the new row: %d %s", status, raw)
	}
	if status, raw := callAs(planner, "PATCH", "/api/dimensions/"+activity+"/members/"+added.ID, map[string]any{"label": "Retail expansion North"}); status != http.StatusOK {
		t.Fatalf("rename: %d %s", status, raw)
	}
	st, raw = callAs(planner, "PATCH", "/api/dimensions/"+activity+"/members/"+added.ID, map[string]any{"code": "X"})
	refused("a planner re-coding a member", st, raw, http.StatusBadRequest, "developer's")
	st, raw = callAs(planner, "DELETE", "/api/dimensions/"+activity+"/members/"+all, nil)
	refused("removing a member with members under it", st, raw, http.StatusConflict, "under it")
	// A dimension the developer has not opened, and one that is time.
	st, raw = callAs(planner, "POST", "/api/dimensions/"+f.deptsDimID+"/members", map[string]any{"label": "New dept"})
	refused("a dimension not business-maintained", st, raw, http.StatusForbidden, "maintained by the model's developer")
	month := must("POST", "/api/developer/dimensions", map[string]any{"name": "plan_month", "revision_id": rev,
		"dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 1})
	st, raw = call("PATCH", "/api/developer/dimensions/"+month, map[string]any{"business_maintained": true})
	refused("a time dimension business-maintained", st, raw, http.StatusBadRequest, "periods")
	// Only business roles reach the route; the developer's own routes stay theirs.
	st, raw = callAs(planner, "POST", "/api/developer/dimensions/"+activity+"/members", map[string]any{"code": "Z", "label": "Z"})
	if st != http.StatusForbidden {
		t.Errorf("a planner on the developer route: %d %s, want 403", st, raw)
	}
	if status, raw := callAs(planner, "DELETE", "/api/dimensions/"+activity+"/members/"+added.ID, nil); status != http.StatusOK {
		t.Fatalf("remove: %d %s", status, raw)
	}
	var left int
	_ = f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.fact_input WHERE metric_id=$1::uuid AND dim_members->>$2 = 'INIT-003'`, amount, activity).Scan(&left)
	if left != 0 {
		t.Errorf("%d facts left on the removed activity", left)
	}

	// ── Copies carry it all ──
	if status, raw := write(dev, comment, "INIT-002", map[string]any{"text": "Owner: K. Weber (confirmed)"}); status != http.StatusOK {
		t.Fatalf("rewrite a note: %d %s", status, raw)
	}
	status, raw := call("POST", "/api/developer/revisions", map[string]any{"name": "Copy with notes", "source_revision_id": rev})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("duplicate: %d %s", status, raw)
	}
	var copyRev struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(raw), &copyRev)
	var copiedNote string
	var copiedMaintained bool
	var copiedRules string
	_ = f.pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT fi.text_value FROM runtime.fact_input fi JOIN model.metric_def m ON m.id = fi.metric_id
		                 WHERE m.revision_id=$1::uuid AND m.name='act_comment' ORDER BY fi.entered_at DESC LIMIT 1),''),
		       (SELECT business_maintained FROM model.dimension_def WHERE revision_id=$1::uuid AND name='plan_initiative'),
		       (SELECT highlight_rules::text FROM model.metric_def WHERE revision_id=$1::uuid AND name='act_share')`, copyRev.ID).
		Scan(&copiedNote, &copiedMaintained, &copiedRules)
	if copiedNote != "Owner: K. Weber (confirmed)" || !copiedMaintained || !strings.Contains(copiedRules, "act_threshold") {
		t.Errorf("the revision copy has note %q, business_maintained %v, rules %s", copiedNote, copiedMaintained, copiedRules)
	}

	// ── A model export and import carry them too ──
	status, pkg := f.do(t, "GET", "/api/admin/models/"+f.modelID+"/export?revision_id="+rev, f.taPersona, nil)
	if status != http.StatusOK {
		t.Fatalf("export: %d %v", status, pkg)
	}
	status, res := f.do(t, "POST", "/api/admin/models/import", f.taPersona, map[string]any{"application_id": f.appID, "model_name": "Imported upkeep", "package": pkg})
	if status != http.StatusOK {
		t.Fatalf("import: %d %v", status, res)
	}
	newRev, _ := res["revision_id"].(string)
	var importedNote, importedRules string
	var importedMaintained bool
	_ = f.pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT fi.text_value FROM runtime.fact_input fi JOIN model.metric_def m ON m.id = fi.metric_id
		                 WHERE m.revision_id=$1::uuid AND m.name='act_comment' AND fi.text_value IS NOT NULL LIMIT 1),''),
		       (SELECT business_maintained FROM model.dimension_def WHERE revision_id=$1::uuid AND name='plan_initiative'),
		       (SELECT highlight_rules::text FROM model.metric_def WHERE revision_id=$1::uuid AND name='act_share')`, newRev).
		Scan(&importedNote, &importedMaintained, &importedRules)
	if importedNote != "Owner: K. Weber (confirmed)" || !importedMaintained || !strings.Contains(importedRules, "act_threshold") {
		t.Errorf("the imported model has note %q, business_maintained %v, rules %s", importedNote, importedMaintained, importedRules)
	}

	// ── The developer API refuses widget_props the console does not read ──
	dash := must("POST", "/api/developer/dashboards", map[string]any{"name": "Upkeep"})
	st, raw = call("POST", "/api/developer/dashboards/"+dash+"/widgets", map[string]any{"widget_type": "text", "content": "x",
		"pos_x": 0, "pos_y": 0, "size_w": 200, "size_h": 60, "widget_props": map[string]any{"button_label": "Start"}})
	refused("a widget prop the console never reads", st, raw, http.StatusBadRequest, "button_label")
	widget := must("POST", "/api/developer/dashboards/"+dash+"/widgets", map[string]any{"widget_type": "text", "content": "x",
		"pos_x": 0, "pos_y": 0, "size_w": 200, "size_h": 60, "widget_props": map[string]any{"background": "#fff"}})
	// A key saved before the check stays editable.
	if _, err := f.pool.Exec(ctx, `UPDATE model.dashboard_widget SET widget_props = widget_props || '{"legacy_key": 1}'::jsonb WHERE id=$1::uuid`, widget); err != nil {
		t.Fatal(err)
	}
	if status, raw := call("PATCH", "/api/developer/dashboards/"+dash+"/widgets/"+widget, map[string]any{
		"widget_props": map[string]any{"background": "#eee", "legacy_key": 1}}); status != http.StatusOK {
		t.Errorf("re-saving an old widget: %d %s", status, raw)
	}
	st, raw = call("PATCH", "/api/developer/dashboards/"+dash+"/widgets/"+widget, map[string]any{"widget_props": map[string]any{"confirm_txt": "Sure?"}})
	refused("a new unknown key on PATCH", st, raw, http.StatusBadRequest, "confirm_txt")
}

func TestNextMemberCode(t *testing.T) {
	for _, c := range []struct {
		codes []string
		label string
		want  string
	}{
		{[]string{"ALL", "INIT-001", "INIT-002", "INIT-010"}, "Retail", "INIT-011"},
		{[]string{"A1", "A2", "A9"}, "x", "A10"},
		{[]string{"NA", "EU"}, "Latin America", "LATIN_AMERICA"},
		{[]string{"LATIN_AMERICA"}, "Latin  America!", "LATIN_AMERICA_2"},
		{nil, "  ", "MEMBER"},
	} {
		if got := nextCode(c.codes, c.label); got != c.want {
			t.Errorf("nextCode(%v, %q) = %q, want %q", c.codes, c.label, got, c.want)
		}
	}
}
