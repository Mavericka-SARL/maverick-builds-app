// Test for a real access-control gap: ResolveStartContext is the one
// shared gate for both manual workflow starts (workflowStartInstance) and
// every automation-rule fire (fireRule/TriggerRule, including the
// dashboard automation_button) — but a non-RACI "Dimension member"
// context var used to be copied straight from the caller-supplied
// candidate map with no check at all. A user hidden from a dimension
// member could start (and, via an on_approve action, eventually write
// facts for) an instance scoped to it, even though the equivalent direct
// grid write is rejected.
package workflow_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

func TestResolveStartContextRejectsHiddenDimensionMember(t *testing.T) {
	f := setupOnApproveFixture(t)
	ctx := context.Background()

	// setupOnApproveFixture never publishes its workflow def (its own
	// tests call StartWorkflow directly, bypassing ResolveStartContext's
	// publish check entirely) — ResolveStartContext requires it.
	if _, err := f.store.PublishWorkflowDef(ctx, f.wfDefID, f.userID); err != nil {
		t.Fatalf("publish workflow def: %v", err)
	}

	var deptAID string
	if err := f.pool.QueryRow(ctx,
		`SELECT id::text FROM model.dimension_member WHERE code='DEPT_A'`,
	).Scan(&deptAID); err != nil {
		t.Fatalf("find DEPT_A: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'hidden')`,
		f.userID, deptAID,
	); err != nil {
		t.Fatalf("seed hidden rule: %v", err)
	}

	_, err := f.store.ResolveStartContext(ctx, f.wfDefID, f.userID, map[string]string{
		"model_id":           f.modelID,
		"revision_id":        f.workingRevID,
		"target_revision_id": f.targetRevID,
		"scope":              "DEPT_A",
	})
	if !errors.Is(err, workflow.ErrHiddenScope) {
		t.Errorf("ResolveStartContext error = %v, want workflow.ErrHiddenScope", err)
	}

	// Sanity: a user with no restriction still resolves normally.
	otherUserID := ""
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('other-starter', 'other@t.com', 'Other') RETURNING id::text`,
	).Scan(&otherUserID); err != nil {
		t.Fatalf("seed other user: %v", err)
	}
	result, err := f.store.ResolveStartContext(ctx, f.wfDefID, otherUserID, map[string]string{
		"model_id":           f.modelID,
		"revision_id":        f.workingRevID,
		"target_revision_id": f.targetRevID,
		"scope":              "DEPT_A",
	})
	if err != nil {
		t.Fatalf("ResolveStartContext (unrestricted user): %v", err)
	}
	if result["scope"] != "DEPT_A" {
		t.Errorf("resolved scope = %q, want DEPT_A", result["scope"])
	}
}
