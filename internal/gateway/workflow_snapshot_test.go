package gateway

// A running instance keeps the definition it started with. The engine used
// to re-read workflow_def.steps on every completion, so editing a published
// workflow changed routes, assignees and conditions of in-flight instances.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestRunningInstanceKeepsItsDefinitionSnapshot(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()
	devSub := "wt-dev3"
	var devID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'dev3@wt.com', 'Dev', $2::uuid) RETURNING id::text`, devSub, f.custID).Scan(&devID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, devID, f.wsID); err != nil {
		t.Fatal(err)
	}
	status, body := f.do(t, "POST", "/api/developer/workflows?application_id="+f.appID+"&revision_id="+f.revID, devSub, map[string]any{"name": "Snapshot", "trigger_event": "manual"})
	if status != http.StatusOK {
		t.Fatalf("create: %d %s", status, body)
	}
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	defID, _ := created["id"].(string)
	stepsV1 := []map[string]any{{"id": "t1", "name": "Owner review", "type": "task", "assignee_roles": []string{"Budget Owners"}, "routes": map[string]string{"next": "end-completed"}}}
	if status, body := f.do(t, "PATCH", "/api/developer/workflows/"+defID, devSub, map[string]any{"steps": stepsV1}); status != http.StatusOK {
		t.Fatalf("patch v1: %d %s", status, body)
	}
	if status, body := f.do(t, "POST", "/api/developer/workflows/"+defID+"/publish", devSub, nil); status != http.StatusOK {
		t.Fatalf("publish: %d %s", status, body)
	}
	if status, body := f.do(t, "POST", "/api/workflow/instances", f.ownerSub, map[string]any{"workflow_def_id": defID, "context": map[string]string{}}); status != http.StatusOK {
		t.Fatalf("start v1: %d %s", status, body)
	}
	tasksOf := func(sub string) []map[string]any {
		_, body := f.do(t, "GET", "/api/tasks", sub, nil)
		var all, out []map[string]any
		_ = json.Unmarshal(body, &all)
		for _, task := range all {
			if task["workflow_name"] == "Snapshot" {
				out = append(out, task)
			}
		}
		return out
	}
	if got := tasksOf(f.ownerSub); len(got) != 1 || got[0]["step_name"] != "Owner review" {
		t.Fatalf("v1 instance in the owner's inbox: %v", got)
	}

	// Edit the PUBLISHED definition: the step is renamed and reassigned.
	stepsV2 := []map[string]any{{"id": "t1", "name": "Approver review", "type": "task", "assignee_roles": []string{"Budget Approvers"}, "routes": map[string]string{"next": "end-completed"}}}
	if status, body := f.do(t, "PATCH", "/api/developer/workflows/"+defID, devSub, map[string]any{"steps": stepsV2}); status != http.StatusOK {
		t.Fatalf("patch v2: %d %s", status, body)
	}

	// The running instance is untouched: still the owner's task, still its name.
	got := tasksOf(f.ownerSub)
	if len(got) != 1 || got[0]["step_name"] != "Owner review" {
		t.Errorf("after editing the published definition the running instance changed: %v (want the v1 snapshot)", got)
	}
	if got := tasksOf(f.approverSub); len(got) != 0 {
		t.Errorf("the running instance's task moved to the new assignee: %v", got)
	}
	// And the owner can still complete it (eligibility from the snapshot).
	if len(got) == 1 {
		if status, body := f.do(t, "POST", "/api/tasks/"+got[0]["id"].(string)+"/complete", f.ownerSub, map[string]any{"decision": "complete", "comment": ""}); status != http.StatusOK {
			t.Errorf("owner completing the v1 task: %d %s", status, body)
		}
	}

	// A new start uses the edited definition.
	if status, body := f.do(t, "POST", "/api/workflow/instances", f.approverSub, map[string]any{"workflow_def_id": defID, "context": map[string]string{}}); status != http.StatusOK {
		t.Fatalf("start v2: %d %s", status, body)
	}
	if got := tasksOf(f.approverSub); len(got) != 1 || got[0]["step_name"] != "Approver review" {
		t.Errorf("v2 instance: %v (want 'Approver review' for the approver)", got)
	}
	if got := tasksOf(f.ownerSub); len(got) != 0 {
		t.Errorf("v2 instance leaked to the old assignee: %v", got)
	}
}
