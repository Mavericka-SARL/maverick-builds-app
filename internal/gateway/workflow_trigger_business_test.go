// Tests that workflows and their triggers stay logically connected, and that
// business users can actually perform every human workflow step from the
// surfaces they are given (role-assigned dashboards with automation-button
// start widgets, and the /api/tasks workflow inbox those dashboards live
// next to in the Business Console).
//
// Three layers:
//
//  1. TestAutomationRuleWorkflowLinkage / TestEventRuleDispatchScoping —
//     store-level: an automation rule always resolves to a real, published
//     workflow (by workflow_def_id, falling back to name), refuses dangling /
//     draft / disabled targets, and event dispatch fires only rules matching
//     the trigger type and source form/grid.
//
//  2. TestBusinessUserPerformsEachWorkflowStepFromDashboard — HTTP
//     end-to-end over the real handler: a business user sees their
//     role-assigned dashboard, starts the workflow from its automation
//     button, and each task/approval step is visible in — and completable
//     from — the inbox of exactly the business role it is assigned to.
//     Steps are assigned to identity.business_role NAMES, which is what the
//     Developer Console workflow designer writes (WorkflowsTab role picker).
//
//  3. TestWorkflowWiringAudit — a data-level audit
//     (auditWorkflowBusinessWiring) of every invariant the two layers above
//     rely on: no dangling rules, published targets, valid step graphs,
//     assignee roles that exist and have members, manual rules reachable via
//     an automation button on a role-assigned dashboard. The audit also runs
//     against a LIVE database via TestWorkflowWiringAuditLiveDB
//     (MAVERICKS_AUDIT_DSN=postgres://... go test ...) so real app
//     configurations can be re-verified at any time.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// ── shared fixture ───────────────────────────────────────────────────────────

type wtFixture struct {
	pool *pgxpool.Pool
	srv  *httptest.Server

	custID, wsID, appID, modelID, revID string
	wfDefID, ruleID, dashID             string

	// keycloak_subs double as X-Dev-User header values (resolveDevActor
	// accepts raw subs for personas not in the predefined map).
	ownerSub, approverSub, bystanderSub string
	ownerID, approverID, bystanderID    string

	ownersRoleID, approversRoleID, viewersRoleID string
}

// wtStepsJSON is a designer-serialized workflow exactly as the Developer
// Console writes it: string step types, routes maps with approve/reject
// branches, and assignee_roles holding identity.business_role NAMES. The
// trailing notification step is configured to notify the "Budget Owners"
// role — same named-role scheme as assignee_roles — so it proves the
// approve branch actually delivers a real notification.notification row
// (not just flips a status flag) while the reject branch skips it entirely.
const wtStepsJSON = `[
  {"id":"step-enter","name":"Enter Budget","type":"task",
   "assignee_roles":["Budget Owners"],"routes":{"next":"step-approve"}},
  {"id":"step-approve","name":"Approve Budget","type":"approval",
   "assignee_roles":["Budget Approvers"],
   "routes":{"approve":"step-notify","reject":"end-rejected"}},
  {"id":"step-notify","name":"Notify Owners","type":"notification",
   "notification":{"recipient_type":"role","recipient_role":"Budget Owners","subject":"Budget approved","message":"Your budget was approved."},
   "routes":{"next":"end-completed"}}
]`

func setupWTFixture(t *testing.T) *wtFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

	f := &wtFixture{pool: pool}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	f.custID = q(`INSERT INTO core.customer (name, plan) VALUES ('WT', 'enterprise') RETURNING id::text`)
	f.wsID = q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, f.custID)
	f.appID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Budget App', 'planning') RETURNING id::text`, f.wsID, f.custID)
	f.modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, f.appID)
	f.revID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, f.modelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, f.revID, f.modelID)

	// Three business users: Olga owns budget entry, Artem approves, Boris
	// holds an unrelated business role (so the "no business roles at all →
	// see everything" demo fallback does NOT apply to him).
	f.ownerSub, f.approverSub, f.bystanderSub = "wt-owner", "wt-approver", "wt-bystander"
	f.ownerID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'olga@wt.com', 'Olga', $2::uuid) RETURNING id::text`, f.ownerSub, f.custID)
	f.approverID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'artem@wt.com', 'Artem', $2::uuid) RETURNING id::text`, f.approverSub, f.custID)
	f.bystanderID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'boris@wt.com', 'Boris', $2::uuid) RETURNING id::text`, f.bystanderSub, f.custID)
	for _, uid := range []string{f.ownerID, f.approverID, f.bystanderID} {
		exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, uid, f.wsID)
	}

	f.ownersRoleID = q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Budget Owners') RETURNING id::text`, f.wsID)
	f.approversRoleID = q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Budget Approvers') RETURNING id::text`, f.wsID)
	f.viewersRoleID = q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Viewers') RETURNING id::text`, f.wsID)
	exec(`INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, f.ownersRoleID, f.ownerID)
	exec(`INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, f.approversRoleID, f.approverID)
	exec(`INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, f.viewersRoleID, f.bystanderID)

	// Published workflow in designer serialization + manual automation rule
	// linked by workflow_def_id.
	f.wfDefID = q(`
		INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, status, published_at)
		VALUES ($1::uuid, 'Budget Approval', 'manual', $2::jsonb, 'published', now())
		RETURNING id::text`, f.appID, wtStepsJSON)
	f.ruleID = q(`
		INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, workflow_def_id, enabled, revision_id)
		VALUES ($1::uuid, 'Start Budget Approval', 'manual', 'Budget Approval', $2::uuid, true, $3::uuid)
		RETURNING id::text`, f.appID, f.wfDefID, f.revID)

	// Dashboard with the start button, assigned to Budget Owners only.
	f.dashID = q(`INSERT INTO model.dashboard_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Budget Entry') RETURNING id::text`, f.modelID, f.revID)
	exec(`INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, content) VALUES ($1::uuid, 'automation_button', $2, 'Start Budget Approval')`, f.dashID, f.ruleID)
	exec(`INSERT INTO identity.business_role_dashboard (role_id, dashboard_id) VALUES ($1::uuid, $2::uuid)`, f.ownersRoleID, f.dashID)

	t.Setenv("DEV_MODE", "true")
	f.srv = httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(f.srv.Close)

	return f
}

// do performs a request as the given dev persona (keycloak_sub) and returns
// (status, raw body). Body is fully consumed and closed here.
func (f *wtFixture) do(t *testing.T, method, path, sub string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader = strings.NewReader("")
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", sub)
	req.Header.Set("X-App-Id", f.appID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

func wtCount(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// ── 1. trigger ↔ workflow linkage (store level) ─────────────────────────────

func TestAutomationRuleWorkflowLinkage(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()
	store := workflow.NewStore(f.pool)

	t.Run("rule fires the workflow it links by def id", func(t *testing.T) {
		exec, err := store.TriggerRule(ctx, f.ruleID, f.ownerID, map[string]string{})
		if err != nil {
			t.Fatalf("TriggerRule: %v", err)
		}
		if exec.InstanceID == "" {
			t.Fatal("execution has no workflow instance — trigger and workflow are not connected")
		}
		var gotDefID string
		if err := f.pool.QueryRow(ctx,
			`SELECT workflow_def_id::text FROM workflow.workflow_instance WHERE id=$1::uuid`,
			exec.InstanceID).Scan(&gotDefID); err != nil {
			t.Fatalf("load instance: %v", err)
		}
		if gotDefID != f.wfDefID {
			t.Errorf("instance workflow_def_id = %s, want the rule's linked workflow %s", gotDefID, f.wfDefID)
		}
		// The execution log row must close the chain rule → instance. Its
		// status mirrors the instance lifecycle: 'running' while human steps
		// are open, 'completed'/'cancelled' once the instance closes.
		n := wtCount(t, f.pool,
			`SELECT count(*) FROM workflow.execution WHERE rule_id=$1::uuid AND instance_id=$2::uuid AND status IN ('running','completed')`,
			f.ruleID, exec.InstanceID)
		if n != 1 {
			t.Errorf("execution rows linking rule to instance = %d, want 1", n)
		}
	})

	t.Run("rule falls back to workflow name when def id is unset", func(t *testing.T) {
		var ruleID string
		if err := f.pool.QueryRow(ctx, `
			INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, enabled)
			VALUES ($1::uuid, 'By Name', 'manual', 'Budget Approval', true) RETURNING id::text`,
			f.appID).Scan(&ruleID); err != nil {
			t.Fatalf("insert rule: %v", err)
		}
		exec, err := store.TriggerRule(ctx, ruleID, f.ownerID, map[string]string{})
		if err != nil {
			t.Fatalf("TriggerRule by name: %v", err)
		}
		var gotDefID string
		_ = f.pool.QueryRow(ctx, `SELECT workflow_def_id::text FROM workflow.workflow_instance WHERE id=$1::uuid`, exec.InstanceID).Scan(&gotDefID)
		if gotDefID != f.wfDefID {
			t.Errorf("name-resolved instance workflow_def_id = %s, want %s", gotDefID, f.wfDefID)
		}
	})

	t.Run("rule pointing at a nonexistent workflow fails and starts nothing", func(t *testing.T) {
		var ruleID string
		if err := f.pool.QueryRow(ctx, `
			INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, enabled)
			VALUES ($1::uuid, 'Dangling', 'manual', 'Ghost Workflow', true) RETURNING id::text`,
			f.appID).Scan(&ruleID); err != nil {
			t.Fatalf("insert rule: %v", err)
		}
		before := wtCount(t, f.pool, `SELECT count(*) FROM workflow.workflow_instance`)
		if _, err := store.TriggerRule(ctx, ruleID, f.ownerID, nil); err == nil {
			t.Fatal("expected TriggerRule to fail for a dangling workflow name")
		}
		after := wtCount(t, f.pool, `SELECT count(*) FROM workflow.workflow_instance`)
		if after != before {
			t.Errorf("dangling rule still created %d workflow instance(s)", after-before)
		}
	})

	t.Run("rule pointing at an unpublished workflow is refused", func(t *testing.T) {
		var draftID, ruleID string
		if err := f.pool.QueryRow(ctx, `
			INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, status)
			VALUES ($1::uuid, 'Draft Flow', 'manual', $2::jsonb, 'draft') RETURNING id::text`,
			f.appID, wtStepsJSON).Scan(&draftID); err != nil {
			t.Fatalf("insert draft def: %v", err)
		}
		if err := f.pool.QueryRow(ctx, `
			INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, workflow_def_id, enabled)
			VALUES ($1::uuid, 'To Draft', 'manual', 'Draft Flow', $2::uuid, true) RETURNING id::text`,
			f.appID, draftID).Scan(&ruleID); err != nil {
			t.Fatalf("insert rule: %v", err)
		}
		_, err := store.TriggerRule(ctx, ruleID, f.ownerID, nil)
		if err == nil || !strings.Contains(err.Error(), workflow.ErrWorkflowNotPublished.Error()) {
			t.Errorf("TriggerRule on draft workflow: err = %v, want ErrWorkflowNotPublished", err)
		}
	})

	t.Run("disabled rule does not fire", func(t *testing.T) {
		var ruleID string
		if err := f.pool.QueryRow(ctx, `
			INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, workflow_def_id, enabled)
			VALUES ($1::uuid, 'Disabled', 'manual', 'Budget Approval', $2::uuid, false) RETURNING id::text`,
			f.appID, f.wfDefID).Scan(&ruleID); err != nil {
			t.Fatalf("insert rule: %v", err)
		}
		if _, err := store.TriggerRule(ctx, ruleID, f.ownerID, nil); err == nil {
			t.Error("expected TriggerRule to refuse a disabled rule")
		}
	})

	t.Run("archived workflows are excluded from name resolution", func(t *testing.T) {
		var archID, ruleID string
		if err := f.pool.QueryRow(ctx, `
			INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, status, archived_at)
			VALUES ($1::uuid, 'Old Flow', 'manual', $2::jsonb, 'archived', now()) RETURNING id::text`,
			f.appID, wtStepsJSON).Scan(&archID); err != nil {
			t.Fatalf("insert archived def: %v", err)
		}
		if err := f.pool.QueryRow(ctx, `
			INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, enabled)
			VALUES ($1::uuid, 'To Archived', 'manual', 'Old Flow', true) RETURNING id::text`,
			f.appID).Scan(&ruleID); err != nil {
			t.Fatalf("insert rule: %v", err)
		}
		if _, err := store.TriggerRule(ctx, ruleID, f.ownerID, nil); err == nil {
			t.Error("expected TriggerRule to fail when the only name match is archived")
		}
	})
}

func TestEventRuleDispatchScoping(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()
	store := workflow.NewStore(f.pool)

	// source_form_id/source_grid_id gained a real FK this pass (migration
	// 064) — DispatchEventRules matches by exact ID, but the IDs
	// themselves must now reference real form_def/grid_def rows, not
	// arbitrary literals.
	mkForm := func(name string) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx,
			`INSERT INTO model.form_def (model_id, name, label, fields, revision_id) VALUES ($1::uuid, $2, $2, '[]'::jsonb, $3::uuid) RETURNING id::text`,
			f.modelID, name, f.revID).Scan(&id); err != nil {
			t.Fatalf("insert form %s: %v", name, err)
		}
		return id
	}
	mkGrid := func(name string) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx,
			`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, $2, $3::uuid) RETURNING id::text`,
			f.modelID, name, f.revID).Scan(&id); err != nil {
			t.Fatalf("insert grid %s: %v", name, err)
		}
		return id
	}
	formA := mkForm("Form A")
	formB := mkForm("Form B")
	gridC := mkGrid("Grid C")

	mkRule := func(name, triggerType, formID, gridID string) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, `
			INSERT INTO workflow.automation_rule
			  (application_id, name, trigger_type, workflow_name, workflow_def_id, enabled, revision_id,
			   source_form_id, source_grid_id)
			VALUES ($1::uuid, $2, $3::workflow.trigger_type, 'Budget Approval', $4::uuid, true, $5::uuid,
			        NULLIF($6,'')::uuid, NULLIF($7,'')::uuid)
			RETURNING id::text`,
			f.appID, name, triggerType, f.wfDefID, f.revID, formID, gridID).Scan(&id); err != nil {
			t.Fatalf("insert rule %s: %v", name, err)
		}
		return id
	}

	ruleFormA := mkRule("On Form A", "form_submit", formA, "")
	ruleFormB := mkRule("On Form B", "form_submit", formB, "")
	ruleAnyForm := mkRule("On Any Form", "form_submit", "", "")
	ruleGridC := mkRule("On Grid C", "grid_change", "", gridC)

	execCount := func(ruleID string) int {
		return wtCount(t, f.pool, `SELECT count(*) FROM workflow.execution WHERE rule_id=$1::uuid`, ruleID)
	}
	waitForExec := func(ruleID, label string, want int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for execCount(ruleID) < want {
			if time.Now().After(deadline) {
				t.Fatalf("rule %s: executions = %d, want %d (dispatch never fired)", label, execCount(ruleID), want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// A form-A submit fires the form-A rule and the any-form rule — not the
	// form-B rule, not the grid rule.
	store.DispatchEventRules(ctx, f.appID, f.revID, "form_submit", formA, f.ownerID, map[string]string{"record_id": "r1"})
	waitForExec(ruleFormA, "On Form A", 1)
	waitForExec(ruleAnyForm, "On Any Form", 1)

	// A grid-C change fires only the grid rule.
	store.DispatchEventRules(ctx, f.appID, f.revID, "grid_change", gridC, f.ownerID, nil)
	waitForExec(ruleGridC, "On Grid C", 1)

	// Give any stray goroutines a moment, then assert the negatives.
	time.Sleep(300 * time.Millisecond)
	if n := execCount(ruleFormB); n != 0 {
		t.Errorf("form-B rule fired %d time(s) for a form-A event", n)
	}
	if n := execCount(ruleFormA); n != 1 {
		t.Errorf("form-A rule fired %d time(s), want exactly 1", n)
	}

	// Every fired execution must be connected through to a real instance of
	// the linked workflow — not just logged.
	orphans := wtCount(t, f.pool, `
		SELECT count(*) FROM workflow.execution e
		WHERE e.application_id=$1::uuid
		  AND (e.instance_id IS NULL
		       OR NOT EXISTS (SELECT 1 FROM workflow.workflow_instance wi
		                      WHERE wi.id = e.instance_id AND wi.workflow_def_id=$2::uuid))`,
		f.appID, f.wfDefID)
	if orphans != 0 {
		t.Errorf("%d execution(s) not linked to an instance of the rule's workflow", orphans)
	}
}

// ── 2. business users perform each step from their surfaces (HTTP e2e) ──────

func TestBusinessUserPerformsEachWorkflowStepFromDashboard(t *testing.T) {
	f := setupWTFixture(t)

	// Olga (Budget Owners) sees the dashboard that carries the start button.
	status, body := f.do(t, "GET", "/api/dashboards", f.ownerSub, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/dashboards as owner: status %d, body %s", status, body)
	}
	if !strings.Contains(string(body), f.dashID) {
		t.Fatalf("owner's dashboard list is missing the Budget Entry dashboard: %s", body)
	}

	// Boris (Viewers, unrelated role) must not see or open it.
	status, body = f.do(t, "GET", "/api/dashboards", f.bystanderSub, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/dashboards as bystander: status %d", status)
	}
	if strings.Contains(string(body), f.dashID) {
		t.Errorf("bystander can see a dashboard not assigned to his role: %s", body)
	}
	if status, _ = f.do(t, "GET", "/api/dashboards/"+f.dashID, f.bystanderSub, nil); status != http.StatusNotFound {
		t.Errorf("GET dashboard detail as bystander: status %d, want 404", status)
	}

	// The dashboard exposes the automation button wired to the rule.
	status, body = f.do(t, "GET", "/api/dashboards/"+f.dashID, f.ownerSub, nil)
	if status != http.StatusOK {
		t.Fatalf("GET dashboard detail as owner: status %d", status)
	}
	if !strings.Contains(string(body), `"automation_button"`) || !strings.Contains(string(body), f.ruleID) {
		t.Fatalf("dashboard detail has no automation button for rule %s: %s", f.ruleID, body)
	}

	// Olga starts the workflow from that button.
	status, body = f.do(t, "POST", "/api/automation/trigger/"+f.ruleID, f.ownerSub, map[string]any{"payload": map[string]string{}})
	if status != http.StatusOK {
		t.Fatalf("trigger rule as owner: status %d, body %s", status, body)
	}

	// inboxHas reports whether the user's /api/tasks inbox contains a step
	// of the given name, returning the step id when present.
	inboxStep := func(sub, stepName string) (string, bool) {
		t.Helper()
		status, body := f.do(t, "GET", "/api/tasks", sub, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /api/tasks as %s: status %d", sub, status)
		}
		var tasks []struct {
			ID       string `json:"id"`
			StepName string `json:"step_name"`
		}
		if err := json.Unmarshal(body, &tasks); err != nil {
			t.Fatalf("parse tasks: %v (%s)", err, body)
		}
		for _, task := range tasks {
			if task.StepName == stepName {
				return task.ID, true
			}
		}
		return "", false
	}

	// Step 1 "Enter Budget" (task, Budget Owners): only Olga sees it.
	enterID, ok := inboxStep(f.ownerSub, "Enter Budget")
	if !ok {
		t.Fatal("owner does not see the 'Enter Budget' step in her inbox — business-role assignees are disconnected from the inbox")
	}
	if _, ok := inboxStep(f.approverSub, "Enter Budget"); ok {
		t.Error("approver sees the owners' 'Enter Budget' step")
	}
	if _, ok := inboxStep(f.bystanderSub, "Enter Budget"); ok {
		t.Error("bystander sees the owners' 'Enter Budget' step")
	}

	// Only Olga can complete it.
	status, _ = f.do(t, "POST", "/api/tasks/"+enterID+"/complete", f.approverSub, map[string]string{"decision": "done"})
	if status != http.StatusForbidden {
		t.Errorf("approver completing owners' step: status %d, want 403", status)
	}
	status, body = f.do(t, "POST", "/api/tasks/"+enterID+"/complete", f.ownerSub, map[string]string{"decision": "done", "comment": "entered"})
	if status != http.StatusOK {
		t.Fatalf("owner completing her step: status %d, body %s", status, body)
	}

	// Step 2 "Approve Budget" (approval, Budget Approvers): only Artem.
	approveID, ok := inboxStep(f.approverSub, "Approve Budget")
	if !ok {
		t.Fatal("approver does not see the 'Approve Budget' step in his inbox")
	}
	if _, ok := inboxStep(f.ownerSub, "Approve Budget"); ok {
		t.Error("owner sees the approvers' 'Approve Budget' step")
	}
	status, _ = f.do(t, "POST", "/api/tasks/"+approveID+"/complete", f.ownerSub, map[string]string{"decision": "approve"})
	if status != http.StatusForbidden {
		t.Errorf("owner completing approvers' step: status %d, want 403", status)
	}
	status, body = f.do(t, "POST", "/api/tasks/"+approveID+"/complete", f.approverSub, map[string]string{"decision": "approve", "comment": "looks good"})
	if status != http.StatusOK {
		t.Fatalf("approver completing his step: status %d, body %s", status, body)
	}

	// Every human step done by a business user → the notification step
	// auto-dispatches and the instance completes.
	var instStatus string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT status::text FROM workflow.workflow_instance
		WHERE workflow_def_id=$1::uuid ORDER BY started_at DESC LIMIT 1`,
		f.wfDefID).Scan(&instStatus); err != nil {
		t.Fatalf("load instance: %v", err)
	}
	if instStatus != "completed" {
		t.Errorf("instance status = %q after all steps were performed, want completed", instStatus)
	}
	var notifyStatus, notifyDecision string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT ws.status::text, COALESCE(ws.decision,'')
		FROM workflow.workflow_step ws
		JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
		WHERE wi.workflow_def_id=$1::uuid AND ws.step_def_id='step-notify'
		ORDER BY wi.started_at DESC LIMIT 1`,
		f.wfDefID).Scan(&notifyStatus, &notifyDecision); err != nil {
		t.Fatalf("load notify step: %v", err)
	}
	if notifyStatus != "completed" || notifyDecision != "sent" {
		t.Errorf("notify step = %s/%s after approval, want completed/sent (auto-dispatched)", notifyStatus, notifyDecision)
	}

	// The dispatch must be real, not just a status flag: Olga (the "Budget
	// Owners" role member) has an actual notification.notification row.
	var notifCount int
	var subject, message string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT count(*), COALESCE(max(template_vars->>'subject'), ''), COALESCE(max(template_vars->>'message'), '')
		FROM notification.notification
		WHERE recipient_user_id=$1::uuid AND template_id='workflow_step_notification'`,
		f.ownerID).Scan(&notifCount, &subject, &message); err != nil {
		t.Fatalf("load notification: %v", err)
	}
	if notifCount != 1 {
		t.Fatalf("notifications for the Budget Owners role member = %d, want 1", notifCount)
	}
	if subject != "Budget approved" || message != "Your budget was approved." {
		t.Errorf("notification subject/message = %q/%q, want the step's configured values", subject, message)
	}
}

// TestRejectPathSkipsDownstreamAndCancels drives the same journey but the
// approver REJECTS: the reject route must end the workflow — the notify
// step is skipped (never dispatched), the instance closes as cancelled, and
// no phantom tasks remain in anyone's inbox.
func TestRejectPathSkipsDownstreamAndCancels(t *testing.T) {
	f := setupWTFixture(t)

	status, body := f.do(t, "POST", "/api/automation/trigger/"+f.ruleID, f.ownerSub, map[string]any{"payload": map[string]string{}})
	if status != http.StatusOK {
		t.Fatalf("trigger rule: status %d, body %s", status, body)
	}

	findTask := func(sub, stepName string) (string, bool) {
		t.Helper()
		status, body := f.do(t, "GET", "/api/tasks", sub, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /api/tasks as %s: status %d", sub, status)
		}
		var tasks []struct {
			ID       string `json:"id"`
			StepName string `json:"step_name"`
		}
		if err := json.Unmarshal(body, &tasks); err != nil {
			t.Fatalf("parse tasks: %v (%s)", err, body)
		}
		for _, task := range tasks {
			if task.StepName == stepName {
				return task.ID, true
			}
		}
		return "", false
	}

	enterID, ok := findTask(f.ownerSub, "Enter Budget")
	if !ok {
		t.Fatal("owner does not see the 'Enter Budget' step")
	}
	if status, body := f.do(t, "POST", "/api/tasks/"+enterID+"/complete", f.ownerSub, map[string]string{"decision": "done", "comment": "entered"}); status != http.StatusOK {
		t.Fatalf("owner completing task: status %d, body %s", status, body)
	}

	approveID, ok := findTask(f.approverSub, "Approve Budget")
	if !ok {
		t.Fatal("approver does not see the 'Approve Budget' step")
	}
	if status, body := f.do(t, "POST", "/api/tasks/"+approveID+"/complete", f.approverSub, map[string]string{"decision": "reject", "comment": "over budget"}); status != http.StatusOK {
		t.Fatalf("approver rejecting: status %d, body %s", status, body)
	}

	ctx := context.Background()
	var instStatus string
	if err := f.pool.QueryRow(ctx, `
		SELECT status::text FROM workflow.workflow_instance
		WHERE workflow_def_id=$1::uuid ORDER BY started_at DESC LIMIT 1`,
		f.wfDefID).Scan(&instStatus); err != nil {
		t.Fatalf("load instance: %v", err)
	}
	// A rejection is a workflow that ran to its end and decided "no" — the
	// instance completes (the decision lives on the step); "cancelled" is
	// reserved for runs aborted before finishing. It used to be mapped to
	// cancelled, making a decided rejection indistinguishable from an
	// abandoned run in every history view.
	if instStatus != "completed" {
		t.Errorf("instance status after reject = %q, want completed (decision is recorded on the step)", instStatus)
	}

	stepStatus := func(defID string) (status, decision string) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, `
			SELECT ws.status::text, COALESCE(ws.decision,'')
			FROM workflow.workflow_step ws
			JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
			WHERE wi.workflow_def_id=$1::uuid AND ws.step_def_id=$2
			ORDER BY wi.started_at DESC LIMIT 1`,
			f.wfDefID, defID).Scan(&status, &decision); err != nil {
			t.Fatalf("load step %s: %v", defID, err)
		}
		return status, decision
	}
	if s, d := stepStatus("step-approve"); s != "rejected" || d != "reject" {
		t.Errorf("approval step = %s/%s, want rejected/reject", s, d)
	}
	if s, _ := stepStatus("step-notify"); s != "skipped" {
		t.Errorf("notify step after reject = %q, want skipped (reject must not fire the approve branch)", s)
	}

	// Nothing left for anyone to do.
	for _, sub := range []string{f.ownerSub, f.approverSub} {
		if id, ok := findTask(sub, "Enter Budget"); ok {
			t.Errorf("%s still sees 'Enter Budget' (%s) after the workflow was rejected", sub, id)
		}
		if id, ok := findTask(sub, "Notify Owners"); ok {
			t.Errorf("%s sees the skipped notify step (%s) as a task", sub, id)
		}
	}
}

// ── 3. wiring audit ─────────────────────────────────────────────────────────

// platform role enum values remain legal assignees for seed-created
// workflows even though the designer UI only offers business roles.
var wtPlatformRoles = map[string]bool{
	"platform_admin": true, "developer": true,
	"business_admin": true, "business_user": true,
}

// auditWorkflowBusinessWiring verifies, for one application, every data-level
// invariant that keeps triggers, workflows, and business-user surfaces
// connected. It returns human-readable findings (empty = fully wired).
func auditWorkflowBusinessWiring(ctx context.Context, pool *pgxpool.Pool, appID string) ([]string, error) {
	var findings []string
	add := func(format string, args ...any) { findings = append(findings, fmt.Sprintf(format, args...)) }

	// -- automation rules resolve to real, published, non-empty workflows --
	ruleRows, err := pool.Query(ctx, `
		SELECT r.id::text, r.name, r.trigger_type::text, r.workflow_name, COALESCE(r.workflow_def_id::text,'')
		FROM workflow.automation_rule r
		WHERE r.application_id=$1::uuid AND r.enabled`, appID)
	if err != nil {
		return nil, err
	}
	type rule struct{ id, name, trigger, wfName, wfDefID string }
	var rules []rule
	for ruleRows.Next() {
		var r rule
		if err := ruleRows.Scan(&r.id, &r.name, &r.trigger, &r.wfName, &r.wfDefID); err != nil {
			ruleRows.Close()
			return nil, err
		}
		rules = append(rules, r)
	}
	ruleRows.Close()
	if err := ruleRows.Err(); err != nil {
		return nil, err
	}

	for _, r := range rules {
		var wfID, wfStatus string
		var stepCount int
		var err error
		if r.wfDefID != "" {
			err = pool.QueryRow(ctx, `
				SELECT id::text, status, jsonb_array_length(steps) FROM workflow.workflow_def
				WHERE id=$1::uuid`, r.wfDefID).Scan(&wfID, &wfStatus, &stepCount)
		} else {
			err = pool.QueryRow(ctx, `
				SELECT id::text, status, jsonb_array_length(steps) FROM workflow.workflow_def
				WHERE application_id=$1::uuid AND name=$2 AND status != 'archived'
				ORDER BY created_at DESC LIMIT 1`, appID, r.wfName).Scan(&wfID, &wfStatus, &stepCount)
		}
		if err != nil {
			add("rule %q: no workflow named %q exists in this application (dangling trigger)", r.name, r.wfName)
			continue
		}
		if wfStatus != "published" {
			add("rule %q: workflow %q is not published (status %q) — trigger can never fire it", r.name, r.wfName, wfStatus)
		}
		if stepCount == 0 {
			add("rule %q: workflow %q has no steps", r.name, r.wfName)
		}
	}

	// -- published workflows: valid step graphs, performable assignees --
	wfRows, err := pool.Query(ctx, `
		SELECT id::text, name, steps FROM workflow.workflow_def
		WHERE application_id=$1::uuid AND status='published'`, appID)
	if err != nil {
		return nil, err
	}
	type wfDef struct {
		id, name string
		steps    []byte
	}
	var wfs []wfDef
	for wfRows.Next() {
		var w wfDef
		if err := wfRows.Scan(&w.id, &w.name, &w.steps); err != nil {
			wfRows.Close()
			return nil, err
		}
		wfs = append(wfs, w)
	}
	wfRows.Close()
	if err := wfRows.Err(); err != nil {
		return nil, err
	}

	for _, w := range wfs {
		var steps []map[string]any
		if err := json.Unmarshal(w.steps, &steps); err != nil {
			add("workflow %q: steps JSON does not parse: %v", w.name, err)
			continue
		}
		ids := map[string]bool{}
		for _, s := range steps {
			if id, _ := s["id"].(string); id != "" {
				ids[id] = true
			}
		}
		for _, s := range steps {
			name, _ := s["name"].(string)
			stepType := wtStepType(s["type"])

			if routes, _ := s["routes"].(map[string]any); routes != nil {
				for key, v := range routes {
					target, _ := v.(string)
					if target != "" && !strings.HasPrefix(target, "end-") && !ids[target] {
						add("workflow %q step %q: route %q points to unknown step %q", w.name, name, key, target)
					}
				}
			}
			if next, _ := s["next_step_ids"].([]any); next != nil {
				for _, v := range next {
					target, _ := v.(string)
					if target != "" && !strings.HasPrefix(target, "end-") && !ids[target] {
						add("workflow %q step %q: next_step_ids points to unknown step %q", w.name, name, target)
					}
				}
			}

			if stepType != "task" && stepType != "approval" {
				continue
			}
			roles, _ := s["assignee_roles"].([]any)
			if len(roles) == 0 {
				add("workflow %q step %q: %s step has no assignee roles — nobody is asked to perform it", w.name, name, stepType)
				continue
			}
			for _, rv := range roles {
				roleName, _ := rv.(string)
				if wtPlatformRoles[roleName] {
					continue // platform enum roles are matched via role_assignment
				}
				var members int
				err := pool.QueryRow(ctx, `
					SELECT count(brm.user_id)
					FROM identity.business_role br
					JOIN core.workspace ws ON ws.id = br.workspace_id
					JOIN core.application app ON app.id=$1::uuid
					     AND (app.workspace_id = ws.id OR app.customer_id = ws.customer_id)
					LEFT JOIN identity.business_role_member brm ON brm.role_id = br.id
					WHERE br.name = $2
					GROUP BY br.id`, appID, roleName).Scan(&members)
				if err != nil {
					add("workflow %q step %q: assignee role %q does not exist as a business role in this application's scope", w.name, name, roleName)
					continue
				}
				if members == 0 {
					add("workflow %q step %q: assignee role %q has no members — nobody can perform the step", w.name, name, roleName)
				}
			}
		}
	}

	// -- manual rules are reachable: a start button on a role-assigned dashboard --
	for _, r := range rules {
		if r.trigger != "manual" {
			continue
		}
		dashRows, err := pool.Query(ctx, `
			SELECT DISTINCT dd.id::text, dd.name
			FROM model.dashboard_widget dw
			JOIN model.dashboard_def dd ON dd.id = dw.dashboard_id
			JOIN core.model m ON m.id = dd.model_id
			WHERE m.application_id=$1::uuid AND dw.widget_type='automation_button' AND dw.ref_id=$2`,
			appID, r.id)
		if err != nil {
			return nil, err
		}
		type dash struct{ id, name string }
		var dashes []dash
		for dashRows.Next() {
			var d dash
			if err := dashRows.Scan(&d.id, &d.name); err != nil {
				dashRows.Close()
				return nil, err
			}
			dashes = append(dashes, d)
		}
		dashRows.Close()
		if err := dashRows.Err(); err != nil {
			return nil, err
		}

		if len(dashes) == 0 {
			add("rule %q: manual rule has no automation button on any dashboard — business users cannot start it", r.name)
			continue
		}

		// If the workspace uses business roles at all, at least one button
		// must sit on a dashboard some role (with members) can see.
		var wsConfigured bool
		_ = pool.QueryRow(ctx, `
			SELECT EXISTS (
			    SELECT 1 FROM identity.business_role_member brm
			    JOIN identity.business_role br ON br.id = brm.role_id
			    JOIN core.workspace ws ON ws.id = br.workspace_id
			    JOIN core.application app ON app.id=$1::uuid
			         AND (app.workspace_id = ws.id OR app.customer_id = ws.customer_id)
			)`, appID).Scan(&wsConfigured)
		if !wsConfigured {
			continue
		}
		reachable := false
		for _, d := range dashes {
			var n int
			_ = pool.QueryRow(ctx, `
				SELECT count(*) FROM identity.business_role_dashboard brd
				JOIN identity.business_role_member brm ON brm.role_id = brd.role_id
				WHERE brd.dashboard_id=$1::uuid`, d.id).Scan(&n)
			if n > 0 {
				reachable = true
				break
			}
		}
		if !reachable {
			add("rule %q: its start button sits only on dashboards no populated business role is assigned to", r.name)
		}
	}

	return findings, nil
}

// wtStepType normalizes designer ("task") and proto-numeric (1) step types.
func wtStepType(v any) string {
	switch tv := v.(type) {
	case string:
		return tv
	case float64:
		switch int(tv) {
		case 1:
			return "task"
		case 2:
			return "approval"
		case 3:
			return "notification"
		case 4:
			return "condition"
		}
	}
	return ""
}

func TestWorkflowWiringAudit(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()

	t.Run("fully wired app has no findings", func(t *testing.T) {
		findings, err := auditWorkflowBusinessWiring(ctx, f.pool, f.appID)
		if err != nil {
			t.Fatalf("audit: %v", err)
		}
		if len(findings) != 0 {
			t.Errorf("expected no findings for the wired fixture app, got:\n  %s", strings.Join(findings, "\n  "))
		}
	})

	t.Run("each broken wiring is detected", func(t *testing.T) {
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

		// Isolated broken app: own customer/workspace so business-role
		// lookups cannot leak into the good fixture's scope.
		custID := q(`INSERT INTO core.customer (name, plan) VALUES ('Broken Co', 'enterprise') RETURNING id::text`)
		wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'broken-ws') RETURNING id::text`, custID)
		appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Broken App', 'planning') RETURNING id::text`, wsID, custID)
		modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'BM') RETURNING id::text`, appID)

		// Workspace is "configured": one populated role exists.
		userID := q(`INSERT INTO identity.user (keycloak_sub, email, customer_id) VALUES ('wt-broken-user', 'u@broken.com', $1::uuid) RETURNING id::text`, custID)
		someRoleID := q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Some Role') RETURNING id::text`, wsID)
		exec(`INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, someRoleID, userID)
		exec(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Empty Role')`, wsID)

		// Published workflow with every step-level defect.
		brokenSteps := `[
		  {"id":"s1","name":"Orphan Route","type":"task",
		   "assignee_roles":["Nonexistent Role"],"routes":{"next":"step-nope"}},
		  {"id":"s2","name":"Unstaffed Approval","type":"approval",
		   "assignee_roles":["Empty Role"],"routes":{"approve":"end-completed","reject":"end-rejected"}},
		  {"id":"s3","name":"Unassigned Task","type":"task","assignee_roles":[],"routes":{"next":"end-completed"}}
		]`
		wfID := q(`
			INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, status, published_at)
			VALUES ($1::uuid, 'Broken Flow', 'manual', $2::jsonb, 'published', now()) RETURNING id::text`,
			appID, brokenSteps)
		draftID := q(`
			INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, status)
			VALUES ($1::uuid, 'Draft Only', 'manual', '[]'::jsonb, 'draft') RETURNING id::text`, appID)

		// Rules: dangling, to-draft, manual-without-button, button-on-unassigned-dashboard.
		exec(`INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, enabled)
		      VALUES ($1::uuid, 'Dangling Rule', 'manual', 'Ghost', true)`, appID)
		exec(`INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, workflow_def_id, enabled)
		      VALUES ($1::uuid, 'Draft Rule', 'manual', 'Draft Only', $2::uuid, true)`, appID, draftID)
		exec(`INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, workflow_def_id, enabled)
		      VALUES ($1::uuid, 'Buttonless Rule', 'manual', 'Broken Flow', $2::uuid, true)`, appID, wfID)
		hiddenRuleID := q(`INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, workflow_def_id, enabled)
		      VALUES ($1::uuid, 'Hidden Button Rule', 'manual', 'Broken Flow', $2::uuid, true) RETURNING id::text`, appID, wfID)
		dashID := q(`INSERT INTO model.dashboard_def (model_id, name) VALUES ($1::uuid, 'Unassigned Dash') RETURNING id::text`, modelID)
		exec(`INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id) VALUES ($1::uuid, 'automation_button', $2)`, dashID, hiddenRuleID)

		findings, err := auditWorkflowBusinessWiring(ctx, f.pool, appID)
		if err != nil {
			t.Fatalf("audit: %v", err)
		}
		joined := strings.Join(findings, "\n")
		for _, want := range []string{
			`rule "Dangling Rule": no workflow named "Ghost"`,
			`rule "Draft Rule": workflow "Draft Only" is not published`,
			`workflow "Broken Flow" step "Orphan Route": route "next" points to unknown step "step-nope"`,
			`workflow "Broken Flow" step "Orphan Route": assignee role "Nonexistent Role" does not exist`,
			`workflow "Broken Flow" step "Unstaffed Approval": assignee role "Empty Role" has no members`,
			`workflow "Broken Flow" step "Unassigned Task": task step has no assignee roles`,
			`rule "Buttonless Rule": manual rule has no automation button`,
			`rule "Hidden Button Rule": its start button sits only on dashboards no populated business role is assigned to`,
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("audit missed defect %q\nfindings were:\n%s", want, joined)
			}
		}
	})
}

// TestWorkflowWiringAuditLiveDB re-runs the wiring audit against a real
// database so a human can re-verify any environment at will:
//
//	MAVERICKS_AUDIT_DSN='postgres://user:pass@localhost:5432/mavericks?sslmode=disable' \
//	  go test ./internal/gateway -run TestWorkflowWiringAuditLiveDB -v
//
// Every application that has workflows or automation rules is audited; each
// finding is reported as a test failure naming the app and the broken link.
func TestWorkflowWiringAuditLiveDB(t *testing.T) {
	dsn := os.Getenv("MAVERICKS_AUDIT_DSN")
	if dsn == "" {
		t.Skip("set MAVERICKS_AUDIT_DSN to audit a live database")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to %s: %v", dsn, err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, `
		SELECT a.id::text, a.name FROM core.application a
		WHERE EXISTS (SELECT 1 FROM workflow.workflow_def wd WHERE wd.application_id = a.id)
		   OR EXISTS (SELECT 1 FROM workflow.automation_rule r WHERE r.application_id = a.id)
		ORDER BY a.name`)
	if err != nil {
		t.Fatalf("list applications: %v", err)
	}
	defer rows.Close()

	type app struct{ id, name string }
	var apps []app
	for rows.Next() {
		var a app
		if err := rows.Scan(&a.id, &a.name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		apps = append(apps, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(apps) == 0 {
		t.Log("no applications with workflows or automation rules found")
		return
	}

	for _, a := range apps {
		findings, err := auditWorkflowBusinessWiring(ctx, pool, a.id)
		if err != nil {
			t.Errorf("app %q: audit failed: %v", a.name, err)
			continue
		}
		if len(findings) == 0 {
			t.Logf("app %q: workflow/trigger/dashboard wiring OK", a.name)
			continue
		}
		for _, finding := range findings {
			t.Errorf("app %q: %s", a.name, finding)
		}
	}
}
