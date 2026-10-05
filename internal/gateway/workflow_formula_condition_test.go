package gateway

// A condition step that reads the model (condition_formula.go): "variance
// over the threshold → another round", as the sales target-setting workbook's
// README describes its loop. The round's task is redone while the formula
// holds, and the instance completes once it does not.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

func TestWorkflowConditionReadsTheModel(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	rev := f.workingRevID
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, dev, f.appID, body)
	}
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
	region := must("POST", "/api/developer/dimensions", map[string]any{"name": "round_region", "revision_id": rev})
	for _, c := range []string{"NA", "EU"} {
		must("POST", "/api/developer/dimensions/"+region+"/members", map[string]any{"code": c, "label": c})
	}
	variance := must("POST", "/api/developer/metrics", map[string]any{"name": "round_variance", "is_input": true, "revision_id": rev})
	threshold := must("POST", "/api/developer/metrics", map[string]any{"name": "round_threshold", "is_input": true, "revision_id": rev})
	byRegion := must("POST", "/api/developer/grids", map[string]any{"name": "Round variance", "revision_id": rev})
	must("POST", "/api/developer/grids/"+byRegion+"/dimensions/"+region, nil)
	must("POST", "/api/developer/grids/"+byRegion+"/metrics/"+variance, nil)
	settings := must("POST", "/api/developer/grids", map[string]any{"name": "Round settings", "revision_id": rev})
	must("POST", "/api/developer/grids/"+settings+"/metrics/"+threshold, nil)
	write := func(metricID, code string, v float64) {
		t.Helper()
		body := map[string]any{"model_id": f.modelID, "revision_id": rev, "metric_id": metricID, "value": v}
		if code != "" {
			body["dim_codes"] = map[string]string{region: code}
		}
		if status, raw := call("POST", "/api/cells", body); status != http.StatusOK {
			t.Fatalf("write: %d %s", status, raw)
		}
	}
	write(threshold, "", 5)
	write(variance, "NA", 4)
	write(variance, "EU", -3)

	store := workflow.NewStore(f.pool)
	def, err := store.CreateWorkflowDefFull(ctx, f.appID, rev, "Target round "+t.Name(), "", "manual", f.approverID)
	if err != nil {
		t.Fatal(err)
	}
	steps := json.RawMessage(`[
		{"id":"correct","name":"Correct targets","type":"task","assignee_roles":["business_user"],"routes":{"next":"check"}},
		{"id":"check","name":"Over threshold?","type":"condition","condition":{"formula":"ABS(round_variance) > round_threshold"},
		 "routes":{"true":"correct","false":"end-completed"}}
	]`)
	schema := json.RawMessage(`[{"key":"region","data_type":"Dimension member","dimension_id":"` + region + `"}]`)
	if _, err := store.UpdateWorkflowDefFull(ctx, def.ID, def.Name, "", "manual", "", f.approverID, steps, schema, nil); err != nil {
		t.Fatal(err)
	}
	full, err := store.GetWorkflowDefFull(ctx, def.ID)
	if err != nil {
		t.Fatal(err)
	}
	if errs := append(workflow.ValidateDef(full), store.CheckConditionNames(ctx, full)...); len(errs) > 0 {
		t.Fatalf("a valid formula condition is refused: %v", errs)
	}
	if _, err := store.PublishWorkflowDef(ctx, def.ID, f.approverID); err != nil {
		t.Fatal(err)
	}

	stepOf := func(instanceID, defID string) (status, decision, comment, id string) {
		t.Helper()
		_ = f.pool.QueryRow(ctx, `
			SELECT status::text, COALESCE(decision,''), COALESCE(comment,''), id::text FROM workflow.workflow_step
			WHERE instance_id=$1::uuid AND step_def_id=$2 ORDER BY (status = 'in_progress') DESC, completed_at DESC NULLS LAST, created_at DESC LIMIT 1`,
			instanceID, defID).Scan(&status, &decision, &comment, &id)
		return
	}
	completeTask := func(instanceID string) {
		t.Helper()
		_, _, _, id := stepOf(instanceID, "correct")
		if _, err := store.CompleteStep(ctx, id, f.managerID, "done", ""); err != nil {
			t.Fatalf("complete the task: %v", err)
		}
	}

	// The whole company: |4 + -3| = 1 is within 5 — the instance completes.
	inst, err := store.StartWorkflow(ctx, def.ID, f.managerID, nil)
	if err != nil {
		t.Fatal(err)
	}
	completeTask(inst.Id)
	if _, decision, comment, _ := stepOf(inst.Id, "check"); decision != "false" {
		t.Errorf("company-wide variance 1 vs 5: decision %q (%s), want false", decision, comment)
	}
	// Over the threshold the round goes back to the task, and once the
	// values come within it the instance completes: |9 + -3| = 6 > 5.
	write(variance, "NA", 9)
	inst2, err := store.StartWorkflow(ctx, def.ID, f.managerID, nil)
	if err != nil {
		t.Fatal(err)
	}
	completeTask(inst2.Id)
	// True routes back to the task, which says so (the rework's note).
	if st, _, comment, _ := stepOf(inst2.Id, "correct"); st != "in_progress" || !strings.Contains(comment, `rework (1) from "Over threshold?"`) {
		t.Errorf("company variance 6 vs 5: the task is %q (%q), want in_progress again, sent back by the condition", st, comment)
	}
	write(variance, "NA", 2)
	time.Sleep(50 * time.Millisecond)
	completeTask(inst2.Id)
	var instStatus string
	_ = f.pool.QueryRow(ctx, `SELECT status::text FROM workflow.workflow_instance WHERE id=$1::uuid`, inst2.Id).Scan(&instStatus)
	if instStatus != "completed" {
		t.Errorf("after the variance comes within the threshold the instance is %q, want completed", instStatus)
	}
	// Pinned by the instance's region: NA alone is 9, over 5, while the
	// company is within it. The round is about NA, and its task may still
	// correct NA: only an open approval locks the round's scope.
	write(variance, "EU", -8)
	inst3, err := store.StartWorkflow(ctx, def.ID, f.managerID, map[string]string{"region": "NA"})
	if err != nil {
		t.Fatal(err)
	}
	write(variance, "NA", 9)
	completeTask(inst3.Id)
	if st, _, comment, _ := stepOf(inst3.Id, "correct"); st != "in_progress" || !strings.Contains(comment, "rework") {
		t.Errorf("NA variance 9 vs 5 (company 1): the task is %q (%q), want sent back — the region pins the point", st, comment)
	}

	// A formula naming nothing in the model is refused at validation.
	bad := json.RawMessage(strings.Replace(string(steps), "round_threshold", "round_thresold", 1))
	if _, err := store.UpdateWorkflowDefFull(ctx, def.ID, def.Name, "", "manual", "", f.approverID, bad, schema, nil); err != nil {
		t.Fatal(err)
	}
	full, _ = store.GetWorkflowDefFull(ctx, def.ID)
	if errs := store.CheckConditionNames(ctx, full); len(errs) != 1 || !strings.Contains(errs[0], "round_thresold") {
		t.Errorf("an unknown name in a condition formula: %v, want one error naming it", errs)
	}
	// A formula that does not parse fails ValidateDef.
	broken := json.RawMessage(strings.Replace(string(steps), "ABS(round_variance) > round_threshold", "ABS(round_variance >", 1))
	if _, err := store.UpdateWorkflowDefFull(ctx, def.ID, def.Name, "", "manual", "", f.approverID, broken, schema, nil); err != nil {
		t.Fatal(err)
	}
	full, _ = store.GetWorkflowDefFull(ctx, def.ID)
	if errs := workflow.ValidateDef(full); len(errs) == 0 || !strings.Contains(strings.Join(errs, ";"), "does not parse") {
		t.Errorf("a broken condition formula: %v, want a parse error", errs)
	}
}
