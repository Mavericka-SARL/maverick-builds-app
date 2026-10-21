package gateway

// Closed cells (migration 147): an input metric's closed_when rules read a
// calculated metric of the same grid at the cell; where one holds the cell
// takes no input — POST /api/cells and the batch refuse it, saying why — and
// the other cells stay open. The rules validate (a calculated metric, on the
// same grid) and travel with the metric.

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestClosedCells_Route(t *testing.T) {
	f := setupMMFixture(t)
	ctx := context.Background()
	const dev = "mm-dev"
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	month := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Period') RETURNING id::text`, f.modelA, f.revA)
	for _, c := range []string{"M01", "M02", "M03"} {
		q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, $2, $2) RETURNING id::text`, month, c)
	}
	grid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Plan') RETURNING id::text`, f.modelA, f.revA)
	q(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid) RETURNING id::text`, grid, month)
	metric := func(name string, input bool, formula string) string {
		id := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula) VALUES ($1::uuid, $2::uuid, $3, $4, NULLIF($5,'')) RETURNING id::text`,
			f.modelA, f.revA, name, input, formula)
		q(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid) RETURNING metric_id::text`, grid, id)
		return id
	}
	plan := metric("plan_pct", true, "")
	actualMonth := metric("actual_month", false, "1")
	other := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula) VALUES ($1::uuid, $2::uuid, 'elsewhere', false, '1') RETURNING id::text`, f.modelA, f.revA)
	// actual_month's stored values: M01 and M02 are actual, M03 is not (a
	// blank counts as 0).
	for _, c := range []string{"M01", "M02"} {
		q(`INSERT INTO runtime.calc_result (model_id, revision_id, metric_id, dim_members, value)
			VALUES ($1::uuid, $2::uuid, $3::uuid, jsonb_build_object($4::text, $5::text), 1) RETURNING id::text`,
			f.modelA, f.revA, actualMonth, month, c)
	}
	_ = other

	patch := func(body any) (int, string) {
		t.Helper()
		code, raw := f.do(t, "PATCH", "/api/developer/metrics/"+plan, dev, f.app1, f.modelA, body)
		return code, string(raw)
	}
	// Validation: an input or another grid's metric is refused.
	for name, rules := range map[string]any{
		"an input":      []map[string]any{{"metric": "plan_pct", "op": "=", "value": 1}},
		"no metric":     []map[string]any{{"metric": "nothing", "op": "=", "value": 1}},
		"an unknown op": []map[string]any{{"metric": "actual_month", "op": "~", "value": 1}},
	} {
		if code, raw := patch(map[string]any{"name": "plan_pct", "formula": "", "closed_when": rules}); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, code, raw)
		}
	}
	if code, raw := patch(map[string]any{"name": "plan_pct", "formula": "", "closed_when": []map[string]any{{"metric": "Actual_Month", "op": "=", "value": 1}}}); code != http.StatusOK {
		t.Fatalf("set closed_when: %d %s", code, raw)
	}
	if got := q(`SELECT closed_when::text FROM model.metric_def WHERE id=$1::uuid`, plan); !strings.Contains(got, `"metric": "actual_month"`) {
		t.Errorf("stored rules %s (the name as the revision spells it)", got)
	}

	write := func(code string, value float64) (int, string) {
		t.Helper()
		status, raw := f.do(t, "POST", "/api/cells", "mm-ba1", f.app1, f.modelA, map[string]any{
			"model_id": f.modelA, "revision_id": f.revA, "metric_id": plan,
			"dim_codes": map[string]string{month: code}, "value": value})
		return status, string(raw)
	}
	if status, raw := write("M01", 5); status != http.StatusForbidden || !strings.Contains(raw, "takes no input here") {
		t.Errorf("closed M01: %d %s, want 403 saying why", status, raw)
	}
	if status, raw := write("M03", 5); status != http.StatusOK {
		t.Errorf("open M03: %d %s, want 200", status, raw)
	}
	// The batch refuses the whole write when one cell is closed.
	status, raw := f.do(t, "POST", "/api/cells/batch", "mm-ba1", f.app1, f.modelA, map[string]any{
		"model_id": f.modelA, "revision_id": f.revA, "cells": []map[string]any{
			{"metric_id": plan, "dim_codes": map[string]string{month: "M03"}, "value": 6},
			{"metric_id": plan, "dim_codes": map[string]string{month: "M02"}, "value": 6},
		}})
	if status == http.StatusOK || !strings.Contains(string(raw), "takes no input here") {
		t.Errorf("batch with a closed cell: %d %s, want refused", status, raw)
	}

	// The grid answers the rules with the metric.
	code, graw := f.do(t, "GET", "/api/grid?grid_def_id="+grid, dev, f.app1, f.modelA, nil)
	if code != http.StatusOK || !strings.Contains(string(graw), `"closed_when":[{`) || !strings.Contains(string(graw), `"metric":"actual_month"`) {
		t.Errorf("grid: %d, closed_when missing in %.400s", code, graw)
	}

	// [] removes them: M01 opens.
	if code, raw := patch(map[string]any{"name": "plan_pct", "formula": "", "closed_when": []any{}}); code != http.StatusOK {
		t.Fatalf("clear closed_when: %d %s", code, raw)
	}
	if status, raw := write("M01", 5); status != http.StatusOK {
		t.Errorf("M01 after clearing: %d %s", status, raw)
	}
}
