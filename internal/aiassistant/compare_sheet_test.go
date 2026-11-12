package aiassistant_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// compare_with_sheet reads a row of an attached file along the metric's time
// dimension and names each period that differs (2026-11-07).
func TestCompareWithSheetNamesEveryDifference(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	ctx := context.Background()
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Working")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	month := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type, time_granularity, fiscal_year_start_month)
		VALUES ($1::uuid, $2::uuid, 'Month', 'time', 'month', 1) RETURNING id::text`, modelID, revID)
	scen := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Scenario') RETURNING id::text`, modelID, revID)
	for i, code := range []string{"2026-01", "2026-02", "2026-03"} {
		q(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index)
			VALUES ($1::uuid, $2, $2, ($2 || '-01')::date, (($2 || '-01')::date + interval '1 month' - interval '1 day')::date, $3) RETURNING id::text`, month, code, i)
	}
	q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'BASE', 'Base') RETURNING id::text`, scen)
	metric := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule, format) VALUES ($1::uuid, $2::uuid, 'net_sales', true, 'sum', 'number') RETURNING id::text`, modelID, revID)
	grid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Sales') RETURNING id::text`, modelID, revID)
	for _, d := range []string{month, scen} {
		q(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid) RETURNING grid_id::text`, grid, d)
	}
	q(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid) RETURNING grid_id::text`, grid, metric)
	for code, v := range map[string]float64{"2026-01": 100, "2026-02": 250} {
		q(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
			VALUES ($1::uuid, $2::uuid, $3::uuid, jsonb_build_object($4::text, $5::text, $6::text, 'BASE'), $7) RETURNING id::text`,
			modelID, revID, metric, month, code, scen, v)
	}

	e := aiassistant.NewToolExecutor(pool, modelID, revID).WithReadHooks(aiassistant.ReadHooks{
		AttachedFile: func(context.Context, string) (string, []byte, error) {
			return "sales.csv", []byte("Line,Unit,Jan,Feb,Mar\nNet sales,EUR,100,200,300\n"), nil
		},
	})
	out, err := e.Execute(ctx, "compare_with_sheet", mustJSON(t, map[string]any{
		"file": "sales.csv", "row": 2, "first_column": "C", "metric": "net_sales", "members": map[string]string{"scenario": "BASE"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 of 3 periods match", "2026-02: sheet 200, model 250", "2026-03: sheet 300, model no value"} {
		if !strings.Contains(out, want) {
			t.Errorf("answer %q lacks %q", out, want)
		}
	}
	// The comparison is kept in the workbook map, with the row's label.
	mapped, err := aiassistant.LoadWorkbookMap(ctx, pool, modelID, revID, nil)
	if err != nil || len(mapped) != 1 || mapped[0].Metric != "net_sales" || mapped[0].Label != "Net sales" || mapped[0].Matched != 1 || mapped[0].Compared != 3 || mapped[0].Members["scenario"] != "BASE" {
		t.Fatalf("workbook map: %+v %v", mapped, err)
	}
	// A session with no draft yet reads the model's map too.
	if rows, err := aiassistant.LoadWorkbookMap(ctx, pool, modelID, "", nil); err != nil || len(rows) != 1 {
		t.Fatalf("workbook map with no revision: %+v %v", rows, err)
	}
	// A row of one number — an event list's line — is not compared period by
	// period: it is said to be no row of periods.
	e = aiassistant.NewToolExecutor(pool, modelID, revID).WithReadHooks(aiassistant.ReadHooks{
		AttachedFile: func(context.Context, string) (string, []byte, error) {
			return "hires.csv", []byte("Function,Month,FTE\nEngineering,2026-02,2\n"), nil
		},
	})
	out, err = e.Execute(ctx, "compare_with_sheet", mustJSON(t, map[string]any{
		"file": "hires.csv", "row": 2, "first_column": "C", "metric": "net_sales", "members": map[string]string{"scenario": "BASE"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, aiassistant.NotPeriodRow) || strings.Contains(out, "periods match") {
		t.Errorf("an event row was compared by period: %q", out)
	}
}
