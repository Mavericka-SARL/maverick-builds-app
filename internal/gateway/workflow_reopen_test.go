package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// A finished, approved run is taken back by Reopen (owner decision,
// 2026-11-07): a business admin or a developer, with a reason, and the run
// becomes cancelled, which releases its lock. Until then cancel answered 409,
// and a business admin's status override released it with no reason at all;
// the override now refuses such a run.
func TestReopenApprovedWorkflowRun(t *testing.T) {
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
	ws := workflow.NewStore(f.pool)
	def, err := ws.CreateWorkflowDefFull(ctx, f.appID, "", "Lock the approved budget", "", "manual", f.devID)
	if err != nil {
		t.Fatal(err)
	}
	steps, _ := json.Marshal([]map[string]any{
		{"id": "a1", "name": "Approve the budget", "type": "approval", "assignee_roles": []string{"business_admin"}},
	})
	if _, err := ws.UpdateWorkflowDefFull(ctx, def.ID, def.Name, def.Description, def.TriggerEvent, "none", f.devID, steps, []byte("[]"), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.PublishWorkflowDef(ctx, def.ID, f.devID); err != nil {
		t.Fatal(err)
	}
	rule, err := ws.CreateAutomationRule(ctx, f.appID, "", "Start", "", "manual", "", def.ID, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Budget') RETURNING id::text`, f.appID)
	custID := q(`SELECT customer_id::text FROM core.application WHERE id = $1::uuid`, f.appID)
	wsID := q(`SELECT workspace_id::text FROM core.application WHERE id = $1::uuid`, f.appID)
	userID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('wfreopen-user', 'reopen@wfruntime.com', 'User', $1::uuid) RETURNING id::text`, custID)
	q(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid) RETURNING user_id::text`, userID, wsID)

	// started by the developer, approved by the business admin
	run := func(decision string) string {
		t.Helper()
		exec, err := ws.TriggerRule(ctx, rule.ID, f.devID, nil)
		if err != nil {
			t.Fatalf("trigger: %v", err)
		}
		stepID := q(`SELECT id::text FROM workflow.workflow_step WHERE instance_id = $1::uuid`, exec.InstanceID)
		if _, err := ws.CompleteStep(ctx, stepID, f.baID, decision, ""); err != nil {
			t.Fatalf("decide: %v", err)
		}
		return exec.InstanceID
	}
	statusOf := func(inst string) string {
		t.Helper()
		return q(`SELECT wi.status::text || '/' || e.status::text FROM workflow.workflow_instance wi
			JOIN workflow.execution e ON e.instance_id = wi.id WHERE wi.id = $1::uuid`, inst)
	}
	reopenable := func(inst string) bool {
		t.Helper()
		_, raw := f.doRaw(t, "GET", "/api/workflow/history", f.baSub, nil)
		var list []struct {
			ID         string `json:"id"`
			Reopenable bool   `json:"reopenable"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("history: %v %s", err, raw)
		}
		for _, r := range list {
			if r.ID == inst {
				return r.Reopenable
			}
		}
		t.Fatalf("run %s is not in the history", inst)
		return false
	}

	inst := run("approve")
	if got := statusOf(inst); got != "completed/completed" {
		t.Fatalf("approved run: %s", got)
	}
	if !reopenable(inst) {
		t.Error("an approved run is not marked reopenable in the history")
	}
	if status, body := f.do(t, "POST", "/api/workflow/instances/"+inst+"/cancel", f.baSub, nil); status != http.StatusConflict {
		t.Errorf("cancel of an approved run: %d %v, want 409", status, body)
	}
	for _, s := range []string{"cancelled", "running", "failed"} {
		if status, body := f.do(t, "PATCH", "/api/workflow/instances/"+inst, f.baSub, map[string]string{"status": s}); status != http.StatusConflict {
			t.Errorf("override to %s: %d %v, want 409", s, status, body)
		}
	}
	if status, body := f.do(t, "POST", "/api/workflow/instances/"+inst+"/reopen", f.baSub, map[string]string{"reason": "  "}); status != http.StatusBadRequest {
		t.Errorf("reopen with no reason: %d %v, want 400", status, body)
	}
	if status, body := f.do(t, "POST", "/api/workflow/instances/"+inst+"/reopen", "wfreopen-user", map[string]string{"reason": "x"}); status != http.StatusForbidden {
		t.Errorf("business user: %d %v, want 403", status, body)
	}
	if got := statusOf(inst); got != "completed/completed" {
		t.Fatalf("a refused attempt changed the run: %s", got)
	}

	status, body := f.do(t, "POST", "/api/workflow/instances/"+inst+"/reopen", f.baSub, map[string]string{"reason": "March was locked with a wrong figure"})
	if status != http.StatusOK || body["notified"] != float64(1) {
		t.Fatalf("business admin reopen: %d %v, want 200 telling the developer who started it", status, body)
	}
	if got := statusOf(inst); got != "cancelled/cancelled" {
		t.Errorf("after reopen: %s, want cancelled/cancelled", got)
	}
	if got := q(`SELECT decision FROM workflow.workflow_step WHERE instance_id = $1::uuid`, inst); got != "approve" {
		t.Errorf("the decision is history and stays: %s", got)
	}
	var msg string
	_ = f.pool.QueryRow(ctx, `SELECT template_vars->>'message' FROM notification.notification
		WHERE recipient_user_id = $1::uuid AND resource_id = $2 AND channel = 'in_app'`, f.devID, inst).Scan(&msg)
	if !strings.Contains(msg, "reopened") || !strings.Contains(msg, "wrong figure") {
		t.Errorf("the developer's message %q, want it to say reopened and why", msg)
	}
	if row := f.latestAuditEvent(t, "workflow_instance.reopened"); row.resourceID != inst {
		t.Errorf("audit row %+v, want resource %s", row, inst)
	}
	if status, body := f.do(t, "POST", "/api/workflow/instances/"+inst+"/reopen", f.devSub, map[string]string{"reason": "again"}); status != http.StatusConflict {
		t.Errorf("reopening a reopened run: %d %v, want 409", status, body)
	}

	// A developer may reopen too; a rejected run holds nothing to reopen.
	inst = run("approve")
	if status, body := f.do(t, "POST", "/api/workflow/instances/"+inst+"/reopen", f.devSub, map[string]string{"reason": "wrong scenario"}); status != http.StatusOK {
		t.Errorf("developer reopen: %d %v, want 200", status, body)
	}
	rejected := run("reject")
	if status, body := f.do(t, "POST", "/api/workflow/instances/"+rejected+"/reopen", f.baSub, map[string]string{"reason": "x"}); status != http.StatusConflict {
		t.Errorf("reopen of a rejected run: %d %v, want 409", status, body)
	}
}

// doRaw is do for an answer that is not an object (a list).
func (f *wfRuntimeAuditFixture) doRaw(t *testing.T, method, path, sub string, body any) (int, []byte) {
	t.Helper()
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", sub)
	req.Header.Set("X-App-Id", f.appID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}
