package gateway

import (
	"context"
	"net/http"
	"testing"
)

// TestCreationRefusesARevisionOfAnotherModel: every endpoint that creates a
// revision-scoped definition checks that the revision it is told to use
// belongs to the model (or, for workflows and automation rules, the
// application) it is writing into.
//
// Metric, dimension and dashboard creation took revision_id from the body and
// stored it unchecked, while the model came from X-Model-Id or the app's
// newest model. A client that named its revision but not its model, in an app
// with two models, got rows in the newest model tagged with the other model's
// revision. Workflow and automation-rule creation took ?revision_id= verbatim.
func TestCreationRefusesARevisionOfAnotherModel(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	count := func(table, name string) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE name=$1`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	expect := func(what string, want int, method, path string, body any) {
		t.Helper()
		if status, raw := doAs(t, f, method, path, dev, f.appID, body); status != want {
			t.Errorf("%s: %d %s, want %d", what, status, raw, want)
		}
	}

	// Another tenant's application, model and revision.
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Other tenant', 'enterprise') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, cust)
	otherApp := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Other', 'planning') RETURNING id::text`, ws, cust)
	otherModel := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Other') RETURNING id::text`, otherApp)
	foreignRev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Theirs') RETURNING id::text`, otherModel)

	// Model definitions: a foreign revision is refused and nothing is written;
	// the model's own revision still works.
	expect("metric, foreign revision", http.StatusNotFound, "POST", "/api/developer/metrics",
		map[string]any{"name": "m_foreign", "is_input": true, "revision_id": foreignRev})
	expect("dimension, foreign revision", http.StatusNotFound, "POST", "/api/developer/dimensions",
		map[string]any{"name": "d_foreign", "revision_id": foreignRev})
	expect("dashboard, foreign revision", http.StatusNotFound, "POST", "/api/developer/dashboards",
		map[string]any{"name": "db_foreign", "revision_id": foreignRev})
	for table, name := range map[string]string{"model.metric_def": "m_foreign", "model.dimension_def": "d_foreign", "model.dashboard_def": "db_foreign"} {
		if n := count(table, name); n != 0 {
			t.Errorf("%s: %d row(s) written with a foreign revision", table, n)
		}
	}
	expect("metric, own revision", http.StatusOK, "POST", "/api/developer/metrics",
		map[string]any{"name": "m_own", "is_input": true, "revision_id": f.workingRevID})
	expect("dimension, own revision", http.StatusOK, "POST", "/api/developer/dimensions",
		map[string]any{"name": "d_own", "revision_id": f.workingRevID})
	expect("dashboard, own revision", http.StatusOK, "POST", "/api/developer/dashboards",
		map[string]any{"name": "db_own", "revision_id": f.workingRevID})

	// Workflows and automation rules: a revision of another application is refused.
	wf := "/api/developer/workflows?application_id=" + f.appID
	expect("workflow, foreign revision", http.StatusNotFound, "POST", wf+"&revision_id="+foreignRev,
		map[string]any{"name": "wf_foreign", "trigger_event": "manual"})
	expect("workflow list, foreign revision", http.StatusNotFound, "GET", wf+"&revision_id="+foreignRev, nil)
	expect("workflow, own revision", http.StatusOK, "POST", wf+"&revision_id="+f.workingRevID,
		map[string]any{"name": "wf_own", "trigger_event": "manual"})
	expect("automation rule, foreign revision", http.StatusNotFound, "POST", "/api/automation/rules?revision_id="+foreignRev,
		map[string]any{"name": "ar_foreign", "trigger_type": "manual", "workflow_name": "wf_own"})
	if n := count("workflow.workflow_def", "wf_foreign") + count("workflow.automation_rule", "ar_foreign"); n != 0 {
		t.Errorf("%d workflow/automation row(s) written with a foreign revision", n)
	}

	// The workflow collection takes its application from the query string
	// and did not check it: another tenant's workflows were listed, and one
	// could be created there.
	q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, status) VALUES ($1::uuid, 'theirs', 'manual', '[]'::jsonb, 'draft') RETURNING id::text`, otherApp)
	expect("list another tenant's workflows", http.StatusForbidden, "GET", "/api/developer/workflows?application_id="+otherApp, nil)
	expect("create in another tenant's app", http.StatusForbidden, "POST", "/api/developer/workflows?application_id="+otherApp,
		map[string]any{"name": "planted", "trigger_event": "manual"})
	if n := count("workflow.workflow_def", "planted"); n != 0 {
		t.Errorf("a workflow was created in another tenant's application")
	}

	// Last, because it changes which model a header-less request resolves
	// to: a second model in the SAME app. The request names the first
	// model's revision in its body but not the model, so the app's newest
	// model (the second) is resolved — and the revision is not one of its.
	q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Second') RETURNING id::text`, f.appID)
	expect("metric, revision of a sibling model", http.StatusNotFound, "POST", "/api/developer/metrics",
		map[string]any{"name": "m_sibling", "is_input": true, "revision_id": f.workingRevID})
	if n := count("model.metric_def", "m_sibling"); n != 0 {
		t.Errorf("metric written into a model its revision does not belong to")
	}
}
