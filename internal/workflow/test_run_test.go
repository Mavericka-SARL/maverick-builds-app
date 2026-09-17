package workflow_test

import (
	"context"
	"encoding/json"
	"testing"

	_ "github.com/mavericks-engine/mavericks/internal/workflow"
)

// A test run must advance through the graph exactly like a real run while
// writing none of the durable business effects. Before this, "test run" was
// context["_test_mode"]="true" — a key nothing read — so the run dispatched
// real notifications to real people and, on approval, copied real facts into
// the planning data.
func TestTestRunProducesNoDurableSideEffects(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	// A notification step (auto-completes, would page the requester) routing
	// into an approval step. Notification config lives in the steps JSON, not
	// on the proto step, so the def is built through UpdateWorkflowDefFull.
	def, err := store.CreateWorkflowDefFull(ctx, appID, "", "Notify then Approve", "", "manual", userID)
	if err != nil {
		t.Fatalf("create def: %v", err)
	}
	stepsJSON := json.RawMessage(`[
		{"id":"notify","name":"Tell the requester","type":"notification","notification":{"recipient_type":"requester","subject":"Started","message":"Your request started"},"routes":{"next":"approve"}},
		{"id":"approve","name":"Approval","type":"approval","approver_roles":[],"routes":{"approve":"end-done","reject":"end-rejected"}}
	]`)
	if _, err := store.UpdateWorkflowDefFull(ctx, def.ID, def.Name, def.Description, def.TriggerEvent, "", userID, stepsJSON, nil, nil); err != nil {
		t.Fatalf("set steps: %v", err)
	}
	if _, err := store.PublishWorkflowDef(ctx, def.ID, userID); err != nil {
		t.Fatalf("publish: %v", err)
	}

	countNotifications := func() int {
		t.Helper()
		var n int
		if err := store.Pool().QueryRow(ctx, `SELECT COUNT(*) FROM notification.notification`).Scan(&n); err != nil {
			t.Fatalf("count notifications: %v", err)
		}
		return n
	}
	before := countNotifications()

	inst, err := store.StartTestRun(ctx, def.ID, userID, map[string]string{})
	if err != nil {
		t.Fatalf("StartTestRun: %v", err)
	}

	// The instance is flagged, and the run still advanced: the notification
	// step auto-completed and handed over to the approval step.
	var testRun bool
	if err := store.Pool().QueryRow(ctx, `SELECT test_run FROM workflow.workflow_instance WHERE id=$1::uuid`, inst.Id).Scan(&testRun); err != nil {
		t.Fatalf("read test_run: %v", err)
	}
	if !testRun {
		t.Error("instance is not marked as a test run")
	}
	if after := countNotifications(); after != before {
		t.Errorf("a test run created %d notification row(s); want none", after-before)
	}

	_, steps, err := store.GetWorkflowInstance(ctx, inst.Id)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}

	// A real run of the same definition DOES notify — proving the suppression
	// is the test-run flag, not a broken notification path.
	realInst, err := store.StartWorkflow(ctx, def.ID, userID, map[string]string{})
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if after := countNotifications(); after <= before {
		t.Errorf("a real run created %d notification row(s); want at least 1 (suppression must be mode-specific)", after-before)
	}
	_ = realInst
}

// The on_approve fact copy is the engine's one path from a workflow decision
// into planning data, so a test run completing an approval must write none.
func TestTestRunApprovalWritesNoFacts(t *testing.T) {
	f := setupOnApproveFixture(t)
	ctx := context.Background()

	if _, err := f.store.PublishWorkflowDef(ctx, f.wfDefID, f.userID); err != nil {
		t.Fatalf("publish: %v", err)
	}

	countFacts := func() int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.fact_input`).Scan(&n); err != nil {
			t.Fatalf("count facts: %v", err)
		}
		return n
	}
	before := countFacts()

	// The same context the fixture's own on_approve tests use — without it
	// the copy no-ops and the assertion below would pass for the wrong reason.
	startCtx := map[string]string{
		"model_id":           f.modelID,
		"revision_id":        f.workingRevID,
		"target_revision_id": f.targetRevID,
		"scope":              "DEPT_A",
	}

	inst, err := f.store.StartTestRun(ctx, f.wfDefID, f.userID, startCtx)
	if err != nil {
		t.Fatalf("StartTestRun: %v", err)
	}
	_, steps, err := f.store.GetWorkflowInstance(ctx, inst.Id)
	if err != nil || len(steps) == 0 {
		t.Fatalf("get instance: %v (steps=%d)", err, len(steps))
	}
	if _, err := f.store.CompleteStep(ctx, steps[0].Id, f.userID, "approve", "test run"); err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}
	afterTest := countFacts()
	if afterTest != before {
		t.Errorf("a test-run approval wrote %d fact row(s); want none", afterTest-before)
	}

	// Control: the identical approval on a REAL run does write facts, so the
	// assertion above can't pass just because the copy never fires here.
	realInst, err := f.store.StartWorkflow(ctx, f.wfDefID, f.userID, startCtx)
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	_, realSteps, err := f.store.GetWorkflowInstance(ctx, realInst.Id)
	if err != nil || len(realSteps) == 0 {
		t.Fatalf("get real instance: %v (steps=%d)", err, len(realSteps))
	}
	if _, err := f.store.CompleteStep(ctx, realSteps[0].Id, f.userID, "approve", "for real"); err != nil {
		t.Fatalf("CompleteStep (real): %v", err)
	}
	if afterReal := countFacts(); afterReal <= afterTest {
		t.Errorf("a real approval wrote %d fact row(s); want at least 1 (suppression must be mode-specific, not a broken copy)", afterReal-afterTest)
	}
}
