package gateway

import (
	"context"
	"net/http"
	"testing"
)

// TestWorkflowSubmitRejectsForeignModel covers the /api/workflow/submit hole.
//
// The route is registered for role "any" and takes model_id and revision_id
// from the request BODY, so the only thing the route guard established was that
// the caller had *some* authenticated session. The handler then looked up a
// workflow def by the supplied model_id and inserted a workflow_instance plus
// its steps — with no actorCanAccessModel, no actorCanAccessApp, and no
// requireResourceAccess anywhere in it.
//
// That let a user in one tenant start a real workflow instance in another
// tenant's application: instance, steps, assignments, and whatever
// notifications those steps dispatch. /api/cells takes model_id from the body
// in exactly the same shape and has always guarded it; this handler was the
// outlier.
func TestWorkflowSubmitRejectsForeignModel(t *testing.T) {
	f := setupRollupFixture(t)
	ft := seedForeignTenant(t, f)
	ctx := context.Background()

	const dev = "rollup-test-approver" // a user in tenant A

	countForeignInstances := func() int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM workflow.workflow_instance WHERE workflow_def_id=$1::uuid`,
			ft.workflowDefID).Scan(&n); err != nil {
			t.Fatalf("count foreign instances: %v", err)
		}
		return n
	}

	// The foreign tenant has a workflow def, so the def lookup this handler
	// performs WILL find one — otherwise the request would fail at "no workflow
	// def found" and the test would prove nothing about authorization.
	if before := countForeignInstances(); before != 0 {
		t.Fatalf("foreign workflow def already has %d instances, want 0", before)
	}

	t.Run("foreign model is refused", func(t *testing.T) {
		status, body := doAs(t, f, "POST", "/api/workflow/submit", dev, f.appID, map[string]any{
			"model_id":    ft.modelID,
			"revision_id": ft.revisionID,
		})
		if status != http.StatusForbidden {
			t.Errorf("submitting against another tenant's model returned %d, want 403\nbody: %s", status, body)
		}
		if n := countForeignInstances(); n != 0 {
			t.Errorf("%d workflow instance(s) were started in the foreign tenant's application — the request was refused but the rows were written anyway", n)
		}
	})

	t.Run("foreign revision with own model is refused", func(t *testing.T) {
		status, body := doAs(t, f, "POST", "/api/workflow/submit", dev, f.appID, map[string]any{
			"model_id":    f.modelID,
			"revision_id": ft.revisionID,
		})
		if status != http.StatusNotFound {
			t.Errorf("submitting with another tenant's revision returned %d, want 404\nbody: %s", status, body)
		}
	})

	// Positive control: the same call with this tenant's own model and revision
	// must still start a workflow, or both rejections above are vacuous.
	t.Run("own model still works", func(t *testing.T) {
		var before int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM workflow.workflow_instance WHERE workflow_def_id=$1::uuid`, f.wfDefID).Scan(&before); err != nil {
			t.Fatalf("count own instances: %v", err)
		}
		// The legacy submit now goes through the engine's start checks like
		// every other start. The fixture's definition requires a
		// RACI-responsible department scope: the approver has none and is
		// refused (the old hand-rolled path started the instance anyway);
		// a manager with a DEPT_A grant submits fine.
		if status, body := doAs(t, f, "POST", "/api/workflow/submit", dev, f.appID, map[string]any{
			"model_id":    f.modelID,
			"revision_id": f.workingRevID,
		}); status != http.StatusForbidden {
			t.Errorf("approver without RACI scope: %d, want 403\nbody: %s", status, body)
		}
		status, body := doAs(t, f, "POST", "/api/workflow/submit", "rollup-test-manager", f.appID, map[string]any{
			"model_id":    f.modelID,
			"revision_id": f.workingRevID,
		})
		if status < 200 || status >= 300 {
			t.Fatalf("submitting against own model returned %d, want 2xx\nbody: %s", status, body)
		}
		var after int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM workflow.workflow_instance WHERE workflow_def_id=$1::uuid`, f.wfDefID).Scan(&after); err != nil {
			t.Fatalf("count own instances: %v", err)
		}
		if after != before+1 {
			t.Fatalf("own-model submit did not start an instance (before=%d after=%d)", before, after)
		}
	})
}
