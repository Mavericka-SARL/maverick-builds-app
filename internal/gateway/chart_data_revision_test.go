package gateway

// Regression: a chart on a dashboard in a NON-active revision must resolve in
// that revision. chart-data used to resolve the viewer's active revision and
// then redirect the chart's grid to that revision's same-named grid, so the
// config's dimension/metric IDs (the designed revision's) were looked up in
// another revision's grid: 400 "plotted dimension not found in grid" for every
// chart outside the active revision (found live, 2026-09-11, "Sales Overview").

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestChartDataResolvesInTheChartsOwnRevisionWhenNotActive(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	seedChartDashboard(t, f, "Draft dash")

	// Copy the working revision through the developer API; the copy is NOT activated.
	status, res := f.do(t, "POST", "/api/developer/revisions", "rollup-test-approver", map[string]any{"name": "Draft copy", "source_revision_id": f.workingRevID})
	if status != http.StatusOK {
		t.Fatalf("duplicate revision: status=%d body=%v", status, res)
	}
	copyRev, _ := res["id"].(string)
	var active string
	if err := f.pool.QueryRow(ctx, `SELECT active_revision_id::text FROM core.model WHERE id=$1::uuid`, f.modelID).Scan(&active); err != nil {
		t.Fatalf("active revision: %v", err)
	}
	if active == copyRev {
		t.Fatalf("precondition: the copy must not be the active revision")
	}
	var copiedChartWidgetID string
	if err := f.pool.QueryRow(ctx, `
		SELECT w.id::text FROM model.dashboard_widget w
		JOIN model.dashboard_def d ON d.id = w.dashboard_id
		WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid AND d.name='Draft dash' AND w.widget_type='chart'
	`, f.modelID, copyRev).Scan(&copiedChartWidgetID); err != nil {
		t.Fatalf("resolve copied chart widget: %v", err)
	}
	if status, body := doAs(t, f, "POST", "/api/dashboard-widgets/"+copiedChartWidgetID+"/chart-data", "rollup-test-approver", f.appID,
		map[string]any{"context": map[string]string{}}); status != http.StatusOK {
		t.Errorf("chart-data for a chart in a non-active revision: status=%d body=%s (want 200)", status, body)
	}
	// The active revision's own chart keeps working too.
	var activeChartWidgetID string
	if err := f.pool.QueryRow(ctx, `
		SELECT w.id::text FROM model.dashboard_widget w
		JOIN model.dashboard_def d ON d.id = w.dashboard_id
		WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid AND d.name='Draft dash' AND w.widget_type='chart'
	`, f.modelID, f.workingRevID).Scan(&activeChartWidgetID); err != nil {
		t.Fatalf("resolve active chart widget: %v", err)
	}
	if status, body := doAs(t, f, "POST", "/api/dashboard-widgets/"+activeChartWidgetID+"/chart-data", "rollup-test-approver", f.appID,
		map[string]any{"context": map[string]string{}}); status != http.StatusOK {
		t.Errorf("chart-data for the active revision's chart: status=%d body=%s (want 200)", status, body)
	}
}

// A widget PATCH that carries only widget_props (or only a title) must leave
// geometry and content alone: decoding absent geometry as 0 and clamping it
// to 20 shrank widgets to 20×20 and blanked their content (found live,
// 2026-09-11 — a chart collapsed to a few pixels after a props-only update).
func TestDashboardWidgetPatchWithOnlyPropsKeepsGeometryAndContent(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	dashID, chartWidgetID := seedChartDashboard(t, f, "Patch dash")
	if _, err := f.pool.Exec(ctx, `UPDATE model.dashboard_widget SET pos_x=40, pos_y=60, size_w=580, size_h=320, content='keep' WHERE id=$1::uuid`, chartWidgetID); err != nil {
		t.Fatalf("seed geometry: %v", err)
	}
	status, res := f.do(t, "PATCH", "/api/developer/dashboards/"+dashID+"/widgets/"+chartWidgetID, "rollup-test-approver",
		map[string]any{"widget_props": map[string]any{"selectors_position": "bottom"}})
	if status != http.StatusOK {
		t.Fatalf("patch status = %d, body = %v", status, res)
	}
	var posX, posY, sizeW, sizeH int
	var content, props string
	if err := f.pool.QueryRow(ctx, `SELECT pos_x, pos_y, size_w, size_h, COALESCE(content,''), widget_props::text FROM model.dashboard_widget WHERE id=$1::uuid`, chartWidgetID).Scan(&posX, &posY, &sizeW, &sizeH, &content, &props); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if posX != 40 || posY != 60 || sizeW != 580 || sizeH != 320 || content != "keep" {
		t.Errorf("props-only PATCH changed geometry/content: pos=(%d,%d) size=(%d,%d) content=%q", posX, posY, sizeW, sizeH, content)
	}
	if props == "" || !strings.Contains(props, "selectors_position") {
		t.Errorf("widget_props not updated: %s", props)
	}
	// Geometry sent explicitly still applies (and is still clamped to the minimum).
	status, res = f.do(t, "PATCH", "/api/developer/dashboards/"+dashID+"/widgets/"+chartWidgetID, "rollup-test-approver",
		map[string]any{"pos_x": 10, "pos_y": 20, "size_w": 5, "size_h": 400})
	if status != http.StatusOK {
		t.Fatalf("geometry patch status = %d, body = %v", status, res)
	}
	if err := f.pool.QueryRow(ctx, `SELECT pos_x, pos_y, size_w, size_h FROM model.dashboard_widget WHERE id=$1::uuid`, chartWidgetID).Scan(&posX, &posY, &sizeW, &sizeH); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if posX != 10 || posY != 20 || sizeW != 20 || sizeH != 400 {
		t.Errorf("explicit geometry PATCH: pos=(%d,%d) size=(%d,%d), want (10,20) (20,400)", posX, posY, sizeW, sizeH)
	}
}
