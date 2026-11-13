package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// An approved round's lock travels with a model package: the export lists
// the approval (its scope, context and approval step), and the import makes
// it again as an approved run of the imported workflow, so a published
// "approved and locked" budget arrives locked (2026-11-13: it arrived open).
// A rejected run and a run whose context names no member lock nothing and
// are not listed.
func TestModelImportCarriesApprovedLocks(t *testing.T) {
	f := setupMMFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	steps := `[{"id": "b-approve", "name": "Approve and lock", "type": "approval", "assignee_roles": ["approver"], "routes": {"approve": "end-completed"}}]`
	schema := `[{"key": "budget_year", "label": "Budget year", "data_type": "Dimension member", "dimension_id": "00000000-0000-0000-0000-000000000001"},
	            {"key": "budget_metric", "label": "What is frozen", "data_type": "Metric"}]`
	def := q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, context_schema, status)
		VALUES ($1::uuid, 'Annual budget approval', 'manual', $2::jsonb, $3::jsonb, 'published') RETURNING id::text`, f.app1, steps, schema)
	run := func(ctxJSON, decision string) {
		t.Helper()
		inst := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, status, context, steps_snapshot, completed_at)
			VALUES ($1::uuid, 'completed', $2::jsonb, $3::jsonb, now()) RETURNING id::text`, def, ctxJSON, steps)
		q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status, decision, completed_at)
			VALUES ($1::uuid, 'b-approve', 'completed', $2, now()) RETURNING id::text`, inst, decision)
	}
	run(`{"budget_year": "FY2027", "budget_metric": "budget_amount"}`, "approve")
	run(`{"budget_year": "FY2028", "budget_metric": "budget_amount"}`, "reject")
	run(`{"budget_metric": "budget_amount"}`, "approve") // no member: locks nothing

	code, raw := f.do(t, "GET", "/api/admin/models/"+f.modelA+"/export?revision_id="+f.revA, "mm-tenant-admin", f.app1, "", nil)
	if code != http.StatusOK {
		t.Fatalf("export: %d %.300s", code, raw)
	}
	var pkg struct {
		ApprovedLocks []struct {
			Workflow string   `json:"workflow"`
			Scope    []string `json:"scope"`
		} `json:"approved_locks"`
	}
	_ = json.Unmarshal(raw, &pkg)
	if len(pkg.ApprovedLocks) != 1 || strings.Join(pkg.ApprovedLocks[0].Scope, "; ") != "Budget year = FY2027; What is frozen = budget_amount" {
		t.Fatalf("approved locks in the package = %+v, want FY2027 only", pkg.ApprovedLocks)
	}
	code, raw = f.do(t, "POST", "/api/admin/models/import", "mm-tenant-admin", f.app2, "",
		map[string]any{"application_id": f.app2, "model_name": "Imported", "package": json.RawMessage(raw)})
	if code != http.StatusOK {
		t.Fatalf("import: %d %s", code, raw)
	}
	var res struct {
		Notes []string `json:"notes"`
	}
	_ = json.Unmarshal(raw, &res)
	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, `"Annual budget approval" had approved and locked Budget year = FY2027`) && strings.Contains(n, "locked here too") {
			found = true
		}
	}
	if !found {
		t.Errorf("import notes %q do not say the FY2027 approval came with the model", res.Notes)
	}
	// The run is there again, approved at its approval step, in the importing application.
	got := q(`SELECT count(*)::text FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		JOIN workflow.workflow_step s ON s.instance_id = wi.id AND s.step_def_id = 'b-approve' AND s.decision = 'approve'
		WHERE wd.application_id = $1::uuid AND wd.name = 'Annual budget approval' AND wi.status = 'completed'
		  AND wi.context->>'budget_year' = 'FY2027' AND wi.context->>'budget_metric' = 'budget_amount'`, f.app2)
	if got != "1" {
		t.Errorf("approved FY2027 runs in the imported application = %s, want 1", got)
	}
}
