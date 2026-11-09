package query_test

import (
	"context"
	"math"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/query"
)

// A chart computes a formula over an input read across a dimension as the
// scheduler does (calculation TestReadAcrossADimensionWithNothingRecordedIsBlank):
// in a month with nothing entered under it on any account the input is
// blank to ISBLANK, and a workflow condition reads it blank there too.
func TestChartReadAcrossADimensionWithNothingRecordedIsBlank(t *testing.T) {
	pool, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-000000000035"
	revisionID := "00000000-0000-0000-0035-000000000035"
	userID := "00000000-0000-0000-0000-000000000335"

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

	glDim := id(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'gl', $2::uuid) RETURNING id::text`, modelID, revisionID)
	monthDim := id(`INSERT INTO model.dimension_def (model_id, name, revision_id, dimension_type, time_granularity, fiscal_year_start_month) VALUES ($1::uuid, 'month', $2::uuid, 'time', 'month', 1) RETURNING id::text`, modelID, revisionID)
	allID := id(`INSERT INTO model.dimension_member (dimension_id, code, label, sort_order) VALUES ($1::uuid, 'ALLGL', 'All', 0) RETURNING id::text`, glDim)
	for i, code := range []string{"G1", "G2"} {
		exec(`INSERT INTO model.dimension_member (dimension_id, code, label, sort_order, parent_member_id) VALUES ($1::uuid, $2, $2, $3, $4::uuid)`, glDim, code, i+1, allID)
	}
	for i, m := range []struct{ code, s, e string }{
		{"JAN", "2026-01-01", "2026-01-31"}, {"FEB", "2026-02-01", "2026-02-28"}, {"MAR", "2026-03-01", "2026-03-31"},
	} {
		exec(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, sort_order) VALUES ($1::uuid, $2, $2, $3::date, $4::date, $5, $5)`, monthDim, m.code, m.s, m.e, i)
	}

	actualID := id(`INSERT INTO model.metric_def (model_id, name, is_input, revision_id) VALUES ($1::uuid, 'actual', true, $2::uuid) RETURNING id::text`, modelID, revisionID)
	flagID := id(`INSERT INTO model.metric_def (model_id, name, is_input, revision_id, formula) VALUES ($1::uuid, 'flag', false, $2::uuid, 'IF(ISBLANK(actual), -1, 1)') RETURNING id::text`, modelID, revisionID)
	exec(`INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid)`, flagID, actualID)

	ledger := id(`INSERT INTO model.grid_def (model_id, name) VALUES ($1::uuid, 'Ledger') RETURNING id::text`, modelID)
	exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid), ($1::uuid, $3::uuid)`, ledger, glDim, monthDim)
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, ledger, actualID)
	monthly := id(`INSERT INTO model.grid_def (model_id, name) VALUES ($1::uuid, 'Monthly') RETURNING id::text`, modelID)
	exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, monthly, monthDim)
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, monthly, flagID)

	for _, f := range []struct {
		gl, month string
		v         float64
	}{{"G1", "JAN", 10}, {"G2", "FEB", 20}} {
		exec(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, $3::uuid, $4::jsonb, $5, $6::uuid)`,
			modelID, revisionID, actualID, `{"`+glDim+`":"`+f.gl+`","`+monthDim+`":"`+f.month+`"}`, f.v, userID)
	}

	resolver := query.NewChartResolver(pool)
	cfg := &query.ChartConfig{ChartType: query.ChartBar, DimensionID: monthDim, MetricIDs: []string{flagID}}
	result, err := resolver.Resolve(ctx, cfg, map[string]string{}, modelID, revisionID, monthly, userID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	data := result.(*query.CategoryChartData)
	want := map[string]float64{"JAN": 1, "FEB": 1, "MAR": -1}
	for i, c := range data.Categories {
		v := data.Series[0].Values[i]
		if v == nil || math.Abs(*v-want[c.Key]) > 1e-9 {
			t.Errorf("flag at %s = %v, want %v", c.Key, v, want[c.Key])
		}
	}

	got, err := resolver.MetricValuesAt(ctx, modelID, revisionID, []string{"actual", "flag"}, map[string]string{monthDim: "MAR"})
	if err != nil {
		t.Fatalf("MetricValuesAt: %v", err)
	}
	if !got["actual"].IsBlank() {
		t.Errorf("actual in March = %v, want blank (nothing entered under it)", got["actual"])
	}
	if n, ok := got["flag"].Number(); !ok || n != -1 {
		t.Errorf("flag in March = %v, want -1", got["flag"])
	}
}
