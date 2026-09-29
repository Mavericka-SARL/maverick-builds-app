package query_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/query"
)

// A chart on a time dimension plots its periods in period order, as the grid
// shows them — not by code. Time members are written with sort_order 0, so a
// developer's custom period codes (JAN, FEB, ... from the time CSV import)
// came out alphabetically: on the axis, in the context selector, and in the
// member a restricted viewer's hidden context default is replaced with.
func TestChartTimeMembersFollowPeriodOrder(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	pool := store.Pool()
	modelID := "00000000-0000-0000-0000-000000000011"
	revisionID := "00000000-0000-0000-0011-000000000011"
	userID := "00000000-0000-0000-0000-000000000399"

	var monthDim, regionDim string
	if err := pool.QueryRow(ctx, `INSERT INTO model.dimension_def (model_id, name, revision_id, dimension_type, time_granularity, fiscal_year_start_month) VALUES ($1::uuid, 'months', $2::uuid, 'time', 'month', 1) RETURNING id::text`, modelID, revisionID).Scan(&monthDim); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'region', $2::uuid) RETURNING id::text`, modelID, revisionID).Scan(&regionDim); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		code, s, e string
		i          int
	}{{"FEB", "2026-02-01", "2026-02-28", 1}, {"MAR", "2026-03-01", "2026-03-31", 2}, {"JAN", "2026-01-01", "2026-01-31", 0}, {"APR", "2026-04-01", "2026-04-30", 3}} {
		if _, err := pool.Exec(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index) VALUES ($1::uuid,$2,$2,$3::date,$4::date,$5)`, monthDim, m.code, m.s, m.e, m.i); err != nil {
			t.Fatal(err)
		}
	}
	var janID string
	_ = pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='JAN'`, monthDim).Scan(&janID)
	if _, err := pool.Exec(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label, sort_order) VALUES ($1::uuid,'EU','EU',1)`, regionDim); err != nil {
		t.Fatal(err)
	}
	var metricID, gridID string
	_ = pool.QueryRow(ctx, `INSERT INTO model.metric_def (model_id, name, is_input, revision_id) VALUES ($1::uuid, 'units', true, $2::uuid) RETURNING id::text`, modelID, revisionID).Scan(&metricID)
	_ = pool.QueryRow(ctx, `INSERT INTO model.grid_def (model_id, name) VALUES ($1::uuid, 'Sales') RETURNING id::text`, modelID).Scan(&gridID)
	for _, d := range []string{monthDim, regionDim} {
		_, _ = pool.Exec(ctx, `INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, gridID, d)
	}
	_, _ = pool.Exec(ctx, `INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, gridID, metricID)

	resolver := query.NewChartResolver(pool)
	cfg := &query.ChartConfig{ChartType: query.ChartBar, DimensionID: monthDim, MetricIDs: []string{metricID}}
	res, err := resolver.Resolve(ctx, cfg, nil, modelID, revisionID, gridID, userID)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, c := range res.(*query.CategoryChartData).Categories {
		keys = append(keys, c.Label)
	}
	t.Logf("plotted time categories: %s", strings.Join(keys, ","))
	if strings.Join(keys, ",") != "JAN,FEB,MAR,APR" {
		t.Errorf("chart plots time members as %s, grid shows JAN,FEB,MAR,APR", strings.Join(keys, ","))
	}

	// Context default on the time dimension, hidden for the viewer: server
	// substitutes its default visible member.
	if _, err := pool.Exec(ctx, `INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'hidden')`, userID, janID); err != nil {
		t.Fatal(err)
	}
	cfg2 := &query.ChartConfig{ChartType: query.ChartBar, DimensionID: regionDim, MetricIDs: []string{metricID}, ContextDefaults: map[string]string{monthDim: "JAN"}}
	res2, err := resolver.Resolve(ctx, cfg2, nil, modelID, revisionID, gridID, userID)
	if err != nil {
		t.Fatal(err)
	}
	cd := res2.(*query.CategoryChartData)
	var ctxCodes []string
	for _, d := range cd.ContextDims {
		if d.ID == monthDim {
			for _, m := range d.Members {
				ctxCodes = append(ctxCodes, m.Code)
			}
		}
	}
	t.Logf("substituted = %q; context selector members = %v (grid order after hiding JAN: FEB,MAR,APR)", cd.Context[monthDim], ctxCodes)
	if cd.Context[monthDim] != "FEB" {
		t.Errorf("substitute for hidden JAN = %q, want FEB (the first visible period, as the client's defaultLeafCode picks from the grid's member order)", cd.Context[monthDim])
	}
}
