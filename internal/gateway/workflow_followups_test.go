package gateway

// Follow-ups from the 2026-09-13 workflow scenario run, each a rule the
// engine now enforces or a capability it now has:
//   - separation of duties covers the automation trigger, not only the
//     direct start endpoint;
//   - a definition can opt out of "one running instance per scope";
//   - integration_completed / integration_failed rules fire from the
//     dispatcher, scoped to one integration or any;
//   - the person whose action could not start a workflow is told why;
//   - business admins count as business users in the business-admin user
//     list, so they can be made members of business roles.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

func TestWorkflowFollowups(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	exec := func(sql string, args ...any) {
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	devSub := "wt-dev2"
	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'dev2@wt.com', 'Dev', $2::uuid) RETURNING id::text`, devSub, f.custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, devID, f.wsID)
	adminOnlySub := "wt-admin-only"
	adminOnlyID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'admin@wt.com', 'Admin', $2::uuid) RETURNING id::text`, adminOnlySub, f.custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_admin', $2::uuid)`, adminOnlyID, f.wsID)
	adminAndUserSub := "wt-admin-user"
	adminAndUserID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'adminuser@wt.com', 'Admin User', $2::uuid) RETURNING id::text`, adminAndUserSub, f.custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_admin', $2::uuid)`, adminAndUserID, f.wsID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, adminAndUserID, f.wsID)

	t.Run("separation of duties also guards the automation trigger", func(t *testing.T) {
		if status, body := f.do(t, "POST", "/api/automation/trigger/"+f.ruleID, adminOnlySub, map[string]any{"payload": map[string]string{}}); status != http.StatusForbidden {
			t.Errorf("approver-only caller through the automation trigger: %d %s (want 403, as on /api/workflow/instances)", status, body)
		}
		if status, body := f.do(t, "POST", "/api/automation/trigger/"+f.ruleID, adminAndUserSub, map[string]any{"payload": map[string]string{}}); status != http.StatusOK {
			t.Errorf("admin who is also a business user: %d %s (want 200)", status, body)
		}
	})

	t.Run("business admins are listed as business users for role membership", func(t *testing.T) {
		req, err := http.NewRequestWithContext(ctx, "GET", f.srv.URL+"/api/business-admin/users", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Dev-User", adminAndUserSub)
		req.Header.Set("X-App-Id", f.appID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf strings.Builder
		if _, err := io.Copy(&buf, resp.Body); err != nil {
			t.Fatal(err)
		}
		body := buf.String()
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "adminuser@wt.com") || !strings.Contains(body, "olga@wt.com") {
			t.Errorf("user list must include business admins and business users: %s", body)
		}
	})

	dimID := q(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'dept', $2::uuid) RETURNING id::text`, f.modelID, f.revID)
	q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'SALES', 'Sales') RETURNING id::text`, dimID)
	devPath := "/api/developer/workflows?application_id=" + f.appID + "&revision_id=" + f.revID
	status, body := f.do(t, "POST", devPath, devSub, map[string]any{"name": "Per request", "trigger_event": "manual"})
	if status != http.StatusOK {
		t.Fatalf("create def: %d %s", status, body)
	}
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	defID, _ := created["id"].(string)
	steps := []map[string]any{{"id": "t1", "name": "Review", "type": "task", "assignee_roles": []string{"Budget Owners"}, "routes": map[string]string{"next": "end-completed"}}}
	ctxSchema := []map[string]any{{"key": "dept", "label": "Department", "data_type": "Dimension member", "required": true, "dimension_id": dimID}}
	if status, body := f.do(t, "PATCH", "/api/developer/workflows/"+defID, devSub, map[string]any{"steps": steps, "context_schema": ctxSchema}); status != http.StatusOK {
		t.Fatalf("patch: %d %s", status, body)
	}
	if status, body := f.do(t, "POST", "/api/developer/workflows/"+defID+"/publish", devSub, nil); status != http.StatusOK {
		t.Fatalf("publish: %d %s", status, body)
	}
	start := func() (int, []byte) {
		return f.do(t, "POST", "/api/workflow/instances", f.ownerSub, map[string]any{"workflow_def_id": defID, "context": map[string]string{"dept": "SALES"}})
	}

	t.Run("single_active_instance is a per-definition choice", func(t *testing.T) {
		if status, body := start(); status != http.StatusOK {
			t.Fatalf("first start: %d %s", status, body)
		}
		if status, body := start(); status != http.StatusConflict {
			t.Errorf("second start with the default (one active per scope): %d %s (want 409)", status, body)
		}
		status, body := f.do(t, "PATCH", "/api/developer/workflows/"+defID, devSub, map[string]any{"single_active_instance": false})
		if status != http.StatusOK || !strings.Contains(string(body), `"single_active_instance":false`) {
			t.Fatalf("turn the flag off: %d %s", status, body)
		}
		if status, body := start(); status != http.StatusOK {
			t.Errorf("second start with the flag off: %d %s (want 200)", status, body)
		}
		_, body = f.do(t, "GET", "/api/developer/workflows/"+defID, devSub, nil)
		if !strings.Contains(string(body), `"single_active_instance":false`) {
			t.Errorf("flag not persisted: %s", body)
		}
	})

	t.Run("integration rules fire from the dispatcher, scoped or unscoped", func(t *testing.T) {
		ws := workflow.NewStore(f.pool)
		integrationA := "11111111-1111-4111-8111-111111111111"
		integrationB := "22222222-2222-4222-8222-222222222222"
		scoped, err := ws.CreateAutomationRuleScoped(ctx, f.appID, f.revID, "On A completed", "", "integration_completed", "", f.wfDefID, "", "", integrationA, nil)
		if err != nil {
			t.Fatalf("scoped rule: %v", err)
		}
		if scoped.SourceIntegrationID != integrationA {
			t.Errorf("scoped rule lost its integration: %+v", scoped)
		}
		anyRule, err := ws.CreateAutomationRuleScoped(ctx, f.appID, f.revID, "On any failed", "", "integration_failed", "", f.wfDefID, "", "", "", nil)
		if err != nil {
			t.Fatalf("unscoped rule: %v", err)
		}
		executions := func(ruleID string) int {
			var n int
			_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM workflow.execution WHERE rule_id = $1::uuid`, ruleID).Scan(&n)
			return n
		}
		waitFor := func(ruleID string, want int) bool {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if executions(ruleID) >= want {
					return true
				}
				time.Sleep(50 * time.Millisecond)
			}
			return executions(ruleID) >= want
		}
		// A completed run of integration B: the scoped (A) rule stays quiet.
		ws.DispatchEventRules(ctx, f.appID, f.revID, "integration_completed", integrationB, f.ownerID, map[string]string{"integration_id": integrationB, "record_count": "3"})
		time.Sleep(300 * time.Millisecond)
		if n := executions(scoped.ID); n != 0 {
			t.Errorf("rule scoped to integration A fired for B: %d executions", n)
		}
		// A completed run of A fires the scoped rule.
		ws.DispatchEventRules(ctx, f.appID, f.revID, "integration_completed", integrationA, f.ownerID, map[string]string{"integration_id": integrationA, "record_count": "3"})
		if !waitFor(scoped.ID, 1) {
			t.Errorf("rule scoped to integration A did not fire for A")
		}
		// A failed run of anything fires the unscoped failed-rule; completed does not.
		if n := executions(anyRule.ID); n != 0 {
			t.Errorf("integration_failed rule fired on a completed run: %d", n)
		}
		ws.DispatchEventRules(ctx, f.appID, f.revID, "integration_failed", integrationB, f.ownerID, map[string]string{"integration_id": integrationB, "error_message": "boom"})
		if !waitFor(anyRule.ID, 1) {
			t.Errorf("unscoped integration_failed rule did not fire")
		}
	})

	t.Run("a refused start notifies the person who triggered it", func(t *testing.T) {
		// Owner already has a running "Per request" instance for SALES from
		// the earlier subtest; turn the flag back on so the next fire is a
		// duplicate, then fire through a rule as the owner.
		if status, body := f.do(t, "PATCH", "/api/developer/workflows/"+defID, devSub, map[string]any{"single_active_instance": true}); status != http.StatusOK {
			t.Fatalf("flag on: %d %s", status, body)
		}
		ws := workflow.NewStore(f.pool)
		rule, err := ws.CreateAutomationRule(ctx, f.appID, f.revID, "Manual per request", "", "manual", "", defID, "", "", nil)
		if err != nil {
			t.Fatalf("rule: %v", err)
		}
		if status, body := f.do(t, "POST", "/api/automation/trigger/"+rule.ID, f.ownerSub, map[string]any{"payload": map[string]string{"dept": "SALES"}}); status == http.StatusOK {
			t.Fatalf("duplicate start through the rule must fail: %d %s", status, body)
		}
		var n int
		if err := f.pool.QueryRow(ctx, `
			SELECT count(*) FROM notification.notification
			WHERE recipient_user_id = $1::uuid AND template_vars->>'subject' = 'Workflow could not start'
			  AND template_vars->>'message' LIKE 'Manual per request:%already running%'
		`, f.ownerID).Scan(&n); err != nil || n != 1 {
			t.Errorf("submitter notification: %d rows (err %v), want exactly 1 saying why the start was refused", n, err)
		}
	})
}
