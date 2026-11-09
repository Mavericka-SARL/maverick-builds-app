package aiassistant_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// The AI Developer's add_grid_dimension and add_grid_metric move the values
// already stored as the console's routes do (modeledit): a member is named
// for the values, or the step is refused; a metric moved onto a grid without
// a dimension its values carry has them combined by its own rule.
func TestAIGridChangesMoveStoredValues(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	ctx := context.Background()
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Draft")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	region := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'region') RETURNING id::text`, modelID, revID)
	scenario := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'scenario') RETURNING id::text`, modelID, revID)
	for dim, codes := range map[string][]string{region: {"EMEA", "US"}, scenario: {"Base", "Alt"}} {
		for _, c := range codes {
			q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, $2, $2) RETURNING id::text`, dim, c)
		}
	}
	revenue := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, 'revenue', true, 'sum') RETURNING id::text`, modelID, revID)
	grid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Plan') RETURNING id::text`, modelID, revID)
	q(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid) RETURNING grid_id::text`, grid, region)
	q(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid) RETURNING grid_id::text`, grid, revenue)
	q(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
		VALUES ($1::uuid, $2::uuid, $3::uuid, jsonb_build_object($4::text, 'EMEA'), 100) RETURNING id::text`, modelID, revID, revenue, region)

	_, _, err := exec.Execute(ctx, "add_grid_dimension", mustJSON(t, map[string]any{"grid_id": grid, "dimension_id": scenario}))
	if err == nil || !strings.Contains(err.Error(), "name the member") {
		t.Fatalf("add_grid_dimension without member: %v, want it refused asking for the member", err)
	}
	res, _, err := exec.Execute(ctx, "add_grid_dimension", mustJSON(t, map[string]any{"grid_id": grid, "dimension_id": "scenario", "member": "Base"}))
	if err != nil {
		t.Fatalf("add_grid_dimension with member: %v", err)
	}
	if !strings.Contains(res, "1 stored values filed") {
		t.Errorf("result %q does not say the value was filed under the member", res)
	}
	if got := q(`SELECT dim_members->>$2 FROM runtime.fact_input WHERE metric_id=$1::uuid`, revenue, scenario); got != "Base" {
		t.Errorf("revenue's value under scenario %q, want Base", got)
	}

	// Moved onto a grid by region only: summed along scenario.
	q(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
		VALUES ($1::uuid, $2::uuid, $3::uuid, jsonb_build_object($4::text, 'EMEA', $5::text, 'Alt'), 20) RETURNING id::text`, modelID, revID, revenue, region, scenario)
	if _, _, err := exec.Execute(ctx, "remove_grid_metric", mustJSON(t, map[string]any{"grid_id": grid, "metric_id": revenue})); err != nil {
		t.Fatalf("remove_grid_metric: %v", err)
	}
	res, newGrid, err := exec.Execute(ctx, "create_grid", mustJSON(t, map[string]any{"name": "By region", "dimensions": []string{"region"}, "metrics": []string{"revenue"}}))
	if err != nil {
		t.Fatalf("create_grid: %v", err)
	}
	_ = res
	if got := q(`SELECT count(*)::text || ':' || sum(value)::text FROM runtime.fact_input WHERE metric_id=$1::uuid`, revenue); got != "1:120" {
		t.Errorf("revenue after the move onto %s = %s (rows:sum), want 1:120", newGrid, got)
	}
}
