package query_test

import (
	"context"
	"math"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/query"
)

// A formula metric over an input that carries fewer dimensions than it does
// reads that input once at an aggregate of the dimensions the input does not
// carry — never rolled up along them. HR Planning's effective global note
// (= drv_global, keyed by cost type only) charted at All Departments × FY read
// the 2.5% rate once per department and month: 7 × 12 × 2.5 = 210, where the
// grid showed 2.5. Here three departments and three months: 22.5 before.
func TestResolveFormulaOverNarrowerInputIgnoresUnrelatedPins(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	pool := store.Pool()
	modelID := "00000000-0000-0000-0000-000000000031"
	revisionID := "00000000-0000-0000-0031-000000000031"
	userID := "00000000-0000-0000-0000-000000000331"

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	id := func(sql string, args ...any) string {
		t.Helper()
		var out string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return out
	}

	costDim := id(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'cost_type', $2::uuid) RETURNING id::text`, modelID, revisionID)
	deptDim := id(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'department', $2::uuid) RETURNING id::text`, modelID, revisionID)
	monthDim := id(`INSERT INTO model.dimension_def (model_id, name, revision_id, dimension_type, time_granularity, fiscal_year_start_month) VALUES ($1::uuid, 'months', $2::uuid, 'time', 'month', 1) RETURNING id::text`, modelID, revisionID)

	for i, code := range []string{"BASE", "HEALTH"} {
		exec(`INSERT INTO model.dimension_member (dimension_id, code, label, sort_order) VALUES ($1::uuid, $2, $2, $3)`, costDim, code, i)
	}
	allID := id(`INSERT INTO model.dimension_member (dimension_id, code, label, sort_order) VALUES ($1::uuid, 'ALL', 'All Departments', 0) RETURNING id::text`, deptDim)
	for i, code := range []string{"D1", "D2", "D3"} {
		exec(`INSERT INTO model.dimension_member (dimension_id, code, label, sort_order, parent_member_id) VALUES ($1::uuid, $2, $2, $3, $4::uuid)`, deptDim, code, i+1, allID)
	}
	fyID := id(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end) VALUES ($1::uuid, 'FY2027', 'FY2027', '2027-01-01', '2027-12-31') RETURNING id::text`, monthDim)
	for i, m := range []struct{ code, s, e string }{
		{"JAN", "2027-01-01", "2027-01-31"}, {"FEB", "2027-02-01", "2027-02-28"}, {"MAR", "2027-03-01", "2027-03-31"},
	} {
		exec(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, parent_member_id) VALUES ($1::uuid, $2, $2, $3::date, $4::date, $5, $6::uuid)`, monthDim, m.code, m.s, m.e, i, fyID)
	}

	rateID := id(`INSERT INTO model.metric_def (model_id, name, is_input, revision_id, format, agg_rule, time_summary) VALUES ($1::uuid, 'drv_global', true, $2::uuid, 'percentage', 'none', 'none') RETURNING id::text`, modelID, revisionID)
	noteID := id(`INSERT INTO model.metric_def (model_id, name, is_input, revision_id, format, formula, agg_rule, time_summary) VALUES ($1::uuid, 'cost_global_pct', false, $2::uuid, 'percentage', 'drv_global', 'formula', 'none') RETURNING id::text`, modelID, revisionID)
	exec(`INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid)`, noteID, rateID)

	driversGrid := id(`INSERT INTO model.grid_def (model_id, name) VALUES ($1::uuid, 'Drivers') RETURNING id::text`, modelID)
	exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, driversGrid, costDim)
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, driversGrid, rateID)
	planGrid := id(`INSERT INTO model.grid_def (model_id, name) VALUES ($1::uuid, 'Monthly Cost Plan') RETURNING id::text`, modelID)
	for _, d := range []string{costDim, deptDim, monthDim} {
		exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, planGrid, d)
	}
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, planGrid, noteID)

	want := map[string]float64{"BASE": 2.5, "HEALTH": 6}
	for code, v := range want {
		exec(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, $3::uuid, $4::jsonb, $5, $6::uuid)`,
			modelID, revisionID, rateID, `{"`+costDim+`":"`+code+`"}`, v, userID)
	}

	resolver := query.NewChartResolver(pool)
	for _, pins := range []map[string]string{
		{deptDim: "ALL", monthDim: "FY2027"},
		{deptDim: "ALL", monthDim: "JAN"},
		{deptDim: "D2", monthDim: "FY2027"},
	} {
		cfg := &query.ChartConfig{ChartType: query.ChartBar, DimensionID: costDim, MetricIDs: []string{noteID}}
		result, err := resolver.Resolve(ctx, cfg, pins, modelID, revisionID, planGrid, userID)
		if err != nil {
			t.Fatalf("Resolve %v: %v", pins, err)
		}
		data := result.(*query.CategoryChartData)
		for i, c := range data.Categories {
			v := data.Series[0].Values[i]
			if v == nil {
				t.Errorf("at %v, %s has no value, want %v", pins, c.Key, want[c.Key])
			} else if math.Abs(*v-want[c.Key]) > 1e-9 {
				t.Errorf("at %v, %s = %v, want %v (the rate once, not rolled up along department or month)", pins, c.Key, *v, want[c.Key])
			}
		}
	}

	// A workflow condition reads the same input at a point the same way.
	got, err := resolver.MetricValuesAt(ctx, modelID, revisionID, []string{"drv_global", "cost_global_pct"},
		map[string]string{costDim: "BASE", deptDim: "ALL", monthDim: "FY2027"})
	if err != nil {
		t.Fatalf("MetricValuesAt: %v", err)
	}
	for name, v := range got {
		if n, ok := v.Number(); !ok || math.Abs(n-2.5) > 1e-9 {
			t.Errorf("MetricValuesAt %s = %v, want 2.5", name, v)
		}
	}
}
