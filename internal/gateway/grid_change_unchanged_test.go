package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// A grid_change rule fires when a write changes a value, not when it writes
// the value the cell already holds: rewriting unchanged drivers started a
// cash-floor warning workflow each time (2026-11-13), by one cell and by a
// batch alike.
func TestGridChangeRuleSkipsUnchangedWrites(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	grid := q(`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Drivers', $2::uuid) RETURNING id::text`, f.modelID, f.revID)
	metric := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, 'spend', true, 'sum') RETURNING id::text`, f.modelID, f.revID)
	q(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 0) RETURNING grid_id::text`, grid, metric)
	rule := q(`INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, workflow_def_id, enabled, revision_id, source_grid_id)
		VALUES ($1::uuid, 'On Drivers', 'grid_change', 'Budget Approval', $2::uuid, true, $3::uuid, $4::uuid) RETURNING id::text`,
		f.appID, f.wfDefID, f.revID, grid)
	runs := func() int {
		return wtCount(t, f.pool, `SELECT count(*) FROM workflow.execution WHERE rule_id=$1::uuid`, rule)
	}
	settle := func(want int, what string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for runs() < want && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(400 * time.Millisecond) // a stray dispatch would land by now
		if got := runs(); got != want {
			t.Fatalf("%s: rule ran %d time(s), want %d", what, got, want)
		}
	}
	one := func(v float64) {
		t.Helper()
		code, raw := f.do(t, "POST", "/api/cells", f.ownerSub, map[string]any{
			"model_id": f.modelID, "revision_id": f.revID, "metric_id": metric, "dim_codes": map[string]string{}, "value": v})
		if code != http.StatusOK {
			t.Fatalf("write %v: %d %s", v, code, raw)
		}
	}
	batch := func(v float64) {
		t.Helper()
		code, raw := f.do(t, "POST", "/api/cells/batch", f.ownerSub, map[string]any{
			"model_id": f.modelID, "revision_id": f.revID,
			"cells": []map[string]any{{"metric_id": metric, "dim_codes": map[string]string{}, "value": v}}})
		if code != http.StatusOK {
			t.Fatalf("batch %v: %d %s", v, code, raw)
		}
	}
	one(100)
	settle(1, "a first value")
	one(100)
	settle(1, "the same value again")
	batch(100)
	settle(1, "the same value in a batch")
	batch(120)
	settle(2, "a new value in a batch")
	one(100)
	settle(3, "a new value")
}
