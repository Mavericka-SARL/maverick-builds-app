package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// Regression tests for the dashboards ↔ platform synchronization audit.
//
// Dashboards reference the rest of the model in two places, not one:
// dashboard_widget.ref_id, and IDs embedded in the widget_props JSON blob
// (chart series/plotted dimension/context defaults, metric_kpi scope). Every
// copy path handled the first and ignored the second. Separately, access to a
// dashboard was decided purely by business-role grants plus a "this
// workspace has no business roles" fallback that no lifecycle operation
// maintained and that never checked the caller's tenant.
//
// Reuses setupRollupFixture from generic_rollup_workflow_test.go (same
// package): a real model with dimensions, metrics, grids and facts, plus a
// business_user ("rollup-test-manager") and a business_admin/developer
// ("rollup-test-approver").

// ── helpers ──────────────────────────────────────────────────────────────────

// doAs issues a request with an explicit X-App-Id (the fixture's own `do`
// always sends f.appID, which is exactly what a cross-tenant test must be
// able to vary — including omitting it entirely, as a caller probing another
// tenant's dashboard by raw ID would).
func doAs(t *testing.T, f *rollupFixture, method, path, persona, appID string, body any) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", persona)
	if appID != "" {
		req.Header.Set("X-App-Id", appID)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.String()
}

// seedChartDashboard adds a dashboard to the fixture's working revision
// carrying three widgets whose widget_props are full of metric/dimension IDs:
//
//	sort_order 0  chart, plotted on staff, series = amount (renderable, so the
//	              same widget can be driven end to end through /chart-data)
//	sort_order 1  metric_kpi scoped to a departments member
//	sort_order 2  chart with saved context_defaults, whose KEY is a dimension
//	              ID — kept off the renderable widget because the chart
//	              endpoint requires every context dimension to be on the
//	              bound grid, and the fixture's grids are single-dimension
//
// Returns the dashboard and the renderable chart widget's ID.
func seedChartDashboard(t *testing.T, f *rollupFixture, name string) (dashID, chartWidgetID string) {
	t.Helper()
	ctx := context.Background()
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.dashboard_def (model_id, name, revision_id) VALUES ($1::uuid, $2, $3::uuid) RETURNING id::text`,
		f.modelID, name, f.workingRevID).Scan(&dashID); err != nil {
		t.Fatalf("seed dashboard: %v", err)
	}
	chartProps := fmt.Sprintf(`{
		"chart": {
			"chart_type": "bar",
			"dimension_id": %q,
			"metric_ids": [%q],
			"bin_count": 12
		},
		"sync_context": true
	}`, f.staffDimID, f.amountMetricID)
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, sort_order, widget_props)
		VALUES ($1::uuid, 'chart', $2, 0, $3::jsonb) RETURNING id::text
	`, dashID, f.gridStaffID, chartProps).Scan(&chartWidgetID); err != nil {
		t.Fatalf("seed chart widget: %v", err)
	}
	kpiProps := fmt.Sprintf(`{"kpi_scope": {"dimension_id": %q, "member_code": "DEPT_A"}, "color": "#123456"}`, f.deptsDimID)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, sort_order, widget_props)
		VALUES ($1::uuid, 'metric_kpi', $2, 1, $3::jsonb)
	`, dashID, f.amountMetricID, kpiProps); err != nil {
		t.Fatalf("seed kpi widget: %v", err)
	}
	ctxChartProps := fmt.Sprintf(`{
		"chart": {
			"chart_type": "line",
			"dimension_id": %q,
			"metric_ids": [%q],
			"context_defaults": {%q: "DEPT_A"}
		}
	}`, f.staffDimID, f.amountMetricID, f.deptsDimID)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, sort_order, widget_props)
		VALUES ($1::uuid, 'chart', $2, 2, $3::jsonb)
	`, dashID, f.gridStaffID, ctxChartProps); err != nil {
		t.Fatalf("seed context chart widget: %v", err)
	}
	return dashID, chartWidgetID
}

// ── SYNC-01: tenancy ─────────────────────────────────────────────────────────

// TestDashboardEndpointsRejectForeignTenant covers the audit's most severe
// finding. GET /api/dashboards/{id} and POST
// /api/dashboard-widgets/{id}/chart-data authorised on "caller holds a
// business-role grant for this dashboard OR this dashboard's workspace
// defines no business roles at all". That second clause is the default state
// of every freshly created workspace, and it is evaluated against the
// DASHBOARD's workspace while saying nothing about the caller's — so any
// authenticated user of any tenant could read any dashboard, and its actual
// chart values, in any role-less workspace. Reproduced live against demo
// data before the fix: a Meridian Industries business_user with zero grants
// pulled Acme Corp's OPEX figures.
//
// The sibling list endpoint was never affected because it filters
// dd.model_id to the caller's resolved model; these two resolve no model of
// their own, hence the explicit dashboardModelInScope check.
func TestDashboardEndpointsRejectForeignTenant(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	// A second, entirely unrelated tenant with its own dashboard. No
	// business roles exist anywhere in this fixture, so the permissive
	// fallback is active for both workspaces — which is the point.
	var otherAppID, otherModelID, otherRevID, otherDashID, otherWidgetID string
	q := func(dest *string, sql string, args ...any) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
			t.Fatalf("seed other tenant %q: %v", sql, err)
		}
	}
	var otherCustID, otherWsID string
	q(&otherCustID, `INSERT INTO core.customer (name, plan) VALUES ('Other Corp', 'enterprise') RETURNING id::text`)
	q(&otherWsID, `INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'other-ws') RETURNING id::text`, otherCustID)
	q(&otherAppID, `INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Other App', 'planning') RETURNING id::text`, otherWsID, otherCustID)
	q(&otherModelID, `INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Other Model') RETURNING id::text`, otherAppID)
	q(&otherRevID, `INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, otherModelID)
	if _, err := f.pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, otherRevID, otherModelID); err != nil {
		t.Fatalf("activate other revision: %v", err)
	}
	var otherDimID, otherMetricID, otherGridID string
	q(&otherDimID, `INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'units') RETURNING id::text`, otherModelID, otherRevID)
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'U1', 'Unit 1')`, otherDimID); err != nil {
		t.Fatalf("seed other member: %v", err)
	}
	q(&otherMetricID, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format) VALUES ($1::uuid, $2::uuid, 'secret_cost', true, 'currency') RETURNING id::text`, otherModelID, otherRevID)
	q(&otherGridID, `INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Other Grid', $2::uuid) RETURNING id::text`, otherModelID, otherRevID)
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, otherGridID, otherDimID); err != nil {
		t.Fatalf("seed other grid dim: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 0)`, otherGridID, otherMetricID); err != nil {
		t.Fatalf("seed other grid metric: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by)
		VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, jsonb_build_object($4::text, 'U1'), 4242, $5::uuid)
	`, otherModelID, otherRevID, otherMetricID, otherDimID, f.managerID); err != nil {
		t.Fatalf("seed other fact: %v", err)
	}
	q(&otherDashID, `INSERT INTO model.dashboard_def (model_id, name, revision_id) VALUES ($1::uuid, 'Other Dashboard', $2::uuid) RETURNING id::text`, otherModelID, otherRevID)
	otherChartProps := fmt.Sprintf(`{"chart":{"chart_type":"bar","dimension_id":%q,"metric_ids":[%q]}}`, otherDimID, otherMetricID)
	q(&otherWidgetID, `INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, sort_order, widget_props) VALUES ($1::uuid, 'chart', $2, 0, $3::jsonb) RETURNING id::text`,
		otherDashID, otherGridID, otherChartProps)

	// Positive control: the fixture's own user reaches their OWN dashboard.
	ownDashID, ownChartWidgetID := seedChartDashboard(t, f, "Own Dashboard")
	if status, body := doAs(t, f, "GET", "/api/dashboards/"+ownDashID, "rollup-test-manager", f.appID, nil); status != http.StatusOK {
		t.Fatalf("own dashboard detail: status=%d body=%s (want 200 — the tenancy gate must not block legitimate access)", status, body)
	}
	if status, body := doAs(t, f, "POST", "/api/dashboard-widgets/"+ownChartWidgetID+"/chart-data", "rollup-test-manager", f.appID, map[string]any{"context": map[string]string{}}); status != http.StatusOK {
		t.Fatalf("own chart data: status=%d body=%s (want 200)", status, body)
	}

	// The leak itself: another tenant's dashboard, by raw ID.
	for _, appHeader := range []string{f.appID, otherAppID, ""} {
		label := appHeader
		if label == "" {
			label = "(no X-App-Id)"
		}
		status, body := doAs(t, f, "GET", "/api/dashboards/"+otherDashID, "rollup-test-manager", appHeader, nil)
		if status == http.StatusOK {
			t.Errorf("cross-tenant dashboard detail with app header %s: status=200 body=%s — a foreign tenant's dashboard must not be readable", label, body)
		}
		status, body = doAs(t, f, "POST", "/api/dashboard-widgets/"+otherWidgetID+"/chart-data", "rollup-test-manager", appHeader, map[string]any{"context": map[string]string{}})
		if status == http.StatusOK {
			t.Errorf("cross-tenant chart data with app header %s: status=200 body=%s — a foreign tenant's values must not be readable", label, body)
		}
		if bytes.Contains([]byte(body), []byte("4242")) {
			t.Errorf("cross-tenant chart data with app header %s leaked the foreign fact value: %s", label, body)
		}
	}

	// The write side of the same hole: a business admin could hand their own
	// role a foreign workspace's dashboard, which the endpoints above then
	// honour on sight.
	var roleID string
	q(&roleID, `INSERT INTO identity.business_role (workspace_id, name) VALUES ((SELECT workspace_id FROM core.application WHERE id=$1::uuid), 'Reviewers') RETURNING id::text`, f.appID)
	status, body := doAs(t, f, "PUT", "/api/business-admin/roles/"+roleID+"/dashboards", "rollup-test-approver", f.appID,
		map[string]any{"dashboard_ids": []string{otherDashID}})
	if status == http.StatusOK {
		t.Errorf("granting a foreign workspace's dashboard: status=200 body=%s — want a rejection", body)
	}
	var granted int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM identity.business_role_dashboard WHERE role_id=$1::uuid`, roleID).Scan(&granted); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if granted != 0 {
		t.Errorf("foreign dashboard grant persisted (%d rows) despite the rejection", granted)
	}

	// …while granting the role its OWN workspace's dashboard still works.
	if status, body := doAs(t, f, "PUT", "/api/business-admin/roles/"+roleID+"/dashboards", "rollup-test-approver", f.appID,
		map[string]any{"dashboard_ids": []string{ownDashID}}); status != http.StatusOK {
		t.Fatalf("granting an in-workspace dashboard: status=%d body=%s (want 200)", status, body)
	}
}

// ── SYNC-02 / SYNC-03: revision duplication ──────────────────────────────────

// TestDuplicateRevisionRemapsWidgetPropsAndCopiesGrants covers the two
// revision-copy gaps found by the audit and reproduced live:
//
//	SYNC-03  Step I remapped dashboard_widget.ref_id but never the IDs inside
//	         widget_props, so a duplicated revision's charts pointed at the
//	         SOURCE revision's metrics and dimensions. The chart endpoint
//	         rejects that outright ("plotted dimension not found in grid");
//	         a stale kpi_scope.dimension_id fails silently instead, falling
//	         back to the metric's whole-model total.
//
//	SYNC-02  identity.business_role_dashboard was never carried onto the
//	         copies, so activating a duplicated revision revoked every
//	         dashboard from every business user at once — with no error, an
//	         empty grant set being indistinguishable from "no dashboards".
func TestDuplicateRevisionRemapsWidgetPropsAndCopiesGrants(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	h := &handler{db: tenantdb.NewHandle(f.pool, nil), log: logger.New("test")}

	dashID, _ := seedChartDashboard(t, f, "Ops Dashboard")

	var roleID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.business_role (workspace_id, name) VALUES ((SELECT workspace_id FROM core.application WHERE id=$1::uuid), 'Reviewers') RETURNING id::text`,
		f.appID).Scan(&roleID); err != nil {
		t.Fatalf("seed business role: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.business_role_dashboard (role_id, dashboard_id) VALUES ($1::uuid, $2::uuid)`,
		roleID, dashID); err != nil {
		t.Fatalf("seed dashboard grant: %v", err)
	}

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	newRevID, err := h.duplicateRevision(ctx, tx, f.modelID, "Copy", f.workingRevID, &f.workingRevID)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("duplicateRevision: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Resolve what the copy's own metric/dimension IDs should be.
	newID := func(table, name string) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT id::text FROM %s WHERE model_id=$1::uuid AND name=$2 AND revision_id=$3::uuid`, table),
			f.modelID, name, newRevID).Scan(&id); err != nil {
			t.Fatalf("resolve copied %s %q: %v", table, name, err)
		}
		return id
	}
	wantMetricID := newID("model.metric_def", "amount")
	wantStaffDimID := newID("model.dimension_def", "staff")
	wantDeptsDimID := newID("model.dimension_def", "departments")

	// SYNC-03: every ID inside widget_props points at the copy, not the source.
	type propsShape struct {
		Chart *struct {
			DimensionID     string            `json:"dimension_id"`
			MetricIDs       []string          `json:"metric_ids"`
			ContextDefaults map[string]string `json:"context_defaults"`
			BinCount        int               `json:"bin_count"`
		} `json:"chart"`
		KpiScope *struct {
			DimensionID string `json:"dimension_id"`
			MemberCode  string `json:"member_code"`
		} `json:"kpi_scope"`
		SyncContext bool   `json:"sync_context"`
		Color       string `json:"color"`
	}
	// sort_order identifies which seeded widget a copy came from — the same
	// key Step I's own old→new widget correlation uses.
	loadProps := func(widgetType string, sortOrder int) propsShape {
		t.Helper()
		var raw []byte
		if err := f.pool.QueryRow(ctx, `
			SELECT w.widget_props FROM model.dashboard_widget w
			JOIN model.dashboard_def d ON d.id = w.dashboard_id
			WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid AND w.widget_type=$3 AND w.sort_order=$4
		`, f.modelID, newRevID, widgetType, sortOrder).Scan(&raw); err != nil {
			t.Fatalf("load copied %s widget props (sort_order %d): %v", widgetType, sortOrder, err)
		}
		var p propsShape
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode copied %s widget props %s: %v", widgetType, raw, err)
		}
		return p
	}

	chart := loadProps("chart", 0)
	if chart.Chart == nil {
		t.Fatal("copied chart widget lost its chart config")
	}
	if chart.Chart.DimensionID != wantStaffDimID {
		t.Errorf("chart.dimension_id = %s, want the copy's staff dimension %s (source was %s)",
			chart.Chart.DimensionID, wantStaffDimID, f.staffDimID)
	}
	if len(chart.Chart.MetricIDs) != 1 || chart.Chart.MetricIDs[0] != wantMetricID {
		t.Errorf("chart.metric_ids = %v, want [%s] (source was [%s])",
			chart.Chart.MetricIDs, wantMetricID, f.amountMetricID)
	}
	// Untouched props must survive the rewrite intact.
	if chart.Chart.BinCount != 12 {
		t.Errorf("chart.bin_count = %d, want 12 preserved through the remap", chart.Chart.BinCount)
	}
	if !chart.SyncContext {
		t.Error("sync_context was dropped by the widget_props remap")
	}

	// context_defaults is keyed BY dimension ID, so the key itself must move.
	ctxChart := loadProps("chart", 2)
	if ctxChart.Chart == nil {
		t.Fatal("copied context-defaults chart widget lost its chart config")
	}
	if _, ok := ctxChart.Chart.ContextDefaults[wantDeptsDimID]; !ok {
		t.Errorf("chart.context_defaults keys = %v, want the copy's departments dimension %s (source was %s)",
			ctxChart.Chart.ContextDefaults, wantDeptsDimID, f.deptsDimID)
	}
	if got := ctxChart.Chart.ContextDefaults[wantDeptsDimID]; got != "DEPT_A" {
		t.Errorf("chart.context_defaults[departments] = %q, want DEPT_A — member codes copy verbatim, only the key is an ID", got)
	}

	kpi := loadProps("metric_kpi", 1)
	if kpi.KpiScope == nil {
		t.Fatal("copied metric_kpi widget lost its kpi_scope")
	}
	if kpi.KpiScope.DimensionID != wantDeptsDimID {
		t.Errorf("kpi_scope.dimension_id = %s, want the copy's departments dimension %s (source was %s) — a stale one silently shows the unscoped total",
			kpi.KpiScope.DimensionID, wantDeptsDimID, f.deptsDimID)
	}
	if kpi.KpiScope.MemberCode != "DEPT_A" {
		t.Errorf("kpi_scope.member_code = %q, want DEPT_A", kpi.KpiScope.MemberCode)
	}
	if kpi.Color != "#123456" {
		t.Errorf("kpi color = %q, want #123456 preserved through the remap", kpi.Color)
	}

	// SYNC-02: the grant follows the dashboard onto the copy.
	var newDashID string
	if err := f.pool.QueryRow(ctx,
		`SELECT id::text FROM model.dashboard_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Ops Dashboard'`,
		f.modelID, newRevID).Scan(&newDashID); err != nil {
		t.Fatalf("resolve copied dashboard: %v", err)
	}
	var grantedToCopy bool
	if err := f.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM identity.business_role_dashboard WHERE role_id=$1::uuid AND dashboard_id=$2::uuid)`,
		roleID, newDashID).Scan(&grantedToCopy); err != nil {
		t.Fatalf("check copied grant: %v", err)
	}
	if !grantedToCopy {
		t.Error("business_role_dashboard grant was not copied onto the duplicated revision's dashboard — activating that revision hides every dashboard from every business user")
	}
	// The source grant is untouched.
	var sourceStillGranted bool
	if err := f.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM identity.business_role_dashboard WHERE role_id=$1::uuid AND dashboard_id=$2::uuid)`,
		roleID, dashID).Scan(&sourceStillGranted); err != nil {
		t.Fatalf("check source grant: %v", err)
	}
	if !sourceStillGranted {
		t.Error("duplicating a revision removed the SOURCE dashboard's grant")
	}

	// End to end: activate the copy and confirm the chart still resolves for
	// a business user — the live repro's exact failure mode (403 with no
	// grant, then 400 "plotted dimension not found in grid" once granted).
	if _, err := h.activateRevision(ctx, newRevID); err != nil {
		t.Fatalf("activate copied revision: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`,
		roleID, f.managerID); err != nil {
		t.Fatalf("add role member: %v", err)
	}
	var copiedChartWidgetID string
	if err := f.pool.QueryRow(ctx, `
		SELECT w.id::text FROM model.dashboard_widget w
		JOIN model.dashboard_def d ON d.id = w.dashboard_id
		WHERE d.id=$1::uuid AND w.widget_type='chart'
	`, newDashID).Scan(&copiedChartWidgetID); err != nil {
		t.Fatalf("resolve copied chart widget: %v", err)
	}
	if status, body := doAs(t, f, "POST", "/api/dashboard-widgets/"+copiedChartWidgetID+"/chart-data", "rollup-test-manager", f.appID,
		map[string]any{"context": map[string]string{}}); status != http.StatusOK {
		t.Errorf("chart-data on the duplicated revision's chart: status=%d body=%s (want 200)", status, body)
	}
	if status, body := doAs(t, f, "GET", "/api/dashboards", "rollup-test-manager", f.appID, nil); status != http.StatusOK || !bytes.Contains([]byte(body), []byte(newDashID)) {
		t.Errorf("business dashboard list after activating the copy: status=%d body=%s (want the copied dashboard %s)", status, body, newDashID)
	}
}

// ── SYNC-09: dashboard folders ───────────────────────────────────────────────

// TestDashboardFolderAssignment covers the audit's open gap: model.
// dashboard_folder had a table, four developer routes, a business listing
// route, revision-copy remapping, import/export and AI-promote support — and
// no way whatsoever to put a dashboard IN one. Neither the create nor the
// update endpoint accepted folder_id, neither read returned it, and the
// frontend had zero references to folders, so dashboard_def.folder_id was
// permanently NULL for every dashboard in the product.
func TestDashboardFolderAssignment(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	type folderResp struct {
		ID       string  `json:"id"`
		Name     string  `json:"name"`
		ParentID *string `json:"parent_id"`
	}
	decode := func(body string, dest any) {
		t.Helper()
		if err := json.Unmarshal([]byte(body), dest); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
	}

	// Create a folder, then a dashboard filed directly into it.
	status, body := doAs(t, f, "POST", "/api/developer/folders", "rollup-test-approver", f.appID, map[string]any{"name": "Finance"})
	if status != http.StatusOK {
		t.Fatalf("create folder: status=%d body=%s", status, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	decode(body, &created)
	folderID := created.ID

	status, body = doAs(t, f, "POST", "/api/developer/dashboards", "rollup-test-approver", f.appID,
		map[string]any{"name": "Filed At Birth", "folder_id": folderID})
	if status != http.StatusOK {
		t.Fatalf("create dashboard in folder: status=%d body=%s", status, body)
	}
	decode(body, &created)
	filedDashID := created.ID

	var storedFolder *string
	if err := f.pool.QueryRow(ctx, `SELECT folder_id::text FROM model.dashboard_def WHERE id=$1::uuid`, filedDashID).Scan(&storedFolder); err != nil {
		t.Fatalf("read created dashboard: %v", err)
	}
	if storedFolder == nil || *storedFolder != folderID {
		t.Fatalf("created dashboard folder_id = %v, want %s", storedFolder, folderID)
	}

	// An unfiled dashboard can be moved into the folder and back out again.
	status, body = doAs(t, f, "POST", "/api/developer/dashboards", "rollup-test-approver", f.appID, map[string]any{"name": "Moved Later"})
	if status != http.StatusOK {
		t.Fatalf("create unfiled dashboard: status=%d body=%s", status, body)
	}
	decode(body, &created)
	movedDashID := created.ID

	readFolderID := func() *string {
		t.Helper()
		var got *string
		if err := f.pool.QueryRow(ctx, `SELECT folder_id::text FROM model.dashboard_def WHERE id=$1::uuid`, movedDashID).Scan(&got); err != nil {
			t.Fatalf("read dashboard folder: %v", err)
		}
		return got
	}
	if got := readFolderID(); got != nil {
		t.Fatalf("new dashboard with no folder_id = %v, want unfiled", *got)
	}

	if status, body = doAs(t, f, "PATCH", "/api/developer/dashboards/"+movedDashID, "rollup-test-approver", f.appID,
		map[string]any{"name": "Moved Later", "folder_id": folderID}); status != http.StatusOK {
		t.Fatalf("move dashboard into folder: status=%d body=%s", status, body)
	}
	if got := readFolderID(); got == nil || *got != folderID {
		t.Errorf("after move, folder_id = %v, want %s", got, folderID)
	}

	// Omitting folder_id must leave the dashboard where it is — a rename
	// must not silently unfile it.
	if status, body = doAs(t, f, "PATCH", "/api/developer/dashboards/"+movedDashID, "rollup-test-approver", f.appID,
		map[string]any{"name": "Renamed Only"}); status != http.StatusOK {
		t.Fatalf("rename dashboard: status=%d body=%s", status, body)
	}
	if got := readFolderID(); got == nil || *got != folderID {
		t.Errorf("after a rename with no folder_id, folder_id = %v, want it unchanged at %s", got, folderID)
	}

	// An explicit empty string moves it back to the root…
	if status, body = doAs(t, f, "PATCH", "/api/developer/dashboards/"+movedDashID, "rollup-test-approver", f.appID,
		map[string]any{"name": "Renamed Only", "folder_id": ""}); status != http.StatusOK {
		t.Fatalf("unfile dashboard: status=%d body=%s", status, body)
	}
	if got := readFolderID(); got != nil {
		t.Errorf("after unfiling, folder_id = %v, want nil", *got)
	}

	// …and so does an explicit null, which is what a JSON client that models
	// the field as nullable will send. encoding/json can't tell null from
	// absent through a *string, so this case is the reason the handler
	// decodes folder_id as a RawMessage.
	if status, body = doAs(t, f, "PATCH", "/api/developer/dashboards/"+movedDashID, "rollup-test-approver", f.appID,
		map[string]any{"name": "Renamed Only", "folder_id": folderID}); status != http.StatusOK {
		t.Fatalf("re-file dashboard: status=%d body=%s", status, body)
	}
	if got := readFolderID(); got == nil || *got != folderID {
		t.Fatalf("re-file left folder_id = %v, want %s", got, folderID)
	}
	if status, body = doAs(t, f, "PATCH", "/api/developer/dashboards/"+movedDashID, "rollup-test-approver", f.appID,
		map[string]any{"name": "Renamed Only", "folder_id": nil}); status != http.StatusOK {
		t.Fatalf("unfile dashboard with null: status=%d body=%s", status, body)
	}
	if got := readFolderID(); got != nil {
		t.Errorf("after unfiling with null, folder_id = %v, want nil", *got)
	}

	// Both reads expose folder_id so the consoles can group by it.
	status, body = doAs(t, f, "GET", "/api/developer/dashboards", "rollup-test-approver", f.appID, nil)
	if status != http.StatusOK {
		t.Fatalf("list dashboards: status=%d body=%s", status, body)
	}
	var devList []struct {
		ID       string  `json:"id"`
		FolderID *string `json:"folder_id"`
	}
	decode(body, &devList)
	foundFiled := false
	for _, d := range devList {
		if d.ID == filedDashID {
			foundFiled = true
			if d.FolderID == nil || *d.FolderID != folderID {
				t.Errorf("developer list folder_id = %v, want %s", d.FolderID, folderID)
			}
		}
	}
	if !foundFiled {
		t.Error("filed dashboard missing from the developer list")
	}

	// The business list too — grant it first, so the role filter passes.
	var roleID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.business_role (workspace_id, name) VALUES ((SELECT workspace_id FROM core.application WHERE id=$1::uuid), 'Viewers') RETURNING id::text`,
		f.appID).Scan(&roleID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.business_role_dashboard (role_id, dashboard_id) VALUES ($1::uuid, $2::uuid)`,
		roleID, filedDashID); err != nil {
		t.Fatalf("grant dashboard: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`,
		roleID, f.managerID); err != nil {
		t.Fatalf("add role member: %v", err)
	}
	status, body = doAs(t, f, "GET", "/api/dashboards", "rollup-test-manager", f.appID, nil)
	if status != http.StatusOK {
		t.Fatalf("business dashboard list: status=%d body=%s", status, body)
	}
	var bizList []struct {
		ID       string  `json:"id"`
		FolderID *string `json:"folder_id"`
	}
	decode(body, &bizList)
	if len(bizList) != 1 || bizList[0].ID != filedDashID {
		t.Fatalf("business list = %s, want just the granted dashboard %s", body, filedDashID)
	}
	if bizList[0].FolderID == nil || *bizList[0].FolderID != folderID {
		t.Errorf("business list folder_id = %v, want %s", bizList[0].FolderID, folderID)
	}
	// …and the folder itself is listed, so the view can name the group.
	status, body = doAs(t, f, "GET", "/api/folders", "rollup-test-manager", f.appID, nil)
	if status != http.StatusOK {
		t.Fatalf("business folder list: status=%d body=%s", status, body)
	}
	var bizFolders []folderResp
	decode(body, &bizFolders)
	if len(bizFolders) != 1 || bizFolders[0].ID != folderID {
		t.Errorf("business folder list = %s, want the one folder %s", body, folderID)
	}

	// A folder from a different model is rejected rather than silently
	// filing the dashboard somewhere it can never be seen.
	var otherModelID, otherFolderID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Other M') RETURNING id::text`, f.appID).Scan(&otherModelID); err != nil {
		t.Fatalf("seed other model: %v", err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.dashboard_folder (model_id, name) VALUES ($1::uuid, 'Foreign') RETURNING id::text`, otherModelID).Scan(&otherFolderID); err != nil {
		t.Fatalf("seed other folder: %v", err)
	}
	if status, body = doAs(t, f, "PATCH", "/api/developer/dashboards/"+movedDashID, "rollup-test-approver", f.appID,
		map[string]any{"name": "Renamed Only", "folder_id": otherFolderID}); status != http.StatusBadRequest {
		t.Errorf("filing under another model's folder: status=%d body=%s, want 400", status, body)
	}
	if got := readFolderID(); got != nil {
		t.Errorf("rejected move still changed folder_id to %v", *got)
	}
}
