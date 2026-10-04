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

// Live, the assistant put KPI tiles at x=1200 and three more at x=1, 2, 3.
func TestWidgetPlacementIsChecked(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()
	_, dash, err := exec.Execute(ctx, "create_dashboard", mustJSON(t, map[string]any{"name": "D"}))
	if err != nil {
		t.Fatal(err)
	}
	add := func(x, y int) error {
		_, _, err := exec.Execute(ctx, "add_dashboard_widget", mustJSON(t, map[string]any{"dashboard_id": dash, "widget_type": "text",
			"content": "t", "pos_x": x, "pos_y": y, "size_w": 300, "size_h": 100}))
		return err
	}
	if err := add(0, 0); err != nil {
		t.Fatal(err)
	}
	if err := add(300, 0); err != nil {
		t.Errorf("a tile beside the first: %v", err)
	}
	if err := add(1200, 0); err == nil || !strings.Contains(err.Error(), "does not fit") {
		t.Errorf("a tile at x=1200: %v, want refused", err)
	}
	if err := add(1, 50); err == nil || !strings.Contains(err.Error(), "overlaps") || !strings.Contains(err.Error(), "below y=100") {
		t.Errorf("a tile on top of another: %v, want refused with where to go", err)
	}
}

// Live: {"chart": {"ref_id": "Dashboard Data", …}} with no widget ref_id and
// no plotted dimension. The grid moves up to the widget, and a grid with one
// dimension plots along it.
func TestChartGridInPropsIsHoisted(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()
	run := func(tool string, p map[string]any) string {
		t.Helper()
		_, id, err := exec.Execute(ctx, tool, mustJSON(t, p))
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return id
	}
	dimID := run("create_dimension", map[string]any{"name": "Month", "members": []map[string]any{{"code": "M1", "label": "M1"}}})
	run("create_metric", map[string]any{"name": "a", "is_input": true})
	run("create_metric", map[string]any{"name": "b", "is_input": true})
	gridID := run("create_grid", map[string]any{"name": "Dashboard Data", "metrics": []string{"a", "b"}, "dimensions": []string{"Month"}})
	dash := run("create_dashboard", map[string]any{"name": "D"})
	wid := run("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "chart", "pos_x": 0, "pos_y": 0, "size_w": 600, "size_h": 300,
		"widget_props": map[string]any{"chart": map[string]any{"ref_id": "Dashboard Data", "chart_type": "line", "metric_ids": []string{"a", "b"}}}})
	var ref, props string
	if err := pool.QueryRow(ctx, `SELECT ref_id::text, widget_props::text FROM model.dashboard_widget WHERE id=$1::uuid`, wid).Scan(&ref, &props); err != nil {
		t.Fatal(err)
	}
	if ref != gridID || !strings.Contains(props, dimID) || strings.Contains(props, `"ref_id"`) {
		t.Errorf("ref_id %s (want %s), props %s (want the Month dimension, no ref_id inside)", ref, gridID, props)
	}
}

// A KPI tile the assistant adds shows the total, and a chart leaves out
// total members, unless the step says otherwise.
func TestAIWidgetDefaults(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()
	run := func(tool string, p map[string]any) string {
		t.Helper()
		_, id, err := exec.Execute(ctx, tool, mustJSON(t, p))
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return id
	}
	run("create_dimension", map[string]any{"name": "Month", "members": []map[string]any{{"code": "M1", "label": "M1"}}})
	run("create_metric", map[string]any{"name": "a", "is_input": true})
	run("create_grid", map[string]any{"name": "G", "metrics": []string{"a"}, "dimensions": []string{"Month"}})
	dash := run("create_dashboard", map[string]any{"name": "D"})
	props := func(id string) string {
		var s string
		_ = pool.QueryRow(ctx, `SELECT COALESCE(widget_props::text,'') FROM model.dashboard_widget WHERE id=$1::uuid`, id).Scan(&s)
		return s
	}
	kpi := run("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "metric_kpi", "ref_id": "a", "pos_x": 0, "pos_y": 0, "size_w": 300, "size_h": 120})
	synced := run("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "metric_kpi", "ref_id": "a", "pos_x": 300, "pos_y": 0, "size_w": 300, "size_h": 120,
		"widget_props": map[string]any{"kpi_context_mode": "sync"}})
	chart := run("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "chart", "ref_id": "G", "pos_x": 0, "pos_y": 200, "size_w": 600, "size_h": 300,
		"widget_props": map[string]any{"chart": map[string]any{"chart_type": "line", "metric_ids": []string{"a"}}}})
	if !strings.Contains(props(kpi), `"kpi_context_mode": "total"`) {
		t.Errorf("kpi props %s, want the total", props(kpi))
	}
	if !strings.Contains(props(synced), `"kpi_context_mode": "sync"`) {
		t.Errorf("an explicit sync was overridden: %s", props(synced))
	}
	if !strings.Contains(props(chart), `"hide_rollup_members": true`) {
		t.Errorf("chart props %s, want total members left out", props(chart))
	}
}

// The AI draft's copy of a revision matched widgets by (type, sort_order);
// KPI tiles added by the assistant all have sort_order 0, so every copied
// tile pointed at one metric (live: twelve tiles all "rolling revenue
// forecast").
func TestCreateRevisionKeepsEachKPITilesMetric(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revA := seedRevision(t, pool, modelID, "A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revA)
	ctx := context.Background()
	run := func(tool string, p map[string]any) string {
		t.Helper()
		_, id, err := exec.Execute(ctx, tool, mustJSON(t, p))
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return id
	}
	for _, m := range []string{"revenue", "cost", "margin_value"} {
		run("create_metric", map[string]any{"name": m, "is_input": true})
	}
	dash := run("create_dashboard", map[string]any{"name": "KPIs"})
	for i, m := range []string{"revenue", "cost", "margin_value"} {
		run("add_dashboard_widget", map[string]any{"dashboard_id": dash, "widget_type": "metric_kpi", "ref_id": m,
			"pos_x": i * 300, "pos_y": 0, "size_w": 300, "size_h": 120})
	}
	revB := run("create_revision", map[string]any{"name": "B", "source_revision_id": revA})
	rows, err := pool.Query(ctx, `SELECT m.name FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id = w.dashboard_id
		JOIN model.metric_def m ON m.id::text = w.ref_id AND m.revision_id = d.revision_id
		WHERE d.revision_id = $1::uuid ORDER BY w.pos_x`, revB)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		got = append(got, n)
	}
	if strings.Join(got, ",") != "revenue,cost,margin_value" {
		t.Errorf("copied tiles point at %v, want revenue, cost, margin_value", got)
	}
}
