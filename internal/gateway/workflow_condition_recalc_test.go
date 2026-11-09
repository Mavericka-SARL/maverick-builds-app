package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// A condition reached while its revision is still being recalculated — a
// file load's records still posting — does not read the old values: it
// waits for the recalculation, and past the wait it is held and decided when
// the recalculation finishes. The July close read "MISSING GL" a second after
// the GL file had loaded (Creator & Agency kit, 2026-11-08).
func TestWorkflowConditionWaitsForThePendingRecalculation(t *testing.T) {
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
	gate := must("POST", "/api/developer/metrics", map[string]any{"name": "gl_rows_loaded", "is_input": true, "revision_id": rev})
	grid := must("POST", "/api/developer/grids", map[string]any{"name": "Load gate", "revision_id": rev})
	must("POST", "/api/developer/grids/"+grid+"/metrics/"+gate, nil)
	write := func(v float64) {
		body, _ := json.Marshal(map[string]any{"model_id": f.modelID, "revision_id": rev, "metric_id": gate, "value": v})
		_, _ = call("POST", "/api/cells", json.RawMessage(body))
	}
	write(0)

	store := workflow.NewStore(f.pool)
	def, err := store.CreateWorkflowDefFull(ctx, f.appID, rev, "Close "+t.Name(), "", "manual", f.approverID)
	if err != nil {
		t.Fatal(err)
	}
	steps := json.RawMessage(`[
		{"id":"check","name":"GL loaded?","type":"condition","condition":{"formula":"gl_rows_loaded > 0"},"routes":{"true":"end-completed","false":"explain"}},
		{"id":"explain","name":"Explain the checks","type":"task","assignee_roles":["business_user"],"routes":{"next":"end-completed"}}
	]`)
	if _, err := store.UpdateWorkflowDefFull(ctx, def.ID, def.Name, "", "manual", "", f.approverID, steps, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishWorkflowDef(ctx, def.ID, f.approverID); err != nil {
		t.Fatal(err)
	}
	check := func(instanceID string) (status, decision, comment string) {
		t.Helper()
		_ = f.pool.QueryRow(ctx, `SELECT status::text, COALESCE(decision,''), COALESCE(comment,'') FROM workflow.workflow_step
			WHERE instance_id=$1::uuid AND step_def_id='check'`, instanceID).Scan(&status, &decision, &comment)
		return
	}
	waitIdle := func() {
		for i := 0; i < 100 && calculation.RecalcPending(ctx, f.pool, rev); i++ {
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitIdle()

	// The load's posting lands while the condition waits: it reads it.
	seq, err := calculation.RequestRecalc(ctx, f.pool, rev)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		write(38)
		_ = calculation.RecalcFinished(ctx, f.pool, rev, seq)
	}()
	inst, err := store.StartWorkflow(ctx, def.ID, f.managerID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st, decision, comment := check(inst.Id); st != "completed" || decision != "true" {
		t.Fatalf("a condition reached during the load's recalculation: %s %q (%s), want completed true — it read the values before the load", st, decision, comment)
	}

	// Past the wait it is held, and decided when the recalculation finishes.
	waitIdle()
	write(0)
	waitIdle()
	old := workflow.ConditionRecalcWait
	workflow.ConditionRecalcWait = 300 * time.Millisecond
	t.Cleanup(func() { workflow.ConditionRecalcWait = old })
	seq, err = calculation.RequestRecalc(ctx, f.pool, rev)
	if err != nil {
		t.Fatal(err)
	}
	inst2, err := store.StartWorkflow(ctx, def.ID, f.managerID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st, _, comment := check(inst2.Id); st != "in_progress" || comment != "Waiting for the model's recalculation to finish before deciding." {
		t.Fatalf("a condition past the wait: %s (%q), want held in progress", st, comment)
	}
	write(12)
	waitIdle()
	if err := calculation.RecalcFinished(ctx, f.pool, rev, seq); err != nil {
		t.Fatal(err)
	}
	if n := store.ResumeHeldConditions(ctx); n != 1 {
		t.Errorf("resumed %d held condition(s), want 1", n)
	}
	if st, decision, _ := check(inst2.Id); st != "completed" || decision != "true" {
		t.Errorf("after the recalculation the held condition is %s %q, want completed true", st, decision)
	}
}
