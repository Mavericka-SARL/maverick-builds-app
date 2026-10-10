package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// A developer or a business admin stops a running instance (owner request,
// 2026-10-10): its open steps leave the inbox, the instance and the
// execution that started it read "cancelled", and the assignees and the
// person who started it are told — not the canceller. Anyone else is
// refused, another tenant's instance is not found, and an ended run cannot
// be cancelled again.
func TestCancelRunningWorkflowInstance(t *testing.T) {
	f := setupWFRuntimeAuditFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var v string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return v
	}

	// An approval assigned to business admins keeps the run waiting.
	ws := workflow.NewStore(f.pool)
	def, err := ws.CreateWorkflowDefFull(ctx, f.appID, "", "Contributor Submission Approval", "", "manual", f.devID)
	if err != nil {
		t.Fatal(err)
	}
	steps, _ := json.Marshal([]map[string]any{
		{"id": "a1", "name": "Approve submission", "type": "approval", "assignee_roles": []string{"business_admin"}},
	})
	if _, err := ws.UpdateWorkflowDefFull(ctx, def.ID, def.Name, def.Description, def.TriggerEvent, "none", f.devID, steps, []byte("[]"), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.PublishWorkflowDef(ctx, def.ID, f.devID); err != nil {
		t.Fatal(err)
	}
	rule, err := ws.CreateAutomationRule(ctx, f.appID, "", "Start from dashboard", "", "manual", "", def.ID, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := func(by string) (instanceID, executionID string) {
		t.Helper()
		exec, err := ws.TriggerRule(ctx, rule.ID, by, nil)
		if err != nil {
			t.Fatalf("trigger: %v", err)
		}
		if exec.Status != "running" || exec.InstanceID == "" {
			t.Fatalf("execution %+v, want a running one with an instance", exec)
		}
		return exec.InstanceID, exec.ID
	}
	statusOf := func(instanceID, executionID string) string {
		t.Helper()
		return q(`SELECT wi.status::text || '/' || e.status::text || '/' ||
			(SELECT count(*) FROM workflow.workflow_step WHERE instance_id = wi.id AND status IN ('pending','in_progress'))
			FROM workflow.workflow_instance wi JOIN workflow.execution e ON e.instance_id = wi.id
			WHERE wi.id = $1::uuid AND e.id = $2::uuid`, instanceID, executionID)
	}
	cancelNotes := func(userID, instanceID string) []string {
		t.Helper()
		rows, err := f.pool.Query(ctx, `
			SELECT template_vars->>'message' FROM notification.notification
			WHERE recipient_user_id = $1::uuid AND template_id = $2 AND resource_id = $3 AND channel = 'in_app'`,
			userID, workflow.CancelledTemplate, instanceID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var m string
			_ = rows.Scan(&m)
			out = append(out, m)
		}
		return out
	}

	// An application opens for a developer through its models.
	q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Budget') RETURNING id::text`, f.appID)

	// A business user, and a developer of another tenant.
	custID := q(`SELECT customer_id::text FROM core.application WHERE id = $1::uuid`, f.appID)
	wsID := q(`SELECT workspace_id::text FROM core.application WHERE id = $1::uuid`, f.appID)
	userID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('wfcancel-user', 'user@wfruntime.com', 'User', $1::uuid) RETURNING id::text`, custID)
	q(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid) RETURNING user_id::text`, userID, wsID)
	otherCust := q(`INSERT INTO core.customer (name, plan) VALUES ('Elsewhere', 'enterprise') RETURNING id::text`)
	otherWS := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, otherCust)
	otherDev := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('wfcancel-other-dev', 'dev@elsewhere.com', 'Other', $1::uuid) RETURNING id::text`, otherCust)
	q(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid) RETURNING user_id::text`, otherDev, otherWS)

	inst, exec := start(f.devID)
	if got := statusOf(inst, exec); got != "running/running/1" {
		t.Fatalf("before: %s", got)
	}
	if status, body := f.do(t, "POST", "/api/workflow/instances/"+inst+"/cancel", "wfcancel-user", nil); status != http.StatusForbidden {
		t.Errorf("business user: %d %v, want 403", status, body)
	}
	if status, body := f.do(t, "POST", "/api/workflow/instances/"+inst+"/cancel", "wfcancel-other-dev", nil); status != http.StatusNotFound {
		t.Errorf("another tenant's developer: %d %v, want 404", status, body)
	}

	// The developer who started it cancels it, with a reason.
	status, body := f.do(t, "POST", "/api/workflow/instances/"+inst+"/cancel", f.devSub, map[string]string{"reason": "started by mistake"})
	if status != http.StatusOK || body["notified"] != float64(1) {
		t.Fatalf("developer cancel: %d %v, want 200 notifying the one assignee", status, body)
	}
	if got := statusOf(inst, exec); got != "cancelled/cancelled/0" {
		t.Errorf("after: %s, want instance and execution cancelled and no open step", got)
	}
	if tasks, err := ws.GetPendingTasks(ctx, f.baID, f.appID); err != nil || len(tasks) != 0 {
		t.Errorf("the assignee's inbox still holds %d task(s) (%v)", len(tasks), err)
	}
	if notes := cancelNotes(f.baID, inst); len(notes) != 1 || !strings.Contains(notes[0], "started by mistake") || !strings.Contains(notes[0], "Dev") {
		t.Errorf("assignee's notifications %v, want one naming the canceller and the reason", notes)
	}
	if notes := cancelNotes(f.devID, inst); len(notes) != 0 {
		t.Errorf("the canceller was notified of their own action: %v", notes)
	}
	if row := f.latestAuditEvent(t, "workflow_instance.cancelled"); row.resourceID != inst {
		t.Errorf("audit row %+v, want resource %s", row, inst)
	}
	if status, body := f.do(t, "POST", "/api/workflow/instances/"+inst+"/cancel", f.devSub, nil); status != http.StatusConflict {
		t.Errorf("cancelling an ended run: %d %v, want 409", status, body)
	}

	// A business admin cancels a run a developer started: the developer is
	// told; the admin, its only assignee, is the canceller.
	inst, exec = start(f.devID)
	if status, body := f.do(t, "POST", "/api/workflow/instances/"+inst+"/cancel", f.baSub, nil); status != http.StatusOK || body["notified"] != float64(1) {
		t.Fatalf("business admin cancel: %d %v", status, body)
	}
	if got := statusOf(inst, exec); got != "cancelled/cancelled/0" {
		t.Errorf("after the business admin's cancel: %s", got)
	}
	if notes := cancelNotes(f.devID, inst); len(notes) != 1 {
		t.Errorf("the starter's notifications %v, want one", notes)
	}

	// Business Admin › History's status override cancels the same way.
	inst, exec = start(f.devID)
	if status, body := f.do(t, "PATCH", "/api/workflow/instances/"+inst, f.baSub, map[string]string{"status": "cancelled"}); status != http.StatusOK {
		t.Fatalf("override: %d %v", status, body)
	}
	if got := statusOf(inst, exec); got != "cancelled/cancelled/0" {
		t.Errorf("after the override: %s", got)
	}
	if notes := cancelNotes(f.devID, inst); len(notes) != 1 {
		t.Errorf("the override told the starter %d time(s), want once", len(notes))
	}
}
