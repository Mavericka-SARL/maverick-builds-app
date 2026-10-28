package modeltransfer

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// A rule's schedule and the integration it waits for go with the model:
// exported from one tenant and imported into another, a scheduled rule
// keeps its cron expression, time zone, misfire policy and retries, and a
// rule that starts after an integration keeps waiting for that
// integration — the importing tenant's copy of it. A package without them
// could not be imported at all (a schedule rule needs its cron, 2026-10-28)
// or left the rule firing after every integration. One exported before
// then is refused with a message that says why.
func TestRuleScheduleAndIntegrationSurviveExportAndImport(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	pkg := decodePackage(t, `{
  "model_name": "Schedules",
  "metrics": [{"id": "met-rev", "name": "Revenue", "is_input": true}],
  "grids": [{"id": "grid-plan", "name": "Plan", "metrics": [{"metric_id": "met-rev"}], "dimensions": []}],
  "workflows": [{"id": "wf-close", "name": "Close", "trigger_event": "manual"}],
  "integrations": [{"id": "int-feed", "name": "Feed", "target_type": "grid", "target_id": "grid-plan", "config": {}}],
  "automation_rules": [
    {"id": "rule-sched", "name": "Close on the 3rd", "trigger_type": "schedule", "workflow_name": "Close",
     "workflow_def_id": "wf-close", "enabled": false,
     "cron_expr": "0 6 3 * *", "timezone": "Europe/Paris", "misfire_policy": "fire_now",
     "max_retries": 2, "retry_backoff_seconds": 120},
    {"id": "rule-feed", "name": "After the feed", "trigger_type": "integration_completed", "workflow_name": "Close",
     "workflow_def_id": "wf-close", "source_integration_id": "int-feed"},
    {"id": "rule-other", "name": "After another model's feed", "trigger_type": "integration_completed", "workflow_name": "Close",
     "workflow_def_id": "wf-close", "source_integration_id": "int-not-in-the-package"}
  ]
}`)
	type rule struct {
		cron, tz, misfire, integration string
		retries, backoff               int
		enabled, armed                 bool
	}
	rulesOf := func(revID string) map[string]rule {
		t.Helper()
		rows, err := pool.Query(ctx, `
			SELECT r.name, COALESCE(r.cron_expr,''), r.timezone, r.misfire_policy, r.max_retries, r.retry_backoff_seconds,
			       r.enabled, r.next_fire_at IS NOT NULL, COALESCE(i.name,'')
			FROM workflow.automation_rule r LEFT JOIN model.integration_def i ON i.id = r.source_integration_id
			WHERE r.revision_id = $1::uuid`, revID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]rule{}
		for rows.Next() {
			var name string
			var r rule
			if err := rows.Scan(&name, &r.cron, &r.tz, &r.misfire, &r.retries, &r.backoff, &r.enabled, &r.armed, &r.integration); err != nil {
				t.Fatal(err)
			}
			out[name] = r
		}
		return out
	}
	integrationOf := func(revID, name string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `SELECT id::text FROM model.integration_def WHERE revision_id = $1::uuid AND name = $2`, revID, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	want := map[string]rule{
		"Close on the 3rd": {cron: "0 6 3 * *", tz: "Europe/Paris", misfire: "fire_now", retries: 2, backoff: 120},
		"After the feed":   {tz: "UTC", misfire: "skip", backoff: 60, enabled: true, integration: "Feed"},
	}

	appA, userA := tenantTarget(t, ctx, pool, "Tenant A")
	modelA, revA, err := importInto(ctx, pool, appA, userA, pkg)
	if err != nil {
		t.Fatalf("import into A: %v", err)
	}
	if got := rulesOf(revA); len(got) != 3 || got["Close on the 3rd"] != want["Close on the 3rd"] || got["After the feed"] != want["After the feed"] {
		t.Fatalf("rules imported into A = %+v, want %+v", got, want)
	}
	// Waiting for an integration the package does not carry, a rule arrives
	// switched off rather than starting after every integration.
	if got := rulesOf(revA)["After another model's feed"]; got.enabled || got.integration != "" {
		t.Fatalf("a rule waiting for an integration outside the package = %+v, want switched off", got)
	}

	exported, err := CollectExportWithOptions(ctx, pool, modelA, revA, "Revision 1", ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ar := range exported.AutomationRules {
		switch ar.Name {
		case "Close on the 3rd":
			if ar.CronExpr != "0 6 3 * *" || ar.Timezone != "Europe/Paris" || ar.MisfirePolicy != "fire_now" ||
				ar.MaxRetries != 2 || ar.RetryBackoffSeconds == nil || *ar.RetryBackoffSeconds != 120 {
				t.Errorf("exported schedule rule = %+v", ar)
			}
		case "After the feed":
			if ar.SourceIntegrationID == nil || *ar.SourceIntegrationID != integrationOf(revA, "Feed") || ar.CronExpr != "" || ar.Timezone != "" {
				t.Errorf("exported integration rule = %+v", ar)
			}
		}
	}

	appB, userB := tenantTarget(t, ctx, pool, "Tenant B")
	_, revB, err := importInto(ctx, pool, appB, userB, *exported)
	if err != nil {
		t.Fatalf("import of the export into B: %v", err)
	}
	if got := rulesOf(revB); got["Close on the 3rd"] != want["Close on the 3rd"] || got["After the feed"] != want["After the feed"] {
		t.Fatalf("rules imported into B = %+v, want %+v", got, want)
	}
	var integrationB string
	if err := pool.QueryRow(ctx, `SELECT source_integration_id::text FROM workflow.automation_rule WHERE revision_id = $1::uuid AND name = 'After the feed'`, revB).Scan(&integrationB); err != nil || integrationB != integrationOf(revB, "Feed") {
		t.Fatalf("B's rule waits for integration %q (%v), want B's Feed %q", integrationB, err, integrationOf(revB, "Feed"))
	}

	old := decodePackage(t, `{"model_name": "Old", "workflows": [{"id": "wf", "name": "Close", "trigger_event": "manual"}],
  "automation_rules": [{"name": "Close on the 3rd", "trigger_type": "schedule", "workflow_name": "Close", "workflow_def_id": "wf"}]}`)
	appC, userC := tenantTarget(t, ctx, pool, "Tenant C")
	if _, _, err := importInto(ctx, pool, appC, userC, old); err == nil || !strings.Contains(err.Error(), "carries no schedule") {
		t.Fatalf("an old package's schedule rule without its schedule: %v, want a refusal that says so", err)
	}
}
