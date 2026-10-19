package gateway

// GET /api/workflow/status: where the latest run of a published workflow
// stands, for anyone who opens the model — each step a person acts on, in the
// definition's order, done / current / sent back / still to come; conditions
// left out; no person named.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestWorkflowStatus_Route(t *testing.T) {
	f := setupMMFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	steps := `[
		{"id": "s-setup", "name": "Set the month", "type": "task", "assignee_roles": ["business_admin"], "routes": {"next": "s-check"}},
		{"id": "s-check", "name": "In range?", "type": "condition", "routes": {"true": "s-plan"}},
		{"id": "s-plan", "name": "Plan", "type": "task", "assignee_roles": ["business_user"], "routes": {"next": "s-approve"}},
		{"id": "s-approve", "name": "Approve", "type": "approval", "assignee_roles": ["approver"], "routes": {"approve": "end-completed"}}
	]`
	def := q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, status)
		VALUES ($1::uuid, 'Monthly cycle', 'manual', $2::jsonb, 'published') RETURNING id::text`, f.app1, steps)

	status := func() (out struct {
		Workflow string `json:"workflow"`
		Found    bool   `json:"found"`
		Instance *struct {
			Status string `json:"status"`
		} `json:"instance"`
		Steps []struct {
			ID, Name, State string
			Roles           []string
		} `json:"steps"`
	}) {
		t.Helper()
		code, raw := f.do(t, "GET", "/api/workflow/status?name=monthly%20cycle", "mm-user", f.app1, f.modelA, nil)
		if code != http.StatusOK {
			t.Fatalf("status: %d %s", code, raw)
		}
		if strings.Contains(string(raw), "comment") || strings.Contains(string(raw), "assignee_user") {
			t.Errorf("the status names more than steps: %s", raw)
		}
		_ = json.Unmarshal(raw, &out)
		return out
	}

	// Never run: every human step still to come, the condition left out.
	s := status()
	if !s.Found || s.Workflow != "Monthly cycle" || s.Instance != nil || len(s.Steps) != 3 {
		t.Fatalf("before a run: %+v", s)
	}
	for _, st := range s.Steps {
		if st.State != "pending" {
			t.Errorf("before a run, %s is %s", st.Name, st.State)
		}
	}

	// A run: the month set, planning waiting.
	inst := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, status, steps_snapshot) VALUES ($1::uuid, 'running', $2::jsonb) RETURNING id::text`, def, steps)
	q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status, decision, completed_at) VALUES ($1::uuid, 's-setup', 'completed', 'done', now()) RETURNING id::text`, inst)
	q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status, due_at) VALUES ($1::uuid, 's-plan', 'pending', now() + interval '2 days') RETURNING id::text`, inst)
	s = status()
	got := []string{}
	for _, st := range s.Steps {
		got = append(got, st.ID+"="+st.State)
	}
	if s.Instance == nil || s.Instance.Status != "running" || strings.Join(got, ",") != "s-setup=done,s-plan=current,s-approve=pending" {
		t.Errorf("a running cycle: %+v %v", s.Instance, got)
	}

	// No such workflow: found false, not an error.
	code, raw := f.do(t, "GET", "/api/workflow/status?name=nothing", "mm-user", f.app1, f.modelA, nil)
	if code != http.StatusOK || !strings.Contains(string(raw), `"found":false`) {
		t.Errorf("unknown workflow: %d %s", code, raw)
	}
	if code, raw := f.do(t, "GET", "/api/workflow/status", "mm-user", f.app1, f.modelA, nil); code != http.StatusBadRequest {
		t.Errorf("no name: %d %s, want 400", code, raw)
	}
}
