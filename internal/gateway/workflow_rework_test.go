package gateway

// Rework loops: a reject that routes back to an earlier task re-activates
// it (with the reviewer's comment as a note) and resets what follows, so
// the request goes round again instead of dead-ending.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestRejectSendsTheDraftBackForRework(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()
	devSub := "wt-dev4"
	var devID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'dev4@wt.com', 'Dev', $2::uuid) RETURNING id::text`, devSub, f.custID).Scan(&devID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, devID, f.wsID); err != nil {
		t.Fatal(err)
	}
	status, body := f.do(t, "POST", "/api/developer/workflows?application_id="+f.appID+"&revision_id="+f.revID, devSub, map[string]any{"name": "Rework", "trigger_event": "manual"})
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	defID, _ := created["id"].(string)
	steps := []map[string]any{
		{"id": "draft", "name": "Draft", "type": "task", "assignee_roles": []string{"Budget Owners"}, "routes": map[string]string{"next": "review"}},
		{"id": "review", "name": "Review", "type": "approval", "assignee_roles": []string{"Budget Approvers"}, "routes": map[string]string{"approve": "notify", "reject": "draft"}},
		{"id": "notify", "name": "Done", "type": "notification", "notification": map[string]any{"recipient_type": "requester", "subject": "Approved", "message": "ok"}, "routes": map[string]string{"next": "end-completed"}},
	}
	if status, body := f.do(t, "PATCH", "/api/developer/workflows/"+defID, devSub, map[string]any{"steps": steps}); status != http.StatusOK {
		t.Fatalf("patch: %d %s", status, body)
	}
	if status, body := f.do(t, "POST", "/api/developer/workflows/"+defID+"/publish", devSub, nil); status != http.StatusOK {
		t.Fatalf("publish (a loop through a task must validate): %d %s", status, body)
	}
	status, body = f.do(t, "POST", "/api/workflow/instances", f.ownerSub, map[string]any{"workflow_def_id": defID, "context": map[string]string{}})
	if status != http.StatusOK {
		t.Fatalf("start: %d %s", status, body)
	}
	var started map[string]any
	_ = json.Unmarshal(body, &started)
	instanceID, _ := started["instance_id"].(string)
	task := func(sub, stepDef string) map[string]any {
		_, body := f.do(t, "GET", "/api/tasks", sub, nil)
		var all []map[string]any
		_ = json.Unmarshal(body, &all)
		for _, x := range all {
			if x["instance_id"] == instanceID && x["step_def_id"] == stepDef {
				return x
			}
		}
		return nil
	}
	complete := func(sub string, task map[string]any, decision, comment string) {
		t.Helper()
		if status, body := f.do(t, "POST", "/api/tasks/"+task["id"].(string)+"/complete", sub, map[string]any{"decision": decision, "comment": comment}); status != http.StatusOK {
			t.Fatalf("%s %s: %d %s", decision, task["step_def_id"], status, body)
		}
	}
	draft := task(f.ownerSub, "draft")
	if draft == nil {
		t.Fatal("draft not in the owner's inbox")
	}
	complete(f.ownerSub, draft, "complete", "first attempt")
	review := task(f.approverSub, "review")
	if review == nil {
		t.Fatal("review not in the approver's inbox")
	}
	complete(f.approverSub, review, "reject", "numbers are off")

	// Round two: the draft is back with the owner, carrying the reason; the
	// review is pending again; the instance is still running.
	draft = task(f.ownerSub, "draft")
	if draft == nil {
		t.Fatal("after the reject the draft must be back in the owner's inbox")
	}
	if draft["rework_count"] != float64(1) || !strings.Contains(draft["rework_note"].(string), "numbers are off") || !strings.Contains(draft["rework_note"].(string), `"Review"`) {
		t.Errorf("rework note: count=%v note=%q", draft["rework_count"], draft["rework_note"])
	}
	if task(f.approverSub, "review") != nil {
		t.Errorf("the review must not be in the approver's inbox while the draft is being reworked")
	}
	var instStatus, reviewStatus string
	_ = f.pool.QueryRow(ctx, `SELECT status::text FROM workflow.workflow_instance WHERE id=$1::uuid`, instanceID).Scan(&instStatus)
	_ = f.pool.QueryRow(ctx, `SELECT status::text FROM workflow.workflow_step WHERE instance_id=$1::uuid AND step_def_id='review'`, instanceID).Scan(&reviewStatus)
	if instStatus != "running" || reviewStatus != "pending" {
		t.Errorf("instance=%s review=%s, want running / pending", instStatus, reviewStatus)
	}
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM notification.notification WHERE recipient_user_id=$1::uuid AND template_vars->>'subject'='Sent back for rework'`, f.ownerID).Scan(&n)
	if n != 1 {
		t.Errorf("requester notification about the rework: %d, want 1", n)
	}

	// Round two completes: fix, approve, done.
	complete(f.ownerSub, draft, "complete", "fixed")
	review = task(f.approverSub, "review")
	if review == nil {
		t.Fatal("review not back in the approver's inbox after the fix")
	}
	complete(f.approverSub, review, "approve", "")
	_ = f.pool.QueryRow(ctx, `SELECT status::text FROM workflow.workflow_instance WHERE id=$1::uuid`, instanceID).Scan(&instStatus)
	if instStatus != "completed" {
		t.Errorf("after approve the instance is %s, want completed", instStatus)
	}
}
