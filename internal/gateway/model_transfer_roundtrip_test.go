package gateway

// Regression tests for four defects found live on 2026-09-10 while
// exporting a specific (non-active) revision of the Regional Expense
// Planning demo as one tenant's tenant_admin and importing it into another
// tenant's application as that tenant's tenant_admin:
//
//  1. duplicateRevision copied form-posted facts with source_ref still
//     pointing at the SOURCE revision's form mapping (Step B copies it
//     verbatim, Step E creates a new mapping, nothing reconciled the two).
//  2. Because of (1), CollectExport tagged those facts with a mapping ID
//     that wasn't in the package and Import silently dropped every one of
//     them — the imported model's travel_cost total was 250 short.
//  3. Neither duplicateRevision nor Import remapped a workflow's
//     subject_config ({"form_id"} / {"grid_id","metric_id"}): the copy kept
//     pointing at the source revision's form, the import at the source
//     TENANT's form.
//  4. Neither path wrote or triggered calc_result rows, so every calc metric
//     in a duplicated or imported revision rendered blank in every grid and
//     dashboard until a user happened to edit a cell.
//  5. The package carried no automation-rule or integration IDs, so
//     automation_button / integration_button dashboard widgets kept the
//     SOURCE tenant's ref_id after import (duplication already remapped
//     them; import did not).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// roundTripFixture extends setupRollupFixture with a form, a form→metric
// mapping, one form-posted fact, one direct fact, a revision-scoped workflow
// bound to the form, and a tenant_admin persona for the fixture's tenant.
type roundTripFixture struct {
	*rollupFixture
	formID, mappingID, wfID string
	ruleID, integrationID   string
	taPersona, taUserID     string
}

func setupRoundTripFixture(t *testing.T) *roundTripFixture {
	t.Helper()
	f := &roundTripFixture{rollupFixture: setupRollupFixture(t)}
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

	// setupRollupFixture declares its calc metrics without model.calc_dependency
	// edges (its tests read synthetic calc_result rows instead of running the
	// scheduler). The scheduler binds formula variables from those edges, so
	// the tests below — which need a REAL calculation on the copied/imported
	// revision — declare them here, by name, exactly as the developer API
	// would have.
	exec(`
		INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id)
		SELECT m.id, d.id
		FROM (VALUES ('dept_total','amount'), ('region_total','amount'), ('dept_total_capped','dept_total'),
		             ('dept_ratio','dept_total'), ('dept_ratio','quota'), ('dept_ratio_avg','dept_total'), ('dept_ratio_avg','budget')) AS e(m, d)
		JOIN model.metric_def m ON m.model_id=$1::uuid AND m.revision_id=$2::uuid AND m.name=e.m
		JOIN model.metric_def d ON d.model_id=$1::uuid AND d.revision_id=$2::uuid AND d.name=e.d
		ON CONFLICT DO NOTHING`, f.modelID, f.workingRevID)

	f.formID = q(`
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'expense', 'Expense', jsonb_build_array(
			jsonb_build_object('name','staff','label','Staff','type','dimension','dimension_id',$3::text),
			jsonb_build_object('name','amt','label','Amt','type','metric','metric_id',$4::text)
		)) RETURNING id::text`, f.modelID, f.workingRevID, f.staffDimID, f.amountMetricID)
	f.mappingID = q(`
		INSERT INTO model.form_metric_mapping (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id, dimension_mappings)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'post amt', 'amt', $5::uuid, jsonb_build_object($6::text, 'staff'))
		RETURNING id::text`, f.modelID, f.workingRevID, f.formID, f.gridStaffID, f.amountMetricID, f.staffDimID)
	// One direct fact and one form-posted fact on the SAME cell: the grid
	// sums them (FactsPolicy), so the cell/total reads 100 + 7 = 107 only
	// while the form-posted row survives every copy.
	exec(`INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by)
	      VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, $4::jsonb, 100, $5::uuid)`,
		f.modelID, f.workingRevID, f.amountMetricID, fmt.Sprintf(`{"%s":"STAFF_A1"}`, f.staffDimID), f.managerID)
	exec(`INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by, source_ref)
	      VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, $4::jsonb, 7, $5::uuid, $6::uuid)`,
		f.modelID, f.workingRevID, f.amountMetricID, fmt.Sprintf(`{"%s":"STAFF_A1"}`, f.staffDimID), f.managerID, f.mappingID)
	f.wfID = q(`
		INSERT INTO workflow.workflow_def (application_id, revision_id, name, description, trigger_event, subject_type, subject_config, steps, context_schema, status, created_by, updated_by)
		VALUES ($1::uuid, $2::uuid, 'Expense Approval', '', 'form.submitted', 'form', jsonb_build_object('form_id', $3::text), '[]'::jsonb, '[]'::jsonb, 'published', $4::uuid, $4::uuid)
		RETURNING id::text`, f.appID, f.workingRevID, f.formID, f.managerID)

	// An automation rule and an integration, each referenced by a dashboard
	// button widget (ref_id): the package carried neither ID, so imported
	// buttons kept pointing at the SOURCE tenant's rule/integration.
	f.ruleID = q(`
		INSERT INTO workflow.automation_rule (application_id, revision_id, name, description, trigger_type, workflow_name, enabled, workflow_def_id, source_form_id)
		VALUES ($1::uuid, $2::uuid, 'Auto-start approval', '', 'manual', 'Expense Approval', true, $3::uuid, $4::uuid)
		RETURNING id::text`, f.appID, f.workingRevID, f.wfID, f.formID)
	f.integrationID = q(`
		INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, target_id, config)
		VALUES ($1::uuid, $2::uuid, 'Staff CSV', 'csv', 'grid', $3::uuid, '{}'::jsonb)
		RETURNING id::text`, f.modelID, f.workingRevID, f.gridStaffID)
	dashID := q(`INSERT INTO model.dashboard_def (model_id, revision_id, name, tags, category) VALUES ($1::uuid, $2::uuid, 'Buttons', '{}', '') RETURNING id::text`, f.modelID, f.workingRevID)
	exec(`INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, content, sort_order, col_start, col_span) VALUES ($1::uuid, 'automation_button', $2, 'Start', 0, 1, 6)`, dashID, f.ruleID)
	exec(`INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, content, sort_order, col_start, col_span) VALUES ($1::uuid, 'integration_button', $2, 'Import', 1, 7, 6)`, dashID, f.integrationID)

	var custID, wsID string
	if err := f.pool.QueryRow(ctx, `SELECT c.id::text, w.id::text FROM core.customer c JOIN core.workspace w ON w.customer_id=c.id LIMIT 1`).Scan(&custID, &wsID); err != nil {
		t.Fatalf("find customer/workspace: %v", err)
	}
	f.taUserID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-roundtrip-ta', 'rt-ta@t.com', 'TA', $1::uuid) RETURNING id::text`, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, f.taUserID, wsID)
	f.taPersona = "rollup-test-roundtrip-ta"
	devPersonas[f.taPersona] = "test-roundtrip-ta"
	t.Cleanup(func() { delete(devPersonas, f.taPersona) })
	return f
}

// scalar runs a single-value query and fails the test on error.
func (f *roundTripFixture) scalar(t *testing.T, dst any, sql string, args ...any) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
}

// gridTotalsByName reads the grid endpoint for one grid def and returns its
// totals keyed by metric NAME (IDs differ between revisions and models).
func (f *roundTripFixture) gridTotalsByName(t *testing.T, gridID, revisionID string) map[string]float64 {
	t.Helper()
	status, body := f.do(t, "GET", fmt.Sprintf("/api/grid?grid_def_id=%s&revision_id=%s", gridID, revisionID), "rollup-test-approver", nil)
	if status != http.StatusOK {
		t.Fatalf("grid %s@%s status = %d, body = %v", gridID, revisionID, status, body)
	}
	names := map[string]string{}
	metrics, _ := body["metrics"].([]any)
	for _, m := range metrics {
		mm, _ := m.(map[string]any)
		id, _ := mm["id"].(string)
		name, _ := mm["name"].(string)
		names[id] = name
	}
	out := map[string]float64{}
	totals, _ := body["totals"].(map[string]any)
	for id, v := range totals {
		if name, ok := names[id]; ok {
			out[name], _ = v.(float64)
		}
	}
	return out
}

func TestDuplicateRevisionRemapsFactSourceRefSubjectConfigAndRecalcs(t *testing.T) {
	f := setupRoundTripFixture(t)

	status, body := f.do(t, "POST", "/api/developer/revisions", "rollup-test-approver",
		map[string]any{"name": "Copy", "source_revision_id": f.workingRevID})
	if status != http.StatusOK {
		t.Fatalf("duplicate status = %d, body = %v", status, body)
	}
	copyRev, _ := body["id"].(string)

	// (1) the copied form-posted fact references the COPY's mapping.
	var newMappingID, newFormID string
	f.scalar(t, &newMappingID, `SELECT id::text FROM model.form_metric_mapping WHERE model_id=$1::uuid AND revision_id=$2::uuid`, f.modelID, copyRev)
	f.scalar(t, &newFormID, `SELECT id::text FROM model.form_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`, f.modelID, copyRev)
	var factRef string
	f.scalar(t, &factRef, `SELECT source_ref::text FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id=$2::uuid AND source_ref IS NOT NULL`, f.modelID, copyRev)
	if factRef != newMappingID {
		t.Errorf("copied form-posted fact source_ref = %s, want the copy's mapping %s (old mapping %s)", factRef, newMappingID, f.mappingID)
	}

	// (3) the copied workflow binds to the COPY's form.
	var subjectFormID string
	f.scalar(t, &subjectFormID, `SELECT subject_config->>'form_id' FROM workflow.workflow_def WHERE application_id=$1::uuid AND revision_id=$2::uuid`, f.appID, copyRev)
	if subjectFormID != newFormID {
		t.Errorf("copied workflow subject_config.form_id = %s, want the copy's form %s (old form %s)", subjectFormID, newFormID, f.formID)
	}

	// (4) calc results exist for the copy without any cell edit, and the
	// copy's grid reads the same numbers as the source once the source is
	// (explicitly) calculated too.
	var calcRows int
	f.scalar(t, &calcRows, `SELECT count(*) FROM runtime.calc_result WHERE model_id=$1::uuid AND revision_id=$2::uuid`, f.modelID, copyRev)
	if calcRows == 0 {
		t.Errorf("duplicated revision has no calc_result rows — calc metrics render blank until a cell is edited")
	}
	sched := calculation.NewScheduler(logger.New("test"), calculation.NewStore(f.pool), nil)
	if err := sched.RecalcAffected(context.Background(), f.modelID, f.workingRevID, []string{f.amountMetricID}); err != nil {
		t.Fatalf("recalc source: %v", err)
	}
	var copyGrid string
	f.scalar(t, &copyGrid, `SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=(SELECT name FROM model.grid_def WHERE id=$3::uuid)`, f.modelID, copyRev, f.gridDeptTotalID)
	src := f.gridTotalsByName(t, f.gridDeptTotalID, f.workingRevID)
	cpy := f.gridTotalsByName(t, copyGrid, copyRev)
	if src["dept_total"] == 0 {
		t.Fatalf("fixture precondition: source dept_total total is 0 (%v)", src)
	}
	if src["dept_total"] != cpy["dept_total"] {
		t.Errorf("dept_total total: source=%v copy=%v (copy must carry the form-posted 7 and a calc result)", src["dept_total"], cpy["dept_total"])
	}
}

func TestModelExportResolvesStaleFactSourceRefToSameRevisionMapping(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()

	// Reproduce pre-fix data: a second revision whose form-posted fact still
	// references the Working revision's mapping (what duplicateRevision used
	// to leave behind, and what any revision copied before the fix still has).
	status, body := f.do(t, "POST", "/api/developer/revisions", "rollup-test-approver",
		map[string]any{"name": "Stale", "source_revision_id": f.workingRevID})
	if status != http.StatusOK {
		t.Fatalf("duplicate status = %d, body = %v", status, body)
	}
	staleRev, _ := body["id"].(string)
	var staleMappingID string
	f.scalar(t, &staleMappingID, `SELECT id::text FROM model.form_metric_mapping WHERE model_id=$1::uuid AND revision_id=$2::uuid`, f.modelID, staleRev)
	if _, err := f.pool.Exec(ctx, `UPDATE runtime.fact_input SET source_ref=$3::uuid WHERE model_id=$1::uuid AND revision_id=$2::uuid AND source_ref IS NOT NULL`,
		f.modelID, staleRev, f.mappingID); err != nil {
		t.Fatalf("stale source_ref: %v", err)
	}

	status, pkg := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s", f.modelID, staleRev), f.taPersona, nil)
	if status != http.StatusOK {
		t.Fatalf("export status = %d, body = %v", status, pkg)
	}
	facts, _ := pkg["facts"].([]any)
	var posted []string
	for _, x := range facts {
		fm, _ := x.(map[string]any)
		if ref, ok := fm["source_mapping_id"].(string); ok {
			posted = append(posted, ref)
		}
	}
	if len(posted) != 1 || posted[0] != staleMappingID {
		t.Errorf("exported form-posted fact source_mapping_id = %v, want exactly [%s] (this revision's mapping, not the Working one %s)", posted, staleMappingID, f.mappingID)
	}
}

func TestModelImportKeepsFormPostedFactsRemapsSubjectConfigAndRecalcs(t *testing.T) {
	f := setupRoundTripFixture(t)

	status, pkg := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s", f.modelID, f.workingRevID), f.taPersona, nil)
	if status != http.StatusOK {
		t.Fatalf("export status = %d, body = %v", status, pkg)
	}
	status, res := f.do(t, "POST", "/api/admin/models/import", f.taPersona,
		map[string]any{"application_id": f.appID, "model_name": "Imported", "package": pkg})
	if status != http.StatusOK {
		t.Fatalf("import status = %d, body = %v", status, res)
	}
	newModel, _ := res["model_id"].(string)
	newRev, _ := res["revision_id"].(string)

	// (2) the form-posted fact survived and references the imported mapping.
	var newMappingID, newFormID string
	f.scalar(t, &newMappingID, `SELECT id::text FROM model.form_metric_mapping WHERE model_id=$1::uuid`, newModel)
	f.scalar(t, &newFormID, `SELECT id::text FROM model.form_def WHERE model_id=$1::uuid`, newModel)
	var posted int
	f.scalar(t, &posted, `SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND source_ref=$2::uuid`, newModel, newMappingID)
	if posted != 1 {
		t.Errorf("imported form-posted facts referencing the imported mapping = %d, want 1", posted)
	}

	// (3) the imported workflow binds to the imported form, not the source's.
	var subjectFormID string
	f.scalar(t, &subjectFormID, `SELECT subject_config->>'form_id' FROM workflow.workflow_def WHERE application_id=$1::uuid AND revision_id=$2::uuid AND name='Expense Approval'`, f.appID, newRev)
	if subjectFormID != newFormID {
		t.Errorf("imported workflow subject_config.form_id = %s, want imported form %s (source form %s)", subjectFormID, newFormID, f.formID)
	}

	// (5) button widgets reference the IMPORTED rule/integration, not the
	// source's (the package carried no rule/integration IDs to remap by).
	var newRuleID, newIntegrationID, btnRuleRef, btnIntegrationRef string
	f.scalar(t, &newRuleID, `SELECT id::text FROM workflow.automation_rule WHERE application_id=$1::uuid AND revision_id=$2::uuid`, f.appID, newRev)
	f.scalar(t, &newIntegrationID, `SELECT id::text FROM model.integration_def WHERE model_id=$1::uuid`, newModel)
	f.scalar(t, &btnRuleRef, `SELECT w.ref_id FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id WHERE d.model_id=$1::uuid AND w.widget_type='automation_button'`, newModel)
	f.scalar(t, &btnIntegrationRef, `SELECT w.ref_id FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id WHERE d.model_id=$1::uuid AND w.widget_type='integration_button'`, newModel)
	if btnRuleRef != newRuleID {
		t.Errorf("imported automation_button ref_id = %s, want imported rule %s (source rule %s)", btnRuleRef, newRuleID, f.ruleID)
	}
	if btnIntegrationRef != newIntegrationID {
		t.Errorf("imported integration_button ref_id = %s, want imported integration %s (source integration %s)", btnIntegrationRef, newIntegrationID, f.integrationID)
	}

	// (4) calc results exist and the imported grid reads the same totals as
	// the source (source calculated explicitly, like the duplication test).
	var calcRows int
	f.scalar(t, &calcRows, `SELECT count(*) FROM runtime.calc_result WHERE model_id=$1::uuid AND revision_id=$2::uuid`, newModel, newRev)
	if calcRows == 0 {
		t.Errorf("imported revision has no calc_result rows — calc metrics render blank until a cell is edited")
	}
	sched := calculation.NewScheduler(logger.New("test"), calculation.NewStore(f.pool), nil)
	if err := sched.RecalcAffected(context.Background(), f.modelID, f.workingRevID, []string{f.amountMetricID}); err != nil {
		t.Fatalf("recalc source: %v", err)
	}
	var importedGrid string
	f.scalar(t, &importedGrid, `SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND name=(SELECT name FROM model.grid_def WHERE id=$2::uuid)`, newModel, f.gridDeptTotalID)
	src := f.gridTotalsByName(t, f.gridDeptTotalID, f.workingRevID)
	imp := f.gridTotalsByName(t, importedGrid, newRev)
	for _, name := range []string{"dept_total"} {
		if src[name] == 0 || src[name] != imp[name] {
			t.Errorf("%s total: source=%v imported=%v (want equal and non-zero)", name, src[name], imp[name])
		}
	}
	srcJSON, _ := json.Marshal(src)
	impJSON, _ := json.Marshal(imp)
	t.Logf("source totals %s / imported totals %s", srcJSON, impJSON)
}

// TestModelExportDefinitionsOnly covers ?include_data=false: the package
// carries the model's structure (dimensions, metrics, grids, forms, mappings,
// workflows) but no fact values and no form records, says so in its
// envelope, and imports into an empty-but-complete model.
func TestModelExportDefinitionsOnly(t *testing.T) {
	f := setupRoundTripFixture(t)

	status, body := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s&include_data=maybe", f.modelID, f.workingRevID), f.taPersona, nil)
	if status != http.StatusBadRequest {
		t.Errorf("include_data=maybe status = %d, want 400 (body %v)", status, body)
	}

	status, full := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s", f.modelID, f.workingRevID), f.taPersona, nil)
	if status != http.StatusOK {
		t.Fatalf("full export status = %d, body = %v", status, full)
	}
	status, defs := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s&include_data=false", f.modelID, f.workingRevID), f.taPersona, nil)
	if status != http.StatusOK {
		t.Fatalf("definitions-only export status = %d, body = %v", status, defs)
	}
	count := func(p map[string]any, key string) int { arr, _ := p[key].([]any); return len(arr) }
	if full["include_data"] != true || defs["include_data"] != false {
		t.Errorf("include_data envelope: full=%v defs=%v, want true/false", full["include_data"], defs["include_data"])
	}
	if n := count(full, "facts"); n == 0 {
		t.Fatalf("fixture precondition: full export has no facts")
	}
	if n := count(defs, "facts"); n != 0 {
		t.Errorf("definitions-only export facts = %d, want 0", n)
	}
	if n := count(defs, "form_records"); n != 0 {
		t.Errorf("definitions-only export form_records = %d, want 0", n)
	}
	for _, key := range []string{"dimensions", "metrics", "grids", "forms", "form_mappings", "workflows", "automation_rules", "dashboards"} {
		if count(defs, key) != count(full, key) {
			t.Errorf("definitions-only export %s = %d, want %d (same as with data)", key, count(defs, key), count(full, key))
		}
	}

	status, res := f.do(t, "POST", "/api/admin/models/import", f.taPersona,
		map[string]any{"application_id": f.appID, "model_name": "Definitions only", "package": defs})
	if status != http.StatusOK {
		t.Fatalf("import status = %d, body = %v", status, res)
	}
	newModel, _ := res["model_id"].(string)
	var facts, metrics int
	f.scalar(t, &facts, `SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, newModel)
	f.scalar(t, &metrics, `SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid`, newModel)
	if facts != 0 || metrics == 0 {
		t.Errorf("imported definitions-only model: facts=%d (want 0) metrics=%d (want >0)", facts, metrics)
	}
}
