package aiassistant_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// TestAIRenamesCarryIntoFormulasAndRules: the AI Developer's renames reach
// what names the renamed thing by text, as the developer's do — a metric's
// in highlight rules (update_metric), a dimension's and a member code's in
// formulas (update_dimension, update_dimension_member) — and delete_metric
// is refused while a highlight rule names the metric.
func TestAIRenamesCarryIntoFormulasAndRules(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()
	mustRun := func(tool string, params map[string]any) string {
		t.Helper()
		out, _, err := exec.Execute(ctx, tool, mustJSON(t, params))
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return out
	}
	one := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}

	mustRun("create_dimension", map[string]any{"name": "region", "members": []map[string]any{
		{"code": "EMEA", "label": "EMEA"}, {"code": "US", "label": "US"}}})
	mustRun("create_metric", map[string]any{"name": "revenue", "is_input": true, "agg_rule": "sum", "format": "number"})
	mustRun("create_metric", map[string]any{"name": "threshold", "is_input": true, "agg_rule": "sum", "format": "number"})
	mustRun("create_metric", map[string]any{"name": "emea", "formula": `LOOKUP(revenue, region, "EMEA")`, "agg_rule": "sum", "format": "number"})
	mustRun("update_metric", map[string]any{"metric_id": "revenue", "highlight_rules": []map[string]any{
		{"op": ">", "than": "threshold", "tone": "negative"}}})

	if _, _, err := exec.Execute(ctx, "delete_metric", mustJSON(t, map[string]any{"metric_id": "threshold"})); err == nil ||
		!strings.Contains(err.Error(), "METRIC_IN_USE") || !strings.Contains(err.Error(), "revenue (its highlight rules)") {
		t.Errorf("delete threshold while revenue's highlight rule names it: %v; want METRIC_IN_USE naming revenue (its highlight rules)", err)
	}

	mustRun("update_metric", map[string]any{"metric_id": "threshold", "name": "limit"})
	if got := one(`SELECT highlight_rules->0->>'than' FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='revenue'`, modelID, revID); got != "limit" {
		t.Errorf("after update_metric renamed threshold, the highlight rule compares with %q, want limit", got)
	}

	out := mustRun("update_dimension", map[string]any{"dimension_id": "region", "name": "territory"})
	formulaOf := func() string {
		return one(`SELECT formula FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='emea'`, modelID, revID)
	}
	if got := formulaOf(); got != `LOOKUP(revenue, territory, "EMEA")` {
		t.Errorf("after update_dimension renamed region, emea reads %q", got)
	}
	if !strings.Contains(out, "1 formula(s)") {
		t.Errorf("update_dimension's result %q does not say a formula changed", out)
	}

	mustRun("update_dimension_member", map[string]any{"dimension_id": "territory", "code": "EMEA", "new_code": "EUR"})
	if got := formulaOf(); got != `LOOKUP(revenue, territory, "EUR")` {
		t.Errorf("after update_dimension_member renamed EMEA, emea reads %q", got)
	}
}
