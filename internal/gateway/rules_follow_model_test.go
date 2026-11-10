package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestRulesFollowTheSelectedModel: automation rules (and workflows, which
// resolve the same way) requested with no revision come from the model the
// request selects (X-Model-Id), as forms, grids and integrations do. They
// came from the application's oldest model — the tour, on a signed-up tenant
// — whatever was selected (owner decision 2026-11-09). With no model
// selected they still do; a model the caller may not open is 404.
func TestRulesFollowTheSelectedModel(t *testing.T) {
	f := setupMMFixture(t)
	ctx := context.Background()
	for _, r := range []struct{ rev, name string }{{f.revA, "Rule of A"}, {f.revB, "Rule of B"}} {
		if _, err := f.pool.Exec(ctx, `INSERT INTO workflow.automation_rule (application_id, revision_id, name, trigger_type, workflow_name)
			VALUES ($1::uuid, $2::uuid, $3, 'manual', 'w')`, f.app1, r.rev, r.name); err != nil {
			t.Fatal(err)
		}
	}
	list := func(model string) (int, string) {
		t.Helper()
		status, raw := f.do(t, "GET", "/api/automation/rules", "mm-dev", f.app1, model, nil)
		return status, string(raw)
	}
	for _, c := range []struct{ model, want, not string }{
		{f.modelB, "Rule of B", "Rule of A"},
		{f.modelA, "Rule of A", "Rule of B"},
		{"", "Rule of A", "Rule of B"}, // none selected: the oldest model, as before
	} {
		status, raw := list(c.model)
		if status != http.StatusOK || !strings.Contains(raw, c.want) || strings.Contains(raw, c.not) {
			t.Errorf("rules with X-Model-Id %q: %d %s, want %q only", c.model, status, raw, c.want)
		}
	}
	if status, raw := list(f.modelD); status != http.StatusNotFound {
		t.Errorf("rules with another tenant's model selected: %d %s, want 404", status, raw)
	}
}
