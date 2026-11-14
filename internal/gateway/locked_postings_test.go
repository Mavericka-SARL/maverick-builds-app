package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/recordpost"
	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// A value posted into a cell a workflow locks stays there until the run is
// reopened: a record moved out of the cell or set back to a status that does
// not post, a posting changed so it moves, adds up again or withdraws it, a
// posting or its table deleted — by the console or the AI Developer — are
// refused, and a re-post (a Sync, a status a workflow sets) keeps it. In the
// consolidation kit a posting deleted and created again took September's
// released figures out, and a match moved to October took its own
// (2026-11-13).
func TestValuesPostedIntoALockedPeriodStay(t *testing.T) {
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
	refused := func(what string, want int, method, path, persona string, body any) {
		t.Helper()
		status, out := f.do(t, method, path, persona, body)
		msg, _ := out["error"].(string)
		if status != want || !strings.Contains(msg, "A1 is locked") {
			t.Fatalf("%s: %d %v; want %d naming the lock on A1", what, status, out, want)
		}
	}

	table := call("POST", "/api/forms?revision_id="+f.workingRevID, dev, map[string]any{
		"name": "claims_t", "label": "Claims",
		"fields": []map[string]any{
			{"name": "staff", "label": "Staff", "type": "dimension", "dimension_id": f.staffDimID, "required": true},
			{"name": "amt", "label": "Amount", "type": "number"},
			{"name": "status", "label": "Status", "type": "status"},
		},
	})["id"].(string)
	mapping := call("POST", "/api/developer/form-integrations", dev, map[string]any{
		"form_id": table, "grid_id": f.gridStaffID, "name": "claims", "source_field": "amt", "target_metric_id": f.amountMetricID,
		"aggregation": "sum", "posting_statuses": []string{"approved"},
		"dimension_mappings": map[string]string{f.staffDimID: "staff"},
	})["id"].(string)
	spec := func(change map[string]any) map[string]any {
		out := map[string]any{
			"grid_id": f.gridStaffID, "name": "claims", "source_field": "amt", "target_metric_id": f.amountMetricID,
			"aggregation": "sum", "posting_statuses": []string{"approved"},
			"dimension_mappings": map[string]string{f.staffDimID: "staff"},
		}
		for k, v := range change {
			out[k] = v
		}
		return out
	}
	record := func(staff string, amt float64) string {
		t.Helper()
		id := call("POST", "/api/forms/"+table+"/records", creator, map[string]any{
			"data": map[string]any{"staff": staff, "amt": amt},
		})["id"].(string)
		call("PUT", "/api/records/"+id, dev, map[string]any{"status": "approved"})
		return id
	}
	a1, a2 := record("STAFF_A1", 5), record("STAFF_A2", 3)
	// posted is what the posting holds at staff, once the background
	// posting has settled on want (or the deadline passes).
	posted := func(staff string, want float64) float64 {
		t.Helper()
		var v float64
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if err := f.pool.QueryRow(ctx, `SELECT COALESCE(sum(value), 0) FROM runtime.fact_input
				WHERE source_ref = $1::uuid AND dim_members->>$2 = $3`, mapping, f.staffDimID, staff).Scan(&v); err != nil {
				t.Fatal(err)
			}
			if v == want || time.Now().After(deadline) {
				return v
			}
		}
	}
	if a, b := posted("STAFF_A1", 5), posted("STAFF_A2", 3); a != 5 || b != 3 {
		t.Fatalf("posted A1 %v A2 %v; want 5 and 3", a, b)
	}

	// A1's amounts are approved and locked.
	ws := workflow.NewStore(f.pool)
	def, err := ws.CreateWorkflowDefFull(ctx, f.appID, f.workingRevID, "Staff sign-off", "", "manual", f.approverID)
	if err != nil {
		t.Fatal(err)
	}
	steps, _ := json.Marshal([]map[string]any{{"id": "r", "name": "Sign off", "type": "approval", "assignee_roles": []string{"business_admin"}}})
	schema, _ := json.Marshal([]map[string]any{{"key": "staff", "label": "Staff", "data_type": "Dimension member", "dimension_id": f.staffDimID, "required": true}})
	if _, err := ws.UpdateWorkflowDefFull(ctx, def.ID, def.Name, def.Description, def.TriggerEvent, "none", f.approverID, steps, schema, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.PublishWorkflowDef(ctx, def.ID, f.approverID); err != nil {
		t.Fatal(err)
	}
	rule, err := ws.CreateAutomationRule(ctx, f.appID, f.workingRevID, "Start", "", "manual", "", def.ID, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	run, err := ws.TriggerRule(ctx, rule.ID, f.approverID, map[string]string{"staff": "STAFF_A1"})
	if err != nil {
		t.Fatal(err)
	}
	var step string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM workflow.workflow_step WHERE instance_id = $1::uuid`, run.InstanceID).Scan(&step); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.CompleteStep(ctx, step, f.approverID, "approve", "signed off"); err != nil {
		t.Fatal(err)
	}

	// Records: A1's claim does not leave A1, A2's moves freely.
	refused("A1's claim moved to A2", http.StatusForbidden, "PUT", "/api/records/"+a1, dev,
		map[string]any{"data": map[string]any{"staff": "STAFF_A2", "amt": 5}})
	refused("A1's claim set back to draft", http.StatusForbidden, "PUT", "/api/records/"+a1, dev, map[string]any{"status": "draft"})
	call("PUT", "/api/records/"+a2, dev, map[string]any{"data": map[string]any{"staff": "STAFF_A2", "amt": 4}})
	if v := posted("STAFF_A2", 4); v != 4 {
		t.Fatalf("A2 after its claim changed: %v; want 4", v)
	}

	// The posting: what reaches A1 is refused, what does not goes ahead.
	refused("posting deleted", http.StatusConflict, "DELETE", "/api/developer/form-integrations/"+mapping, dev, nil)
	refused("posting adds up again", http.StatusConflict, "PATCH", "/api/developer/form-integrations/"+mapping, dev, spec(map[string]any{"aggregation": "max"}))
	refused("posting leaves A1 out", http.StatusConflict, "PATCH", "/api/developer/form-integrations/"+mapping, dev,
		spec(map[string]any{"row_filter": []map[string]any{{"field": "staff", "is_not": []string{"STAFF_A1"}}}}))
	refused("posting moved to another metric", http.StatusConflict, "PATCH", "/api/developer/form-integrations/"+mapping, dev,
		spec(map[string]any{"target_metric_id": f.budgetMetricID}))
	call("PATCH", "/api/developer/form-integrations/"+mapping, dev, spec(map[string]any{"name": "claims (renamed)"}))
	call("PATCH", "/api/developer/form-integrations/"+mapping, dev,
		spec(map[string]any{"name": "claims (renamed)", "row_filter": []map[string]any{{"field": "staff", "is_not": []string{"STAFF_A2"}}}}))
	if v := posted("STAFF_A2", 0); v != 0 {
		t.Fatalf("A2 after the posting left it out: %v; want 0", v)
	}
	refused("table deleted", http.StatusConflict, "DELETE", "/api/forms/"+table, dev, nil)

	// The AI Developer meets the same refusals.
	ai := aiassistant.NewWriteExecutor(f.pool, f.modelID, f.workingRevID)
	for tool, params := range map[string]map[string]any{
		"delete_form_integration": {"form_integration_id": mapping},
		"update_form_integration": {"form_integration_id": mapping, "aggregation": "average"},
		"delete_form_def":         {"form_id": table},
	} {
		raw, _ := json.Marshal(params)
		if _, _, err := ai.Execute(ctx, tool, raw); err == nil || !strings.Contains(err.Error(), "A1 is locked") {
			t.Fatalf("AI Developer %s: %v; want refused naming the lock on A1", tool, err)
		}
	}

	// A re-post keeps it: a status a workflow sets, then a Sync.
	poster := recordpost.OnPool(f.pool, zerolog.Nop())
	poster.WorkflowRecordStatus(ctx, a1, "rejected", f.approverID)
	time.Sleep(1500 * time.Millisecond) // the re-post runs in the background
	if v := posted("STAFF_A1", 5); v != 5 {
		t.Fatalf("A1 after a workflow rejected its claim: %v; want 5 kept", v)
	}
	out := call("POST", "/api/forms/"+table+"/sync", dev, nil)
	if out["status"] != "partial" || posted("STAFF_A1", 5) != 5 {
		t.Fatalf("sync with A1's claim rejected: %v, A1 %v; want partial and 5 kept", out, posted("STAFF_A1", 5))
	}

	// Reopened, the run lets it go.
	if _, err := ws.ReopenInstance(ctx, run.InstanceID, f.approverID, "correct A1"); err != nil {
		t.Fatal(err)
	}
	call("POST", "/api/forms/"+table+"/sync", dev, nil)
	if v := posted("STAFF_A1", 0); v != 0 {
		t.Fatalf("A1 after the reopened sync: %v; want 0 (its claim is rejected)", v)
	}
	// Open, the AI Developer moves the posting to another metric, and the
	// old metric's values go with it, as on the console's retarget.
	call("PATCH", "/api/developer/form-integrations/"+mapping, dev, spec(map[string]any{"name": "claims (renamed)", "row_filter": []any{}}))
	if v := posted("STAFF_A2", 4); v != 4 {
		t.Fatalf("A2 once the row filter is gone: %v; want 4", v)
	}
	raw, _ := json.Marshal(map[string]any{"form_integration_id": mapping, "target_metric_id": f.budgetMetricID})
	if _, _, err := ai.Execute(ctx, "update_form_integration", raw); err != nil {
		t.Fatalf("AI Developer retarget once reopened: %v", err)
	}
	var left int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input WHERE source_ref = $1::uuid AND metric_id = $2::uuid`, mapping, f.amountMetricID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the old metric keeps %d of the posting's values after the AI Developer's retarget (%v); want 0", left, err)
	}
	call("DELETE", "/api/developer/form-integrations/"+mapping, dev, nil)
}
