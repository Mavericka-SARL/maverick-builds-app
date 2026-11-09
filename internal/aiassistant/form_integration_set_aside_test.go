package aiassistant_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// A mapping into a metric people typed into sets those values aside: the
// metric shows its records only. The AI Developer is told how many, as the
// developer console is, and where they are cleared.
func TestFormIntegrationSaysWhatItSetsAside(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	userID := seedActor(t, pool)
	ctx := context.Background()
	writer := aiassistant.NewWriteExecutorWithActor(pool, modelID, revID, userID)

	_, metricID, err := writer.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "expense", "is_input": true, "agg_rule": "sum", "format": "number"}))
	if err != nil {
		t.Fatal(err)
	}
	_, dimID, err := writer.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{"name": "department"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"OPS", "HR"} {
		if _, err := pool.Exec(ctx, `INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
			VALUES ($1::uuid, $2::uuid, $3::uuid, jsonb_build_object($4::text, $5::text), 10)`, modelID, revID, metricID, dimID, code); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := writer.Execute(ctx, "create_form_def", mustJSON(t, map[string]any{
		"name": "expense_request", "label": "Expense request",
		"fields": []map[string]any{
			{"name": "amount", "label": "Amount", "type": "number", "required": true},
			{"name": "dept", "label": "Department", "type": "dimension", "dimension_id": dimID, "required": true},
		},
	})); err != nil {
		t.Fatal(err)
	}
	res, _, err := writer.Execute(ctx, "create_form_integration", mustJSON(t, map[string]any{
		"form_id": "expense_request", "name": "Post expenses", "source_field": "amount", "target_metric_id": "expense",
		"dimension_mappings": map[string]string{"department": "dept"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "2 typed value(s) of 'expense' are set aside") || !strings.Contains(res, "Clear values") {
		t.Errorf("result = %q; want it to say 2 typed values are set aside and where they are cleared", res)
	}
}
