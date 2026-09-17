// Automation (/api/automation/...) rule CRUD and manual triggering were
// unaudited — only the scheduler's own scheduled-fire path was covered.
// EventAutomationRuleTriggeredManually stays distinct from
// automation_rule.scheduled_fire so manual vs. scheduled firing are each
// independently auditable.
package gateway

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

func TestAutomationMutationsAreAudited(t *testing.T) {
	f := setupDevAuthoringFixture(t)

	// A minimal published workflow def for the automation rule to fire.
	ws := workflow.NewStore(f.pool)
	def, err := ws.CreateWorkflowDefFull(t.Context(), f.appID, "", "Automation Target", "", "manual", f.devIDForAutomation(t))
	if err != nil {
		t.Fatalf("create workflow def: %v", err)
	}
	steps, _ := json.Marshal([]map[string]any{
		{"id": "s1", "name": "Notify", "type": "notification", "config": map[string]any{"subject": "hi", "message": "hi"}},
	})
	if _, err := ws.UpdateWorkflowDefFull(t.Context(), def.ID, def.Name, def.Description, def.TriggerEvent, "none", f.devIDForAutomation(t), steps, []byte("[]"), []byte("{}")); err != nil {
		t.Fatalf("update workflow def steps: %v", err)
	}
	if _, err := ws.PublishWorkflowDef(t.Context(), def.ID, f.devIDForAutomation(t)); err != nil {
		t.Fatalf("publish workflow def: %v", err)
	}

	// ── rule: create, update, trigger, delete ───────────────────────────
	status, body := f.do(t, "POST", "/api/automation/rules", map[string]string{
		"name": "Manual Rule", "trigger_type": "manual", "workflow_def_id": def.ID,
	})
	if status != http.StatusOK {
		t.Fatalf("create rule: status=%d body=%v", status, body)
	}
	ruleID, _ := body["id"].(string)
	if ruleID == "" {
		t.Fatalf("expected rule id, got %v", body)
	}
	if row := f.latestAuditEvent(t, "automation_rule.created"); row.applicationID == nil || *row.applicationID != f.appID {
		t.Errorf("automation_rule.created application_id = %v, want %s", row.applicationID, f.appID)
	}

	status, _ = f.do(t, "PATCH", "/api/automation/rules/"+ruleID, map[string]string{
		"name": "Manual Rule v2", "trigger_type": "manual", "workflow_def_id": def.ID,
	})
	if status != http.StatusOK {
		t.Fatalf("update rule: status=%d", status)
	}
	f.latestAuditEvent(t, "automation_rule.updated")

	status, body = f.do(t, "POST", "/api/automation/trigger/"+ruleID, map[string]any{"payload": map[string]string{}})
	if status != http.StatusOK {
		t.Fatalf("trigger rule: status=%d body=%v", status, body)
	}
	if row := f.latestAuditEvent(t, "automation_rule.triggered_manually"); row.resourceID != ruleID {
		t.Errorf("automation_rule.triggered_manually resource_id = %s, want %s", row.resourceID, ruleID)
	}

	status, _ = f.do(t, "DELETE", "/api/automation/rules/"+ruleID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete rule: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "automation_rule.deleted"); row.resourceID != ruleID {
		t.Errorf("automation_rule.deleted resource_id = %s, want %s", row.resourceID, ruleID)
	}
}

// devIDForAutomation resolves the fixture's developer user id for direct
// workflow.Store calls that bypass HTTP (workflow def setup only).
func (f *devAuthoringFixture) devIDForAutomation(t *testing.T) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(t.Context(), `SELECT id::text FROM identity.user WHERE keycloak_sub='dev-audit-pa'`).Scan(&id); err != nil {
		t.Fatalf("resolve developer id: %v", err)
	}
	return id
}
