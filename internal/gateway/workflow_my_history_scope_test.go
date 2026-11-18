package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// A person's Workflow History answers for the selected application, as the
// business admin's does: with one model chosen both pages list its runs. One
// run of theirs in another application is found by id, with its application,
// so a notification about it can switch there (owner, 2026-11-09).
func TestMyWorkflowHistoryFollowsTheSelectedApplication(t *testing.T) {
	f := setupDevRouteFixture(t)
	ctx := context.Background()
	m := f.gbUser(t, "mh-member", f.cust1, [2]string{"business_user", f.ws1a}, [2]string{"business_user", f.ws1b})
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid), ($1::uuid, $3::uuid)`, m, f.app1, f.app2); err != nil {
		t.Fatal(err)
	}
	runIn := map[string]string{}
	for _, app := range []string{f.app1, f.app2} {
		def := f.one(t, `INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, status) VALUES ($1::uuid, 'Close', 'manual', '[]'::jsonb, 'published') RETURNING id::text`, app)
		runIn[app] = f.one(t, `INSERT INTO workflow.workflow_instance (workflow_def_id, started_by, context) VALUES ($1::uuid, $2::uuid, '{}') RETURNING id::text`, def, m)
	}
	type run struct {
		ID            string `json:"id"`
		ApplicationID string `json:"application_id"`
	}
	list := func(app, query string) []run {
		t.Helper()
		var out []run
		if err := json.Unmarshal(f.expect(t, "mh-member", "GET", "/api/workflow/my-history"+query, app, nil, http.StatusOK), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, app := range []string{f.app1, f.app2} {
		if got := list(app, ""); len(got) != 1 || got[0].ID != runIn[app] || got[0].ApplicationID != app {
			t.Errorf("with %s selected: %+v, want only its run %s", app, got, runIn[app])
		}
	}
	if got := list(f.app1, "?instance_id="+runIn[f.app2]); len(got) != 1 || got[0].ApplicationID != f.app2 {
		t.Errorf("app2's run looked up from app1: %+v, want it with application %s", got, f.app2)
	}
	// Not the person's run: not found, whatever the id.
	other := f.gbUser(t, "mh-other", f.cust1, [2]string{"business_user", f.ws1a})
	_ = other
	var none []run
	_ = json.Unmarshal(f.expect(t, "mh-other", "GET", "/api/workflow/my-history?instance_id="+runIn[f.app1], f.app1, nil, http.StatusOK), &none)
	if len(none) != 0 {
		t.Errorf("another person's lookup found %+v", none)
	}
}

// Both Workflow Histories are searched by submitter, model and stage (owner,
// 2026-11-18), so each run says who submitted it and which model it is
// about, with its steps; and a search reaches past the newest page, so
// ?before= (the last run shown) lists the older ones. Runs one automation
// starts in a transaction share a started_at: none may be lost or listed
// twice at a page boundary.
func TestWorkflowHistoryPagesAndNamesSubmitterAndModel(t *testing.T) {
	f := setupDevRouteFixture(t)
	ctx := context.Background()
	m := f.gbUser(t, "hp-member", f.cust1, [2]string{"business_user", f.ws1a})
	f.gbUser(t, "hp-admin", f.cust1, [2]string{"business_admin", f.ws1a})
	def := f.one(t, `INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, status)
		VALUES ($1::uuid, 'Budget round', 'manual', '[{"id":"cfo","name":"CFO review","type":"approval"}]'::jsonb, 'published') RETURNING id::text`, f.app1)
	modelName := f.one(t, `SELECT name FROM core.model WHERE application_id=$1::uuid ORDER BY created_at LIMIT 1`, f.app1)
	const runs = historyPageSize + 5
	if _, err := f.pool.Exec(ctx, `
		WITH started AS (
			INSERT INTO workflow.workflow_instance (workflow_def_id, started_by, context, started_at)
			SELECT $1::uuid, $2::uuid, '{}', '2026-11-10T09:00:00Z' FROM generate_series(1, $3)
			RETURNING id)
		INSERT INTO workflow.workflow_step (instance_id, step_def_id, status)
		SELECT id, 'cfo', 'in_progress' FROM started`, def, m, runs); err != nil {
		t.Fatal(err)
	}
	// The business admin's history also lists the fixture's other runs.
	ours := map[string]bool{}
	rows, err := f.pool.Query(ctx, `SELECT id::text FROM workflow.workflow_instance WHERE workflow_def_id=$1::uuid`, def)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ours[id] = true
	}
	rows.Close()
	type run struct {
		ID          string `json:"id"`
		SubmittedBy string `json:"submitted_by"`
		ModelName   string `json:"model_name"`
		Steps       []struct {
			StepName string `json:"step_name"`
			Status   string `json:"status"`
		} `json:"steps"`
	}
	for _, c := range []struct{ sub, path string }{
		{"hp-member", "/api/workflow/my-history"},
		{"hp-admin", "/api/workflow/history"},
	} {
		seen := map[string]bool{}
		before, pages := "", 0
		for {
			var page []run
			q := c.path
			if before != "" {
				q += "?before=" + before
			}
			if err := json.Unmarshal(f.expect(t, c.sub, "GET", q, f.app1, nil, http.StatusOK), &page); err != nil {
				t.Fatal(err)
			}
			pages++
			for _, r := range page {
				if seen[r.ID] {
					t.Fatalf("%s: run %s listed twice", c.path, r.ID)
				}
				seen[r.ID] = true
				if !ours[r.ID] {
					continue
				}
				if r.SubmittedBy != "hp-member" || r.ModelName != modelName {
					t.Fatalf("%s: run %s submitted_by %q model_name %q, want hp-member and %q", c.path, r.ID, r.SubmittedBy, r.ModelName, modelName)
				}
				if len(r.Steps) != 1 || r.Steps[0].StepName != "CFO review" || r.Steps[0].Status != "in_progress" {
					t.Fatalf("%s: run %s steps %+v, want CFO review in progress", c.path, r.ID, r.Steps)
				}
			}
			if len(page) < historyPageSize {
				break
			}
			before = page[len(page)-1].ID
			if pages > 10 {
				t.Fatalf("%s: more pages than runs", c.path)
			}
		}
		n := 0
		for id := range ours {
			if seen[id] {
				n++
			}
		}
		if n != runs || pages < 2 {
			t.Errorf("%s: %d of the %d runs over %d pages, want all over 2 or more", c.path, n, runs, pages)
		}
	}
}
