// Tests for tenant-admin-only model export/import: role enforcement (no
// other role — not even developer or platform-scoped admin paths — may call
// the endpoints) and a full round trip asserting that an imported model is a
// working replica with every cross-entity reference remapped to the new
// model's own rows.
package gateway

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestModelExportImportTenantAdminOnly(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	// A tenant_admin persona (the fixture only creates business/dev users).
	var custID, wsID string
	if err := f.pool.QueryRow(ctx, `
		SELECT c.id::text, w.id::text FROM core.customer c JOIN core.workspace w ON w.customer_id=c.id LIMIT 1
	`).Scan(&custID, &wsID); err != nil {
		t.Fatalf("find customer/workspace: %v", err)
	}
	tenantAdminID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-tenantadmin', 'ta@t.com', 'TA', $1::uuid) RETURNING id::text`, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, tenantAdminID, wsID)
	devPersonas["rollup-test-tenantadmin"] = "test-tenantadmin"
	t.Cleanup(func() { delete(devPersonas, "rollup-test-tenantadmin") })

	// Seed the entity kinds the fixture lacks so the round trip covers the
	// full set (same shape as TestRevisionDuplicationCopiesFullModel).
	exec(`INSERT INTO model.dimension_property (dimension_id, name, data_type) VALUES ($1::uuid, 'owner', 'text')`, f.deptsDimID)
	formID := q(`
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'expense', 'Expense', jsonb_build_array(
			jsonb_build_object('name','dept','label','Dept','type','dimension','dimension_id',$3::text),
			jsonb_build_object('name','amt','label','Amt','type','metric','metric_id',$4::text)
		)) RETURNING id::text`, f.modelID, f.workingRevID, f.deptsDimID, f.amountMetricID)
	exec(`INSERT INTO runtime.form_record (form_id, data, status, created_by) VALUES ($1::uuid, '{"amt": 5}', 'submitted', $2::uuid)`, formID, f.managerID)
	mappingID := q(`
		INSERT INTO model.form_metric_mapping (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id, dimension_mappings)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'post amt', 'amt', $5::uuid, jsonb_build_object($6::text, 'dept'))
		RETURNING id::text`,
		f.modelID, f.workingRevID, formID, f.gridStaffID, f.amountMetricID, f.deptsDimID)
	// A form-posted fact for the SAME cell (STAFF_A1) as the fixture's
	// existing direct fact (value 100, source_ref NULL) — regression
	// coverage for a bug where export collapsed "latest row per cell"
	// across direct and form-posted rows, silently dropping one of them
	// (grid() sums the latest direct row with EVERY form-posted row; they
	// are not interchangeable "whichever is newest" alternatives).
	exec(`
		INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by, source_ref)
		VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, $4::jsonb, 25, $5::uuid, $6::uuid)`,
		f.modelID, f.workingRevID, f.amountMetricID, fmt.Sprintf(`{"%s":"STAFF_A1"}`, f.staffDimID), f.managerID, mappingID)
	exec(`
		INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, target_id)
		VALUES ($1::uuid, $2::uuid, 'staff import', 'csv_import', 'grid', $3::uuid)`,
		f.modelID, f.workingRevID, f.gridStaffID)
	rootFolderID := q(`INSERT INTO model.dashboard_folder (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Root') RETURNING id::text`, f.modelID, f.workingRevID)
	dashID := q(`INSERT INTO model.dashboard_def (model_id, revision_id, name, folder_id) VALUES ($1::uuid, $2::uuid, 'Filed Dash', $3::uuid) RETURNING id::text`, f.modelID, f.workingRevID, rootFolderID)
	exec(`INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, content) VALUES ($1::uuid, 'grid', $2, 'Staff')`, dashID, f.gridStaffID)
	// A chart widget: ref_id points at the grid, but the metric/dimension it
	// actually plots live inside widget_props, which the import used to copy
	// verbatim — guaranteeing a dead chart in every imported model, since an
	// import always allocates new metric/dimension IDs.
	exec(`INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, content, widget_props) VALUES ($1::uuid, 'chart', $2, 'Staff Chart', $3::jsonb)`,
		dashID, f.gridStaffID, fmt.Sprintf(
			`{"chart":{"chart_type":"bar","dimension_id":%q,"metric_ids":[%q],"context_defaults":{%q:"DEPT_A"}}}`,
			f.staffDimID, f.amountMetricID, f.deptsDimID))
	exec(`INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, content, widget_props) VALUES ($1::uuid, 'metric_kpi', $2, 'Amount', $3::jsonb)`,
		dashID, f.amountMetricID, fmt.Sprintf(`{"kpi_scope":{"dimension_id":%q,"member_code":"DEPT_A"}}`, f.deptsDimID))
	exec(`UPDATE workflow.workflow_def SET revision_id=$1::uuid WHERE id=$2::uuid`, f.workingRevID, f.wfDefID)
	exec(`
		INSERT INTO workflow.automation_rule (application_id, revision_id, name, trigger_type, workflow_name, workflow_def_id, source_form_id)
		VALUES ($1::uuid, $2::uuid, 'route expenses', 'form_submit', 'Test Approval', $3::uuid, $4::uuid)`,
		f.appID, f.workingRevID, f.wfDefID, formID)

	exportPath := fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s", f.modelID, f.workingRevID)

	// ── role enforcement ─────────────────────────────────────────────────────
	// approver holds developer + business_admin, manager holds business_user;
	// none of them may export or import.
	for _, persona := range []string{"rollup-test-approver", "rollup-test-manager"} {
		if status, _ := f.do(t, "GET", exportPath, persona, nil); status != http.StatusForbidden {
			t.Errorf("export as %s status = %d, want 403", persona, status)
		}
		if status, _ := f.do(t, "POST", "/api/admin/models/import", persona, map[string]any{"application_id": f.appID}); status != http.StatusForbidden {
			t.Errorf("import as %s status = %d, want 403", persona, status)
		}
	}

	// ── export ───────────────────────────────────────────────────────────────
	status, pkg := f.do(t, "GET", exportPath, "rollup-test-tenantadmin", nil)
	if status != http.StatusOK {
		t.Fatalf("export status = %d, body = %v", status, pkg)
	}
	if pkg["format"] != "mavericks-model-export" {
		t.Fatalf("format = %v, want mavericks-model-export", pkg["format"])
	}
	if pkg["revision_name"] != "Working" {
		t.Errorf("revision_name = %v, want Working", pkg["revision_name"])
	}
	if n := len(pkg["dimensions"].([]any)); n != 3 {
		t.Errorf("exported dimensions = %d, want 3", n)
	}
	if n := len(pkg["grids"].([]any)); n != 5 {
		t.Errorf("exported grids = %d, want 5 (gridStaffID/gridDeptsID/gridRegionsID + the calc-convergence fixture's gridDeptTotalID/gridRegionTotalID)", n)
	}

	// ── import ───────────────────────────────────────────────────────────────
	status, res := f.do(t, "POST", "/api/admin/models/import", "rollup-test-tenantadmin", map[string]any{
		"application_id": f.appID,
		"model_name":     "M Imported",
		"package":        pkg,
	})
	if status != http.StatusOK {
		t.Fatalf("import status = %d, body = %v", status, res)
	}
	newModelID, _ := res["model_id"].(string)
	newRevID, _ := res["revision_id"].(string)
	if newModelID == "" || newRevID == "" || newModelID == f.modelID {
		t.Fatalf("unexpected import result: %v", res)
	}

	// Imported IDs the remapped references must point at.
	var newDeptsDimID, newStaffDimID, newMetricID, newStaffGridID string
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND name='departments'`, newModelID).Scan(&newDeptsDimID)
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND name='staff'`, newModelID).Scan(&newStaffDimID)
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND name='amount'`, newModelID).Scan(&newMetricID)
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND name='Staff Grid'`, newModelID).Scan(&newStaffGridID)
	if newDeptsDimID == "" || newStaffDimID == "" || newMetricID == "" || newStaffGridID == "" {
		t.Fatalf("imported core entities missing (%q/%q/%q/%q)", newDeptsDimID, newStaffDimID, newMetricID, newStaffGridID)
	}

	// Dimension hierarchy + property-derived dimension remapped.
	var staffParentDim, regionsSourceDim string
	_ = f.pool.QueryRow(ctx, `SELECT COALESCE(parent_dimension_id::text,'') FROM model.dimension_def WHERE id=$1::uuid`, newStaffDimID).Scan(&staffParentDim)
	_ = f.pool.QueryRow(ctx, `SELECT COALESCE(source_dimension_id::text,'') FROM model.dimension_def WHERE model_id=$1::uuid AND name='regions'`, newModelID).Scan(&regionsSourceDim)
	if staffParentDim != newDeptsDimID {
		t.Errorf("staff parent dim = %q, want new departments %q", staffParentDim, newDeptsDimID)
	}
	if regionsSourceDim != newStaffDimID {
		t.Errorf("regions source dim = %q, want new staff %q", regionsSourceDim, newStaffDimID)
	}

	// Member parent remap: STAFF_A1 must hang under the NEW DEPT_A member.
	var a1ParentCode string
	_ = f.pool.QueryRow(ctx, `
		SELECT COALESCE(p.code,'')
		FROM model.dimension_member m
		LEFT JOIN model.dimension_member p ON p.id = m.parent_member_id
		WHERE m.dimension_id=$1::uuid AND m.code='STAFF_A1'`, newStaffDimID).Scan(&a1ParentCode)
	if a1ParentCode != "DEPT_A" {
		t.Errorf("imported STAFF_A1 parent = %q, want DEPT_A", a1ParentCode)
	}

	// Typed dimension property came across.
	var propCount int
	_ = f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.dimension_property WHERE dimension_id=$1::uuid AND name='owner'`, newDeptsDimID).Scan(&propCount)
	if propCount != 1 {
		t.Errorf("imported dimension property count = %d, want 1", propCount)
	}

	// Grid rollup ref remapped.
	var rollupSrc string
	_ = f.pool.QueryRow(ctx, `SELECT COALESCE(rollup_source_grid_id::text,'') FROM model.grid_def WHERE model_id=$1::uuid AND name='Dept Rollup'`, newModelID).Scan(&rollupSrc)
	if rollupSrc != newStaffGridID {
		t.Errorf("Dept Rollup source = %q, want new Staff Grid %q", rollupSrc, newStaffGridID)
	}

	// Form field refs remapped; record attributed to the importer; mapping intact.
	var newFormID, fieldDimID string
	if err := f.pool.QueryRow(ctx, `
		SELECT id::text, fields->0->>'dimension_id' FROM model.form_def WHERE model_id=$1::uuid AND name='expense'
	`, newModelID).Scan(&newFormID, &fieldDimID); err != nil {
		t.Fatalf("imported form not found: %v", err)
	}
	if fieldDimID != newDeptsDimID {
		t.Errorf("imported form field dimension_id = %q, want %q", fieldDimID, newDeptsDimID)
	}
	var recCreator string
	_ = f.pool.QueryRow(ctx, `SELECT created_by::text FROM runtime.form_record WHERE form_id=$1::uuid`, newFormID).Scan(&recCreator)
	if recCreator != tenantAdminID {
		t.Errorf("imported record created_by = %q, want importer %q", recCreator, tenantAdminID)
	}
	var mapMetric, mapDimField string
	_ = f.pool.QueryRow(ctx, `
		SELECT target_metric_id::text, COALESCE(dimension_mappings->>$2,'')
		FROM model.form_metric_mapping WHERE form_id=$1::uuid`, newFormID, newDeptsDimID).Scan(&mapMetric, &mapDimField)
	if mapMetric != newMetricID || mapDimField != "dept" {
		t.Errorf("imported mapping metric/dimkey = %q/%q, want %q/dept", mapMetric, mapDimField, newMetricID)
	}

	// Integration target, workflow context binding, automation refs remapped.
	var intTarget string
	_ = f.pool.QueryRow(ctx, `SELECT COALESCE(target_id::text,'') FROM model.integration_def WHERE model_id=$1::uuid AND name='staff import'`, newModelID).Scan(&intTarget)
	if intTarget != newStaffGridID {
		t.Errorf("imported integration target = %q, want %q", intTarget, newStaffGridID)
	}
	var newWfID, schemaDimID string
	if err := f.pool.QueryRow(ctx, `
		SELECT id::text, COALESCE((SELECT e->>'dimension_id' FROM jsonb_array_elements(context_schema) e WHERE e->>'key'='scope'),'')
		FROM workflow.workflow_def WHERE revision_id=$1::uuid AND name='Test Approval'`, newRevID).Scan(&newWfID, &schemaDimID); err != nil {
		t.Fatalf("imported workflow not found: %v", err)
	}
	if schemaDimID != newDeptsDimID {
		t.Errorf("imported workflow scope dim = %q, want %q", schemaDimID, newDeptsDimID)
	}
	var ruleWf, ruleForm string
	_ = f.pool.QueryRow(ctx, `
		SELECT COALESCE(workflow_def_id::text,''), COALESCE(source_form_id::text,'')
		FROM workflow.automation_rule WHERE revision_id=$1::uuid AND name='route expenses'`, newRevID).Scan(&ruleWf, &ruleForm)
	if ruleWf != newWfID || ruleForm != newFormID {
		t.Errorf("imported rule refs = %q/%q, want %q/%q", ruleWf, ruleForm, newWfID, newFormID)
	}

	// Widget ref remapped to the imported grid.
	var widgetRef string
	_ = f.pool.QueryRow(ctx, `
		SELECT COALESCE(w.ref_id,'') FROM model.dashboard_widget w
		JOIN model.dashboard_def d ON d.id = w.dashboard_id
		WHERE d.model_id=$1::uuid AND d.name='Filed Dash' AND w.widget_type='grid'`, newModelID).Scan(&widgetRef)
	if widgetRef != newStaffGridID {
		t.Errorf("imported widget ref = %q, want %q", widgetRef, newStaffGridID)
	}

	// …and so are the metric/dimension IDs INSIDE widget_props, which ref_id
	// remapping never reached.
	widgetProp := func(widgetType, jsonPath string) string {
		t.Helper()
		var got string
		if err := f.pool.QueryRow(ctx, fmt.Sprintf(`
			SELECT COALESCE(%s, '') FROM model.dashboard_widget w
			JOIN model.dashboard_def d ON d.id = w.dashboard_id
			WHERE d.model_id=$1::uuid AND d.name='Filed Dash' AND w.widget_type=$2`, jsonPath),
			newModelID, widgetType).Scan(&got); err != nil {
			t.Fatalf("read imported %s widget prop %s: %v", widgetType, jsonPath, err)
		}
		return got
	}
	if got := widgetProp("chart", `w.widget_props->'chart'->>'dimension_id'`); got != newStaffDimID {
		t.Errorf("imported chart dimension_id = %q, want new staff dim %q (source was %q)", got, newStaffDimID, f.staffDimID)
	}
	if got := widgetProp("chart", `w.widget_props->'chart'->'metric_ids'->>0`); got != newMetricID {
		t.Errorf("imported chart metric_ids[0] = %q, want new amount metric %q (source was %q)", got, newMetricID, f.amountMetricID)
	}
	var ctxDefaultCode string
	_ = f.pool.QueryRow(ctx, `
		SELECT COALESCE(w.widget_props->'chart'->'context_defaults'->>$2, '') FROM model.dashboard_widget w
		JOIN model.dashboard_def d ON d.id = w.dashboard_id
		WHERE d.model_id=$1::uuid AND d.name='Filed Dash' AND w.widget_type='chart'`,
		newModelID, newDeptsDimID).Scan(&ctxDefaultCode)
	if ctxDefaultCode != "DEPT_A" {
		t.Errorf("imported chart context_defaults[%s] = %q, want DEPT_A — the key is a dimension ID and must be remapped too", newDeptsDimID, ctxDefaultCode)
	}
	if got := widgetProp("metric_kpi", `w.widget_props->'kpi_scope'->>'dimension_id'`); got != newDeptsDimID {
		t.Errorf("imported kpi_scope dimension_id = %q, want new departments dim %q (source was %q) — a stale one silently shows the unscoped total", got, newDeptsDimID, f.deptsDimID)
	}

	// The direct fact (source_ref NULL) survives with its dim_members key
	// remapped, alongside — not instead of — the form-posted fact for the
	// SAME cell (source_ref set, remapped to the imported mapping's own new
	// ID): both a direct value and a form-posted value on one cell must
	// both come through, exactly as the source model had them.
	var directCount int
	var directValue float64
	_ = f.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(MAX(value),0) FROM runtime.fact_input
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND dim_members->>$3::text = 'STAFF_A1' AND source_ref IS NULL`,
		newModelID, newRevID, newStaffDimID).Scan(&directCount, &directValue)
	if directCount != 1 || directValue != 100 {
		t.Errorf("imported direct fact count/value = %d/%v, want 1/100", directCount, directValue)
	}

	var newMappingID string
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.form_metric_mapping WHERE model_id=$1::uuid AND name='post amt'`, newModelID).Scan(&newMappingID)
	if newMappingID == "" {
		t.Fatal("imported form_metric_mapping not found")
	}
	var postedCount int
	var postedValue float64
	_ = f.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(MAX(value),0) FROM runtime.fact_input
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND dim_members->>$3::text = 'STAFF_A1' AND source_ref=$4::uuid`,
		newModelID, newRevID, newStaffDimID, newMappingID).Scan(&postedCount, &postedValue)
	if postedCount != 1 || postedValue != 25 {
		t.Errorf("imported form-posted fact count/value = %d/%v, want 1/25 (source_ref=%s)", postedCount, postedValue, newMappingID)
	}
	var activeRev string
	_ = f.pool.QueryRow(ctx, `SELECT COALESCE(active_revision_id::text,'') FROM core.model WHERE id=$1::uuid`, newModelID).Scan(&activeRev)
	if activeRev != newRevID {
		t.Errorf("imported model active revision = %q, want %q", activeRev, newRevID)
	}

	// The source model is untouched: same entity counts as before the import.
	var srcMetricCount int
	_ = f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.metric_def WHERE model_id=$1::uuid`, f.modelID).Scan(&srcMetricCount)
	if srcMetricCount != 8 {
		t.Errorf("source model metric count changed to %d, want 8 (amount + the calc-convergence fixture's dept_total/dept_total_capped/region_total/quota/dept_ratio/budget/dept_ratio_avg)", srcMetricCount)
	}
}

// TestModelExportIncludesRevisionGlobalEntities is a regression test for a
// bug found while exporting the real payroll demo cross-tenant: every
// collectExport query used an exact revision_id=$2 match, silently dropping
// any entity whose revision_id is NULL ("revision-global" — e.g. a workflow
// def created via the older workflow.Store.CreateWorkflowDef, which doesn't
// take a revisionID at all). An exported "self-contained" package was
// missing its workflow entirely whenever the model used that path — as
// the former salary demo's did. setupRollupFixture's workflow def is left
// revision-global by default (this test does NOT stamp it, unlike
// TestModelExportImportTenantAdminOnly above, which does) — exactly
// reproducing the shape that broke.
func TestModelExportIncludesRevisionGlobalEntities(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	var wfRevisionID *string
	if err := f.pool.QueryRow(ctx, `SELECT revision_id::text FROM workflow.workflow_def WHERE id=$1::uuid`, f.wfDefID).Scan(&wfRevisionID); err != nil {
		t.Fatalf("query workflow revision_id: %v", err)
	}
	if wfRevisionID != nil {
		t.Fatalf("fixture precondition failed: workflow def already has revision_id=%v, want NULL (revision-global)", *wfRevisionID)
	}

	tenantAdminID := f.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-ta-global', 'ta-global@t.com', 'TA', (SELECT customer_id FROM core.workspace WHERE id=(SELECT workspace_id FROM core.application WHERE id=$1::uuid))) RETURNING id::text`, f.appID)
	var taID string
	if err := tenantAdminID.Scan(&taID); err != nil {
		t.Fatalf("insert tenant admin: %v", err)
	}
	var wsID string
	if err := f.pool.QueryRow(ctx, `SELECT workspace_id::text FROM core.application WHERE id=$1::uuid`, f.appID).Scan(&wsID); err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, taID, wsID); err != nil {
		t.Fatalf("grant tenant_admin: %v", err)
	}
	devPersonas["rollup-test-ta-global"] = "test-ta-global"
	t.Cleanup(func() { delete(devPersonas, "rollup-test-ta-global") })

	status, pkg := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s", f.modelID, f.workingRevID), "rollup-test-ta-global", nil)
	if status != http.StatusOK {
		t.Fatalf("export status = %d, body = %v", status, pkg)
	}
	workflows, _ := pkg["workflows"].([]any)
	if len(workflows) != 1 {
		t.Fatalf("exported workflows = %v (%d entries), want exactly 1 — a revision-global (NULL revision_id) workflow def must still be captured", workflows, len(workflows))
	}
	if name := workflows[0].(map[string]any)["name"]; name != "Test Approval" {
		t.Errorf("exported workflow name = %v, want Test Approval", name)
	}
}

// TestModelExportImportRejectsCrossTenantAccess is a regression test for a
// second bug found in the same cross-tenant session: neither adminModelExport
// nor adminModelImport checked that the caller's tenant scope actually
// covered the model/application being touched — the route guard only checks
// "holds the tenant_admin role somewhere," not "owns this tenant." Before the
// fix, any tenant_admin could export (read: exfiltrate) any other tenant's
// model, or import (write) a model into any other tenant's application.
func TestModelExportImportRejectsCrossTenantAccess(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	// A second, wholly separate tenant: its own customer/workspace/app, and
	// a tenant_admin who has never been granted anything on f's tenant.
	otherCustID := f.pool.QueryRow(ctx, `INSERT INTO core.customer (name, plan) VALUES ('Other Co', 'standard') RETURNING id::text`)
	var otherCustomerID string
	if err := otherCustID.Scan(&otherCustomerID); err != nil {
		t.Fatalf("insert other customer: %v", err)
	}
	var otherWsID, otherAppID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Other WS') RETURNING id::text`, otherCustomerID).Scan(&otherWsID); err != nil {
		t.Fatalf("insert other workspace: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Other App', 'planning') RETURNING id::text`, otherWsID, otherCustomerID).Scan(&otherAppID); err != nil {
		t.Fatalf("insert other application: %v", err)
	}
	var otherTAID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-ta-other', 'ta-other@t.com', 'Other TA', $1::uuid) RETURNING id::text`, otherCustomerID).Scan(&otherTAID); err != nil {
		t.Fatalf("insert other tenant admin: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, otherTAID, otherWsID); err != nil {
		t.Fatalf("grant other tenant_admin: %v", err)
	}
	devPersonas["rollup-test-ta-other"] = "test-ta-other"

	// f's own tenant_admin (same pattern as TestModelExportImportTenantAdminOnly).
	var custID string
	if err := f.pool.QueryRow(ctx, `SELECT customer_id::text FROM core.application WHERE id=$1::uuid`, f.appID).Scan(&custID); err != nil {
		t.Fatalf("resolve f's customer: %v", err)
	}
	var wsID string
	if err := f.pool.QueryRow(ctx, `SELECT workspace_id::text FROM core.application WHERE id=$1::uuid`, f.appID).Scan(&wsID); err != nil {
		t.Fatalf("resolve f's workspace: %v", err)
	}
	var taID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-ta-own', 'ta-own@t.com', 'Own TA', $1::uuid) RETURNING id::text`, custID).Scan(&taID); err != nil {
		t.Fatalf("insert own tenant admin: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, taID, wsID); err != nil {
		t.Fatalf("grant own tenant_admin: %v", err)
	}
	devPersonas["rollup-test-ta-own"] = "test-ta-own"
	t.Cleanup(func() {
		delete(devPersonas, "rollup-test-ta-other")
		delete(devPersonas, "rollup-test-ta-own")
	})

	exportPath := fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s", f.modelID, f.workingRevID)

	// The OTHER tenant's admin must not be able to export f's model.
	if status, body := f.do(t, "GET", exportPath, "rollup-test-ta-other", nil); status != http.StatusForbidden {
		t.Errorf("other tenant's admin exporting f's model status = %d, want 403, body = %v", status, body)
	}

	// f's own admin exporting f's own model must still work (sanity, and
	// gives us a real package for the next check).
	status, pkg := f.do(t, "GET", exportPath, "rollup-test-ta-own", nil)
	if status != http.StatusOK {
		t.Fatalf("f's own admin exporting f's own model status = %d, body = %v", status, pkg)
	}

	// f's own admin must not be able to import INTO the other tenant's app.
	if status, body := f.do(t, "POST", "/api/admin/models/import", "rollup-test-ta-own",
		map[string]any{"application_id": otherAppID, "package": pkg},
	); status != http.StatusForbidden {
		t.Errorf("f's admin importing into another tenant's app status = %d, want 403, body = %v", status, body)
	}

	// The other tenant's admin CAN import into their own app (sanity — the
	// scope check isn't accidentally blocking legitimate same-tenant use).
	if status, body := f.do(t, "POST", "/api/admin/models/import", "rollup-test-ta-other",
		map[string]any{"application_id": otherAppID, "package": pkg},
	); status != http.StatusOK {
		t.Errorf("other tenant's admin importing into their OWN app status = %d, want 200, body = %v", status, body)
	}
}
