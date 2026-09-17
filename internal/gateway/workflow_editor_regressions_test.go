package gateway

// Regression tests for defects found on 2026-09-10 while click-testing the
// workflow constructor against the live dev stack:
//
//  1. A PATCH carrying only `steps` wiped name, description, trigger_event
//     and subject_type (the store's UPDATE set them unconditionally, the
//     handler decoded absent strings as ""). The editor's Validate sent
//     exactly such a PATCH, so validating an unsaved draft erased the
//     workflow's name, trigger and subject.
//  2. Validate had no way to check an unsaved draft without persisting it.
//  3. A notification step addressed to "Specific role" with no role passed
//     validation and would notify nobody at runtime.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestWorkflowPatchWithOnlyStepsKeepsNameTriggerAndSubject(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE workflow.workflow_def SET description='keep me', subject_type='form_record', subject_config='{"form_id":"f-1"}'::jsonb WHERE id=$1::uuid`, f.wfDefID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	steps := []map[string]any{{"id": "s1", "name": "Only step", "type": "task", "assignee_roles": []string{"business_admin"}, "routes": map[string]string{"next": "end-completed"}}}
	status, body := f.do(t, "PATCH", "/api/developer/workflows/"+f.wfDefID, "rollup-test-approver", map[string]any{"steps": steps})
	if status != http.StatusOK {
		t.Fatalf("patch status = %d, body = %v", status, body)
	}
	var name, description, trigger, subjectType string
	if err := f.pool.QueryRow(ctx, `SELECT name, COALESCE(description,''), trigger_event, COALESCE(subject_type,'') FROM workflow.workflow_def WHERE id=$1::uuid`, f.wfDefID).Scan(&name, &description, &trigger, &subjectType); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if name != "Test Approval" || description != "keep me" || trigger == "" || subjectType != "form_record" {
		t.Errorf("steps-only PATCH changed other fields: name=%q description=%q trigger=%q subject_type=%q", name, description, trigger, subjectType)
	}
	if got, _ := body["name"].(string); got != "Test Approval" {
		t.Errorf("response name = %q, want Test Approval", got)
	}
}

func TestWorkflowValidateDraftBodyDoesNotPersist(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	var before string
	if err := f.pool.QueryRow(ctx, `SELECT steps::text FROM workflow.workflow_def WHERE id=$1::uuid`, f.wfDefID).Scan(&before); err != nil {
		t.Fatalf("read: %v", err)
	}
	// A draft with an unassigned task: the error must be about the DRAFT.
	draft := map[string]any{"name": "Draft name", "steps": []map[string]any{{"id": "d1", "name": "Unassigned task", "type": "task", "assignee_roles": []string{}, "routes": map[string]string{}}}}
	status, res := f.do(t, "POST", "/api/developer/workflows/"+f.wfDefID+"/validate", "rollup-test-approver", draft)
	if status != http.StatusOK {
		t.Fatalf("validate status = %d, body = %v", status, res)
	}
	errs, _ := res["errors"].([]any)
	joined := fmt.Sprint(errs)
	if !strings.Contains(joined, "Unassigned task") {
		t.Errorf("validate did not check the posted draft: errors = %v", errs)
	}
	var after, name string
	if err := f.pool.QueryRow(ctx, `SELECT steps::text, name FROM workflow.workflow_def WHERE id=$1::uuid`, f.wfDefID).Scan(&after, &name); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after != before || name != "Test Approval" {
		t.Errorf("validate persisted the draft: steps changed=%v name=%q", after != before, name)
	}
	// Without a body it still validates the stored definition.
	status, res = f.do(t, "POST", "/api/developer/workflows/"+f.wfDefID+"/validate", "rollup-test-approver", nil)
	if status != http.StatusOK {
		t.Fatalf("validate (stored) status = %d, body = %v", status, res)
	}
}

func TestWorkflowValidateFlagsNotificationToRoleWithoutRole(t *testing.T) {
	f := setupRollupFixture(t)
	mk := func(recipientRole string) map[string]any {
		return map[string]any{"name": "N", "steps": []map[string]any{{
			"id": "n1", "name": "Tell finance", "type": "notification", "routes": map[string]string{"next": "end-completed"},
			"notification": map[string]any{"recipient_type": "role", "recipient_role": recipientRole, "subject": "s", "message": "m"},
		}}}
	}
	status, res := f.do(t, "POST", "/api/developer/workflows/"+f.wfDefID+"/validate", "rollup-test-approver", mk(""))
	if status != http.StatusOK {
		t.Fatalf("validate status = %d, body = %v", status, res)
	}
	if errs, _ := json.Marshal(res["errors"]); !strings.Contains(string(errs), "no recipient role") {
		t.Errorf("notification to a role without a role passed validation: %s", errs)
	}
	status, res = f.do(t, "POST", "/api/developer/workflows/"+f.wfDefID+"/validate", "rollup-test-approver", mk("business_admin"))
	if status != http.StatusOK {
		t.Fatalf("validate status = %d, body = %v", status, res)
	}
	if errs, _ := json.Marshal(res["errors"]); strings.Contains(string(errs), "recipient role") {
		t.Errorf("notification with a role must not be flagged: %s", errs)
	}
}

func TestWorkflowArchiveCanBeRestoredToDraft(t *testing.T) {
	f := setupRollupFixture(t)
	status, res := f.do(t, "POST", "/api/developer/workflows/"+f.wfDefID+"/archive", "rollup-test-approver", nil)
	if status != http.StatusOK || res["status"] != "archived" {
		t.Fatalf("archive status = %d, body = %v", status, res)
	}
	status, res = f.do(t, "POST", "/api/developer/workflows/"+f.wfDefID+"/restore", "rollup-test-approver", nil)
	if status != http.StatusOK || res["status"] != "draft" {
		t.Fatalf("restore status = %d, body = %v (want 200 + draft)", status, res)
	}
	// Restoring something that is not archived is refused, not silently ignored.
	if status, _ = f.do(t, "POST", "/api/developer/workflows/"+f.wfDefID+"/restore", "rollup-test-approver", nil); status != http.StatusBadRequest {
		t.Errorf("restore of a draft status = %d, want 400", status)
	}
}
