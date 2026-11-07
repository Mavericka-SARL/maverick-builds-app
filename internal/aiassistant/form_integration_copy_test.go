package aiassistant_test

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// A form integration named by its id in the revision a plan was checked
// against is taken as its copy in the draft the plan runs in: a confirmed
// plan stopped there with "not in the current working revision" (2026-11-07).
func TestFormIntegrationIDFromAnotherRevisionNamesItsCopy(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revA := seedRevision(t, pool, modelID, "Rev A")
	revB := seedRevision(t, pool, modelID, "Draft B")
	userID := seedActor(t, pool)
	ctx := context.Background()
	build := func(rev string) string {
		t.Helper()
		w := aiassistant.NewWriteExecutorWithActor(pool, modelID, rev, userID)
		for _, step := range []struct {
			tool   string
			params map[string]any
		}{
			{"create_metric", map[string]any{"name": "expense", "is_input": true}},
			{"create_form_def", map[string]any{"name": "expense_request", "label": "Expense request",
				"fields": []map[string]any{{"name": "amount", "label": "Amount", "type": "number", "required": true}}}},
		} {
			if _, _, err := w.Execute(ctx, step.tool, mustJSON(t, step.params)); err != nil {
				t.Fatalf("%s in %s: %v", step.tool, rev, err)
			}
		}
		_, id, err := w.Execute(ctx, "create_form_integration", mustJSON(t, map[string]any{
			"form_id": "expense_request", "name": "Post expenses", "source_field": "amount", "target_metric_id": "expense"}))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	idA, idB := build(revA), build(revB)
	draft := aiassistant.NewWriteExecutorWithActor(pool, modelID, revB, userID)
	if _, _, err := draft.Execute(ctx, "update_form_integration", mustJSON(t, map[string]any{
		"form_integration_id": idA, "name": "Post approved expenses"})); err != nil {
		t.Fatalf("update by the other revision's id: %v", err)
	}
	var nameA, nameB string
	_ = pool.QueryRow(ctx, `SELECT name FROM model.form_metric_mapping WHERE id=$1::uuid`, idA).Scan(&nameA)
	_ = pool.QueryRow(ctx, `SELECT name FROM model.form_metric_mapping WHERE id=$1::uuid`, idB).Scan(&nameB)
	if nameB != "Post approved expenses" || nameA != "Post expenses" {
		t.Errorf("after the update: rev A %q (want unchanged), draft %q (want renamed)", nameA, nameB)
	}
}
