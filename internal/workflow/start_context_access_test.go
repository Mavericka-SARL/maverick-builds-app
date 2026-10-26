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
	"strings"
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

// An instance locks its member and everything under it, so a start is
// refused when the caller may not write all of that: a Read rule on the
// member itself, or a Read or Hidden rule on a member under it
// (2026-10-25: a regional manager with Read on AMERICAS started its
// budget approval, and the Americas owner's writes were refused).
func TestResolveStartContextRejectsScopeTheCallerMayNotWrite(t *testing.T) {
	f := setupOnApproveFixture(t)
	ctx := context.Background()
	if _, err := f.store.PublishWorkflowDef(ctx, f.wfDefID, f.userID); err != nil {
		t.Fatalf("publish workflow def: %v", err)
	}
	memberID := func(code string) string {
		var id string
		if err := f.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE code=$1`, code).Scan(&id); err != nil {
			t.Fatalf("find %s: %v", code, err)
		}
		return id
	}
	start := func(userID string) error {
		_, err := f.store.ResolveStartContext(ctx, f.wfDefID, userID, map[string]string{
			"model_id":           f.modelID,
			"revision_id":        f.workingRevID,
			"target_revision_id": f.targetRevID,
			"scope":              "DEPT_A",
		})
		return err
	}
	for _, tc := range []struct{ name, code, access, want string }{
		{"read on the scope", "DEPT_A", "read", "departments DEPT_A is read-only for you"},
		{"read under the scope", "STAFF_A1", "read", "STAFF_A1, inside this scope, is read-only for you"},
		{"hidden under the scope", "STAFF_A1", "hidden", "inside this scope is hidden from you"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var userID string
			if err := f.pool.QueryRow(ctx,
				`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1, $1 || '@t.com', $1) RETURNING id::text`,
				"scope-"+tc.access+"-"+tc.code,
			).Scan(&userID); err != nil {
				t.Fatalf("seed user: %v", err)
			}
			if _, err := f.pool.Exec(ctx,
				`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, $3)`,
				userID, memberID(tc.code), tc.access,
			); err != nil {
				t.Fatalf("seed rule: %v", err)
			}
			err := start(userID)
			if !errors.Is(err, workflow.ErrReadOnlyScope) {
				t.Fatalf("ResolveStartContext error = %v, want workflow.ErrReadOnlyScope", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
		})
	}
}
