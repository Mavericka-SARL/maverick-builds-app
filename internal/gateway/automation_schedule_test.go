package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// A developer schedules a manual workflow through the same HTTP body the
// Triggers tab sends (web/src/consoles/developer/AutomationTab.tsx): the rule
// comes back with its cron, zone, misfire policy and a next fire time, and
// the list the tab renders carries them too.
func TestAutomationRuleScheduleOverHTTP(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()
	var userID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('wt-dev', 'dev@wt.com', 'Dev', $1::uuid) RETURNING id::text`, f.custID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, userID, f.wsID); err != nil {
		t.Fatal(err)
	}

	missing := map[string]any{"name": "No cron", "trigger_type": "schedule", "workflow_def_id": f.wfDefID}
	if status, body := f.do(t, "POST", "/api/automation/rules", "wt-dev", missing); status < 400 {
		t.Errorf("schedule rule without cron_expr: %d %s, want an error", status, body)
	}

	create := map[string]any{
		"name": "Monthly close", "description": "", "trigger_type": "schedule", "workflow_def_id": f.wfDefID,
		"source_form_id": "", "source_grid_id": "", "source_integration_id": "",
		"cron_expr": "0 6 1 * *", "timezone": "Europe/Paris", "misfire_policy": "fire_now",
	}
	status, body := f.do(t, "POST", "/api/automation/rules", "wt-dev", create)
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("create schedule rule: %d %s", status, body)
	}
	var rule map[string]any
	if err := json.Unmarshal(body, &rule); err != nil {
		t.Fatal(err)
	}
	if rule["trigger_type"] != "schedule" || rule["cron_expr"] != "0 6 1 * *" || rule["timezone"] != "Europe/Paris" ||
		rule["misfire_policy"] != "fire_now" || rule["next_fire_at"] == nil {
		t.Errorf("created rule: %s", body)
	}

	status, body = f.do(t, "GET", "/api/automation/rules", "wt-dev", nil)
	var rules []map[string]any
	if err := json.Unmarshal(body, &rules); status != http.StatusOK || err != nil {
		t.Fatalf("list: %d %s", status, body)
	}
	for _, r := range rules {
		if r["id"] == rule["id"] {
			if r["cron_expr"] != "0 6 1 * *" || r["next_fire_at"] == nil {
				t.Errorf("listed rule lacks its schedule: %v", r)
			}
			return
		}
	}
	t.Errorf("new rule not listed: %s", body)
}
