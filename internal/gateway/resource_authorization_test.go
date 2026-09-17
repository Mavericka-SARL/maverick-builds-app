package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// foreignTenant is a complete second tenant — its own customer, workspace,
// application, model, revision and one row of every resource kind whose ID a
// developer/admin handler accepts from the URL. Tenant A's developer must not
// be able to touch any of it.
type foreignTenant struct {
	custID, wsID, appID, modelID, revisionID string
	dashboardID, widgetID, folderID          string
	gridID, metricID, dimensionID, memberID  string
	formID, integrationID, mappingID         string
	workflowDefID, ruleID                    string
}

func seedForeignTenant(t *testing.T, f *rollupFixture) *foreignTenant {
	t.Helper()
	ctx := context.Background()
	ft := &foreignTenant{}
	q := func(dest *string, sql string, args ...any) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
			t.Fatalf("seed foreign tenant (%s): %v", sql, err)
		}
	}

	q(&ft.custID, `INSERT INTO core.customer (name, plan) VALUES ('Foreign Corp', 'enterprise') RETURNING id::text`)
	q(&ft.wsID, `INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'foreign-ws') RETURNING id::text`, ft.custID)
	q(&ft.appID, `INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Foreign App', 'planning') RETURNING id::text`, ft.wsID, ft.custID)
	q(&ft.modelID, `INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Foreign Model') RETURNING id::text`, ft.appID)
	q(&ft.revisionID, `INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, ft.modelID)
	if _, err := f.pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, ft.revisionID, ft.modelID); err != nil {
		t.Fatalf("activate foreign revision: %v", err)
	}

	q(&ft.metricID, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format) VALUES ($1::uuid, $2::uuid, 'foreign_cost', true, 'currency') RETURNING id::text`, ft.modelID, ft.revisionID)
	q(&ft.dimensionID, `INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'foreign_units') RETURNING id::text`, ft.modelID, ft.revisionID)
	q(&ft.memberID, `INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'FU1', 'Foreign Unit 1') RETURNING id::text`, ft.dimensionID)
	q(&ft.gridID, `INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Foreign Grid', $2::uuid) RETURNING id::text`, ft.modelID, ft.revisionID)
	q(&ft.folderID, `INSERT INTO model.dashboard_folder (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Foreign Folder') RETURNING id::text`, ft.modelID, ft.revisionID)
	q(&ft.dashboardID, `INSERT INTO model.dashboard_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Foreign Dashboard') RETURNING id::text`, ft.modelID, ft.revisionID)
	q(&ft.widgetID, `INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, sort_order) VALUES ($1::uuid, 'grid', $2, 0) RETURNING id::text`, ft.dashboardID, ft.gridID)
	q(&ft.formID, `INSERT INTO model.form_def (model_id, revision_id, name, label, fields) VALUES ($1::uuid, $2::uuid, 'foreign_form', 'Foreign Form', '[]'::jsonb) RETURNING id::text`, ft.modelID, ft.revisionID)
	q(&ft.integrationID, `INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, target_id) VALUES ($1::uuid, $2::uuid, 'foreign import', 'csv_import', 'grid', $3::uuid) RETURNING id::text`, ft.modelID, ft.revisionID, ft.gridID)
	q(&ft.mappingID, `
		INSERT INTO model.form_metric_mapping (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id, dimension_mappings)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'foreign map', 'amt', $5::uuid, '{}'::jsonb) RETURNING id::text`,
		ft.modelID, ft.revisionID, ft.formID, ft.gridID, ft.metricID)
	q(&ft.workflowDefID, `
		INSERT INTO workflow.workflow_def (application_id, revision_id, name, trigger_event, steps, status)
		VALUES ($1::uuid, $2::uuid, 'Foreign Approval', 'manual', '[]'::jsonb, 'draft') RETURNING id::text`, ft.appID, ft.revisionID)
	q(&ft.ruleID, `
		INSERT INTO workflow.automation_rule (application_id, revision_id, name, trigger_type, workflow_name, workflow_def_id, enabled)
		VALUES ($1::uuid, $2::uuid, 'Foreign Rule', 'manual', 'Foreign Approval', $3::uuid, true) RETURNING id::text`,
		ft.appID, ft.revisionID, ft.workflowDefID)

	return ft
}

// TestResourceHandlersRejectForeignTenant is the release-gate test for the
// resource-authorization finding: route-level guard(...) proves only that the
// caller holds the developer/admin ROLE, never that the UUID in the path names
// a row in a tenant they may touch. Before requireResourceAccess, every route
// below acted on another customer's data on sight — including destructive ones
// (DELETE a metric, DELETE a model) and ones that change what other people
// see (PUT .../revisions/{id}/activate repoints a foreign model's active
// revision).
//
// The assertion is deliberately "not 2xx": 404 for rows the caller may not
// even know exist, 403 once the row is resolvable but out of scope — the
// distinction is requireResourceAccess's business, not each route's.
func TestResourceHandlersRejectForeignTenant(t *testing.T) {
	f := setupRollupFixture(t)
	ft := seedForeignTenant(t, f)

	// A tenant_admin in tenant A, for the /api/admin/* routes. Scoped to
	// tenant A's customer, so tenant B must be out of reach.
	ctx := context.Background()
	var custID string
	if err := f.pool.QueryRow(ctx, `SELECT customer_id::text FROM core.workspace WHERE id=(SELECT workspace_id FROM core.application WHERE id=$1::uuid)`, f.appID).Scan(&custID); err != nil {
		t.Fatalf("resolve tenant A customer: %v", err)
	}
	var taID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-authz-ta', 'authz-ta@t.com', 'TA', $1::uuid) RETURNING id::text`,
		custID).Scan(&taID); err != nil {
		t.Fatalf("seed tenant admin: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', (SELECT workspace_id FROM core.application WHERE id=$2::uuid))`,
		taID, f.appID); err != nil {
		t.Fatalf("assign tenant_admin: %v", err)
	}
	devPersonas["authz-test-tenantadmin"] = "test-authz-ta"
	t.Cleanup(func() { delete(devPersonas, "authz-test-tenantadmin") })

	const dev = "rollup-test-approver" // developer + business_admin in tenant A
	const adm = "authz-test-tenantadmin"

	// Tenant A's own dashboard, for the "authorized parent, foreign child"
	// shape below: the dashboard guard passes, so only a widget statement
	// scoped to that dashboard stops the write.
	var ownDashID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.dashboard_def (model_id, name, revision_id) VALUES ($1::uuid, 'Mine', $2::uuid) RETURNING id::text`,
		f.modelID, f.workingRevID).Scan(&ownDashID); err != nil {
		t.Fatalf("seed own dashboard: %v", err)
	}

	cases := []struct {
		name    string
		persona string
		method  string
		path    string
		body    any
	}{
		// ── developer surface ────────────────────────────────────────────
		{"dashboard rename", dev, "PATCH", "/api/developer/dashboards/" + ft.dashboardID, map[string]any{"name": "pwned"}},
		{"dashboard delete", dev, "DELETE", "/api/developer/dashboards/" + ft.dashboardID, nil},
		{"widget add", dev, "POST", "/api/developer/dashboards/" + ft.dashboardID + "/widgets", map[string]any{"widget_type": "text", "content": "pwned"}},
		{"widget update", dev, "PATCH", "/api/developer/dashboards/" + ft.dashboardID + "/widgets/" + ft.widgetID, map[string]any{"content": "pwned"}},
		{"widget delete", dev, "DELETE", "/api/developer/dashboards/" + ft.dashboardID + "/widgets/" + ft.widgetID, nil},
		// Authorized parent, foreign child: the dashboard in the path is the
		// caller's own, so the resource guard passes and only the widget
		// statement's own dashboard scoping can stop it.
		{"foreign widget via own dashboard (update)", dev, "PATCH", "/api/developer/dashboards/" + ownDashID + "/widgets/" + ft.widgetID, map[string]any{"content": "pwned"}},
		{"foreign widget via own dashboard (delete)", dev, "DELETE", "/api/developer/dashboards/" + ownDashID + "/widgets/" + ft.widgetID, nil},
		{"folder rename", dev, "PATCH", "/api/developer/folders/" + ft.folderID, map[string]any{"name": "pwned"}},
		{"folder delete", dev, "DELETE", "/api/developer/folders/" + ft.folderID, nil},
		{"grid rename", dev, "PATCH", "/api/developer/grids/" + ft.gridID, map[string]any{"name": "pwned"}},
		{"grid delete", dev, "DELETE", "/api/developer/grids/" + ft.gridID, nil},
		{"grid attach metric", dev, "POST", "/api/developer/grids/" + ft.gridID + "/metrics/" + ft.metricID, nil},
		{"metric update", dev, "PATCH", "/api/developer/metrics/" + ft.metricID, map[string]any{"name": "pwned"}},
		{"metric delete", dev, "DELETE", "/api/developer/metrics/" + ft.metricID, nil},
		{"dimension update", dev, "PATCH", "/api/developer/dimensions/" + ft.dimensionID, map[string]any{"name": "pwned"}},
		{"dimension delete", dev, "DELETE", "/api/developer/dimensions/" + ft.dimensionID, nil},
		{"member add", dev, "POST", "/api/developer/dimensions/" + ft.dimensionID + "/members", map[string]any{"code": "PWNED", "label": "pwned"}},
		{"member update", dev, "PATCH", "/api/developer/dimensions/" + ft.dimensionID + "/members/" + ft.memberID, map[string]any{"code": "PWNED", "label": "pwned"}},
		{"member delete", dev, "DELETE", "/api/developer/dimensions/" + ft.dimensionID + "/members/" + ft.memberID, nil},
		{"integration update", dev, "PATCH", "/api/developer/integrations/" + ft.integrationID, map[string]any{"name": "pwned", "target_type": "grid", "target_id": ft.gridID}},
		{"integration delete", dev, "DELETE", "/api/developer/integrations/" + ft.integrationID, nil},
		{"form mapping delete", dev, "DELETE", "/api/developer/form-integrations/" + ft.mappingID, nil},
		{"form mapping backfill", dev, "POST", "/api/developer/form-integrations/" + ft.mappingID + "/backfill", nil},
		{"workflow update", dev, "PATCH", "/api/developer/workflows/" + ft.workflowDefID, map[string]any{"name": "pwned"}},
		{"workflow delete", dev, "DELETE", "/api/developer/workflows/" + ft.workflowDefID, nil},
		{"workflow publish", dev, "POST", "/api/developer/workflows/" + ft.workflowDefID + "/publish", nil},
		{"workflow test-run", dev, "POST", "/api/developer/workflows/" + ft.workflowDefID + "/test-run", map[string]any{}},
		{"automation rule update", dev, "PATCH", "/api/automation/rules/" + ft.ruleID, map[string]any{"name": "pwned", "enabled": false}},
		{"automation rule delete", dev, "DELETE", "/api/automation/rules/" + ft.ruleID, nil},
		{"revision activate", dev, "PUT", "/api/developer/revisions/" + ft.revisionID + "/activate", nil},
		{"revision delete", dev, "DELETE", "/api/developer/revisions/" + ft.revisionID, nil},

		// ── resource named in the BODY, not the path ─────────────────────
		// The original sweep behind requireResourceAccess covered IDs taken
		// from the URL. These take the model from the request body and were
		// missed by it: migration/apply generates and APPLIES DDL against the
		// named model, and workflow/submit (role "any", not even "developer")
		// starts a real workflow instance in the named model's application.
		{"schema migration apply", dev, "POST", "/api/developer/migration/apply", map[string]any{"model_id": ft.modelID, "version_number": 1}},
		{"workflow submit", dev, "POST", "/api/workflow/submit", map[string]any{"model_id": ft.modelID, "revision_id": ft.revisionID}},

		// ── admin surface (tenant_admin is NOT platform_admin) ───────────
		{"admin model delete", adm, "DELETE", "/api/admin/models/" + ft.modelID, nil},
		{"admin set active revision", adm, "PUT", "/api/admin/models/" + ft.modelID + "/active-revision", map[string]any{"revision_name": "Working"}},
		{"admin revision create", adm, "POST", "/api/admin/revisions", map[string]any{"model_id": ft.modelID, "name": "pwned"}},
		{"admin revision delete", adm, "DELETE", "/api/admin/revisions/" + ft.revisionID, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doAs(t, f, tc.method, tc.path, tc.persona, f.appID, tc.body)
			if status >= 200 && status < 300 {
				t.Errorf("%s %s as %s returned %d — a foreign tenant's resource must not be reachable\nbody: %s",
					tc.method, tc.path, tc.persona, status, body)
			}
		})
	}

	// The foreign tenant's rows must all still be there: a rejected request
	// must not have half-applied before the guard fired.
	for _, check := range []struct {
		label, query, id string
	}{
		{"dashboard", `SELECT COUNT(*) FROM model.dashboard_def WHERE id=$1::uuid`, ft.dashboardID},
		{"widget content", `SELECT COUNT(*) FROM model.dashboard_widget WHERE id=$1::uuid AND content IS DISTINCT FROM 'pwned'`, ft.widgetID},
		{"widget", `SELECT COUNT(*) FROM model.dashboard_widget WHERE id=$1::uuid`, ft.widgetID},
		{"folder", `SELECT COUNT(*) FROM model.dashboard_folder WHERE id=$1::uuid`, ft.folderID},
		{"grid", `SELECT COUNT(*) FROM model.grid_def WHERE id=$1::uuid`, ft.gridID},
		{"metric", `SELECT COUNT(*) FROM model.metric_def WHERE id=$1::uuid`, ft.metricID},
		{"dimension", `SELECT COUNT(*) FROM model.dimension_def WHERE id=$1::uuid`, ft.dimensionID},
		{"member", `SELECT COUNT(*) FROM model.dimension_member WHERE id=$1::uuid`, ft.memberID},
		{"integration", `SELECT COUNT(*) FROM model.integration_def WHERE id=$1::uuid`, ft.integrationID},
		{"form mapping", `SELECT COUNT(*) FROM model.form_metric_mapping WHERE id=$1::uuid`, ft.mappingID},
		{"workflow def", `SELECT COUNT(*) FROM workflow.workflow_def WHERE id=$1::uuid`, ft.workflowDefID},
		{"automation rule", `SELECT COUNT(*) FROM workflow.automation_rule WHERE id=$1::uuid`, ft.ruleID},
		{"revision", `SELECT COUNT(*) FROM model.revision WHERE id=$1::uuid`, ft.revisionID},
		{"model", `SELECT COUNT(*) FROM core.model WHERE id=$1::uuid`, ft.modelID},
	} {
		var n int
		if err := f.pool.QueryRow(ctx, check.query, check.id).Scan(&n); err != nil {
			t.Fatalf("count foreign %s: %v", check.label, err)
		}
		if n != 1 {
			t.Errorf("foreign %s was destroyed by a rejected request (count=%d)", check.label, n)
		}
	}

	// The foreign model's active revision is untouched — "revision activate"
	// and "admin set active revision" must not have repointed it.
	var activeRev string
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(active_revision_id::text,'') FROM core.model WHERE id=$1::uuid`, ft.modelID).Scan(&activeRev); err != nil {
		t.Fatalf("read foreign active revision: %v", err)
	}
	if activeRev != ft.revisionID {
		t.Errorf("foreign model's active revision changed to %q, want %q", activeRev, ft.revisionID)
	}
}

// TestResourceGuardAllowsOwnTenant is the other half: the guard must not
// over-block. Every route the test above proves is closed to a foreigner must
// stay open to the resource's own developer.
func TestResourceGuardAllowsOwnTenant(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	var dashID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.dashboard_def (model_id, name, revision_id) VALUES ($1::uuid, 'Mine', $2::uuid) RETURNING id::text`,
		f.modelID, f.workingRevID).Scan(&dashID); err != nil {
		t.Fatalf("seed dashboard: %v", err)
	}
	var folderID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.dashboard_folder (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Mine') RETURNING id::text`,
		f.modelID, f.workingRevID).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"own dashboard rename", "PATCH", "/api/developer/dashboards/" + dashID, map[string]any{"name": "Mine renamed"}},
		{"own widget add", "POST", "/api/developer/dashboards/" + dashID + "/widgets", map[string]any{"widget_type": "text", "content": "hello"}},
		{"own folder rename", "PATCH", "/api/developer/folders/" + folderID, map[string]any{"name": "Mine renamed"}},
		{"own grid rename", "PATCH", "/api/developer/grids/" + f.gridStaffID, map[string]any{"name": "Staff Grid"}},
		{"own metric update", "PATCH", "/api/developer/metrics/" + f.amountMetricID, map[string]any{"name": "amount", "agg_rule": "sum", "format": "currency"}},
		{"own dimension update", "PATCH", "/api/developer/dimensions/" + f.deptsDimID, map[string]any{"name": "departments", "agg_rule": "sum"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doAs(t, f, tc.method, tc.path, "rollup-test-approver", f.appID, tc.body)
			if status != http.StatusOK {
				t.Errorf("%s %s on the caller's OWN resource returned %d, want 200 — the guard is over-blocking\nbody: %s",
					tc.method, tc.path, status, body)
			}
		})
	}

	// And the guard 404s a well-formed but nonexistent ID rather than 500ing.
	status, body := doAs(t, f, "PATCH", "/api/developer/grids/"+fmt.Sprintf("%s", "00000000-0000-0000-0000-000000000009"),
		"rollup-test-approver", f.appID, map[string]any{"name": "ghost"})
	if status != http.StatusNotFound {
		t.Errorf("PATCH of a nonexistent grid returned %d, want 404\nbody: %s", status, body)
	}
}

// TestWidgetRefCannotCrossModel covers the chart-isolation finding: ref_id is
// a bare TEXT column with no foreign key, and chart-data resolves the model to
// query FROM the referenced grid. A widget on the caller's own dashboard
// naming another tenant's grid therefore reads that tenant's numbers, and the
// dashboard-level tenancy check can't see it — the dashboard really is theirs.
func TestWidgetRefCannotCrossModel(t *testing.T) {
	f := setupRollupFixture(t)
	ft := seedForeignTenant(t, f)
	ctx := context.Background()

	var ownDashID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.dashboard_def (model_id, name, revision_id) VALUES ($1::uuid, 'Mine', $2::uuid) RETURNING id::text`,
		f.modelID, f.workingRevID).Scan(&ownDashID); err != nil {
		t.Fatalf("seed dashboard: %v", err)
	}

	// ── create: every ref-carrying widget type must reject a foreign target
	for _, tc := range []struct {
		widgetType, refID string
	}{
		{"grid", ft.gridID},
		{"chart", ft.gridID},
		{"import", ft.gridID},
		{"form", ft.formID},
		{"metric_kpi", ft.metricID},
		{"integration_button", ft.integrationID},
		{"automation_button", ft.ruleID},
	} {
		t.Run("create "+tc.widgetType, func(t *testing.T) {
			status, body := doAs(t, f, "POST", "/api/developer/dashboards/"+ownDashID+"/widgets", "rollup-test-approver", f.appID,
				map[string]any{"widget_type": tc.widgetType, "ref_id": tc.refID})
			if status >= 200 && status < 300 {
				t.Errorf("creating a %s widget pointing at a foreign %s returned %d, want a rejection\nbody: %s",
					tc.widgetType, tc.widgetType, status, body)
			}
		})
	}

	// A widget pointing at the caller's OWN grid still works.
	status, body := doAs(t, f, "POST", "/api/developer/dashboards/"+ownDashID+"/widgets", "rollup-test-approver", f.appID,
		map[string]any{"widget_type": "grid", "ref_id": f.gridStaffID})
	if status != http.StatusOK {
		t.Fatalf("creating a widget on the caller's own grid returned %d, want 200\nbody: %s", status, body)
	}
	var ownWidgetID string
	if err := f.pool.QueryRow(ctx,
		`SELECT id::text FROM model.dashboard_widget WHERE dashboard_id=$1::uuid AND widget_type='grid'`, ownDashID).Scan(&ownWidgetID); err != nil {
		t.Fatalf("resolve own widget: %v", err)
	}

	// ── update: can't be repointed at a foreign grid either
	if status, body = doAs(t, f, "PATCH", "/api/developer/dashboards/"+ownDashID+"/widgets/"+ownWidgetID, "rollup-test-approver", f.appID,
		map[string]any{"ref_id": ft.gridID}); status >= 200 && status < 300 {
		t.Errorf("repointing a widget at a foreign grid returned %d, want a rejection\nbody: %s", status, body)
	}
	var refAfter string
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(ref_id,'') FROM model.dashboard_widget WHERE id=$1::uuid`, ownWidgetID).Scan(&refAfter); err != nil {
		t.Fatalf("read widget ref: %v", err)
	}
	if refAfter != f.gridStaffID {
		t.Errorf("widget ref_id is now %q, want it left at %q", refAfter, f.gridStaffID)
	}

	// ── chart-data: a row written directly (bypassing the checks above, as
	// any pre-existing corrupted row would have been) must still not resolve.
	var corruptID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, sort_order, widget_props)
		VALUES ($1::uuid, 'chart', $2, 99, $3::jsonb) RETURNING id::text`,
		ownDashID, ft.gridID,
		`{"chart":{"chart_type":"bar","dimension_id":"`+ft.dimensionID+`","metric_ids":["`+ft.metricID+`"]}}`,
	).Scan(&corruptID); err != nil {
		t.Fatalf("seed corrupted widget: %v", err)
	}
	status, body = doAs(t, f, "POST", "/api/dashboard-widgets/"+corruptID+"/chart-data", "rollup-test-approver", f.appID,
		map[string]any{"context": map[string]string{}})
	if status >= 200 && status < 300 {
		t.Errorf("chart-data on a widget whose stored ref_id names a foreign grid returned %d — stored refs must not be trusted\nbody: %s", status, body)
	}
}

// TestRoleMemberMustBelongToWorkspace covers the business-role reference
// finding's user half (the dashboard half is TestDashboardEndpointsRejectForeignTenant's
// grant check): membership is what grants dashboard visibility, so accepting
// any user UUID let a business admin seat another tenant's user in their own
// workspace's roles.
func TestRoleMemberMustBelongToWorkspace(t *testing.T) {
	f := setupRollupFixture(t)
	ft := seedForeignTenant(t, f)
	ctx := context.Background()

	var foreignUserID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-foreign-user', 'foreign@t.com', 'Foreign User', $1::uuid) RETURNING id::text`,
		ft.custID).Scan(&foreignUserID); err != nil {
		t.Fatalf("seed foreign user: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`,
		foreignUserID, ft.wsID); err != nil {
		t.Fatalf("assign foreign role: %v", err)
	}

	var roleID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.business_role (workspace_id, name) VALUES ((SELECT workspace_id FROM core.application WHERE id=$1::uuid), 'Reviewers') RETURNING id::text`,
		f.appID).Scan(&roleID); err != nil {
		t.Fatalf("seed role: %v", err)
	}

	status, body := doAs(t, f, "POST", "/api/business-admin/roles/"+roleID+"/members", "rollup-test-approver", f.appID,
		map[string]any{"user_id": foreignUserID})
	if status >= 200 && status < 300 {
		t.Errorf("seating a foreign tenant's user in a workspace role returned %d, want a rejection\nbody: %s", status, body)
	}
	var seated int
	if err := f.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM identity.business_role_member WHERE role_id=$1::uuid AND user_id=$2::uuid`, roleID, foreignUserID).Scan(&seated); err != nil {
		t.Fatalf("count members: %v", err)
	}
	if seated != 0 {
		t.Errorf("foreign user was seated anyway (%d rows)", seated)
	}

	// A user of this workspace is still accepted.
	if status, body = doAs(t, f, "POST", "/api/business-admin/roles/"+roleID+"/members", "rollup-test-approver", f.appID,
		map[string]any{"user_id": f.managerID}); status != http.StatusOK {
		t.Errorf("seating the workspace's own user returned %d, want 200\nbody: %s", status, body)
	}
}

// TestAdminAuditIsTenantScoped covers the admin-audit finding: /api/admin/audit
// is gated on "admin", which admits tenant_admin as well as platform_admin,
// and returned the 200 most recent events across every tenant with no filter —
// leaking other customers' event types, actor names, resource IDs and metadata.
func TestAdminAuditIsTenantScoped(t *testing.T) {
	f := setupRollupFixture(t)
	ft := seedForeignTenant(t, f)
	ctx := context.Background()

	// One event in each tenant, distinguishable by resource_id.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO audit.audit_event (category, event_type, application_id, resource_type, resource_id, metadata)
		VALUES ('model_change', 'dashboard.created', $1::uuid, 'dashboard', 'FOREIGN-SECRET', '{"name":"foreign"}'),
		       ('model_change', 'dashboard.created', $2::uuid, 'dashboard', 'OWN-EVENT', '{"name":"mine"}')
	`, ft.appID, f.appID); err != nil {
		t.Fatalf("seed audit events: %v", err)
	}

	var custID string
	if err := f.pool.QueryRow(ctx, `SELECT customer_id::text FROM core.workspace WHERE id=(SELECT workspace_id FROM core.application WHERE id=$1::uuid)`, f.appID).Scan(&custID); err != nil {
		t.Fatalf("resolve customer: %v", err)
	}
	var taID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-audit-ta', 'audit-ta@t.com', 'Audit TA', $1::uuid) RETURNING id::text`,
		custID).Scan(&taID); err != nil {
		t.Fatalf("seed tenant admin: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', (SELECT workspace_id FROM core.application WHERE id=$2::uuid))`,
		taID, f.appID); err != nil {
		t.Fatalf("assign tenant_admin: %v", err)
	}
	devPersonas["audit-test-tenantadmin"] = "test-audit-ta"
	t.Cleanup(func() { delete(devPersonas, "audit-test-tenantadmin") })

	status, body := doAs(t, f, "GET", "/api/admin/audit", "audit-test-tenantadmin", f.appID, nil)
	if status != http.StatusOK {
		t.Fatalf("admin audit as tenant admin: status=%d body=%s", status, body)
	}
	if strings.Contains(body, "FOREIGN-SECRET") {
		t.Errorf("tenant admin sees another customer's audit event:\n%s", body)
	}
	if !strings.Contains(body, "OWN-EVENT") {
		t.Errorf("tenant admin cannot see their OWN audit event — the scope filter is too tight:\n%s", body)
	}
}

// TestAdminRevisionCreateCopiesFullGraph covers the revision-duplication
// finding: POST /api/admin/revisions carried its own copy implementation that
// duplicated metric_def and dimension_def only — no members, grids,
// dashboards, widgets, forms, facts or calc_dependency — discarded every copy
// error (`_, _ = Exec`), ran outside a transaction, and dropped the metrics'
// format columns. Activating such a revision blanks the model. Both admin and
// developer duplication now go through the same transactional
// duplicateRevision.
func TestAdminRevisionCreateCopiesFullGraph(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	// A tenant_admin scoped to this fixture's customer.
	var custID string
	if err := f.pool.QueryRow(ctx, `SELECT customer_id::text FROM core.workspace WHERE id=(SELECT workspace_id FROM core.application WHERE id=$1::uuid)`, f.appID).Scan(&custID); err != nil {
		t.Fatalf("resolve customer: %v", err)
	}
	var taID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-revcopy-ta', 'revcopy@t.com', 'TA', $1::uuid) RETURNING id::text`,
		custID).Scan(&taID); err != nil {
		t.Fatalf("seed tenant admin: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', (SELECT workspace_id FROM core.application WHERE id=$2::uuid))`,
		taID, f.appID); err != nil {
		t.Fatalf("assign tenant_admin: %v", err)
	}
	devPersonas["revcopy-test-tenantadmin"] = "test-revcopy-ta"
	t.Cleanup(func() { delete(devPersonas, "revcopy-test-tenantadmin") })

	// Give the source revision a dashboard too, so the copy has one of every
	// entity class the old implementation silently skipped.
	var dashID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.dashboard_def (model_id, name, revision_id) VALUES ($1::uuid, 'Ops', $2::uuid) RETURNING id::text`,
		f.modelID, f.workingRevID).Scan(&dashID); err != nil {
		t.Fatalf("seed dashboard: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, sort_order) VALUES ($1::uuid, 'grid', $2, 0)`,
		dashID, f.gridStaffID); err != nil {
		t.Fatalf("seed widget: %v", err)
	}
	// dept_total's formula is "=amount"; the fixture doesn't materialise the
	// edge, and copying calc_dependency is one of the things the old admin
	// implementation skipped, so seed it rather than skip the assertion.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid)`,
		f.deptTotalMetricID, f.amountMetricID); err != nil {
		t.Fatalf("seed calc dependency: %v", err)
	}

	status, body := doAs(t, f, "POST", "/api/admin/revisions", "revcopy-test-tenantadmin", f.appID,
		map[string]any{"model_id": f.modelID, "name": "Admin Copy", "description": "made by admin"})
	if status != http.StatusOK {
		t.Fatalf("admin revision create: status=%d body=%s", status, body)
	}
	var newRevID string
	if err := f.pool.QueryRow(ctx,
		`SELECT id::text FROM model.revision WHERE model_id=$1::uuid AND name='Admin Copy'`, f.modelID).Scan(&newRevID); err != nil {
		t.Fatalf("resolve new revision: %v", err)
	}

	// Every entity class the source revision has must exist in the copy.
	countIn := func(label, query string, revID string) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx, query, f.modelID, revID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", label, err)
		}
		return n
	}
	for _, c := range []struct{ label, query string }{
		{"metrics", `SELECT COUNT(*) FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`},
		{"dimensions", `SELECT COUNT(*) FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`},
		{"grids", `SELECT COUNT(*) FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`},
		{"dashboards", `SELECT COUNT(*) FROM model.dashboard_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`},
		{"dimension members", `SELECT COUNT(*) FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid`},
		{"widgets", `SELECT COUNT(*) FROM model.dashboard_widget w JOIN model.dashboard_def dd ON dd.id=w.dashboard_id WHERE dd.model_id=$1::uuid AND dd.revision_id=$2::uuid`},
		{"calc dependencies", `SELECT COUNT(*) FROM model.calc_dependency cd JOIN model.metric_def m ON m.id=cd.metric_id WHERE m.model_id=$1::uuid AND m.revision_id=$2::uuid`},
	} {
		src := countIn(c.label, c.query, f.workingRevID)
		dst := countIn(c.label, c.query, newRevID)
		if src == 0 {
			t.Fatalf("fixture has no %s in the source revision — the assertion would be vacuous", c.label)
		}
		if dst != src {
			t.Errorf("copied %s = %d, want %d (the source revision's count)", c.label, dst, src)
		}
	}

	// Metric formatting survives — the old copy dropped format columns.
	var format, currency string
	if err := f.pool.QueryRow(ctx,
		`SELECT format, format_currency FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='amount'`,
		f.modelID, newRevID).Scan(&format, &currency); err != nil {
		t.Fatalf("read copied metric: %v", err)
	}
	if format != "currency" {
		t.Errorf("copied metric format = %q, want \"currency\" — format columns were dropped by the old copy", format)
	}

	// The description the admin endpoint accepts is applied.
	var desc string
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(description,'') FROM model.revision WHERE id=$1::uuid`, newRevID).Scan(&desc); err != nil {
		t.Fatalf("read description: %v", err)
	}
	if desc != "made by admin" {
		t.Errorf("revision description = %q, want %q", desc, "made by admin")
	}
}
