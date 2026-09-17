package gateway

import (
	"context"
	"net/http"
	"testing"
)

// Tests for a real access-control bypass found in the functionality/
// user-roles-model audit: /api/automation/rules* and
// /api/automation/trigger/{id} had zero role check AND zero ownership
// check — any authenticated user, of any role, in any tenant, could
// create/edit/delete automation rules wired to any published workflow,
// and fire any rule by guessing its UUID. Reuses setupWTFixture from
// workflow_trigger_business_test.go (same package) — a real published
// workflow + manual automation rule + a "bystander" business user with
// no relevant grants.

func TestAutomationRuleManagementRequiresDeveloperRole(t *testing.T) {
	f := setupWTFixture(t)

	create := map[string]any{"name": "Bystander's Rule", "trigger_type": "manual", "workflow_def_id": f.wfDefID}
	if status, body := f.do(t, "POST", "/api/automation/rules", f.bystanderSub, create); status != http.StatusForbidden {
		t.Errorf("bystander (business_user) POST /api/automation/rules: status=%d, want 403, body=%s", status, body)
	}

	update := map[string]any{"name": "Renamed"}
	if status, body := f.do(t, "PATCH", "/api/automation/rules/"+f.ruleID, f.bystanderSub, update); status != http.StatusForbidden {
		t.Errorf("bystander PATCH /api/automation/rules/{id}: status=%d, want 403, body=%s", status, body)
	}

	if status, body := f.do(t, "DELETE", "/api/automation/rules/"+f.ruleID, f.bystanderSub, nil); status != http.StatusForbidden {
		t.Errorf("bystander DELETE /api/automation/rules/{id}: status=%d, want 403, body=%s", status, body)
	}

	// The rule must still exist and be unmodified — a 403 that
	// nonetheless partially applied the change would be worse than no
	// check at all.
	var name string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT name FROM workflow.automation_rule WHERE id=$1::uuid`, f.ruleID,
	).Scan(&name); err != nil {
		t.Fatalf("rule missing after rejected DELETE: %v", err)
	}
	if name != "Start Budget Approval" {
		t.Errorf("rule name = %q, want unchanged %q — a 403 PATCH must not have partially applied", name, "Start Budget Approval")
	}
}

// foreignDeveloper creates a real developer user under a wholly separate
// core.customer (tenant) from the fixture's own. actorCanAccessApp
// (internal/gateway/handler.go:808) joins on
// "w.id=app.workspace_id OR w.customer_id=app.customer_id" — i.e.
// *customer* is the real isolation boundary here, not workspace; a
// developer in a different workspace of the SAME customer legitimately
// has cross-app reach by that same join, so this deliberately uses a
// different customer, not just a different workspace, to test real
// tenant isolation. f.do always sends X-App-Id: f.appID (the rule's real
// app), so a request against f.appID's resources from this user proves
// the ownership check, not just the role check, is doing real work.
func foreignDeveloper(t *testing.T, f *wtFixture) string {
	t.Helper()
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	foreignCustID := q(`INSERT INTO core.customer (name, plan) VALUES ('Foreign Tenant', 'enterprise') RETURNING id::text`)
	foreignWsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'foreign-ws') RETURNING id::text`, foreignCustID)
	sub := "wt-foreign-dev"
	userID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'foreign-dev@wt.com', 'Foreign Dev', $2::uuid) RETURNING id::text`, sub, foreignCustID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, userID, foreignWsID)
	return sub
}

func TestAutomationRuleActionRejectsForeignApp(t *testing.T) {
	f := setupWTFixture(t)
	foreignSub := foreignDeveloper(t, f)

	update := map[string]any{"name": "Hijacked"}
	if status, body := f.do(t, "PATCH", "/api/automation/rules/"+f.ruleID, foreignSub, update); status != http.StatusForbidden && status != http.StatusNotFound {
		t.Errorf("foreign developer PATCH /api/automation/rules/{id}: status=%d, want 403 or 404, body=%s", status, body)
	}
	if status, body := f.do(t, "DELETE", "/api/automation/rules/"+f.ruleID, foreignSub, nil); status != http.StatusForbidden && status != http.StatusNotFound {
		t.Errorf("foreign developer DELETE /api/automation/rules/{id}: status=%d, want 403 or 404, body=%s", status, body)
	}

	var name string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT name FROM workflow.automation_rule WHERE id=$1::uuid`, f.ruleID,
	).Scan(&name); err != nil {
		t.Fatalf("rule missing after rejected requests: %v", err)
	}
	if name != "Start Budget Approval" {
		t.Errorf("rule name = %q, want unchanged %q", name, "Start Budget Approval")
	}
}

func TestAutomationTriggerRejectsForeignApp(t *testing.T) {
	f := setupWTFixture(t)
	foreignSub := foreignDeveloper(t, f)

	status, body := f.do(t, "POST", "/api/automation/trigger/"+f.ruleID, foreignSub, map[string]any{"payload": map[string]string{}})
	if status != http.StatusForbidden && status != http.StatusNotFound {
		t.Errorf("foreign developer POST /api/automation/trigger/{id}: status=%d, want 403 or 404, body=%s", status, body)
	}

	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM workflow.workflow_instance WHERE workflow_def_id=$1::uuid`, f.wfDefID,
	).Scan(&n); err != nil {
		t.Fatalf("count instances: %v", err)
	}
	if n != 0 {
		t.Errorf("foreign developer's rejected trigger still started %d workflow instance(s), want 0", n)
	}
}
