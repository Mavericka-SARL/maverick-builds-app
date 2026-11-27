package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A second approval of the same workflow and scope supersedes the first
// (migration 201, owner decision 2026-11-27): its freeze writes through the
// first run's lock, which until then keeps everyone else out; once it is
// approved its own lock replaces the first's, which is marked superseded and
// can no longer be reopened — the scope is never unlocked in between.
func TestSecondApprovalSupersedesTheFirst(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()
	projDim := idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": "project", "revision_id": d.revID}))
	projects := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{
		"name": "projects", "label": "Projects", "edit_access": "everyone",
		"fields": []map[string]any{
			{"name": "code", "label": "Project ID", "type": "text", "code": true, "core": true},
			{"name": "name", "label": "Project", "type": "text", "required": true},
		},
		"keeps_dimension_id": projDim, "member_label_field": "name",
	}))
	d.call("POST", "/api/forms/"+projects+"/records", d.dev, map[string]any{"data": map[string]any{"code": "P1", "name": "Clinic"}})
	grid := idOf(d.call("POST", "/api/developer/grids", d.dev, map[string]any{"name": "Project budget", "revision_id": d.revID}))
	d.call("POST", "/api/developer/grids/"+grid+"/dimensions/"+projDim, d.dev, nil)
	metric := func(name string) string {
		id := idOf(d.call("POST", "/api/developer/metrics", d.dev, map[string]any{"name": name, "label": name, "is_input": true,
			"revision_id": d.revID, "format": "number", "agg_rule": "sum"}))
		d.call("POST", "/api/developer/grids/"+grid+"/metrics/"+id, d.dev, nil)
		return id
	}
	plan, approved := metric("plan"), metric("approved_budget")
	write := func(persona, metricID string, v float64) int {
		st, _ := d.req("POST", "/api/cells", persona, map[string]any{"model_id": d.modelID, "revision_id": d.revID, "metric_id": metricID,
			"dim_codes": map[string]string{projDim: "P1"}, "value": v})
		return st
	}
	held := func(metricID string) float64 {
		var v float64
		_ = d.pool.QueryRow(ctx, `SELECT value FROM runtime.fact_input WHERE metric_id = $1::uuid AND revision_id = $2::uuid AND source_ref IS NULL
			ORDER BY entered_at DESC, id DESC LIMIT 1`, metricID, d.revID).Scan(&v)
		return v
	}

	wf := idOf(d.call("POST", "/api/developer/workflows?application_id="+d.appID+"&revision_id="+d.revID, d.dev, map[string]any{"name": "Budget approval"}))
	d.call("PATCH", "/api/developer/workflows/"+wf, d.dev, map[string]any{
		"context_schema": []map[string]any{
			{"key": "project", "data_type": "Dimension member", "required": true, "dimension_id": projDim},
			{"key": "locked", "data_type": "Metric", "required": true},
		},
		"steps": []map[string]any{{"id": "review", "name": "Approve the budget", "type": "approval", "assignee_roles": []string{"business_admin"},
			"routes":     map[string]string{"approve": "end-completed", "reject": "end-rejected"},
			"on_approve": map[string]any{"freeze": map[string]any{"pairs": []map[string]string{{"from": "plan", "to": "approved_budget"}}, "at": map[string]string{"project": "{project}"}}}}},
	})
	if st, raw := d.req("POST", "/api/developer/workflows/"+wf+"/publish", d.dev, nil); st != http.StatusOK {
		t.Fatalf("publish: %d %s", st, raw)
	}
	run := func() string {
		t.Helper()
		st, raw := d.req("POST", "/api/workflow/instances", d.dev, map[string]any{"workflow_def_id": wf,
			"context": map[string]string{"project": "P1", "locked": "approved_budget"}})
		if st != http.StatusOK && st != http.StatusCreated {
			t.Fatalf("start: %d %s", st, raw)
		}
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		id, _ := out["id"].(string)
		if id == "" {
			id, _ = out["instance_id"].(string)
		}
		return id
	}
	approve := func(instance string) (int, string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			var tasks []map[string]any
			_, raw := d.req("GET", "/api/tasks", d.admin, nil)
			_ = json.Unmarshal(raw, &tasks)
			for _, tk := range tasks {
				if tk["instance_id"] == instance {
					st, body := d.req("POST", "/api/tasks/"+tk["id"].(string)+"/complete", d.admin, map[string]any{"decision": "approve", "comment": "ok"})
					return st, string(body)
				}
			}
		}
		t.Fatalf("no task of %s in the admin's inbox", instance)
		return 0, ""
	}

	if st := write(d.dev, plan, 100); st != http.StatusOK {
		t.Fatalf("plan 100: %d", st)
	}
	first := run()
	if st, raw := approve(first); st != http.StatusOK {
		t.Fatalf("the first approval: %d %s", st, raw)
	}
	if got := held(approved); got != 100 {
		t.Fatalf("the approved budget after the first approval: %v, want 100", got)
	}
	if st := write(d.dev, approved, 1); st == http.StatusOK {
		t.Fatalf("the approved budget is not locked after the first approval")
	}

	// The second revision: the plan changes (outside the lock), and its
	// approval freezes over the first.
	if st := write(d.dev, plan, 150); st != http.StatusOK {
		t.Fatalf("plan 150 (not locked): %d", st)
	}
	second := run()
	if st := write(d.dev, approved, 1); st == http.StatusOK {
		t.Errorf("the approved budget is open while the second revision waits")
	}
	if st, raw := approve(second); st != http.StatusOK {
		t.Fatalf("the second approval, freezing over the first: %d %s", st, raw)
	}
	if got := held(approved); got != 150 {
		t.Errorf("the approved budget after the second approval: %v, want 150", got)
	}
	var supersededBy string
	if err := d.pool.QueryRow(ctx, `SELECT COALESCE(superseded_by::text, '') FROM workflow.workflow_instance WHERE id = $1::uuid`, first).Scan(&supersededBy); err != nil || supersededBy != second {
		t.Errorf("the first run is superseded by %q (%v), want the second", supersededBy, err)
	}
	if st := write(d.dev, approved, 1); st == http.StatusOK {
		t.Errorf("the approved budget is open after the second approval")
	}
	if st, raw := d.req("POST", "/api/workflow/instances/"+first+"/reopen", d.admin, map[string]any{"reason": "again"}); st == http.StatusOK {
		t.Errorf("the superseded run was reopened: %s", raw)
	}
}
