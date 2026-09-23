package gateway

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// Tests for real revision-duplication data-integrity gaps found by the
// dashboards/dimensions/metrics/forms/workflows sync audit, fixed as part
// of the "eliminate real gaps" pass. Reuses setupRollupFixture from
// generic_rollup_workflow_test.go (same package) — a real model with
// dimensions, a staff/rollup grid pair, and direct-entry facts already
// seeded.

// TestDuplicateRevisionCopiesFactMetadataAndRemapsWidgetRefs is a B1/B2
// regression test: Step B (fact copy) used to drop source_ref/entered_at,
// misclassifying every copied form-posted fact as direct-entry (which then
// collapses a summed multi-record cell down to one arbitrary value via the
// grid read's "latest direct entry wins" ordering); dashboard_widget.ref_id
// was copied verbatim, leaving chart/metric_kpi/form/integration_button/
// automation_button/import widgets pointing at the SOURCE revision's rows
// after duplication.
func TestDuplicateRevisionCopiesFactMetadataAndRemapsWidgetRefs(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	h := &handler{db: tenantdb.NewHandle(f.pool, nil), log: logger.New("test")}

	sourceRefID := "22222222-2222-2222-2222-222222222222"
	var enteredAt time.Time
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_name, metric_id, dim_members, value, entered_by, revision_id, source_ref, entered_at)
		VALUES ($1::uuid, 'Working', $2::uuid, jsonb_build_object($3::text, 'STAFF_A1'), 999, $4::uuid, $5::uuid, $6::uuid, now() - interval '3 days')
		RETURNING entered_at
	`, f.modelID, f.amountMetricID, f.staffDimID, f.approverID, f.workingRevID, sourceRefID).Scan(&enteredAt); err != nil {
		t.Fatalf("seed form-posted fact: %v", err)
	}

	var formID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.form_def (model_id, name, label, fields, revision_id) VALUES ($1::uuid, 'Expense Form', 'Expense Form', '[]'::jsonb, $2::uuid) RETURNING id::text`,
		f.modelID, f.workingRevID).Scan(&formID); err != nil {
		t.Fatalf("seed form: %v", err)
	}
	var intID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.integration_def (model_id, name, type, target_type, target_id, config, revision_id) VALUES ($1::uuid, 'CSV Import', 'csv_import', 'grid', $2::uuid, '{}'::jsonb, $3::uuid) RETURNING id::text`,
		f.modelID, f.gridStaffID, f.workingRevID).Scan(&intID); err != nil {
		t.Fatalf("seed integration: %v", err)
	}
	var wfDefID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, status, published_at, revision_id)
		VALUES ($1::uuid, 'Approve Expense', 'manual', '[]'::jsonb, 'published', now(), $2::uuid) RETURNING id::text
	`, f.appID, f.workingRevID).Scan(&wfDefID); err != nil {
		t.Fatalf("seed workflow def: %v", err)
	}
	var ruleID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, workflow_def_id, enabled, revision_id)
		VALUES ($1::uuid, 'Start Approval', 'manual', 'Approve Expense', $2::uuid, true, $3::uuid) RETURNING id::text
	`, f.appID, wfDefID, f.workingRevID).Scan(&ruleID); err != nil {
		t.Fatalf("seed automation rule: %v", err)
	}
	var dashID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.dashboard_def (model_id, name, revision_id) VALUES ($1::uuid, 'Amount Dashboard', $2::uuid) RETURNING id::text`,
		f.modelID, f.workingRevID).Scan(&dashID); err != nil {
		t.Fatalf("seed dashboard: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, sort_order) VALUES
		  ($1::uuid, 'chart', $2, 0),
		  ($1::uuid, 'metric_kpi', $3, 1),
		  ($1::uuid, 'form', $4, 2),
		  ($1::uuid, 'integration_button', $5, 3),
		  ($1::uuid, 'automation_button', $6, 4),
		  ($1::uuid, 'import', $2, 5)
	`, dashID, f.gridStaffID, f.amountMetricID, formID, intID, ruleID); err != nil {
		t.Fatalf("seed widgets: %v", err)
	}

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	newRevID, err := h.duplicateRevision(ctx, tx, f.modelID, "Copy", f.workingRevID, &f.workingRevID)
	if err != nil {
		t.Fatalf("duplicateRevision: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// B1: source_ref/entered_at must survive the copy verbatim.
	// value=999 (not one of the fixture's own STAFF_A1/A2/B1 direct-entry
	// amounts) uniquely identifies this row within the new revision — no
	// need to resolve the copied metric's new ID first.
	var copiedSourceRef *string
	var copiedEnteredAt time.Time
	if err := f.pool.QueryRow(ctx, `
		SELECT source_ref::text, entered_at FROM runtime.fact_input
		WHERE revision_id=$1::uuid AND value=999
	`, newRevID).Scan(&copiedSourceRef, &copiedEnteredAt); err != nil {
		t.Fatalf("query copied fact_input: %v", err)
	}
	if copiedSourceRef == nil || *copiedSourceRef != sourceRefID {
		t.Errorf("copied fact_input.source_ref = %v, want %s", copiedSourceRef, sourceRefID)
	}
	if !copiedEnteredAt.Equal(enteredAt) {
		t.Errorf("copied fact_input.entered_at = %v, want %v", copiedEnteredAt, enteredAt)
	}

	// B2: every ref_id must resolve to the NEW revision's copy of its
	// target, not the source revision's original row.
	var newGridID, newMetricID, newFormID, newIntID, newRuleID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Staff Grid'`, f.modelID, newRevID).Scan(&newGridID); err != nil {
		t.Fatalf("query copied grid: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='amount'`, f.modelID, newRevID).Scan(&newMetricID); err != nil {
		t.Fatalf("query copied metric: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM model.form_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Expense Form'`, f.modelID, newRevID).Scan(&newFormID); err != nil {
		t.Fatalf("query copied form: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM model.integration_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='CSV Import'`, f.modelID, newRevID).Scan(&newIntID); err != nil {
		t.Fatalf("query copied integration: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM workflow.automation_rule WHERE application_id=$1::uuid AND revision_id=$2::uuid AND name='Start Approval'`, f.appID, newRevID).Scan(&newRuleID); err != nil {
		t.Fatalf("query copied automation rule: %v", err)
	}

	var newDashID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM model.dashboard_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Amount Dashboard'`, f.modelID, newRevID).Scan(&newDashID); err != nil {
		t.Fatalf("query copied dashboard: %v", err)
	}

	type widgetRef struct {
		widgetType string
		refID      string
	}
	rows, err := f.pool.Query(ctx, `SELECT widget_type, ref_id FROM model.dashboard_widget WHERE dashboard_id=$1::uuid ORDER BY sort_order`, newDashID)
	if err != nil {
		t.Fatalf("query copied widgets: %v", err)
	}
	var got []widgetRef
	for rows.Next() {
		var w widgetRef
		if err := rows.Scan(&w.widgetType, &w.refID); err != nil {
			rows.Close()
			t.Fatalf("scan widget: %v", err)
		}
		got = append(got, w)
	}
	rows.Close()

	want := []widgetRef{
		{"chart", newGridID},
		{"metric_kpi", newMetricID},
		{"form", newFormID},
		{"integration_button", newIntID},
		{"automation_button", newRuleID},
		{"import", newGridID},
	}
	if len(got) != len(want) {
		t.Fatalf("copied widget count = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].widgetType != w.widgetType {
			t.Fatalf("widget[%d].widget_type = %q, want %q", i, got[i].widgetType, w.widgetType)
		}
		if got[i].refID != w.refID {
			t.Errorf("widget[%d] (%s) ref_id = %s, want the copied-revision entity %s (not the source revision's)",
				i, w.widgetType, got[i].refID, w.refID)
		}
	}
}

// TestAutomationRuleSourceFormGridForeignKeysSetNull is a B3 regression
// test for migration 064: source_form_id/source_grid_id used to be plain
// UUID columns with no FK at all, so deleting a form or grid silently and
// permanently disabled any automation rule scoped to it. Now both use
// ON DELETE SET NULL, matching sibling workflow_def_id/rollup_source_grid_id.
func TestAutomationRuleSourceFormGridForeignKeysSetNull(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	var formID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.form_def (model_id, name, label, fields, revision_id) VALUES ($1::uuid, 'Temp Form', 'Temp Form', '[]'::jsonb, $2::uuid) RETURNING id::text`,
		f.modelID, f.workingRevID).Scan(&formID); err != nil {
		t.Fatalf("seed form: %v", err)
	}
	var ruleID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO workflow.automation_rule (application_id, name, trigger_type, workflow_name, source_form_id, source_grid_id, enabled, revision_id)
		VALUES ($1::uuid, 'Scoped Rule', 'form_submit', 'irrelevant', $2::uuid, $3::uuid, true, $4::uuid) RETURNING id::text
	`, f.appID, formID, f.gridStaffID, f.workingRevID).Scan(&ruleID); err != nil {
		t.Fatalf("seed automation rule: %v", err)
	}

	if _, err := f.pool.Exec(ctx, `DELETE FROM model.form_def WHERE id=$1::uuid`, formID); err != nil {
		t.Fatalf("delete form: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM model.grid_def WHERE id=$1::uuid`, f.gridStaffID); err != nil {
		t.Fatalf("delete grid: %v", err)
	}

	var exists bool
	var sourceFormID, sourceGridID *string
	if err := f.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM workflow.automation_rule WHERE id=$1::uuid),
		       (SELECT source_form_id::text FROM workflow.automation_rule WHERE id=$1::uuid),
		       (SELECT source_grid_id::text FROM workflow.automation_rule WHERE id=$1::uuid)
	`, ruleID).Scan(&exists, &sourceFormID, &sourceGridID); err != nil {
		t.Fatalf("query rule after deletes: %v", err)
	}
	if !exists {
		t.Fatal("automation rule was deleted along with its source form/grid — want it to survive with source_form_id/source_grid_id set to NULL")
	}
	if sourceFormID != nil {
		t.Errorf("source_form_id = %v after form deletion, want NULL", *sourceFormID)
	}
	if sourceGridID != nil {
		t.Errorf("source_grid_id = %v after grid deletion, want NULL", *sourceGridID)
	}
}

// TestDeleteRevisionWithFormMappingTargetingItsMetric is the regression test
// for the long-flagged revision-delete 500: DELETE /api/developer/revisions/
// {id} died on form_metric_mapping_target_metric_id_fkey whenever a mapping
// row referenced one of the revision's metrics without itself being reached
// by the revision cascade — the shape a legacy mapping with revision_id NULL
// (predating 056's backfill) produces. Migration 072 makes target_metric_id
// cascade, so the delete now removes the mapping with the metric.
func TestDeleteRevisionWithFormMappingTargetingItsMetric(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	h := &handler{db: tenantdb.NewHandle(f.pool, nil), log: logger.New("test")}

	// A second revision to delete, with its own metric copy.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	newRevID, err := h.duplicateRevision(ctx, tx, f.modelID, "Doomed", f.workingRevID, &f.workingRevID)
	if err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("duplicateRevision: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var dupMetricID string
	if err := f.pool.QueryRow(ctx, `
		SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='amount'
	`, f.modelID, newRevID).Scan(&dupMetricID); err != nil {
		t.Fatalf("find duplicated metric: %v", err)
	}

	// A form + a LEGACY mapping (revision_id NULL) targeting the doomed
	// revision's metric — exactly the row that used to block the delete.
	var formID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO model.form_def (model_id, name, label, fields, revision_id)
		VALUES ($1::uuid, 'Legacy Form', 'Legacy Form', '[]'::jsonb, $2::uuid) RETURNING id::text
	`, f.modelID, f.workingRevID).Scan(&formID); err != nil {
		t.Fatalf("seed form: %v", err)
	}
	var mappingID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO model.form_metric_mapping (model_id, form_id, name, source_field, target_metric_id, aggregation)
		VALUES ($1::uuid, $2::uuid, 'Legacy mapping', 'amount', $3::uuid, 'sum') RETURNING id::text
	`, f.modelID, formID, dupMetricID).Scan(&mappingID); err != nil {
		t.Fatalf("seed legacy mapping: %v", err)
	}

	status, body := f.do(t, "DELETE", "/api/developer/revisions/"+newRevID, "rollup-test-approver", nil)
	if status != http.StatusOK {
		t.Fatalf("revision delete: status %d, want 200 (the old FK 500)\nbody: %v", status, body)
	}

	// The metric cascade must have taken the mapping with it.
	var mappingLeft bool
	_ = f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model.form_metric_mapping WHERE id=$1::uuid)`, mappingID).Scan(&mappingLeft)
	if mappingLeft {
		t.Error("legacy mapping still exists after revision delete — target_metric_id cascade missing")
	}
}

// TestGridDeleteCascadesWidgetsAndMemberRenameRekeysWidgetProps covers
// SYNC-01 and SYNC-02 from the dashboard-sync audit:
//   - deleting a grid drops the dashboard widgets referencing it (ref_id is
//     bare TEXT with no FK; a dangling chart widget 500'd on chart-data);
//   - renaming a member's code re-keys the member-code references inside
//     widget_props (filter_sel / context_defaults / kpi_scope), which the
//     fact_input/calc_result re-key used to leave behind.
func TestGridDeleteCascadesWidgetsAndMemberRenameRekeysWidgetProps(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	var dashID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO model.dashboard_def (model_id, name, revision_id)
		VALUES ($1::uuid, 'Sync Dash', $2::uuid) RETURNING id::text
	`, f.modelID, f.workingRevID).Scan(&dashID); err != nil {
		t.Fatalf("seed dashboard: %v", err)
	}
	props := fmt.Sprintf(`{
		"default_view": {"rows": ["__metrics__"], "cols": [%q], "context": [], "filter_sel": {%q: "DEPT_A"}},
		"chart": {"chart_type": "bar", "dimension_id": %q, "metric_ids": [%q], "context_defaults": {%q: "DEPT_A"}},
		"kpi_scope": {"dimension_id": %q, "member_code": "DEPT_A"}
	}`, f.deptsDimID, f.deptsDimID, f.deptsDimID, f.deptTotalMetricID, f.deptsDimID, f.deptsDimID)
	var gridWidgetID, kpiWidgetID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, sort_order, widget_props)
		VALUES ($1::uuid, 'grid', $2, 0, $3::jsonb) RETURNING id::text
	`, dashID, f.gridDeptTotalID, props).Scan(&gridWidgetID); err != nil {
		t.Fatalf("seed grid widget: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, sort_order, widget_props)
		VALUES ($1::uuid, 'metric_kpi', $2, 1, $3::jsonb) RETURNING id::text
	`, dashID, f.deptTotalMetricID, props).Scan(&kpiWidgetID); err != nil {
		t.Fatalf("seed kpi widget: %v", err)
	}

	// SYNC-02: rename DEPT_A -> DEPT_ALPHA through the member PATCH.
	var deptAID string
	if err := f.pool.QueryRow(ctx, `
		SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='DEPT_A'
	`, f.deptsDimID).Scan(&deptAID); err != nil {
		t.Fatalf("find DEPT_A: %v", err)
	}
	status, body := f.do(t, "PATCH",
		"/api/developer/dimensions/"+f.deptsDimID+"/members/"+deptAID,
		"rollup-test-approver", map[string]any{"code": "DEPT_ALPHA", "label": "Dept Alpha"})
	if status != 200 {
		t.Fatalf("member rename: status %d %v", status, body)
	}
	var fs, cd, ks string
	if err := f.pool.QueryRow(ctx, `
		SELECT widget_props #>> ARRAY['default_view','filter_sel',$2::text],
		       widget_props #>> ARRAY['chart','context_defaults',$2::text],
		       widget_props #>> ARRAY['kpi_scope','member_code']
		FROM model.dashboard_widget WHERE id=$1::uuid
	`, gridWidgetID, f.deptsDimID).Scan(&fs, &cd, &ks); err != nil {
		t.Fatalf("read widget_props: %v", err)
	}
	if fs != "DEPT_ALPHA" || cd != "DEPT_ALPHA" || ks != "DEPT_ALPHA" {
		t.Errorf("widget_props after rename: filter_sel=%q context_defaults=%q kpi_scope=%q, want DEPT_ALPHA in all three", fs, cd, ks)
	}

	// SYNC-01: deleting the grid drops the grid widget; the KPI widget
	// (referencing a metric, not the grid) survives.
	status, body = f.do(t, "DELETE", "/api/developer/grids/"+f.gridDeptTotalID, "rollup-test-approver", nil)
	if status != 200 {
		t.Fatalf("grid delete: status %d %v", status, body)
	}
	var gridWidgetLeft, kpiWidgetLeft bool
	_ = f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model.dashboard_widget WHERE id=$1::uuid)`, gridWidgetID).Scan(&gridWidgetLeft)
	_ = f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model.dashboard_widget WHERE id=$1::uuid)`, kpiWidgetID).Scan(&kpiWidgetLeft)
	if gridWidgetLeft {
		t.Error("grid widget still exists after its grid was deleted — SYNC-01 cascade missing")
	}
	if !kpiWidgetLeft {
		t.Error("metric_kpi widget was deleted although its metric still exists")
	}

	// A chart names its metrics INSIDE widget_props. Deleting one of two
	// plotted metrics strips it from the list; deleting the last one drops
	// the chart, which would otherwise fail as "metric not accessible". A
	// chart that never had metrics (still being designed) is left alone.
	var twoMetricChart, designedChart string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, widget_props, sort_order)
		VALUES ($1::uuid, 'chart', $2, jsonb_build_object('chart', jsonb_build_object('metric_ids', jsonb_build_array($3::text, $4::text))), 5)
		RETURNING id::text`, dashID, f.gridStaffID, f.amountMetricID, f.deptTotalMetricID).Scan(&twoMetricChart); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, widget_props, sort_order)
		VALUES ($1::uuid, 'chart', $2, '{"chart": {"metric_ids": []}}'::jsonb, 6) RETURNING id::text`, dashID, f.gridStaffID).Scan(&designedChart); err != nil {
		t.Fatal(err)
	}
	if status, body = f.do(t, "DELETE", "/api/developer/metrics/"+f.deptTotalMetricID, "rollup-test-approver", nil); status != 200 {
		t.Fatalf("metric delete: status %d %v", status, body)
	}
	var left string
	if err := f.pool.QueryRow(ctx, `SELECT widget_props->'chart'->'metric_ids' FROM model.dashboard_widget WHERE id=$1::uuid`, twoMetricChart).Scan(&left); err != nil {
		t.Fatalf("chart after metric delete: %v", err)
	}
	if left != `["`+f.amountMetricID+`"]` {
		t.Errorf("chart metric_ids after deleting one of two = %s, want only the surviving metric", left)
	}
	if status, body = f.do(t, "DELETE", "/api/developer/metrics/"+f.amountMetricID, "rollup-test-approver", nil); status != 200 {
		t.Fatalf("metric delete: status %d %v", status, body)
	}
	var chartLeft, designedLeft bool
	_ = f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model.dashboard_widget WHERE id=$1::uuid)`, twoMetricChart).Scan(&chartLeft)
	_ = f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model.dashboard_widget WHERE id=$1::uuid)`, designedChart).Scan(&designedLeft)
	if chartLeft {
		t.Error("a chart whose last metric was deleted still exists")
	}
	if !designedLeft {
		t.Error("a chart that never had metrics was deleted by an unrelated metric's deletion")
	}
}
