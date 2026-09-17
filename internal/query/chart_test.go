package query_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/query"
)

// TestResolveCascadesHiddenMemberFromAncestorDimension is a regression test
// proving ChartResolver.Resolve cascades a hidden dimension_member rule down
// the dimension hierarchy — a rule set directly on a parent dimension's
// member (e.g. departments) must also hide every child-dimension member
// that rolls up to it (e.g. staff), even though the chart is plotted
// directly on the child dimension and the two dimensions never appear
// together on the same grid. Mirrors internal/gateway's grid() cascade
// (generic_rollup_workflow_test.go), the only other place this logic is
// exercised end-to-end.
func TestResolveCascadesHiddenMemberFromAncestorDimension(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	pool := store.Pool()

	modelID := "00000000-0000-0000-0000-000000000010"
	revisionID := "00000000-0000-0000-0010-000000000010"
	userID := "00000000-0000-0000-0000-000000000199"

	var deptDimID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'departments', $2::uuid) RETURNING id::text`,
		modelID, revisionID,
	).Scan(&deptDimID); err != nil {
		t.Fatalf("insert departments dim: %v", err)
	}
	var staffDimID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO model.dimension_def (model_id, name, revision_id, parent_dimension_id) VALUES ($1::uuid, 'staff', $2::uuid, $3::uuid) RETURNING id::text`,
		modelID, revisionID, deptDimID,
	).Scan(&staffDimID); err != nil {
		t.Fatalf("insert staff dim: %v", err)
	}

	var deptAID, deptBID string
	if err := pool.QueryRow(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'DEPT_A', 'Dept A') RETURNING id::text`, deptDimID).Scan(&deptAID); err != nil {
		t.Fatalf("insert DEPT_A: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'DEPT_B', 'Dept B') RETURNING id::text`, deptDimID).Scan(&deptBID); err != nil {
		t.Fatalf("insert DEPT_B: %v", err)
	}
	var staffA1ID, staffB1ID string
	if err := pool.QueryRow(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id) VALUES ($1::uuid, 'STAFF_A1', 'A1', $2::uuid) RETURNING id::text`, staffDimID, deptAID).Scan(&staffA1ID); err != nil {
		t.Fatalf("insert STAFF_A1: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id) VALUES ($1::uuid, 'STAFF_B1', 'B1', $2::uuid) RETURNING id::text`, staffDimID, deptBID).Scan(&staffB1ID); err != nil {
		t.Fatalf("insert STAFF_B1: %v", err)
	}

	var metricID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO model.metric_def (model_id, name, is_input, revision_id) VALUES ($1::uuid, 'amount', true, $2::uuid) RETURNING id::text`,
		modelID, revisionID,
	).Scan(&metricID); err != nil {
		t.Fatalf("insert metric: %v", err)
	}

	var gridID string
	if err := pool.QueryRow(ctx, `INSERT INTO model.grid_def (model_id, name) VALUES ($1::uuid, 'Staff Grid') RETURNING id::text`, modelID).Scan(&gridID); err != nil {
		t.Fatalf("insert grid: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, gridID, staffDimID); err != nil {
		t.Fatalf("insert grid_dimension: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, gridID, metricID); err != nil {
		t.Fatalf("insert grid_metric: %v", err)
	}

	for _, f := range []struct {
		dimMembers string
		value      float64
	}{
		{`{"` + staffDimID + `":"STAFF_A1"}`, 100},
		{`{"` + staffDimID + `":"STAFF_B1"}`, 200},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, $3::uuid, $4::jsonb, $5, $6::uuid)`,
			modelID, revisionID, metricID, f.dimMembers, f.value, userID,
		); err != nil {
			t.Fatalf("insert fact: %v", err)
		}
	}

	// Hidden rule on DEPT_B only — deliberately no rule at all on STAFF_B1.
	if _, err := pool.Exec(ctx,
		`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'hidden')`,
		userID, deptBID,
	); err != nil {
		t.Fatalf("hide DEPT_B: %v", err)
	}

	resolver := query.NewChartResolver(pool)
	cfg := &query.ChartConfig{ChartType: query.ChartBar, DimensionID: staffDimID, MetricIDs: []string{metricID}}

	result, err := resolver.Resolve(ctx, cfg, nil, modelID, revisionID, gridID, userID)
	if err != nil {
		t.Fatalf("Resolve (restricted user): %v", err)
	}
	data, ok := result.(*query.CategoryChartData)
	if !ok {
		t.Fatalf("result type = %T, want *query.CategoryChartData", result)
	}
	var codes []string
	for _, c := range data.Categories {
		codes = append(codes, c.Key)
	}
	if len(codes) != 1 || codes[0] != "STAFF_A1" {
		t.Errorf("restricted user's categories = %v, want exactly [STAFF_A1] (STAFF_B1 must cascade-hide from DEPT_B's rule even without its own rule)", codes)
	}
	if len(data.Series) != 1 || len(data.Series[0].Values) != 1 || data.Series[0].Values[0] == nil || *data.Series[0].Values[0] != 100 {
		t.Errorf("restricted user's series = %+v, want one series with a single value 100", data.Series)
	}

	// Unrestricted user (no access rules at all) sees both.
	otherUserID := "00000000-0000-0000-0000-000000000299"
	resultAll, err := resolver.Resolve(ctx, cfg, nil, modelID, revisionID, gridID, otherUserID)
	if err != nil {
		t.Fatalf("Resolve (unrestricted user): %v", err)
	}
	dataAll, ok := resultAll.(*query.CategoryChartData)
	if !ok {
		t.Fatalf("result type = %T, want *query.CategoryChartData", resultAll)
	}
	if len(dataAll.Categories) != 2 {
		t.Errorf("unrestricted user's categories = %v, want 2 (both STAFF_A1 and STAFF_B1)", dataAll.Categories)
	}
}

// TestResolveCalcMetricRollsUpThroughRollupPackage proves a calculated
// metric's chart value is composed correctly at both a leaf member and a
// same-dimension rollup member (a "TEAM_A" parent within staff itself,
// unrelated to any grid/dept structure) — the calculated metric's INPUT
// dependency (amount) must roll up TEAM_A's two children (STAFF_A1,
// STAFF_A2) via internal/rollup before the formula evaluates, proving
// resolveMetricPerMember/evalCalcMetricVisited's new rollup.Resolve wiring
// end to end, not just a plain per-leaf formula evaluation.
func TestResolveCalcMetricRollsUpThroughRollupPackage(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	pool := store.Pool()

	modelID := "00000000-0000-0000-0000-000000000020"
	revisionID := "00000000-0000-0000-0020-000000000020"
	userID := "00000000-0000-0000-0000-000000000299"

	var staffDimID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'staff', $2::uuid) RETURNING id::text`,
		modelID, revisionID,
	).Scan(&staffDimID); err != nil {
		t.Fatalf("insert staff dim: %v", err)
	}

	var teamAID string
	if err := pool.QueryRow(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label, sort_order) VALUES ($1::uuid, 'TEAM_A', 'Team A', 0) RETURNING id::text`, staffDimID).Scan(&teamAID); err != nil {
		t.Fatalf("insert TEAM_A: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label, sort_order, parent_member_id) VALUES ($1::uuid, 'STAFF_A1', 'A1', 1, $2::uuid)`, staffDimID, teamAID); err != nil {
		t.Fatalf("insert STAFF_A1: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label, sort_order, parent_member_id) VALUES ($1::uuid, 'STAFF_A2', 'A2', 2, $2::uuid)`, staffDimID, teamAID); err != nil {
		t.Fatalf("insert STAFF_A2: %v", err)
	}

	var amountID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO model.metric_def (model_id, name, is_input, revision_id) VALUES ($1::uuid, 'amount', true, $2::uuid) RETURNING id::text`,
		modelID, revisionID,
	).Scan(&amountID); err != nil {
		t.Fatalf("insert amount metric: %v", err)
	}
	var totalID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO model.metric_def (model_id, name, is_input, revision_id, formula) VALUES ($1::uuid, 'total', false, $2::uuid, 'amount * 1.1') RETURNING id::text`,
		modelID, revisionID,
	).Scan(&totalID); err != nil {
		t.Fatalf("insert total metric: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid)`,
		totalID, amountID,
	); err != nil {
		t.Fatalf("insert calc_dependency: %v", err)
	}

	var gridID string
	if err := pool.QueryRow(ctx, `INSERT INTO model.grid_def (model_id, name) VALUES ($1::uuid, 'Staff Grid') RETURNING id::text`, modelID).Scan(&gridID); err != nil {
		t.Fatalf("insert grid: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, gridID, staffDimID); err != nil {
		t.Fatalf("insert grid_dimension: %v", err)
	}
	for _, metricID := range []string{amountID, totalID} {
		if _, err := pool.Exec(ctx, `INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, gridID, metricID); err != nil {
			t.Fatalf("insert grid_metric: %v", err)
		}
	}

	for _, f := range []struct {
		code  string
		value float64
	}{
		{"STAFF_A1", 100},
		{"STAFF_A2", 200},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, $3::uuid, $4::jsonb, $5, $6::uuid)`,
			modelID, revisionID, amountID, `{"`+staffDimID+`":"`+f.code+`"}`, f.value, userID,
		); err != nil {
			t.Fatalf("insert fact %s: %v", f.code, err)
		}
	}

	resolver := query.NewChartResolver(pool)
	cfg := &query.ChartConfig{ChartType: query.ChartBar, DimensionID: staffDimID, MetricIDs: []string{totalID}}

	result, err := resolver.Resolve(ctx, cfg, nil, modelID, revisionID, gridID, userID)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	data, ok := result.(*query.CategoryChartData)
	if !ok {
		t.Fatalf("result type = %T, want *query.CategoryChartData", result)
	}
	if len(data.Series) != 1 {
		t.Fatalf("series count = %d, want 1", len(data.Series))
	}

	got := make(map[string]*float64, len(data.Categories))
	for i, c := range data.Categories {
		got[c.Key] = data.Series[0].Values[i]
	}

	want := map[string]float64{
		"STAFF_A1": 100 * 1.1,
		"STAFF_A2": 200 * 1.1,
		"TEAM_A":   (100 + 200) * 1.1, // same-dimension rollup: sum of A1+A2, then the formula applied
	}
	for code, wantVal := range want {
		v, ok := got[code]
		if !ok || v == nil {
			t.Errorf("category %s: missing value, want %v", code, wantVal)
			continue
		}
		if diff := *v - wantVal; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("category %s total = %v, want %v", code, *v, wantVal)
		}
	}
}

// TestResolveSubstitutesHiddenContextMember: a chart whose saved
// context_defaults (or the caller's runtime context) name a member hidden
// from THIS viewer must still resolve — for the dimension's default visible
// member, reported back in Context — rather than fail. Failing 403'd the
// whole widget in a retry loop for a business user with a hidden-member
// rule and, through the dashboard's shared selectors, pushed the hidden
// value into every other widget (reported live 2026-09-13, "Sales
// Overview": Laptop hidden, line chart pinned to Laptop). A context member
// that does not exist at all is still an error — that is a broken config,
// not an access rule.
func TestResolveSubstitutesHiddenContextMember(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	pool := store.Pool()

	modelID := "00000000-0000-0000-0000-000000000011"
	revisionID := "00000000-0000-0000-0011-000000000011"
	userID := "00000000-0000-0000-0000-000000000399"
	otherUserID := "00000000-0000-0000-0000-000000000499"

	var regionDimID, productDimID string
	if err := pool.QueryRow(ctx, `INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'region', $2::uuid) RETURNING id::text`, modelID, revisionID).Scan(&regionDimID); err != nil {
		t.Fatalf("insert region dim: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'product', $2::uuid) RETURNING id::text`, modelID, revisionID).Scan(&productDimID); err != nil {
		t.Fatalf("insert product dim: %v", err)
	}
	for _, code := range []string{"EU", "US"} {
		if _, err := pool.Exec(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label, sort_order) VALUES ($1::uuid, $2, $2, 0)`, regionDimID, code); err != nil {
			t.Fatalf("insert region %s: %v", code, err)
		}
	}
	var laptopID string
	if _, err := pool.Exec(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label, sort_order) VALUES ($1::uuid, 'MONITOR', 'Monitor', 1)`, productDimID); err != nil {
		t.Fatalf("insert MONITOR: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label, sort_order) VALUES ($1::uuid, 'LAPTOP', 'Laptop', 2) RETURNING id::text`, productDimID).Scan(&laptopID); err != nil {
		t.Fatalf("insert LAPTOP: %v", err)
	}

	var metricID string
	if err := pool.QueryRow(ctx, `INSERT INTO model.metric_def (model_id, name, is_input, revision_id) VALUES ($1::uuid, 'units', true, $2::uuid) RETURNING id::text`, modelID, revisionID).Scan(&metricID); err != nil {
		t.Fatalf("insert metric: %v", err)
	}
	var gridID string
	if err := pool.QueryRow(ctx, `INSERT INTO model.grid_def (model_id, name) VALUES ($1::uuid, 'Sales') RETURNING id::text`, modelID).Scan(&gridID); err != nil {
		t.Fatalf("insert grid: %v", err)
	}
	for _, dimID := range []string{regionDimID, productDimID} {
		if _, err := pool.Exec(ctx, `INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, gridID, dimID); err != nil {
			t.Fatalf("insert grid_dimension: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, gridID, metricID); err != nil {
		t.Fatalf("insert grid_metric: %v", err)
	}
	for _, f := range []struct {
		region, product string
		value           float64
	}{
		{"EU", "LAPTOP", 10}, {"US", "LAPTOP", 20}, {"EU", "MONITOR", 1}, {"US", "MONITOR", 2},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, $3::uuid, $4::jsonb, $5, $6::uuid)`,
			modelID, revisionID, metricID, `{"`+regionDimID+`":"`+f.region+`","`+productDimID+`":"`+f.product+`"}`, f.value, userID,
		); err != nil {
			t.Fatalf("insert fact: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'hidden')`, userID, laptopID); err != nil {
		t.Fatalf("hide LAPTOP: %v", err)
	}

	resolver := query.NewChartResolver(pool)
	cfg := &query.ChartConfig{
		ChartType: query.ChartBar, DimensionID: regionDimID, MetricIDs: []string{metricID},
		ContextDefaults: map[string]string{productDimID: "LAPTOP"},
	}
	values := func(res interface{}) []float64 {
		data, ok := res.(*query.CategoryChartData)
		if !ok {
			t.Fatalf("result type = %T, want *query.CategoryChartData", res)
		}
		var out []float64
		for _, v := range data.Series[0].Values {
			if v == nil {
				out = append(out, -1)
			} else {
				out = append(out, *v)
			}
		}
		return out
	}

	// Restricted viewer: LAPTOP is hidden → resolved for MONITOR, and the
	// response says so.
	res, err := resolver.Resolve(ctx, cfg, nil, modelID, revisionID, gridID, userID)
	if err != nil {
		t.Fatalf("Resolve for the restricted viewer must not fail on a hidden context default: %v", err)
	}
	if got := res.(*query.CategoryChartData).Context[productDimID]; got != "MONITOR" {
		t.Errorf("Context[product] = %q, want MONITOR (the default visible member)", got)
	}
	if got := values(res); len(got) != 2 || got[0]+got[1] != 3 {
		t.Errorf("restricted viewer's values = %v, want MONITOR's (1 and 2), never LAPTOP's", got)
	}

	// Unrestricted viewer: the saved default applies as designed.
	res, err = resolver.Resolve(ctx, cfg, nil, modelID, revisionID, gridID, otherUserID)
	if err != nil {
		t.Fatalf("Resolve for the unrestricted viewer: %v", err)
	}
	if got := res.(*query.CategoryChartData).Context[productDimID]; got != "LAPTOP" {
		t.Errorf("unrestricted Context[product] = %q, want LAPTOP", got)
	}
	if got := values(res); len(got) != 2 || got[0]+got[1] != 30 {
		t.Errorf("unrestricted viewer's values = %v, want LAPTOP's (10 and 20)", got)
	}

	// A member that does not exist is a config error, not an access rule.
	_, err = resolver.Resolve(ctx, cfg, map[string]string{productDimID: "TABLET"}, modelID, revisionID, gridID, otherUserID)
	if err == nil || !strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "hidden") {
		t.Errorf("nonexistent context member: err = %v, want a 'not found' error that is not classed as hidden", err)
	}
}
