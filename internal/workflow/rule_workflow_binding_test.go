package workflow_test

import (
	"context"
	"strings"
	"testing"

	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// An automation rule's workflow_def_id has to name a published workflow in
// the SAME application and revision as the rule. Only the "published" half
// was enforced, and only at creation: update validated nothing at all, and
// fireRule trusted whatever was stored. A rule could therefore be repointed
// at another APPLICATION's published workflow and fired, starting an
// instance of another tenant's definition — the gateway's resource guard
// covers the rule being edited, not the definition that rule names.
func TestAutomationRuleWorkflowBinding(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	publish := func(defID string) {
		t.Helper()
		if _, err := store.PublishWorkflowDef(ctx, defID, userID); err != nil {
			t.Fatalf("publish %s: %v", defID, err)
		}
	}
	mkDef := func(name string) string {
		t.Helper()
		def, err := store.CreateWorkflowDef(ctx, appID, name, "manual", []*workflowv1.WorkflowStepDef{
			{Id: "approve", Name: "Approval", Type: workflowv1.StepType_STEP_TYPE_APPROVAL},
		})
		if err != nil {
			t.Fatalf("create def %s: %v", name, err)
		}
		return def.Id
	}

	ownDefID := mkDef("Own Flow")
	publish(ownDefID)
	draftDefID := mkDef("Draft Flow") // deliberately left unpublished

	// A second application with its own published workflow.
	var foreignAppID string
	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO core.application (workspace_id, name)
		VALUES ((SELECT workspace_id FROM core.application WHERE id=$1::uuid), 'Other App')
		RETURNING id::text`, appID).Scan(&foreignAppID); err != nil {
		t.Fatalf("seed foreign app: %v", err)
	}
	foreignDef, err := store.CreateWorkflowDef(ctx, foreignAppID, "Foreign Flow", "manual", []*workflowv1.WorkflowStepDef{
		{Id: "approve", Name: "Approval", Type: workflowv1.StepType_STEP_TYPE_APPROVAL},
	})
	if err != nil {
		t.Fatalf("create foreign def: %v", err)
	}
	publish(foreignDef.Id)

	// ── create ───────────────────────────────────────────────────────────
	if _, err := store.CreateAutomationRule(ctx, appID, "", "Cross-app rule", "", "manual", "Foreign Flow", foreignDef.Id, "", "", nil); err == nil {
		t.Error("creating a rule bound to another application's workflow was accepted; want a rejection")
	} else if !strings.Contains(err.Error(), "different application") {
		t.Errorf("create rejection = %q, want it to name the application mismatch", err)
	}
	if _, err := store.CreateAutomationRule(ctx, appID, "", "Draft rule", "", "manual", "Draft Flow", draftDefID, "", "", nil); err == nil {
		t.Error("creating a rule bound to a draft workflow was accepted; want a rejection")
	}

	rule, err := store.CreateAutomationRule(ctx, appID, "", "Good rule", "", "manual", "Own Flow", ownDefID, "", "", nil)
	if err != nil {
		t.Fatalf("creating a rule bound to a valid workflow: %v", err)
	}

	// ── update ───────────────────────────────────────────────────────────
	if _, err := store.UpdateAutomationRule(ctx, rule.ID, "", "", "", "", foreignDef.Id, "", "", nil, nil); err == nil {
		t.Error("repointing a rule at another application's workflow was accepted; want a rejection")
	}
	if _, err := store.UpdateAutomationRule(ctx, rule.ID, "", "", "", "", draftDefID, "", "", nil, nil); err == nil {
		t.Error("repointing a rule at a draft workflow was accepted; want a rejection")
	}
	var boundDefID string
	if err := store.Pool().QueryRow(ctx,
		`SELECT COALESCE(workflow_def_id::text,'') FROM workflow.automation_rule WHERE id=$1::uuid`, rule.ID).Scan(&boundDefID); err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if boundDefID != ownDefID {
		t.Errorf("rule is now bound to %q; a rejected update must not persist (want %q)", boundDefID, ownDefID)
	}

	// ── execution ────────────────────────────────────────────────────────
	// The binding is valid, so the rule fires.
	if _, err := store.TriggerRule(ctx, rule.ID, userID, map[string]string{}); err != nil {
		t.Fatalf("triggering a validly-bound rule: %v", err)
	}

	// Now let the binding drift out from under the rule. Written straight to
	// the column, bypassing the update path, exactly as a row saved before
	// these checks existed would look. ResolveStartContext already rejects an
	// unpublished def at fire time, so the case that actually exercises the
	// execution-time check is a PUBLISHED def in another application: without
	// it, fireRule would happily start another tenant's workflow.
	if _, err := store.Pool().Exec(ctx,
		`UPDATE workflow.automation_rule SET workflow_def_id=$2::uuid WHERE id=$1::uuid`, rule.ID, foreignDef.Id); err != nil {
		t.Fatalf("force cross-app binding: %v", err)
	}
	foreignBefore := countInstances(t, store, foreignDef.Id)
	if _, err := store.TriggerRule(ctx, rule.ID, userID, map[string]string{}); err == nil {
		t.Error("firing a rule whose stored binding names another application's workflow was accepted; want a rejection")
	}
	if after := countInstances(t, store, foreignDef.Id); after != foreignBefore {
		t.Errorf("a rejected fire started %d instance(s) of another application's workflow", after-foreignBefore)
	}
}

func countInstances(t *testing.T, store *workflow.Store, defID string) int {
	t.Helper()
	var n int
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT COUNT(*) FROM workflow.workflow_instance WHERE workflow_def_id=$1::uuid`, defID).Scan(&n); err != nil {
		t.Fatalf("count instances: %v", err)
	}
	return n
}
