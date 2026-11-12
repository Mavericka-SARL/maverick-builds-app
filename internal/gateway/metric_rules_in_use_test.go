package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// metricRulesFixture: on the dimensional fixture's grid, threshold (an
// input), flag = IF(revenue > 100, 1, 0) and plan_pct (an input); revenue's
// highlight rules compare with threshold and test flag, plan_pct closes
// when flag is 1, and the workflow Gate's condition reads threshold.
func metricRulesFixture(t *testing.T) *dimFormulaFixture {
	f := setupDimFormulaFixture(t)
	for name, text := range map[string]string{"threshold": "", "flag": "IF(revenue > 100, 1, 0)", "plan_pct": ""} {
		f.metric[name] = f.call("POST", "/api/developer/metrics", f.metricBody(name, text))
		f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric[name], nil)
	}
	f.call("PATCH", "/api/developer/metrics/"+f.metric["revenue"], map[string]any{"highlight_rules": []map[string]any{
		{"op": ">", "than": "threshold", "tone": "negative"},
		{"metric": "flag", "op": "=", "value": 1, "tone": "info"},
	}})
	f.call("PATCH", "/api/developer/metrics/"+f.metric["plan_pct"], map[string]any{"closed_when": []map[string]any{
		{"metric": "flag", "op": "=", "value": 1},
	}})
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO workflow.workflow_def (application_id, revision_id, name, trigger_event, steps, status)
		VALUES ($1::uuid, $2::uuid, 'Gate', 'manual',
		        '[{"id":"check","name":"Over threshold?","type":"condition","condition":{"formula":"threshold > 0"}}]', 'published')`,
		f.appID, f.revID); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestMetricDeleteRefusedWhileRulesNameIt: a metric that a highlight rule, a
// closed rule or a workflow condition names is not deleted (409
// METRIC_IN_USE naming each holder). It used to go, leaving the rule
// silently off, the condition parked for a person and every write to the
// closed rule's input failing with a 500.
func TestMetricDeleteRefusedWhileRulesNameIt(t *testing.T) {
	f := metricRulesFixture(t)
	for _, c := range []struct {
		metric  string
		holders []string
	}{
		{"threshold", []string{"revenue (its highlight rules)", "workflow Gate (condition step Over threshold?)"}},
		{"flag", []string{"revenue (its highlight rules)", "plan_pct (its closed rule)"}},
	} {
		status, raw := f.req("DELETE", "/api/developer/metrics/"+f.metric[c.metric], nil)
		if status != http.StatusConflict || !strings.Contains(string(raw), "METRIC_IN_USE") {
			t.Errorf("delete %s: %d %s; want 409 METRIC_IN_USE", c.metric, status, raw)
			continue
		}
		for _, h := range c.holders {
			if !strings.Contains(string(raw), h) {
				t.Errorf("delete %s: %s does not name %s", c.metric, raw, h)
			}
		}
	}
}

// TestMetricRenameCarriesIntoRules: renaming a metric renames it in the
// highlight rules and closed rules naming it, so the grid keeps tinting and
// writes to the closed rule's input keep working (they answered 500 "closed
// rule names "flag", which is no metric of this revision").
func TestMetricRenameCarriesIntoRules(t *testing.T) {
	f := metricRulesFixture(t)
	f.call("PATCH", "/api/developer/metrics/"+f.metric["flag"], map[string]any{"name": "flag_ind"})
	var highlight, closed string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT (SELECT highlight_rules->1->>'metric' FROM model.metric_def WHERE id=$1::uuid),
		       (SELECT closed_when->0->>'metric' FROM model.metric_def WHERE id=$2::uuid)`,
		f.metric["revenue"], f.metric["plan_pct"]).Scan(&highlight, &closed); err != nil {
		t.Fatal(err)
	}
	if highlight != "flag_ind" || closed != "flag_ind" {
		t.Errorf("after the rename the highlight rule names %q and the closed rule %q; want flag_ind", highlight, closed)
	}
	status, raw := f.req("POST", "/api/cells", map[string]any{
		"model_id": f.modelID, "metric_id": f.metric["plan_pct"], "revision_id": f.revID,
		"dim_codes": map[string]string{f.region: "EMEA"}, "value": 5,
	})
	if status != http.StatusOK {
		t.Errorf("a write to plan_pct after the rename: %d %s, want 200", status, raw)
	}
}

// Metrics that read each other are deleted together (?with=), refused only
// while a metric outside them reads one; a grid name the revision has is
// refused (409 GRID_NAME_TAKEN), on create and on rename; publishing a
// published workflow answers it as it is, an archived one 409.
func TestDeleteTogetherGridNamesAndRepublish(t *testing.T) {
	f := metricRulesFixture(t)
	for name, text := range map[string]string{"copy_a": "revenue * 1", "copy_b": "copy_a + 1", "copy_c": "copy_b * 2"} {
		f.metric[name] = f.call("POST", "/api/developer/metrics", f.metricBody(name, text))
		f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric[name], nil)
	}
	status, raw := f.req("DELETE", "/api/developer/metrics/"+f.metric["copy_a"]+"?with="+f.metric["copy_b"], nil)
	if status != http.StatusConflict || !strings.Contains(string(raw), "copy_c") || strings.Contains(string(raw), "copy_a is read by copy_b") {
		t.Fatalf("delete copy_a with copy_b while copy_c reads copy_b: %d %s", status, raw)
	}
	if status, raw := f.req("DELETE", "/api/developer/metrics/"+f.metric["copy_a"]+"?with="+f.metric["copy_b"]+","+f.metric["copy_c"], nil); status != http.StatusOK {
		t.Fatalf("delete together: %d %s", status, raw)
	}
	var left int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM model.metric_def WHERE name LIKE 'copy_%'`).Scan(&left)
	if left != 0 {
		t.Fatalf("%d copies left", left)
	}

	if status, raw := f.req("POST", "/api/developer/grids", map[string]any{"name": "plan", "revision_id": f.revID}); status != http.StatusConflict || !strings.Contains(string(raw), "GRID_NAME_TAKEN") {
		t.Fatalf("a second grid named plan: %d %s", status, raw)
	}
	other := f.call("POST", "/api/developer/grids", map[string]any{"name": "Other", "revision_id": f.revID})
	if status, raw := f.req("PATCH", "/api/developer/grids/"+other, map[string]any{"name": "Plan"}); status != http.StatusConflict || !strings.Contains(string(raw), "GRID_NAME_TAKEN") {
		t.Fatalf("renaming a grid to Plan: %d %s", status, raw)
	}

	var gate string
	_ = f.pool.QueryRow(context.Background(), `SELECT id::text FROM workflow.workflow_def WHERE name = 'Gate'`).Scan(&gate)
	if status, raw := f.req("POST", "/api/developer/workflows/"+gate+"/publish", nil); status != http.StatusOK || !strings.Contains(string(raw), `"published"`) {
		t.Fatalf("publishing the published Gate: %d %s", status, raw)
	}
	_, _ = f.pool.Exec(context.Background(), `UPDATE workflow.workflow_def SET status = 'archived' WHERE id = $1::uuid`, gate)
	if status, raw := f.req("POST", "/api/developer/workflows/"+gate+"/publish", nil); status != http.StatusConflict {
		t.Fatalf("publishing an archived workflow: %d %s", status, raw)
	}
}
