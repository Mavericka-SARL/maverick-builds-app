package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// An approved run that locks a period keeps the figures of that period; an
// input outside the lock (a rate with no period) still moves them, and that
// is said instead of hidden (owner decision 2026-11-13). In the consolidation
// kit a tax rate changed after the CFO's release moved September's total
// assets with nothing to tell anyone.
func TestApprovedFiguresChangedSinceApproval(t *testing.T) {
	f := setupWFRuntimeAuditFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var v string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return v
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	model := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Group') RETURNING id::text`, f.appID)
	rev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Live') RETURNING id::text`, model)
	exec(`UPDATE core.model SET active_revision_id = $2::uuid WHERE id = $1::uuid`, model, rev)
	exec(`UPDATE core.application SET default_model_id = $2::uuid WHERE id = $1::uuid`, f.appID, model)
	period := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'period') RETURNING id::text`, model, rev)
	for _, c := range []string{"SEP", "OCT"} {
		exec(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, $2, $2)`, period, c)
	}
	assets := q(`INSERT INTO model.metric_def (model_id, revision_id, name, label, is_input, format) VALUES ($1::uuid, $2::uuid, 'total_assets', 'Total assets', false, 'currency') RETURNING id::text`, model, rev)
	rate := q(`INSERT INTO model.metric_def (model_id, revision_id, name, label, is_input, format) VALUES ($1::uuid, $2::uuid, 'tax_rate', 'Tax rate', true, 'number') RETURNING id::text`, model, rev)
	calc := func(code string, v float64) {
		exec(`DELETE FROM runtime.calc_result WHERE metric_id = $1::uuid AND dim_members->>$2 = $3`, assets, period, code)
		exec(`INSERT INTO runtime.calc_result (model_id, revision_id, metric_id, dim_members, value, partition_key)
			VALUES ($1::uuid, $2::uuid, $3::uuid, jsonb_build_object($4::text, $5::text), $6, 'p')`, model, rev, assets, period, code, v)
	}
	calc("SEP", 1946.75)
	calc("OCT", 2000)
	exec(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by)
		VALUES ($1::uuid, $2::uuid, $3::uuid, '{}'::jsonb, 30, $4::uuid)`, model, rev, rate, f.devID)

	ws := workflow.NewStore(f.pool)
	def, err := ws.CreateWorkflowDefFull(ctx, f.appID, rev, "Group close", "", "manual", f.devID)
	if err != nil {
		t.Fatal(err)
	}
	steps, _ := json.Marshal([]map[string]any{{"id": "r", "name": "Release", "type": "approval", "assignee_roles": []string{"business_admin"}}})
	schema, _ := json.Marshal([]map[string]any{{"key": "period", "label": "Period", "data_type": "Dimension member", "dimension_id": period, "required": true}})
	if _, err := ws.UpdateWorkflowDefFull(ctx, def.ID, def.Name, def.Description, def.TriggerEvent, "none", f.devID, steps, schema, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.PublishWorkflowDef(ctx, def.ID, f.devID); err != nil {
		t.Fatal(err)
	}
	rule, err := ws.CreateAutomationRule(ctx, f.appID, rev, "Start", "", "manual", "", def.ID, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ex, err := ws.TriggerRule(ctx, rule.ID, f.devID, map[string]string{"period": "SEP"})
	if err != nil {
		t.Fatal(err)
	}
	step := q(`SELECT id::text FROM workflow.workflow_step WHERE instance_id = $1::uuid`, ex.InstanceID)
	if _, err := ws.CompleteStep(ctx, step, f.baID, "approve", "released"); err != nil {
		t.Fatal(err)
	}
	if n := q(`SELECT count(*)::text FROM workflow.approval_snapshot WHERE instance_id = $1::uuid`, ex.InstanceID); n != "1" {
		t.Fatalf("kept %s value(s); want 1: September's total assets, not October's, not the undated rate", n)
	}

	changes := func() []map[string]any {
		t.Helper()
		status, body := f.do(t, "GET", "/api/workflow/approval-changes", f.devSub, nil)
		if status != http.StatusOK {
			t.Fatalf("approval changes: %d %v", status, body)
		}
		runs, _ := body["runs"].([]any)
		out := []map[string]any{}
		for _, r := range runs {
			out = append(out, r.(map[string]any))
		}
		return out
	}
	if got := changes(); len(got) != 0 {
		t.Fatalf("nothing moved yet, but %v", got)
	}
	// October moves: outside the approved scope, nothing to say.
	calc("OCT", 2100)
	if got := changes(); len(got) != 0 {
		t.Fatalf("a change outside the approved period reported: %v", got)
	}
	// The undated rate moves September's figure.
	calc("SEP", 1946.5)
	got := changes()
	if len(got) != 1 || got[0]["changed"] != float64(1) || got[0]["scope"] != "Period: SEP" {
		t.Fatalf("changes after September moved: %v; want one run, one change, scope Period: SEP", got)
	}
	ex0 := got[0]["examples"].([]any)[0].(map[string]any)
	if ex0["metric"] != "Total assets" || ex0["approved"] != 1946.75 || ex0["now"] != 1946.5 {
		t.Fatalf("example %v; want Total assets 1946.75 → 1946.5", ex0)
	}
	status, body := f.do(t, "GET", "/api/workflow/status?name=Group%20close", f.devSub, nil)
	if inst, _ := body["instance"].(map[string]any); status != http.StatusOK || inst["changed_since_approval"] != float64(1) {
		t.Fatalf("status widget: %d %v; want changed_since_approval 1", status, body)
	}
	// Set back, it is what was approved again.
	calc("SEP", 1946.75)
	if got := changes(); len(got) != 0 {
		t.Fatalf("set back, still reported: %v", got)
	}
}
