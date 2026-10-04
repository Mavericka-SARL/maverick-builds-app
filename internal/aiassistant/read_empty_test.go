package aiassistant_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// On an empty revision list_dimensions answered "", and the model, reading
// no answer, called it three times running until it was stopped as stuck.
func TestReadToolsSayWhenThereIsNothing(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Empty")
	out, err := aiassistant.NewToolExecutor(pool, modelID, revID).Execute(context.Background(), "list_dimensions", nil)
	if err != nil || !strings.Contains(out, "nothing yet") {
		t.Errorf("list_dimensions on an empty revision = %q, %v; want an explicit nothing-yet answer", out, err)
	}
}

// create_grid read only "metric_ids" and "dimension_ids"; the model wrote
// "metrics" and "dimensions" by name, and every grid it built held nothing.
func TestCreateGridAttachesNamedMetricsAndDimensions(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()
	run := func(tool string, params map[string]any) string {
		t.Helper()
		res, id, err := exec.Execute(ctx, tool, mustJSON(t, params))
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		_ = res
		return id
	}
	run("create_dimension", map[string]any{"name": "Region", "members": []map[string]any{{"code": "EU", "label": "EU"}}})
	for _, m := range []string{"revenue", "cost", "margin_value"} {
		run("create_metric", map[string]any{"name": m, "is_input": true})
	}
	res, gridID, err := exec.Execute(ctx, "create_grid", mustJSON(t, map[string]any{
		"name": "P&L", "metrics": []string{"margin_value", "revenue", "cost"}, "dimensions": []string{"Region"}}))
	if err != nil || !strings.Contains(res, "3 metrics, 1 dimensions attached") {
		t.Fatalf("create_grid: %q, %v", res, err)
	}
	rows, err := pool.Query(ctx, `SELECT m.name FROM model.grid_metric gm JOIN model.metric_def m ON m.id = gm.metric_id WHERE gm.grid_id=$1::uuid ORDER BY gm.sort_order`, gridID)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		order = append(order, n)
	}
	rows.Close()
	if strings.Join(order, ",") != "margin_value,revenue,cost" {
		t.Errorf("grid metric order %v, want the order given", order)
	}
}

// The model copies ids from list output and slips a character; one that is
// within two edits of exactly one grid's id is that grid.
func TestNearMissGridIDResolves(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()
	_, gridID, err := exec.Execute(ctx, "create_grid", mustJSON(t, map[string]any{"name": "G"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "units", "is_input": true})); err != nil {
		t.Fatal(err)
	}
	slip := []byte(gridID)
	if slip[len(slip)-1] == 'a' {
		slip[len(slip)-1] = 'b'
	} else {
		slip[len(slip)-1] = 'a'
	}
	if _, _, err := exec.Execute(ctx, "add_grid_metric", mustJSON(t, map[string]any{"grid_id": string(slip), "metric_id": "units"})); err != nil {
		t.Fatalf("a one-character slip in the grid id: %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.grid_metric WHERE grid_id=$1::uuid`, gridID).Scan(&n)
	if n != 1 {
		t.Errorf("metric attached to %d grid(s) with the slipped id's grid, want 1", n)
	}
}

// The plan is checked on the active revision and runs in a draft copied
// from it: a slipped id of the active revision's grid must land on the
// draft's copy of that grid, as an exact one does.
func TestNearMissIDFromAnotherRevisionRemaps(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	active := seedRevision(t, pool, modelID, "Active")
	ctx := context.Background()
	src := aiassistant.NewWriteExecutor(pool, modelID, active)
	_, gridID, err := src.Execute(ctx, "create_grid", mustJSON(t, map[string]any{"name": "G"}))
	if err != nil {
		t.Fatal(err)
	}
	draft := seedRevision(t, pool, modelID, "Draft")
	var draftGrid string
	if err := pool.QueryRow(ctx, `INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'G') RETURNING id::text`, modelID, draft).Scan(&draftGrid); err != nil {
		t.Fatal(err)
	}
	exec := aiassistant.NewWriteExecutor(pool, modelID, draft)
	if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "units", "is_input": true})); err != nil {
		t.Fatal(err)
	}
	slip := []byte(gridID)
	if slip[0] == 'a' {
		slip[0] = 'b'
	} else {
		slip[0] = 'a'
	}
	if _, _, err := exec.Execute(ctx, "add_grid_metric", mustJSON(t, map[string]any{"grid_id": string(slip), "metric_id": "units"})); err != nil {
		t.Fatalf("a slipped id of the active revision's grid: %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.grid_metric WHERE grid_id=$1::uuid`, draftGrid).Scan(&n)
	if n != 1 {
		t.Errorf("the draft's grid holds %d metric(s), want 1", n)
	}
}

// A grid step listing something it cannot attach fails as a whole; it used
// to succeed with "not attached", and passed the plan check with formulas
// written where metric names belong.
func TestCreateGridFailsWhenAMetricCannotBeAttached(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()
	if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "revenue", "is_input": true})); err != nil {
		t.Fatal(err)
	}
	_, _, err := exec.Execute(ctx, "create_grid", mustJSON(t, map[string]any{"name": "P&L", "metrics": []string{"revenue", "revenue - cost"}}))
	if err == nil || !strings.Contains(err.Error(), "could not attach") || !strings.Contains(err.Error(), "revenue - cost") {
		t.Errorf("create_grid with a formula for a metric: %v, want a refusal naming it", err)
	}
}
