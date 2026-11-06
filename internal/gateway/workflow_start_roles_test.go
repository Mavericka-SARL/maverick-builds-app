package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestWorkflowStartRoles: a workflow names who may start it by hand. Someone
// else is refused on the start endpoint and the automation trigger, and the
// inbox's list and the dashboard button's rule say so beforehand; naming
// nobody lets anyone start it again.
func TestWorkflowStartRoles(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	devSub := "wt-dev-starters"
	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'starters@wt.com', 'Dev', $2::uuid) RETURNING id::text`, devSub, f.custID)
	q(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid) RETURNING user_id::text`, devID, f.wsID)
	defID := q(`SELECT wd.id::text FROM workflow.automation_rule r JOIN workflow.workflow_def wd
		ON wd.id = r.workflow_def_id OR (r.workflow_def_id IS NULL AND wd.application_id = r.application_id AND wd.name = r.workflow_name)
		WHERE r.id = $1::uuid LIMIT 1`, f.ruleID)
	// Several runs at once: the test is about who starts one.
	q(`UPDATE workflow.workflow_def SET single_active_instance = false WHERE id = $1::uuid RETURNING id::text`, defID)

	setStarters := func(roles []string, want string) {
		t.Helper()
		status, body := f.do(t, "PATCH", "/api/developer/workflows/"+defID+"?application_id="+f.appID, devSub, map[string]any{"start_roles": roles})
		if status != http.StatusOK || !strings.Contains(string(body), `"start_roles":`+want) {
			t.Fatalf("set start_roles %q: %d %s, want %s", roles, status, body, want)
		}
		if got := q(`SELECT start_roles::text FROM workflow.workflow_def WHERE id = $1::uuid`, defID); got != want {
			t.Errorf("stored start_roles = %s, want %s", got, want)
		}
	}
	mayStart := func(sub string) bool {
		t.Helper()
		status, body := f.do(t, "GET", "/api/workflow/definitions", sub, nil)
		if status != http.StatusOK {
			t.Fatalf("definitions as %s: %d %s", sub, status, body)
		}
		var defs []struct {
			ID       string `json:"id"`
			MayStart bool   `json:"may_start"`
		}
		_ = json.Unmarshal(body, &defs)
		for _, d := range defs {
			if d.ID == defID {
				return d.MayStart
			}
		}
		t.Fatalf("definitions as %s do not list the workflow: %s", sub, body)
		return false
	}
	mayRun := func(sub string) bool {
		t.Helper()
		status, body := f.do(t, "GET", "/api/automation/rules", sub, nil)
		if status != http.StatusOK {
			t.Fatalf("rules as %s: %d %s", sub, status, body)
		}
		var rules []struct {
			ID     string `json:"id"`
			MayRun bool   `json:"may_run"`
		}
		_ = json.Unmarshal(body, &rules)
		for _, r := range rules {
			if r.ID == f.ruleID {
				return r.MayRun
			}
		}
		t.Fatalf("rules as %s do not list the rule: %s", sub, body)
		return false
	}

	setStarters([]string{" Budget Owners ", "Budget Owners", ""}, `["Budget Owners"]`)
	if !mayStart(f.ownerSub) || !mayRun(f.ownerSub) {
		t.Error("a Budget Owner is not offered the workflow")
	}
	if mayStart(f.bystanderSub) || mayRun(f.bystanderSub) {
		t.Error("someone outside Budget Owners is offered the workflow")
	}
	if status, body := f.do(t, "POST", "/api/automation/trigger/"+f.ruleID, f.bystanderSub, map[string]any{"payload": map[string]string{}}); status != http.StatusForbidden || !strings.Contains(string(body), "is started by Budget Owners") {
		t.Errorf("trigger by someone outside the start roles: %d %s, want 403 naming who starts it", status, body)
	}
	if status, body := f.do(t, "POST", "/api/workflow/instances", f.bystanderSub, map[string]any{"workflow_def_id": defID}); status != http.StatusForbidden {
		t.Errorf("start by someone outside the start roles: %d %s, want 403", status, body)
	}
	if status, body := f.do(t, "POST", "/api/automation/trigger/"+f.ruleID, f.ownerSub, map[string]any{"payload": map[string]string{}}); status != http.StatusOK {
		t.Errorf("trigger by a Budget Owner: %d %s, want 200", status, body)
	}

	setStarters([]string{}, `[]`)
	if !mayStart(f.bystanderSub) || !mayRun(f.bystanderSub) {
		t.Error("with no start roles, anyone is no longer offered the workflow")
	}
	if status, body := f.do(t, "POST", "/api/automation/trigger/"+f.ruleID, f.bystanderSub, map[string]any{"payload": map[string]string{}}); status != http.StatusOK {
		t.Errorf("with no start roles, a trigger by anyone: %d %s, want 200", status, body)
	}
}
