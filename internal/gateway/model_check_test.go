package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/modelcheck"
	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// TestModelCheck: one revision with an example of every rule — a schedule
// that cannot start its workflow, a text field mapped into a number, a
// pick-list total worked out from the row's member, a SUMIFS adding the
// members of a dimension whose total is "first", a widget pinning members
// of a business-maintained dimension, a role whose hidden member blanks a
// figure read across it, an input open to every role — and each finding's
// absence once fixed; ignoring with a reason (kept by a revision copy),
// un-ignoring, and the warnings a metric save answers with (2026-11-21).
func TestModelCheck(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	rev := f.workingRevID
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, dev, f.appID, body)
	}
	check := func() modelcheck.Report {
		t.Helper()
		status, raw := call("GET", "/api/developer/model-check?revision_id="+rev, nil)
		if status != http.StatusOK {
			t.Fatalf("model check: %d %s", status, raw)
		}
		var rep modelcheck.Report
		if err := json.Unmarshal([]byte(raw), &rep); err != nil {
			t.Fatal(err)
		}
		return rep
	}
	find := func(rep modelcheck.Report, rule, object string) *modelcheck.Finding {
		for i, fd := range rep.Findings {
			if fd.Rule == rule && strings.EqualFold(fd.Object.Name, object) {
				return &rep.Findings[i]
			}
		}
		return nil
	}

	// A planning cube: dept (business-maintained) × scen (total = first).
	scen := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, agg_rule) VALUES ($1::uuid, $2::uuid, 'scen', 'first') RETURNING id::text`, f.modelID, rev)
	exec(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'BASE', 'Base'), ($1::uuid, 'HIGH', 'High')`, scen)
	dept := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, business_maintained) VALUES ($1::uuid, $2::uuid, 'dept', true) RETURNING id::text`, f.modelID, rev)
	hiddenMember := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'D1', 'D1') RETURNING id::text`, dept)
	exec(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'D2', 'D2')`, dept)
	grid := q(`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Plan', $2::uuid) RETURNING id::text`, f.modelID, rev)
	exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id, display_level) VALUES ($1::uuid, $2::uuid, 0), ($1::uuid, $3::uuid, 0)`, grid, dept, scen)
	metric := func(name, formula string, input bool, extra string) string {
		t.Helper()
		var fp *string
		if formula != "" {
			fp = &formula
		}
		id := q(`INSERT INTO model.metric_def (model_id, revision_id, name, formula, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, $3, $4, $5, 'sum') RETURNING id::text`, f.modelID, rev, name, fp, input)
		if extra != "" {
			exec(`UPDATE model.metric_def SET `+extra+` WHERE id = $1::uuid`, id)
		}
		exec(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 0)`, grid, id)
		return id
	}
	val := metric("val", "", true, "")
	metric("flag", "", true, "")
	metric("sumflag", "SUMIFS(val, flag, 1)", false, "")
	metric("sumflag_pinned", "SUMIFS(val, flag, 1, scen, scen)", false, "")
	metric("dept_share", `val / SUMIFS(val, dept, "*")`, false, "")
	action := metric("action", `IF(dept = "D1", "BASE", "HIGH")`, false, "agg_rule = 'formula', format = 'picklist', picklist_dimension_id = '"+scen+"'::uuid")
	metric("action_none", `IF(dept = "D1", "BASE", "HIGH")`, false, "agg_rule = 'none', format = 'picklist', picklist_dimension_id = '"+scen+"'::uuid")

	// A table whose text field is mapped into val (saved before the refusal).
	form := q(`INSERT INTO model.form_def (model_id, name, label, fields, revision_id) VALUES ($1::uuid, 'requests', 'Requests',
		'[{"name":"reason","label":"Reason","type":"text"},{"name":"qty","label":"Qty","type":"number"}]', $2::uuid) RETURNING id::text`, f.modelID, rev)
	exec(`INSERT INTO model.form_metric_mapping (model_id, form_id, name, source_field, target_metric_id, aggregation) VALUES
		($1::uuid, $2::uuid, 'reason into val', 'reason', $3::uuid, 'sum'), ($1::uuid, $2::uuid, 'qty into val', 'qty', $3::uuid, 'sum')`, f.modelID, form, val)

	// A dashboard pinning two dept members in a grid widget.
	dash := q(`INSERT INTO model.dashboard_def (model_id, name, revision_id) VALUES ($1::uuid, 'Cockpit', $2::uuid) RETURNING id::text`, f.modelID, rev)
	exec(`INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, content, sort_order, widget_props) VALUES
		($1::uuid, 'grid', $2, 'Plan by dept', 0, jsonb_build_object('show_members', jsonb_build_object($3::text, '["D1","D2"]'::jsonb)))`, dash, grid, dept)

	// A role that cannot see D1, and that may only read val.
	ws := q(`SELECT workspace_id::text FROM core.application WHERE id = $1::uuid`, f.appID)
	role := q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Store ops') RETURNING id::text`, ws)
	exec(`INSERT INTO identity.business_role_access_rule (role_id, rule_type, ref_id, access, ref_lineage_id)
		SELECT $1::uuid, 'dimension_member', id::text, 'hidden', lineage_id FROM model.dimension_member WHERE id = $2::uuid`, role, hiddenMember)
	exec(`INSERT INTO identity.business_role_access_rule (role_id, rule_type, ref_id, access, ref_lineage_id)
		SELECT $1::uuid, 'metric', id::text, 'read', lineage_id FROM model.metric_def WHERE id = $2::uuid`, role, val)

	// A schedule of a workflow that needs a department (saved before the refusal).
	store := workflow.NewStore(f.pool)
	def, err := store.CreateWorkflowDefFull(ctx, f.appID, rev, "Review", "", "manual", f.approverID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateWorkflowDefFull(ctx, def.ID, "Review", "", "manual", "", f.approverID,
		json.RawMessage(`[{"id":"s","name":"Review","type":"task","assignee_roles":["business_user"],"routes":{"next":"end-completed"}}]`),
		json.RawMessage(`[{"key":"dept","label":"Dept","data_type":"Dimension member","dimension_id":"`+dept+`","required":true}]`), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishWorkflowDef(ctx, def.ID, f.approverID); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO workflow.automation_rule (application_id, revision_id, name, trigger_type, workflow_name, workflow_def_id, cron_expr)
		VALUES ($1::uuid, $2::uuid, 'Monday review', 'schedule', 'Review', $3::uuid, '0 6 * * 1')`, f.appID, rev, def.ID)

	rep := check()
	for _, want := range []struct{ rule, object, severity, says string }{
		{"schedule_without_values", "Monday review", "error", "needs dept"},
		{"field_posts_nothing", "reason into val", "warning", "field reason, a text field"},
		{"total_from_blanks", "action", "warning", "calculated pick-list"},
		{"adds_unsummed_members", "sumflag", "warning", "add the criterion"},
		{"pinned_members", "Plan by dept", "warning", "fixed list of 2 dept members"},
		{"hidden_members_blank_figures", "Store ops", "warning", "dept_share"},
		{"input_open_to_every_role", "flag", "info", "every business role may write flag"},
	} {
		fd := find(rep, want.rule, want.object)
		if fd == nil {
			t.Errorf("want a %s finding about %s; got %+v", want.rule, want.object, rep.Findings)
			continue
		}
		if fd.Severity != want.severity || !strings.Contains(strings.ToLower(fd.Message+" "+fd.Fix), strings.ToLower(want.says)) {
			t.Errorf("%s about %s: %s %q / %q, want %s saying %q", want.rule, want.object, fd.Severity, fd.Message, fd.Fix, want.severity, want.says)
		}
	}
	for _, clean := range []struct{ rule, object string }{
		{"field_posts_nothing", "qty into val"},
		{"total_from_blanks", "action_none"},
		{"adds_unsummed_members", "sumflag_pinned"},
		{"input_open_to_every_role", "val"},
	} {
		if fd := find(rep, clean.rule, clean.object); fd != nil {
			t.Errorf("no %s finding about %s expected: %+v", clean.rule, clean.object, fd)
		}
	}
	if rep.Counts["error"] != 1 {
		t.Errorf("counts: %v", rep.Counts)
	}

	// Ignore with a reason; a copy of the revision keeps it; un-ignore.
	pinned := find(rep, "pinned_members", "Plan by dept")
	if status, raw := call("PUT", "/api/developer/model-check/ignores", map[string]any{"revision_id": rev, "key": pinned.Key, "reason": ""}); status != http.StatusBadRequest {
		t.Errorf("an ignore without a reason must be refused: %d %s", status, raw)
	}
	if status, raw := call("PUT", "/api/developer/model-check/ignores", map[string]any{"revision_id": rev, "key": "no|such", "reason": "x"}); status != http.StatusNotFound {
		t.Errorf("ignoring a finding the revision does not have: %d %s", status, raw)
	}
	if status, raw := call("PUT", "/api/developer/model-check/ignores", map[string]any{"revision_id": rev, "key": pinned.Key, "reason": "the cockpit shows the two flagship stores on purpose"}); status != http.StatusOK {
		t.Fatalf("ignore: %d %s", status, raw)
	}
	rep = check()
	if fd := find(rep, "pinned_members", "Plan by dept"); fd == nil || !fd.Ignored || fd.IgnoreReason == "" || rep.Counts["ignored"] != 1 {
		t.Errorf("the finding must be ignored with its reason: %+v %v", fd, rep.Counts)
	}
	status, raw := call("POST", "/api/developer/revisions", map[string]any{"name": "Copy", "source_revision_id": rev})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("copy revision: %d %s", status, raw)
	}
	var copied string
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.revision WHERE model_id = $1::uuid AND name = 'Copy'`, f.modelID).Scan(&copied)
	var kept int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM model.check_ignore WHERE revision_id = $1::uuid AND key = $2`, copied, pinned.Key).Scan(&kept)
	if kept != 1 {
		t.Errorf("a revision copy must keep the ignore")
	}
	if status, raw := call("DELETE", "/api/developer/model-check/ignores?revision_id="+rev+"&key="+url.QueryEscape(pinned.Key), nil); status != http.StatusOK {
		t.Fatalf("un-ignore: %d %s", status, raw)
	}
	if fd := find(check(), "pinned_members", "Plan by dept"); fd == nil || fd.Ignored {
		t.Errorf("un-ignored, the finding is open again: %+v", fd)
	}

	// A metric save answers with the findings about it.
	status, raw = call("PATCH", "/api/developer/metrics/"+action, map[string]any{"formula": `IF(dept = "D2", "HIGH", "BASE")`})
	if status != http.StatusOK || !strings.Contains(raw, `"warnings":[{`) || !strings.Contains(raw, "total_from_blanks") {
		t.Errorf("the save of a pick-list whose total is worked out from blanks must warn: %d %s", status, raw)
	}
}
