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
