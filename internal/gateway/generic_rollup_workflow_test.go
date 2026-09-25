// Tests for the generic engine work added to support the Salary Budgeting
// demo (since removed with its database-direct seed) without any
// bespoke payroll-specific endpoints or widget types: grid() exposing
// rollup_source_grid_id / dimension_member.properties / dimension_def.
// source_dimension_id+source_property, workflowStartInstance's RACI-scoped
// context resolution, cells()'s generic workflow-instance write-lock,
// taskAction's required_comment enforcement and on_approve copy-to-revision
// action, and revision duplication remapping the new self-referencing
// columns. Uses a small synthetic model (departments/staff/regions), not
// the real payroll seed, so these stay fast and independent of it.
//
// The rollup *arithmetic* itself (resolveCrossDimensionValue,
// resolveCell/getVal, defaultLeafCode) is frontend TypeScript with no
// existing unit-test runner in this repo (only mocked Playwright E2E) — it
// was verified this session by hand against a real running instance
// (browser + curl, cross-checked against hand-computed sums). These tests
// cover the backend half: that grid()/cells()/taskAction/workflowStartInstance
// correctly expose and enforce the data those frontend functions depend on.
package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"

	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
)

// ── shared fixture ───────────────────────────────────────────────────────────

type rollupFixture struct {
	pool *pgxpool.Pool
	srv  *httptest.Server

	appID, modelID                          string
	workingRevID, annualRevID               string
	deptsDimID, staffDimID, regionsDimID    string
	deptAID, deptBID                        string
	staffA1ID, staffA2ID                    string
	amountMetricID                          string
	gridStaffID, gridDeptsID, gridRegionsID string
	wfDefID                                 string
	managerID, approverID                   string
	managerBID                              string // RACI-responsible for DEPT_B, mirrors managerID's DEPT_A scoping
	regionGroupID                           string // same-dimension parent of WEST (regionsDimID) — the fixture's only non-leaf member, for leaf-check tests

	// Calc metrics for grid()/BusinessConsole convergence tests (see the
	// TestGridCalc* functions below). Not read/used by any other existing
	// test in this file.
	deptTotalMetricID       string // dims=[departments], formula "=amount" — cross-dimension reference (departments is amount's own staff dim's structural parent)
	deptTotalCappedMetricID string // dims=[departments], formula "=IF(dept_total>120,120,dept_total)" — depends on deptTotal, and non-linear (for scoped-vs-unscoped total divergence tests)
	regionTotalMetricID     string // dims=[regions], formula "=amount" — same-dimension hierarchy (regions has REGION_GROUP as a non-leaf parent of WEST)
	gridDeptTotalID         string // own (non-rollup-mirror) grid: dims=[departments], metrics=[deptTotal, deptTotalCapped, quota, deptRatio]
	gridRegionTotalID       string // own (non-rollup-mirror) grid: dims=[regions], metrics=[regionTotal]

	// quota is an input entered for DEPT_A only (not DEPT_B); deptRatio =
	// dept_total/quota is genuinely undefined (#DIV/0!) for DEPT_B —
	// reproduces a real bug found via manual verification against live seed
	// data (budget_variance_pct dividing by a budget_target only entered for
	// some months): one combo's runtime formula error must not blank out a
	// calc metric's OTHER, otherwise-valid combos.
	quotaMetricID     string
	deptRatioMetricID string

	// budget is an input entered for BOTH departments with deliberately
	// unequal values (40, 60) so per-combo ratios of dept_total/budget
	// genuinely differ across DEPT_A/DEPT_B — unlike quota/deptRatio above
	// (DEPT_B undefined), this exercises average-of-ratios vs.
	// ratio-of-sums divergence specifically, for an agg_rule="average"
	// calc metric whose formula isn't itself dimension-conditional.
	budgetMetricID       string
	deptRatioAvgMetricID string
}

func setupRollupFixture(t *testing.T) *rollupFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

	f := &rollupFixture{pool: pool}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('T', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	f.appID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, wsID, custID)
	f.modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, f.appID)

	f.workingRevID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, f.modelID)
	f.annualRevID = q(`INSERT INTO model.revision (model_id, name, system_managed) VALUES ($1::uuid, 'Annual', true) RETURNING id::text`, f.modelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, f.workingRevID, f.modelID)

	f.deptsDimID = q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'departments') RETURNING id::text`, f.modelID, f.workingRevID)
	f.deptAID = q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'DEPT_A', 'Dept A') RETURNING id::text`, f.deptsDimID)
	f.deptBID = q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'DEPT_B', 'Dept B') RETURNING id::text`, f.deptsDimID)

	f.staffDimID = q(`INSERT INTO model.dimension_def (model_id, revision_id, name, parent_dimension_id) VALUES ($1::uuid, $2::uuid, 'staff', $3::uuid) RETURNING id::text`, f.modelID, f.workingRevID, f.deptsDimID)
	f.staffA1ID = q(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, properties) VALUES ($1::uuid, 'STAFF_A1', 'A1', $2::uuid, '{"region":"WEST"}') RETURNING id::text`, f.staffDimID, f.deptAID)
	f.staffA2ID = q(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, properties) VALUES ($1::uuid, 'STAFF_A2', 'A2', $2::uuid, '{"region":"EAST"}') RETURNING id::text`, f.staffDimID, f.deptAID)
	exec(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, properties) VALUES ($1::uuid, 'STAFF_B1', 'B1', $2::uuid, '{"region":"WEST"}')`, f.staffDimID, f.deptBID)

	f.regionsDimID = q(`INSERT INTO model.dimension_def (model_id, revision_id, name, source_dimension_id, source_property) VALUES ($1::uuid, $2::uuid, 'regions', $3::uuid, 'region') RETURNING id::text`, f.modelID, f.workingRevID, f.staffDimID)
	westID := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'WEST', 'West') RETURNING id::text`, f.regionsDimID)
	exec(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'EAST', 'East')`, f.regionsDimID)
	// A same-dimension hierarchy parent (regions has none otherwise — every
	// other dimension in this fixture is either flat or cross-dimension
	// linked) so leaf-only-member tests have a real non-leaf member to
	// exercise against: writing/importing "REGION_GROUP" must be rejected,
	// "WEST" (now a leaf with a parent) must still be accepted.
	f.regionGroupID = q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'REGION_GROUP', 'Region Group') RETURNING id::text`, f.regionsDimID)
	exec(`UPDATE model.dimension_member SET parent_member_id=$1::uuid WHERE id=$2::uuid`, f.regionGroupID, westID)

	f.amountMetricID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format) VALUES ($1::uuid, $2::uuid, 'amount', true, 'currency') RETURNING id::text`, f.modelID, f.workingRevID)

	f.gridStaffID = q(`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Staff Grid', $2::uuid) RETURNING id::text`, f.modelID, f.workingRevID)
	exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, f.gridStaffID, f.staffDimID)
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 0)`, f.gridStaffID, f.amountMetricID)

	f.gridDeptsID = q(`INSERT INTO model.grid_def (model_id, name, revision_id, rollup_source_grid_id) VALUES ($1::uuid, 'Dept Rollup', $2::uuid, $3::uuid) RETURNING id::text`, f.modelID, f.workingRevID, f.gridStaffID)
	exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, f.gridDeptsID, f.deptsDimID)

	f.gridRegionsID = q(`INSERT INTO model.grid_def (model_id, name, revision_id, rollup_source_grid_id) VALUES ($1::uuid, 'Region Rollup', $2::uuid, $3::uuid) RETURNING id::text`, f.modelID, f.workingRevID, f.gridStaffID)
	exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, f.gridRegionsID, f.regionsDimID)

	// Users: a "manager" (RACI responsible on DEPT_A.*, business_user), a
	// second "managerB" (RACI responsible on DEPT_B.*, mirrors managerID —
	// exists so tests can prove two scopes are independent) and an
	// "approver" (business_admin, matches the step's assignee_roles; also
	// granted developer for the revision-duplication test, which is a
	// developer-only endpoint — no need for a fourth persona in this fixture).
	f.managerID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-manager', 'manager@t.com', 'Manager', $1::uuid) RETURNING id::text`, custID)
	f.managerBID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-manager-b', 'managerb@t.com', 'Manager B', $1::uuid) RETURNING id::text`, custID)
	f.approverID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-approver', 'approver@t.com', 'Approver', $1::uuid) RETURNING id::text`, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, f.managerID, wsID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, f.managerBID, wsID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_admin', $2::uuid)`, f.approverID, wsID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, f.approverID, wsID)
	exec(`INSERT INTO security.raci_rule (application_id, user_id, resource_pattern, raci_type) VALUES ($1::uuid, $2::uuid, 'DEPT_A.*', 'responsible')`, f.appID, f.managerID)
	exec(`INSERT INTO security.raci_rule (application_id, user_id, resource_pattern, raci_type) VALUES ($1::uuid, $2::uuid, 'DEPT_B.*', 'responsible')`, f.appID, f.managerBID)

	// Hidden dimension_member access rules — department-level only. staff is
	// a declared child of departments (parent_dimension_id/parent_member_id,
	// see the staff dimension setup above), so hiding a department now
	// automatically cascades to every staff member under it (writeguard.
	// ExpandHidden on the read side — see hiddenCodesByDim/factRowHidden in
	// handler.go — and AncestorChain on the write side, see writeguard.
	// CheckWrite) with no staff-level rule needed. This mirrors
	// the former salary demo's cost-center scoping after the same simplification.
	hide := func(userID string, memberIDs ...string) {
		for _, id := range memberIDs {
			exec(`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'hidden')`, userID, id)
		}
	}
	hide(f.managerID, f.deptBID)
	hide(f.managerBID, f.deptAID)

	// Facts: STAFF_A1=100, STAFF_A2=50 (both DEPT_A, manager's scope — total
	// 150), STAFF_B1=200 (DEPT_B, managerB's scope) — lets grid-scoping tests
	// assert a specific scoped total, not just "fewer than everything".
	for _, fct := range []struct {
		code string
		val  float64
	}{{"STAFF_A1", 100}, {"STAFF_A2", 50}, {"STAFF_B1", 200}} {
		dimMembers := fmt.Sprintf(`{"%s":"%s"}`, f.staffDimID, fct.code)
		exec(`INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, $4::jsonb, $5, $6::uuid)`,
			f.modelID, f.workingRevID, f.amountMetricID, dimMembers, fct.val, f.managerID)
	}

	// ── calc metrics for grid()/BusinessConsole convergence tests ────────────
	// Two OWN (non-rollup-mirror) grids, each with a genuine grid_metric
	// assignment at a grain different from `amount`'s own (staff) grain —
	// unlike gridDeptsID/gridRegionsID above, which only ever MIRROR the
	// staff grid's own metrics by name via rollup_source_grid_id and so
	// never give a metric a real dimension_ids of its own.
	f.deptTotalMetricID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format, formula) VALUES ($1::uuid, $2::uuid, 'dept_total', false, 'currency', '=amount') RETURNING id::text`, f.modelID, f.workingRevID)
	f.deptTotalCappedMetricID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format, formula) VALUES ($1::uuid, $2::uuid, 'dept_total_capped', false, 'currency', '=IF(dept_total>120,120,dept_total)') RETURNING id::text`, f.modelID, f.workingRevID)
	f.regionTotalMetricID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format, formula) VALUES ($1::uuid, $2::uuid, 'region_total', false, 'currency', '=amount') RETURNING id::text`, f.modelID, f.workingRevID)

	f.gridDeptTotalID = q(`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Dept Total Grid', $2::uuid) RETURNING id::text`, f.modelID, f.workingRevID)
	exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, f.gridDeptTotalID, f.deptsDimID)
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 0)`, f.gridDeptTotalID, f.deptTotalMetricID)
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 1)`, f.gridDeptTotalID, f.deptTotalCappedMetricID)

	f.gridRegionTotalID = q(`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Region Total Grid', $2::uuid) RETURNING id::text`, f.modelID, f.workingRevID)
	exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, f.gridRegionTotalID, f.regionsDimID)
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 0)`, f.gridRegionTotalID, f.regionTotalMetricID)

	// Synthetic calc_result rows, as if the calculation scheduler had already
	// run (these tests exercise grid()'s READ side, not the scheduler itself
	// — that's internal/calculation's own test suite). Hand-computed from the
	// facts above:
	//   dept_total:        DEPT_A=100+50=150, DEPT_B=200,          {}=350
	//   dept_total_capped: DEPT_A=IF(150>120,120,150)=120, DEPT_B=IF(200>120,120,200)=120, {}=240
	//   region_total (WEST=STAFF_A1+STAFF_B1 via the region property, EAST=STAFF_A2):
	//                       WEST=100+200=300, EAST=50,             {}=350
	//                       (no REGION_GROUP row: it's a non-leaf member —
	//                       matches how the scheduler only ever writes leaf
	//                       combos + the aggregate, never a group code)
	writeCalc := func(metricID, dimID, code string, val float64) {
		dm := "{}"
		if dimID != "" {
			dm = fmt.Sprintf(`{"%s":"%s"}`, dimID, code)
		}
		exec(`INSERT INTO runtime.calc_result (model_id, revision_id, dim_members, metric_id, value, partition_key) VALUES ($1::uuid, $2::uuid, $3::jsonb, $4::uuid, $5, 'test')`,
			f.modelID, f.workingRevID, dm, metricID, val)
	}
	writeCalc(f.deptTotalMetricID, f.deptsDimID, "DEPT_A", 150)
	writeCalc(f.deptTotalMetricID, f.deptsDimID, "DEPT_B", 200)
	writeCalc(f.deptTotalMetricID, "", "", 350)
	writeCalc(f.deptTotalCappedMetricID, f.deptsDimID, "DEPT_A", 120)
	writeCalc(f.deptTotalCappedMetricID, f.deptsDimID, "DEPT_B", 120)
	writeCalc(f.deptTotalCappedMetricID, "", "", 240)
	writeCalc(f.regionTotalMetricID, f.regionsDimID, "WEST", 300)
	writeCalc(f.regionTotalMetricID, f.regionsDimID, "EAST", 50)
	writeCalc(f.regionTotalMetricID, "", "", 350)

	// quota (input, dims=[departments]) is entered for DEPT_A only; deptRatio
	// = dept_total/quota is a genuine #DIV/0! for DEPT_B. No synthetic
	// calc_result row is written for either — this scenario is specifically
	// for the SCOPED (hidden-member) recomputation path, which never reads
	// calc_result at all.
	f.quotaMetricID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format) VALUES ($1::uuid, $2::uuid, 'quota', true, 'currency') RETURNING id::text`, f.modelID, f.workingRevID)
	f.deptRatioMetricID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format, formula) VALUES ($1::uuid, $2::uuid, 'dept_ratio', false, 'number', '=dept_total/quota') RETURNING id::text`, f.modelID, f.workingRevID)
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 2)`, f.gridDeptTotalID, f.quotaMetricID)
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 3)`, f.gridDeptTotalID, f.deptRatioMetricID)
	exec(`INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, $4::jsonb, $5, $6::uuid)`,
		f.modelID, f.workingRevID, f.quotaMetricID, fmt.Sprintf(`{"%s":"DEPT_A"}`, f.deptsDimID), 50.0, f.managerID)

	// budget (input, dims=[departments]) entered for BOTH departments with
	// unequal values — dept_ratio_avg = dept_total/budget, agg_rule
	// "average": DEPT_A=100/40=2.5, DEPT_B=200/60=3.333..., average of
	// those two = 2.9166...; but the correct collapsed value (matching
	// executePartition's own "average agg_rule + non-dimension-conditional
	// formula" special case, mirrored in scopeCalcCells) is the ratio of
	// SUMS: (100+200)/(40+60) = 300/100 = 3.0 exactly. The synthetic
	// calc_result row below is that same ground-truth value, as if the
	// scheduler had already run — for the unrestricted-persona comparison
	// half of the regression test.
	f.budgetMetricID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format) VALUES ($1::uuid, $2::uuid, 'budget', true, 'currency') RETURNING id::text`, f.modelID, f.workingRevID)
	f.deptRatioAvgMetricID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format, formula, agg_rule) VALUES ($1::uuid, $2::uuid, 'dept_ratio_avg', false, 'number', '=dept_total/budget', 'average') RETURNING id::text`, f.modelID, f.workingRevID)
	// budget must be a real grid_metric on this grid too, not just an
	// existing metric_def row — otherwise its facts never load into
	// allMetrics/cells for this request, dept_ratio_avg's dependency
	// resolves as a miss (0), and dept_total/0 is a #DIV/0! that gets
	// silently skipped, not what this test means to exercise.
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 4)`, f.gridDeptTotalID, f.budgetMetricID)
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 5)`, f.gridDeptTotalID, f.deptRatioAvgMetricID)
	exec(`INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, $4::jsonb, $5, $6::uuid)`,
		f.modelID, f.workingRevID, f.budgetMetricID, fmt.Sprintf(`{"%s":"DEPT_A"}`, f.deptsDimID), 40.0, f.managerID)
	exec(`INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, $4::jsonb, $5, $6::uuid)`,
		f.modelID, f.workingRevID, f.budgetMetricID, fmt.Sprintf(`{"%s":"DEPT_B"}`, f.deptsDimID), 60.0, f.managerID)
	writeCalc(f.deptRatioAvgMetricID, "", "", 3.0)

	// Wire test personas into devPersonas so X-Dev-User works in requests.
	devPersonas["rollup-test-manager"] = "test-manager"
	devPersonas["rollup-test-manager-b"] = "test-manager-b"
	devPersonas["rollup-test-approver"] = "test-approver"
	t.Cleanup(func() {
		delete(devPersonas, "rollup-test-manager")
		delete(devPersonas, "rollup-test-manager-b")
		delete(devPersonas, "rollup-test-approver")
	})

	// Workflow: context_schema binds "scope" to departments via RACI;
	// "target_revision_id" is a plain manual value. Step requires a comment
	// and, on approval, copies facts scoped to "scope" from context's
	// revision_id into target_revision_id.
	wfStore := workflow.NewStore(pool)
	wfDef, err := wfStore.CreateWorkflowDef(ctx, f.appID, "Test Approval", "manual", []*workflowv1.WorkflowStepDef{
		{Id: "step-1", Name: "Approve", Type: workflowv1.StepType_STEP_TYPE_APPROVAL, AssigneeRoles: []string{"business_admin"}, NextStepIds: []string{}},
	})
	if err != nil {
		t.Fatalf("create workflow def: %v", err)
	}
	f.wfDefID = wfDef.Id

	type contextVarDef struct {
		Key         string `json:"key"`
		DataType    string `json:"data_type"`
		Required    bool   `json:"required"`
		SourceHint  string `json:"source_hint,omitempty"`
		DimensionID string `json:"dimension_id,omitempty"`
	}
	schema, _ := json.Marshal([]contextVarDef{
		{Key: "scope", DataType: "Dimension member", Required: true, SourceHint: "raci_responsible", DimensionID: f.deptsDimID},
		{Key: "revision_id", DataType: "Text", Required: true},
		{Key: "target_revision_id", DataType: "Text", Required: true},
	})
	exec(`UPDATE workflow.workflow_def SET context_schema=$1::jsonb WHERE id=$2::uuid`, schema, f.wfDefID)
	onApprove, _ := json.Marshal(map[string]string{"copy_facts_to_context_key": "target_revision_id", "scope_context_key": "scope"})
	exec(`UPDATE workflow.workflow_def SET steps = jsonb_set(steps, '{0,on_approve}', $1::jsonb) WHERE id=$2::uuid`, onApprove, f.wfDefID)
	exec(`UPDATE workflow.workflow_def SET steps = jsonb_set(steps, '{0,required_comment}', 'true'::jsonb) WHERE id=$1::uuid`, f.wfDefID)
	if _, err := wfStore.PublishWorkflowDef(ctx, f.wfDefID, f.approverID); err != nil {
		t.Fatalf("publish workflow def: %v", err)
	}

	t.Setenv("DEV_MODE", "true")
	f.srv = httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(f.srv.Close)

	return f
}

// do performs a request and returns (status code, parsed JSON body). The
// response body is consumed and closed here so call sites can't leak it.
func (f *rollupFixture) do(t *testing.T, method, path, persona string, body any) (int, map[string]any) {
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
	req.Header.Set("X-App-Id", f.appID) // needed by endpoints (e.g. import) that resolve the model ambiently
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

// ── tests ─────────────────────────────────────────────────────────────────

func TestGridExposesRollupAndPropertyMetadata(t *testing.T) {
	f := setupRollupFixture(t)
	resp, body := f.do(t, "GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.workingRevID, f.gridDeptsID), "rollup-test-approver", nil)
	if resp != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp, body)
	}
	if got := body["rollup_source_grid_id"]; got != f.gridStaffID {
		t.Errorf("rollup_source_grid_id = %v, want %v", got, f.gridStaffID)
	}
	metrics, _ := body["metrics"].([]any)
	if len(metrics) != 1 {
		t.Fatalf("expected the rollup grid to mirror the source grid's 1 metric, got %d", len(metrics))
	}
	if name := metrics[0].(map[string]any)["name"]; name != "amount" {
		t.Errorf("metric name = %v, want amount", name)
	}

	// Region-rollup grid: dimension_def must expose source_dimension_id/
	// source_property, and dimension_member.properties must round-trip.
	resp2, body2 := f.do(t, "GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.workingRevID, f.gridRegionsID), "rollup-test-approver", nil)
	if resp2 != http.StatusOK {
		t.Fatalf("status = %d, body = %v", resp2, body2)
	}
	allDims, _ := body2["all_dimensions"].([]any)
	var staffDim map[string]any
	for _, d := range allDims {
		dm := d.(map[string]any)
		if dm["id"] == f.staffDimID {
			staffDim = dm
		}
	}
	if staffDim == nil {
		t.Fatal("staff dimension not found in all_dimensions")
	}
	members, _ := staffDim["members"].([]any)
	var a1 map[string]any
	for _, m := range members {
		mm := m.(map[string]any)
		if mm["code"] == "STAFF_A1" {
			a1 = mm
		}
	}
	if a1 == nil {
		t.Fatal("STAFF_A1 not found")
	}
	props, _ := a1["properties"].(map[string]any)
	if props["region"] != "WEST" {
		t.Errorf("STAFF_A1 properties.region = %v, want WEST", props["region"])
	}

	regionsDim := findDim(body2["dimensions"].([]any), f.regionsDimID)
	if regionsDim["source_dimension_id"] != f.staffDimID {
		t.Errorf("regions.source_dimension_id = %v, want %v", regionsDim["source_dimension_id"], f.staffDimID)
	}
	if regionsDim["source_property"] != "region" {
		t.Errorf("regions.source_property = %v, want region", regionsDim["source_property"])
	}
}

func findDim(dims []any, id string) map[string]any {
	for _, d := range dims {
		dm := d.(map[string]any)
		if dm["id"] == id {
			return dm
		}
	}
	return nil
}

func TestWorkflowStartInstanceRACIResolutionAndDedup(t *testing.T) {
	f := setupRollupFixture(t)

	// Manager starts the workflow with no "scope" in the request body — and
	// also tries to spoof DEPT_B, which must be overridden, not honored.
	resp, body := f.do(t, "POST", "/api/workflow/instances", "rollup-test-manager", map[string]any{
		"workflow_def_id": f.wfDefID,
		"context":         map[string]string{"scope": "DEPT_B", "revision_id": f.workingRevID, "target_revision_id": f.annualRevID},
	})
	if resp != http.StatusOK {
		t.Fatalf("submit status = %d, body = %v", resp, body)
	}
	instanceID, _ := body["instance_id"].(string)
	if instanceID == "" {
		t.Fatal("expected an instance_id")
	}

	var scope string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT context->>'scope' FROM workflow.workflow_instance WHERE id=$1::uuid`, instanceID,
	).Scan(&scope); err != nil {
		t.Fatalf("query instance: %v", err)
	}
	if scope != "DEPT_A" {
		t.Errorf("scope = %q, want DEPT_A (server-resolved from RACI, not the spoofed DEPT_B)", scope)
	}

	// A second submit for the same manager/scope while the first is still
	// running must be rejected (dedup).
	resp2, body2 := f.do(t, "POST", "/api/workflow/instances", "rollup-test-manager", map[string]any{
		"workflow_def_id": f.wfDefID,
		"context":         map[string]string{"model_id": f.modelID, "revision_id": f.workingRevID, "target_revision_id": f.annualRevID},
	})
	if resp2 != http.StatusConflict {
		t.Errorf("second submit status = %d, want 409, body = %v", resp2, body2)
	}
}

func TestCellsWriteLockDistinguishesApproveFromReject(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	writeBody := map[string]any{
		"model_id": f.modelID, "revision_id": f.workingRevID, "metric_id": f.amountMetricID,
		"dim_codes": map[string]string{f.staffDimID: "STAFF_A1"}, "value": 200,
	}

	// Baseline: open, writable.
	if resp, body := f.do(t, "POST", "/api/cells", "rollup-test-manager", writeBody); resp != http.StatusOK {
		t.Fatalf("baseline write status = %d, body = %v", resp, body)
	}

	submit := func() (instanceID, stepID string) {
		_, body := f.do(t, "POST", "/api/workflow/instances", "rollup-test-manager", map[string]any{
			"workflow_def_id": f.wfDefID,
			"context":         map[string]string{"model_id": f.modelID, "revision_id": f.workingRevID, "target_revision_id": f.annualRevID},
		})
		instanceID = body["instance_id"].(string)
		if err := f.pool.QueryRow(ctx, `SELECT id::text FROM workflow.workflow_step WHERE instance_id=$1::uuid`, instanceID).Scan(&stepID); err != nil {
			t.Fatalf("find step: %v", err)
		}
		return
	}

	_, stepID := submit()

	// Locked while running.
	if resp, _ := f.do(t, "POST", "/api/cells", "rollup-test-manager", writeBody); resp != http.StatusForbidden {
		t.Errorf("write while submitted status = %d, want 403", resp)
	}

	// Reject without a comment is rejected (required_comment enforcement).
	if resp, _ := f.do(t, "POST", fmt.Sprintf("/api/tasks/%s/complete", stepID), "rollup-test-approver", map[string]string{"decision": "reject", "comment": ""}); resp != http.StatusBadRequest {
		t.Errorf("reject without comment status = %d, want 400", resp)
	}

	// Reject with a comment unlocks.
	if resp, body := f.do(t, "POST", fmt.Sprintf("/api/tasks/%s/complete", stepID), "rollup-test-approver", map[string]string{"decision": "reject", "comment": "needs work"}); resp != http.StatusOK {
		t.Fatalf("reject status = %d, body = %v", resp, body)
	}
	if resp, body := f.do(t, "POST", "/api/cells", "rollup-test-manager", writeBody); resp != http.StatusOK {
		t.Errorf("write after reject status = %d, want 200, body = %v", resp, body)
	}

	// Resubmit + approve must permanently lock (this is the exact bug found
	// and fixed this session: taskAction leaves the instance status
	// "completed" for both approve and reject, so the lock must be derived
	// from the step's decision, not just "not running").
	_, stepID2 := submit()
	if resp, body := f.do(t, "POST", fmt.Sprintf("/api/tasks/%s/complete", stepID2), "rollup-test-approver", map[string]string{"decision": "approve", "comment": "looks good"}); resp != http.StatusOK {
		t.Fatalf("approve status = %d, body = %v", resp, body)
	}
	if resp, body := f.do(t, "POST", "/api/cells", "rollup-test-manager", writeBody); resp != http.StatusForbidden {
		t.Errorf("write after approve status = %d, want 403 (permanently locked), body = %v", resp, body)
	}

	// system_managed revision is always rejected, workflow state aside.
	annualWrite := map[string]any{
		"model_id": f.modelID, "revision_id": f.annualRevID, "metric_id": f.amountMetricID,
		"dim_codes": map[string]string{f.staffDimID: "STAFF_A1"}, "value": 1,
	}
	if resp, _ := f.do(t, "POST", "/api/cells", "rollup-test-approver", annualWrite); resp != http.StatusForbidden {
		t.Errorf("write to system_managed revision status = %d, want 403", resp)
	}
}

func TestOnApproveCopiesFactsToTargetRevision(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	_, body := f.do(t, "POST", "/api/workflow/instances", "rollup-test-manager", map[string]any{
		"workflow_def_id": f.wfDefID,
		"context":         map[string]string{"model_id": f.modelID, "revision_id": f.workingRevID, "target_revision_id": f.annualRevID},
	})
	instanceID := body["instance_id"].(string)
	var stepID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM workflow.workflow_step WHERE instance_id=$1::uuid`, instanceID).Scan(&stepID); err != nil {
		t.Fatalf("find step: %v", err)
	}

	if resp, respBody := f.do(t, "POST", fmt.Sprintf("/api/tasks/%s/complete", stepID), "rollup-test-approver", map[string]string{"decision": "approve", "comment": "approved"}); resp != http.StatusOK {
		t.Fatalf("approve status = %d, body = %v", resp, respBody)
	}

	var copiedValue float64
	var copiedCount int
	if err := f.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(MAX(value), 0) FROM runtime.fact_input
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid
		  AND dim_members->>$4::text = 'STAFF_A1'
	`, f.modelID, f.annualRevID, f.amountMetricID, f.staffDimID).Scan(&copiedCount, &copiedValue); err != nil {
		t.Fatalf("query annual facts: %v", err)
	}
	if copiedCount != 1 {
		t.Fatalf("expected exactly 1 copied fact for STAFF_A1 in the annual revision, got %d", copiedCount)
	}
	if copiedValue != 100 {
		t.Errorf("copied value = %v, want 100", copiedValue)
	}

	// STAFF_B1 belongs to DEPT_B, not DEPT_A — must not have been copied.
	var otherCount int
	if err := f.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM runtime.fact_input
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND dim_members->>$3::text = 'STAFF_B1'
	`, f.modelID, f.annualRevID, f.staffDimID).Scan(&otherCount); err != nil {
		t.Fatalf("query: %v", err)
	}
	if otherCount != 0 {
		t.Errorf("STAFF_B1 (a different department) should not have been copied, found %d rows", otherCount)
	}
}

func TestRevisionDuplicationRemapsRollupAndSourceDimension(t *testing.T) {
	f := setupRollupFixture(t)

	resp, body := f.do(t, "POST", fmt.Sprintf("/api/developer/revisions?model_id=%s", f.modelID), "rollup-test-approver", map[string]string{
		"name": "Copy", "source_revision_id": f.workingRevID,
	})
	if resp != http.StatusOK {
		t.Fatalf("duplicate revision status = %d, body = %v", resp, body)
	}
	newRevID, _ := body["id"].(string)
	if newRevID == "" {
		t.Fatal("expected a new revision id")
	}

	ctx := context.Background()
	var rollupSourceInNewRev, sourceDimInNewRev bool
	if err := f.pool.QueryRow(ctx, `
		SELECT
		  (SELECT revision_id FROM model.grid_def WHERE id = (SELECT rollup_source_grid_id FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Dept Rollup')) = $2::uuid,
		  (SELECT revision_id FROM model.dimension_def WHERE id = (SELECT source_dimension_id FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='regions')) = $2::uuid
	`, f.modelID, newRevID).Scan(&rollupSourceInNewRev, &sourceDimInNewRev); err != nil {
		t.Fatalf("query remapped refs: %v", err)
	}
	if !rollupSourceInNewRev {
		t.Error("duplicated 'Dept Rollup' grid's rollup_source_grid_id should point at the NEW revision's own 'Staff Grid', not the source revision's")
	}
	if !sourceDimInNewRev {
		t.Error("duplicated 'regions' dimension's source_dimension_id should point at the NEW revision's own 'staff' dimension, not the source revision's")
	}
}

// TestRevisionDuplicationCopiesFullModel seeds one of every revision-scoped
// entity kind that migration 056 added to the copy — dimension property,
// form (with dimension/metric field refs), form record, form-metric mapping,
// integration, dashboard folders (nested) with a dashboard inside, workflow
// def (dimension-bound context_schema) and automation rule — duplicates the
// revision, and asserts every copy exists in the new revision with its
// cross-entity references remapped to the new revision's own rows.
func TestRevisionDuplicationCopiesFullModel(t *testing.T) {
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

	// ── seed one of each entity in the working revision ─────────────────────
	exec(`INSERT INTO model.dimension_property (dimension_id, name, data_type) VALUES ($1::uuid, 'owner', 'text')`, f.deptsDimID)

	formID := q(`
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'expense', 'Expense', jsonb_build_array(
			jsonb_build_object('name','dept','label','Dept','type','dimension','dimension_id',$3::text),
			jsonb_build_object('name','amt','label','Amt','type','metric','metric_id',$4::text)
		)) RETURNING id::text`, f.modelID, f.workingRevID, f.deptsDimID, f.amountMetricID)

	exec(`INSERT INTO runtime.form_record (form_id, data, status, created_by) VALUES ($1::uuid, '{"amt": 5}', 'submitted', $2::uuid)`, formID, f.managerID)

	exec(`
		INSERT INTO model.form_metric_mapping (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id, dimension_mappings)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'post amt', 'amt', $5::uuid, jsonb_build_object($6::text, 'dept'))`,
		f.modelID, f.workingRevID, formID, f.gridStaffID, f.amountMetricID, f.deptsDimID)

	exec(`
		INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, target_id)
		VALUES ($1::uuid, $2::uuid, 'staff import', 'csv_import', 'grid', $3::uuid)`,
		f.modelID, f.workingRevID, f.gridStaffID)

	rootFolderID := q(`INSERT INTO model.dashboard_folder (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Root') RETURNING id::text`, f.modelID, f.workingRevID)
	exec(`INSERT INTO model.dashboard_folder (model_id, revision_id, name, parent_id) VALUES ($1::uuid, $2::uuid, 'Child', $3::uuid)`, f.modelID, f.workingRevID, rootFolderID)
	exec(`INSERT INTO model.dashboard_def (model_id, revision_id, name, folder_id) VALUES ($1::uuid, $2::uuid, 'Filed Dash', $3::uuid)`, f.modelID, f.workingRevID, rootFolderID)

	// The fixture's workflow def (context_schema binds "scope" to the
	// departments dimension) is revision-global by default; stamp it into
	// the working revision so the copy picks it up.
	exec(`UPDATE workflow.workflow_def SET revision_id=$1::uuid WHERE id=$2::uuid`, f.workingRevID, f.wfDefID)
	exec(`
		INSERT INTO workflow.automation_rule (application_id, revision_id, name, trigger_type, workflow_name, workflow_def_id, source_form_id)
		VALUES ($1::uuid, $2::uuid, 'route expenses', 'form_submit', 'Test Approval', $3::uuid, $4::uuid)`,
		f.appID, f.workingRevID, f.wfDefID, formID)

	// ── duplicate ────────────────────────────────────────────────────────────
	resp, body := f.do(t, "POST", fmt.Sprintf("/api/developer/revisions?model_id=%s", f.modelID), "rollup-test-approver", map[string]string{
		"name": "Full Copy", "source_revision_id": f.workingRevID,
	})
	if resp != http.StatusOK {
		t.Fatalf("duplicate revision status = %d, body = %v", resp, body)
	}
	newRevID, _ := body["id"].(string)
	if newRevID == "" {
		t.Fatal("expected a new revision id")
	}

	// New-revision entity IDs the remaps must point at.
	var newDeptsDimID, newMetricID, newGridID string
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='departments'`, f.modelID, newRevID).Scan(&newDeptsDimID)
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='amount'`, f.modelID, newRevID).Scan(&newMetricID)
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Staff Grid'`, f.modelID, newRevID).Scan(&newGridID)
	if newDeptsDimID == "" || newMetricID == "" || newGridID == "" {
		t.Fatalf("expected copied departments/amount/Staff Grid in the new revision (got %q/%q/%q)", newDeptsDimID, newMetricID, newGridID)
	}

	// 1. Dimension property copied onto the new departments dimension.
	var propCount int
	_ = f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.dimension_property WHERE dimension_id=$1::uuid AND name='owner'`, newDeptsDimID).Scan(&propCount)
	if propCount != 1 {
		t.Errorf("dimension property 'owner' copies on new departments dim = %d, want 1", propCount)
	}

	// 2. Form copied with its field refs remapped to the new revision.
	var newFormID, fieldDimID, fieldMetricID string
	if err := f.pool.QueryRow(ctx, `
		SELECT id::text, fields->0->>'dimension_id', fields->1->>'metric_id'
		FROM model.form_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='expense'
	`, f.modelID, newRevID).Scan(&newFormID, &fieldDimID, &fieldMetricID); err != nil {
		t.Fatalf("copied form not found: %v", err)
	}
	if fieldDimID != newDeptsDimID {
		t.Errorf("form field dimension_id = %s, want the NEW revision's departments dim %s", fieldDimID, newDeptsDimID)
	}
	if fieldMetricID != newMetricID {
		t.Errorf("form field metric_id = %s, want the NEW revision's amount metric %s", fieldMetricID, newMetricID)
	}

	// 3. Form record copied to the new form.
	var recCount int
	_ = f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.form_record WHERE form_id=$1::uuid`, newFormID).Scan(&recCount)
	if recCount != 1 {
		t.Errorf("records on copied form = %d, want 1", recCount)
	}

	// 4. Form-metric mapping remapped: form, grid, metric and the
	// dimension_mappings key must all point at new-revision rows.
	var mapGridID, mapMetricID, mapDimField string
	if err := f.pool.QueryRow(ctx, `
		SELECT COALESCE(grid_id::text,''), target_metric_id::text, COALESCE(dimension_mappings->>$3, '')
		FROM model.form_metric_mapping WHERE form_id=$1::uuid AND revision_id=$2::uuid
	`, newFormID, newRevID, newDeptsDimID).Scan(&mapGridID, &mapMetricID, &mapDimField); err != nil {
		t.Fatalf("copied mapping not found: %v", err)
	}
	if mapGridID != newGridID {
		t.Errorf("mapping grid_id = %s, want new Staff Grid %s", mapGridID, newGridID)
	}
	if mapMetricID != newMetricID {
		t.Errorf("mapping target_metric_id = %s, want new amount metric %s", mapMetricID, newMetricID)
	}
	if mapDimField != "dept" {
		t.Errorf("mapping dimension_mappings[%s] = %q, want \"dept\" (key remapped to new dim)", newDeptsDimID, mapDimField)
	}

	// 5. Integration target remapped to the new revision's grid.
	var intTargetID string
	if err := f.pool.QueryRow(ctx, `
		SELECT COALESCE(target_id::text,'') FROM model.integration_def
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='staff import'
	`, f.modelID, newRevID).Scan(&intTargetID); err != nil {
		t.Fatalf("copied integration not found: %v", err)
	}
	if intTargetID != newGridID {
		t.Errorf("integration target_id = %s, want new Staff Grid %s", intTargetID, newGridID)
	}

	// 6. Folders copied with hierarchy intact and the dashboard refiled.
	var newRootID, childParentID, dashFolderID string
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.dashboard_folder WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Root'`, f.modelID, newRevID).Scan(&newRootID)
	_ = f.pool.QueryRow(ctx, `SELECT COALESCE(parent_id::text,'') FROM model.dashboard_folder WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Child'`, f.modelID, newRevID).Scan(&childParentID)
	_ = f.pool.QueryRow(ctx, `SELECT COALESCE(folder_id::text,'') FROM model.dashboard_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Filed Dash'`, f.modelID, newRevID).Scan(&dashFolderID)
	if newRootID == "" || childParentID != newRootID {
		t.Errorf("copied Child folder parent = %q, want new Root folder %q", childParentID, newRootID)
	}
	if dashFolderID != newRootID {
		t.Errorf("copied dashboard folder_id = %q, want new Root folder %q", dashFolderID, newRootID)
	}

	// 7. Workflow def copied with context_schema's dimension binding remapped.
	var newWfID, schemaDimID string
	if err := f.pool.QueryRow(ctx, `
		SELECT id::text, COALESCE((
			SELECT e->>'dimension_id' FROM jsonb_array_elements(context_schema) e WHERE e->>'key'='scope'
		), '')
		FROM workflow.workflow_def WHERE application_id=$1::uuid AND revision_id=$2::uuid AND name='Test Approval'
	`, f.appID, newRevID).Scan(&newWfID, &schemaDimID); err != nil {
		t.Fatalf("copied workflow def not found: %v", err)
	}
	if schemaDimID != newDeptsDimID {
		t.Errorf("workflow context_schema scope dimension_id = %s, want new departments dim %s", schemaDimID, newDeptsDimID)
	}

	// 8. Automation rule copied with workflow and form refs remapped.
	var ruleWfID, ruleFormID string
	if err := f.pool.QueryRow(ctx, `
		SELECT COALESCE(workflow_def_id::text,''), COALESCE(source_form_id::text,'')
		FROM workflow.automation_rule WHERE application_id=$1::uuid AND revision_id=$2::uuid AND name='route expenses'
	`, f.appID, newRevID).Scan(&ruleWfID, &ruleFormID); err != nil {
		t.Fatalf("copied automation rule not found: %v", err)
	}
	if ruleWfID != newWfID {
		t.Errorf("rule workflow_def_id = %s, want copied workflow def %s", ruleWfID, newWfID)
	}
	if ruleFormID != newFormID {
		t.Errorf("rule source_form_id = %s, want copied form %s", ruleFormID, newFormID)
	}

	// The source revision keeps its own rows untouched (isolation in the
	// other direction): still exactly one 'expense' form there, pointing at
	// the ORIGINAL dimension.
	var srcFieldDimID string
	_ = f.pool.QueryRow(ctx, `
		SELECT fields->0->>'dimension_id' FROM model.form_def
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='expense'
	`, f.modelID, f.workingRevID).Scan(&srcFieldDimID)
	if srcFieldDimID != f.deptsDimID {
		t.Errorf("source form field dimension_id = %s, want original %s (source revision must be untouched)", srcFieldDimID, f.deptsDimID)
	}
}

// failOnNthExecTx wraps a real pgx.Tx and fails the Nth call to Exec (1-indexed)
// with a synthetic error, passing every other call and every other method
// straight through to the embedded real Tx. There is no real schema
// constraint duplicateRevision's copy steps can violate deterministically —
// every unique constraint on the tables it touches is scoped by the new
// revision's own server-generated UUID, unknowable before the request runs —
// so this is the only way to exercise a genuine mid-copy failure.
type failOnNthExecTx struct {
	pgx.Tx
	n     int
	calls int
}

func (f *failOnNthExecTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.calls++
	if f.calls == f.n {
		return pgconn.CommandTag{}, fmt.Errorf("synthetic failure injected at Exec call #%d", f.n)
	}
	return f.Tx.Exec(ctx, sql, args...)
}

// TestRevisionDuplicationRollsBackOnMidCopyFailure proves the atomicity fix:
// before it, only Step A's failure aborted the request and every later step
// (A2 through I) was best-effort, so a failure there still left the new
// revision row — and everything Steps A-through-that-point had already
// copied — behind as an orphan. Forcing a failure at the FIRST copy step
// (Step A, the 1st Exec call) and, separately, at the LAST copy step (Step I,
// the dashboard-widget ref_id remap added by the "eliminate real gaps" pass —
// the 12th Exec call) and asserting the new revision row itself is gone
// either way proves the whole operation is now one transaction, not twelve
// independent ones.
func TestRevisionDuplicationRollsBackOnMidCopyFailure(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	h := &handler{db: tenantdb.NewHandle(f.pool, nil), log: logger.New("test")}

	for _, tc := range []struct {
		name   string
		failAt int
	}{
		{"first step (Step A)", 1},
		{"last step (Step I)", 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck

			failingTx := &failOnNthExecTx{Tx: tx, n: tc.failAt}
			newID, err := h.duplicateRevision(ctx, failingTx, f.modelID, "Copy-"+tc.name, f.workingRevID, &f.workingRevID)
			if err == nil {
				t.Fatalf("expected duplicateRevision to fail at Exec call #%d, got newID=%s, nil error", tc.failAt, newID)
			}
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				t.Fatalf("rollback: %v", rbErr)
			}

			// Nothing from any step should be visible — check via a fresh
			// query against the shared pool (rolled-back tx changes are
			// invisible to any connection either way, but this also matches
			// how a real caller would check afterward).
			if newID != "" {
				var revisionExists bool
				if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model.revision WHERE id=$1::uuid)`, newID).Scan(&revisionExists); err != nil {
					t.Fatalf("check revision existence: %v", err)
				}
				if revisionExists {
					t.Errorf("revision row for newID=%s survived a rolled-back mid-copy failure — the old orphan-revision bug is back", newID)
				}
				var metricCount, dimCount int
				_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM model.metric_def WHERE revision_id=$1::uuid`, newID).Scan(&metricCount)
				_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM model.dimension_def WHERE revision_id=$1::uuid`, newID).Scan(&dimCount)
				if metricCount != 0 || dimCount != 0 {
					t.Errorf("partial copy survived rollback: metric_def rows=%d, dimension_def rows=%d for newID=%s", metricCount, dimCount, newID)
				}
			}
		})
	}
}

func TestImportRespectsWorkflowLockAndSystemManaged(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	// Submit and approve DEPT_A — permanently locks it. Uses "approver"
	// (developer + business_admin) throughout: /api/import/upload is
	// developer-gated, and the lock check that matters here is on the
	// *target* dimension member, not the caller's own RACI ownership.
	_, body := f.do(t, "POST", "/api/workflow/instances", "rollup-test-manager", map[string]any{
		"workflow_def_id": f.wfDefID,
		"context":         map[string]string{"model_id": f.modelID, "revision_id": f.workingRevID, "target_revision_id": f.annualRevID},
	})
	instanceID := body["instance_id"].(string)
	var stepID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM workflow.workflow_step WHERE instance_id=$1::uuid`, instanceID).Scan(&stepID); err != nil {
		t.Fatalf("find step: %v", err)
	}
	if resp, respBody := f.do(t, "POST", fmt.Sprintf("/api/tasks/%s/complete", stepID), "rollup-test-approver", map[string]string{"decision": "approve", "comment": "approved"}); resp != http.StatusOK {
		t.Fatalf("approve status = %d, body = %v", resp, respBody)
	}

	// Importing into the now-locked DEPT_A (via STAFF_A1) is rejected —
	// this endpoint used to call importpkg.CommitImport directly, bypassing
	// cells()'s write checks entirely.
	csvLocked := fmt.Sprintf("metric_id,staff,value\n%s,STAFF_A1,999\n", f.amountMetricID)
	resp, body2 := f.do(t, "POST", "/api/import/upload", "rollup-test-approver", map[string]string{"csv": csvLocked, "revision_id": f.workingRevID})
	if resp != http.StatusForbidden {
		t.Errorf("import into a locked (approved) scope status = %d, want 403, body = %v", resp, body2)
	}

	// A sibling department, still open, is unaffected.
	csvOpen := fmt.Sprintf("metric_id,staff,value\n%s,STAFF_B1,555\n", f.amountMetricID)
	resp2, body3 := f.do(t, "POST", "/api/import/upload", "rollup-test-approver", map[string]string{"csv": csvOpen, "revision_id": f.workingRevID})
	if resp2 != http.StatusOK {
		t.Fatalf("import into an open scope status = %d, want 200, body = %v", resp2, body3)
	}

	// system_managed revision is rejected outright, lock state aside.
	csvAnnual := fmt.Sprintf("metric_id,staff,value\n%s,STAFF_B1,1\n", f.amountMetricID)
	resp3, _ := f.do(t, "POST", "/api/import/upload", "rollup-test-approver", map[string]string{"csv": csvAnnual, "revision_id": f.annualRevID})
	if resp3 != http.StatusForbidden {
		t.Errorf("import into a system_managed revision status = %d, want 403", resp3)
	}

	// The system_managed guard must win even when the row itself can't be
	// staged (e.g. a metric identifier the client failed to resolve to a
	// UUID) — the guard has to run before store.StageRows, not after, or a
	// locked/system_managed target leaks a raw staging error (500) instead
	// of the intended 403. Regression for a real bug found while exercising
	// the actual Import Wizard: it resolves metric labels client-side via
	// metric_def rows scoped to the *target* revision, which a
	// system_managed revision always has none of, so the label is sent
	// through unresolved.
	csvUnresolved := "metric_id,staff,value\nnot-a-uuid,STAFF_B1,1\n"
	resp4, body4 := f.do(t, "POST", "/api/import/upload", "rollup-test-approver", map[string]string{"csv": csvUnresolved, "revision_id": f.annualRevID})
	if resp4 != http.StatusForbidden {
		t.Errorf("import of an unresolved metric into a system_managed revision status = %d, want 403, body = %v", resp4, body4)
	}
}

// TestGridScopesCellsTotalsAndAllDimensionsByHiddenMembers is the core
// regression for the security gap BUILD_INSTRUCTIONS.md flagged: a caller
// with hidden dimension_member access rules (identity.user_access_rule)
// used to still receive every other caller's facts in grid()'s cells,
// totals, and all_dimensions — only the primary `dimensions` member list was
// filtered. manager (DEPT_A-scoped, STAFF_A1=100+STAFF_A2=50 visible,
// STAFF_B1=200 hidden) must never observe STAFF_B1 or its value through any
// of the four representations grid() returns, on either the direct grid
// (Staff Grid) or the rollup grid (Dept Rollup) that mirrors it — while the
// unrestricted approver persona still sees the full company-wide picture.
func TestGridScopesCellsTotalsAndAllDimensionsByHiddenMembers(t *testing.T) {
	f := setupRollupFixture(t)

	// ── direct grid (Staff Grid): dimensions, cells, totals ──────────────────
	_, body := f.do(t, "GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.workingRevID, f.gridStaffID), "rollup-test-manager", nil)

	staffMembers := body["dimensions"].([]any)[0].(map[string]any)["members"].([]any)
	var codes []string
	for _, m := range staffMembers {
		codes = append(codes, m.(map[string]any)["code"].(string))
	}
	if len(codes) != 2 || contains(codes, "STAFF_B1") {
		t.Errorf("manager's staff members = %v, want exactly [STAFF_A1 STAFF_A2] (STAFF_B1 hidden)", codes)
	}

	cells := body["cells"].(map[string]any)
	if _, leaked := cells[f.amountMetricID+":STAFF_B1"]; leaked {
		t.Error("manager's cells leaked STAFF_B1 — a hidden-member fact reached the response")
	}
	if v := cells[f.amountMetricID+":STAFF_A1"]; v != 100.0 {
		t.Errorf("manager's STAFF_A1 cell = %v, want 100", v)
	}

	totals := body["totals"].(map[string]any)
	if got := totals[f.amountMetricID]; got != 150.0 {
		t.Errorf("manager's Staff Grid total = %v, want 150 (100+50, STAFF_B1's 200 excluded)", got)
	}

	// ── rollup grid (Dept Rollup): all_dimensions must be scoped too — this
	// is what the client's cross-dimension aggregation (resolveCrossDimensionValue
	// in BusinessConsole.tsx) reads to know which staff belong to which dept,
	// so a leak here silently reintroduces STAFF_B1 into a manager's rollup
	// total even though the direct grid correctly hides it.
	_, deptBody := f.do(t, "GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.workingRevID, f.gridDeptsID), "rollup-test-manager", nil)
	allDims := deptBody["all_dimensions"].([]any)
	var allDimsStaffCodes []string
	for _, d := range allDims {
		dm := d.(map[string]any)
		if dm["id"] != f.staffDimID {
			continue
		}
		for _, m := range dm["members"].([]any) {
			allDimsStaffCodes = append(allDimsStaffCodes, m.(map[string]any)["code"].(string))
		}
	}
	if contains(allDimsStaffCodes, "STAFF_B1") {
		t.Errorf("manager's Dept Rollup all_dimensions[staff] = %v, must not contain hidden STAFF_B1", allDimsStaffCodes)
	}
	deptCells := deptBody["cells"].(map[string]any)
	if _, leaked := deptCells[f.amountMetricID+":STAFF_B1"]; leaked {
		t.Error("manager's Dept Rollup cells leaked STAFF_B1")
	}

	// ── the unrestricted approver still sees everything (fast path unaffected) ──
	_, approverBody := f.do(t, "GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.workingRevID, f.gridStaffID), "rollup-test-approver", nil)
	approverTotals := approverBody["totals"].(map[string]any)
	if got := approverTotals[f.amountMetricID]; got != 350.0 {
		t.Errorf("approver's (unrestricted) Staff Grid total = %v, want 350 (100+50+200, nothing hidden)", got)
	}

	// ── managerB (DEPT_B-scoped) is symmetric: sees only STAFF_B1 ────────────
	_, bBody := f.do(t, "GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.workingRevID, f.gridStaffID), "rollup-test-manager-b", nil)
	bTotals := bBody["totals"].(map[string]any)
	if got := bTotals[f.amountMetricID]; got != 200.0 {
		t.Errorf("managerB's Staff Grid total = %v, want 200 (STAFF_B1 only)", got)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestImportXLSXNativeParsingNameBasedMapping proves the generic import
// pipeline (importpkg.ParseXLSXRows -> ResolveRows) parses a REAL .xlsx
// workbook server-side — not a client-side CSV transcode — and maps columns
// to metric/dimension IDs by NAME ("staff", "amount"), never a pasted UUID.
// It also exercises the leaf-only-member rule using the fixture's one
// same-dimension hierarchy parent (REGION_GROUP, parent of WEST): importing
// against a non-leaf member must be rejected, a sibling leaf must succeed.
func TestImportXLSXNativeParsingNameBasedMapping(t *testing.T) {
	f := setupRollupFixture(t)

	buildWorkbook := func(rows [][]string) string {
		t.Helper()
		wb := excelize.NewFile()
		defer wb.Close() //nolint:errcheck
		sheet := wb.GetSheetName(0)
		for i, row := range rows {
			for j, cell := range row {
				ref, _ := excelize.CoordinatesToCellName(j+1, i+1)
				_ = wb.SetCellValue(sheet, ref, cell)
			}
		}
		buf, err := wb.WriteToBuffer()
		if err != nil {
			t.Fatalf("write workbook: %v", err)
		}
		return base64.StdEncoding.EncodeToString(buf.Bytes())
	}

	// Name-based columns: "staff" (dimension) + "amount" (metric) — no UUIDs
	// anywhere in the file, satisfying the native-import acceptance
	// criterion that a spreadsheet author never pastes an internal ID.
	xlsxB64 := buildWorkbook([][]string{
		{"staff", "amount"},
		{"STAFF_A1", "777"},
	})
	status, body := f.do(t, "POST", "/api/import/upload", "rollup-test-approver", map[string]any{
		"xlsx_base64": xlsxB64, "revision_id": f.workingRevID, "import_mode": "replace",
	})
	if status != http.StatusOK {
		t.Fatalf("xlsx import status = %d, body = %v", status, body)
	}
	if body["valid_rows"] != 1.0 {
		t.Errorf("valid_rows = %v, want 1", body["valid_rows"])
	}

	var committed float64
	if err := f.pool.QueryRow(context.Background(), `
		SELECT value FROM runtime.fact_input
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid AND dim_members->>$4::text='STAFF_A1'
		ORDER BY entered_at DESC LIMIT 1
	`, f.modelID, f.workingRevID, f.amountMetricID, f.staffDimID).Scan(&committed); err != nil {
		t.Fatalf("query committed value: %v", err)
	}
	if committed != 777 {
		t.Errorf("committed STAFF_A1 value = %v, want 777 (from the native xlsx upload)", committed)
	}

	// Leaf check: "region" column values must be leaf members. WEST (now a
	// leaf, parented under REGION_GROUP) succeeds; REGION_GROUP itself (has
	// a child) is rejected.
	leafOK := buildWorkbook([][]string{
		{"staff", "region", "amount"},
		{"STAFF_A1", "WEST", "111"},
	})
	status2, body2 := f.do(t, "POST", "/api/import/upload", "rollup-test-approver", map[string]any{
		"xlsx_base64": leafOK, "revision_id": f.workingRevID,
	})
	// "region" isn't a dimension on this model (only "regions", plural, and
	// staff/depts/months) — expect an unknown-column rejection, proving
	// column classification is genuinely name-based rather than guessing;
	// swap in the real dimension name and confirm leaf enforcement instead.
	if status2 != http.StatusBadRequest {
		t.Errorf("import with an unrecognized column name status = %d, want 400, body = %v", status2, body2)
	}

	nonLeaf := buildWorkbook([][]string{
		{"staff", "regions", "amount"},
		{"STAFF_A2", "REGION_GROUP", "222"},
	})
	status3, body3 := f.do(t, "POST", "/api/import/upload", "rollup-test-approver", map[string]any{
		"xlsx_base64": nonLeaf, "revision_id": f.workingRevID,
	})
	if status3 != http.StatusUnprocessableEntity {
		t.Fatalf("import against a non-leaf member status = %d, want 422, body = %v", status3, body3)
	}
	errs, _ := body3["errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["code"] != "NOT_LEAF" {
		t.Errorf("errors = %v, want one NOT_LEAF error", errs)
	}

	leafPass := buildWorkbook([][]string{
		{"staff", "regions", "amount"},
		{"STAFF_A2", "WEST", "333"},
	})
	status4, body4 := f.do(t, "POST", "/api/import/upload", "rollup-test-approver", map[string]any{
		"xlsx_base64": leafPass, "revision_id": f.workingRevID, "import_mode": "replace",
	})
	if status4 != http.StatusOK {
		t.Errorf("import against a leaf member (WEST) status = %d, want 200, body = %v", status4, body4)
	}
}

// TestImportRejectsNegativeValueAtomically proves the whole-file atomic
// rejection: a workbook with one bad row (negative value) must commit
// NOTHING, not the other valid rows in the same file.
func TestImportRejectsNegativeValueAtomically(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	var beforeCount int
	_ = f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id=$2::uuid`, f.modelID, f.workingRevID).Scan(&beforeCount)

	csv := fmt.Sprintf("metric_id,staff,value\n%s,STAFF_A1,999\n%s,STAFF_A2,-5\n", f.amountMetricID, f.amountMetricID)
	status, body := f.do(t, "POST", "/api/import/upload", "rollup-test-approver", map[string]string{"csv": csv, "revision_id": f.workingRevID})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("import with one negative-value row status = %d, want 422, body = %v", status, body)
	}
	errs, _ := body["errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["code"] != "NEGATIVE_VALUE" {
		t.Errorf("errors = %v, want one NEGATIVE_VALUE error", errs)
	}

	var afterCount int
	_ = f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id=$2::uuid`, f.modelID, f.workingRevID).Scan(&afterCount)
	if afterCount != beforeCount {
		t.Errorf("fact_input row count changed %d -> %d; the valid STAFF_A1=999 row must NOT have been committed alongside the rejected one", beforeCount, afterCount)
	}
}

// TestWorkflowTwoScopesIndependentInstancesAndApproveRequiresComment covers
// two of the spec's scenarios together since they share one submit/approve
// flow: (a) DEPT_A and DEPT_B can have concurrently pending instances with
// independent decisions — rejecting one must not affect the other — and (b)
// approve, like reject, requires a comment (symmetry with the existing
// reject-without-comment test).
func TestWorkflowTwoScopesIndependentInstancesAndApproveRequiresComment(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	submit := func(persona, targetRev string) (instanceID, stepID string) {
		t.Helper()
		_, body := f.do(t, "POST", "/api/workflow/instances", persona, map[string]any{
			"workflow_def_id": f.wfDefID,
			"context":         map[string]string{"model_id": f.modelID, "revision_id": f.workingRevID, "target_revision_id": targetRev},
		})
		instanceID, _ = body["instance_id"].(string)
		if instanceID == "" {
			t.Fatalf("submit as %s: expected an instance_id, got %v", persona, body)
		}
		if err := f.pool.QueryRow(ctx, `SELECT id::text FROM workflow.workflow_step WHERE instance_id=$1::uuid`, instanceID).Scan(&stepID); err != nil {
			t.Fatalf("find step for %s: %v", persona, err)
		}
		return
	}

	_, stepA := submit("rollup-test-manager", f.annualRevID)
	_, stepB := submit("rollup-test-manager-b", f.annualRevID)
	if stepA == stepB {
		t.Fatal("DEPT_A and DEPT_B submissions produced the same step id — fixture setup is wrong")
	}

	// Approve without a comment is rejected — same rule as reject, tested
	// for the opposite decision this time.
	if status, _ := f.do(t, "POST", fmt.Sprintf("/api/tasks/%s/complete", stepA), "rollup-test-approver", map[string]string{"decision": "approve", "comment": ""}); status != http.StatusBadRequest {
		t.Errorf("approve without comment status = %d, want 400", status)
	}

	// Reject DEPT_A (with a comment) — must unlock DEPT_A's Working cells
	// without touching DEPT_B's still-pending instance.
	if status, body := f.do(t, "POST", fmt.Sprintf("/api/tasks/%s/complete", stepA), "rollup-test-approver", map[string]string{"decision": "reject", "comment": "needs revision"}); status != http.StatusOK {
		t.Fatalf("reject DEPT_A status = %d, body = %v", status, body)
	}
	writeA := map[string]any{"model_id": f.modelID, "revision_id": f.workingRevID, "metric_id": f.amountMetricID, "dim_codes": map[string]string{f.staffDimID: "STAFF_A1"}, "value": 150}
	if status, body := f.do(t, "POST", "/api/cells", "rollup-test-manager", writeA); status != http.StatusOK {
		t.Errorf("write to DEPT_A after its own rejection status = %d, want 200 (unlocked), body = %v", status, body)
	}
	writeB := map[string]any{"model_id": f.modelID, "revision_id": f.workingRevID, "metric_id": f.amountMetricID, "dim_codes": map[string]string{f.staffDimID: "STAFF_B1"}, "value": 250}
	if status, _ := f.do(t, "POST", "/api/cells", "rollup-test-manager-b", writeB); status != http.StatusForbidden {
		t.Errorf("write to DEPT_B while its instance is still running status = %d, want 403 (unaffected by DEPT_A's reject)", status)
	}

	// Approve DEPT_B (with a comment) — must copy only DEPT_B's facts to
	// Annual; DEPT_A (rejected, still open in Working) must not appear.
	if status, body := f.do(t, "POST", fmt.Sprintf("/api/tasks/%s/complete", stepB), "rollup-test-approver", map[string]string{"decision": "approve", "comment": "approved"}); status != http.StatusOK {
		t.Fatalf("approve DEPT_B status = %d, body = %v", status, body)
	}
	var annualB, annualA int
	_ = f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id=$2::uuid AND dim_members->>$3::text='STAFF_B1'`, f.modelID, f.annualRevID, f.staffDimID).Scan(&annualB)
	_ = f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id=$2::uuid AND dim_members->>$3::text='STAFF_A1'`, f.modelID, f.annualRevID, f.staffDimID).Scan(&annualA)
	if annualB != 1 {
		t.Errorf("Annual STAFF_B1 fact count = %d, want 1 (DEPT_B approved)", annualB)
	}
	if annualA != 0 {
		t.Errorf("Annual STAFF_A1 fact count = %d, want 0 (DEPT_A was rejected, never approved)", annualA)
	}
}

// TestCellsRejectsDirectWriteToHiddenMember covers the write-side half of
// the hidden-member scoping BUILD_INSTRUCTIONS.md requires ("reject a
// request that includes an employee outside the caller's cost center even
// if the caller constructs the request manually") — independent of any
// workflow lock (no instance is submitted here at all): a manager whose
// identity.user_access_rule hides a member must be rejected by cells() even
// with no running/approved workflow instance in the picture, and remains
// free to write their own visible members.
func TestCellsRejectsDirectWriteToHiddenMember(t *testing.T) {
	f := setupRollupFixture(t)

	hiddenWrite := map[string]any{
		"model_id": f.modelID, "revision_id": f.workingRevID, "metric_id": f.amountMetricID,
		"dim_codes": map[string]string{f.staffDimID: "STAFF_B1"}, "value": 1,
	}
	if status, body := f.do(t, "POST", "/api/cells", "rollup-test-manager", hiddenWrite); status != http.StatusForbidden {
		t.Errorf("manager writing to hidden STAFF_B1 (no workflow involved) status = %d, want 403, body = %v", status, body)
	}

	ownWrite := map[string]any{
		"model_id": f.modelID, "revision_id": f.workingRevID, "metric_id": f.amountMetricID,
		"dim_codes": map[string]string{f.staffDimID: "STAFF_A1"}, "value": 123,
	}
	if status, body := f.do(t, "POST", "/api/cells", "rollup-test-manager", ownWrite); status != http.StatusOK {
		t.Errorf("manager writing to their own visible STAFF_A1 status = %d, want 200, body = %v", status, body)
	}
}

// TestCellsRejectsCascadeHiddenWrite is a regression test for the write-side
// half of the hidden-member cascade: a user here has ONLY a department-level
// hidden rule — no staff-level rule at all, unlike setupRollupFixture's
// managers (which, before the cascade existed, needed one) — so a write to
// a staff member under that hidden department must be rejected purely by
// writeguard.CheckWrite walking AncestorChain, not by any direct rule on the
// staff member itself. TestCellsRejectsDirectWriteToHiddenMember above only
// covers a member with its own direct rule; this covers the gap it doesn't.
func TestCellsRejectsCascadeHiddenWrite(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	var custID, wsID string
	if err := f.pool.QueryRow(ctx, `SELECT customer_id::text, workspace_id::text FROM core.application WHERE id=$1::uuid`, f.appID).Scan(&custID, &wsID); err != nil {
		t.Fatalf("resolve customer/workspace: %v", err)
	}
	var userID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-cascade-only', 'cascade-only@t.com', 'Cascade Only', $1::uuid) RETURNING id::text`,
		custID,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, userID, wsID); err != nil {
		t.Fatalf("grant business_user: %v", err)
	}
	// Department-level rule only — deliberately no rule at all on any staff member.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'hidden')`,
		userID, f.deptBID,
	); err != nil {
		t.Fatalf("hide dept B: %v", err)
	}
	devPersonas["rollup-test-cascade-only"] = "test-cascade-only"
	t.Cleanup(func() { delete(devPersonas, "rollup-test-cascade-only") })

	cascadeWrite := map[string]any{
		"model_id": f.modelID, "revision_id": f.workingRevID, "metric_id": f.amountMetricID,
		"dim_codes": map[string]string{f.staffDimID: "STAFF_B1"}, "value": 1,
	}
	if status, body := f.do(t, "POST", "/api/cells", "rollup-test-cascade-only", cascadeWrite); status != http.StatusForbidden {
		t.Errorf("writing to STAFF_B1 under dept-only-hidden DEPT_B status = %d, want 403 (no direct rule on STAFF_B1 itself — must cascade from DEPT_B), body = %v", status, body)
	}

	visibleWrite := map[string]any{
		"model_id": f.modelID, "revision_id": f.workingRevID, "metric_id": f.amountMetricID,
		"dim_codes": map[string]string{f.staffDimID: "STAFF_A1"}, "value": 42,
	}
	if status, body := f.do(t, "POST", "/api/cells", "rollup-test-cascade-only", visibleWrite); status != http.StatusOK {
		t.Errorf("writing to STAFF_A1 (DEPT_A, not hidden) status = %d, want 200, body = %v", status, body)
	}
}

// ── grid()/BusinessConsole calc-value convergence ────────────────────────────
// The tests below cover grid()'s new calc-metric cells/totals — merging
// runtime.calc_result values into the same `cells`/`totals` maps fact values
// already populate, instead of BusinessConsole.tsx deriving every calc value
// client-side. See the fixture's dept_total/dept_total_capped/region_total
// metrics and their synthetic calc_result rows above for the numbers
// asserted here.

// TestGridCalcMetricCellsCrossDimensionAndCalcOnCalc covers the unscoped
// (fast) path: dept_total (declared at [departments], formula "=amount",
// where amount itself lives at the finer [staff] grain) proves a calc
// metric's cells correctly surface a cross-dimension-grain calc_result row
// verbatim; dept_total_capped (depends on dept_total) proves a calc-metric-
// depends-on-calc-metric chain surfaces correctly through the same response.
func TestGridCalcMetricCellsCrossDimensionAndCalcOnCalc(t *testing.T) {
	f := setupRollupFixture(t)

	_, body := f.do(t, "GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.workingRevID, f.gridDeptTotalID), "rollup-test-approver", nil)
	cells := body["cells"].(map[string]any)
	totals := body["totals"].(map[string]any)

	if v := cells[f.deptTotalMetricID+":DEPT_A"]; v != 150.0 {
		t.Errorf("dept_total[DEPT_A] = %v, want 150 (100+50, rolled up from the finer staff grain)", v)
	}
	if v := cells[f.deptTotalMetricID+":DEPT_B"]; v != 200.0 {
		t.Errorf("dept_total[DEPT_B] = %v, want 200", v)
	}
	if v := totals[f.deptTotalMetricID]; v != 350.0 {
		t.Errorf("dept_total total = %v, want 350", v)
	}
	if v := cells[f.deptTotalCappedMetricID+":DEPT_A"]; v != 120.0 {
		t.Errorf("dept_total_capped[DEPT_A] (calc-on-calc) = %v, want 120 (150 capped at 120)", v)
	}
	if v := cells[f.deptTotalCappedMetricID+":DEPT_B"]; v != 120.0 {
		t.Errorf("dept_total_capped[DEPT_B] (calc-on-calc) = %v, want 120 (200 capped at 120)", v)
	}
	if v := totals[f.deptTotalCappedMetricID]; v != 240.0 {
		t.Errorf("dept_total_capped total = %v, want 240", v)
	}
}

// TestGridCalcMetricCellsExcludeNonLeafSameDimensionGroup covers a calc
// metric declared at a dimension WITH a same-dimension hierarchy
// (region_total at [regions], where REGION_GROUP is a non-leaf parent of
// WEST): cells must have entries for the leaves (WEST, EAST) only, never a
// group-code entry — matching how input cells already behave, and matching
// what the calculation scheduler itself ever writes (leaf combos + the '{}'
// aggregate, never an intermediate group combo). Group-level rollup for
// display stays a client-side concern.
func TestGridCalcMetricCellsExcludeNonLeafSameDimensionGroup(t *testing.T) {
	f := setupRollupFixture(t)

	_, body := f.do(t, "GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.workingRevID, f.gridRegionTotalID), "rollup-test-approver", nil)
	cells := body["cells"].(map[string]any)
	totals := body["totals"].(map[string]any)

	if v := cells[f.regionTotalMetricID+":WEST"]; v != 300.0 {
		t.Errorf("region_total[WEST] = %v, want 300 (STAFF_A1+STAFF_B1, related via the region property)", v)
	}
	if v := cells[f.regionTotalMetricID+":EAST"]; v != 50.0 {
		t.Errorf("region_total[EAST] = %v, want 50", v)
	}
	if _, present := cells[f.regionTotalMetricID+":REGION_GROUP"]; present {
		t.Error("region_total must not have a cells entry for REGION_GROUP (a non-leaf, same-dimension group member)")
	}
	if v := totals[f.regionTotalMetricID]; v != 350.0 {
		t.Errorf("region_total total = %v, want 350", v)
	}
}

// setupStaffLevelHiddenPersona registers a new persona whose ONLY hidden
// access rule is on one specific staff member directly — no department-level
// rule at all, unlike the fixture's own manager/managerB (who each hide a
// whole department). This is the exact shape the hidden-member-leak finding
// needs: a calc metric declared at a COARSER grain (departments) than the
// hidden dependency (staff) has dim_members that never touch the hidden
// staff member at all, so a naive per-row filter of calc_result itself could
// never catch the leak — only recomputing from already-scoped inputs can.
func (f *rollupFixture) setupStaffLevelHiddenPersona(t *testing.T, hiddenStaffMemberID string) string {
	t.Helper()
	ctx := context.Background()
	var custID, wsID string
	if err := f.pool.QueryRow(ctx, `SELECT customer_id::text, workspace_id::text FROM core.application WHERE id=$1::uuid`, f.appID).Scan(&custID, &wsID); err != nil {
		t.Fatalf("resolve customer/workspace: %v", err)
	}
	var userID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-staff-hidden', 'staff-hidden@t.com', 'Staff Hidden', $1::uuid) RETURNING id::text`,
		custID,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, userID, wsID); err != nil {
		t.Fatalf("grant business_user: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'hidden')`,
		userID, hiddenStaffMemberID,
	); err != nil {
		t.Fatalf("hide staff member: %v", err)
	}
	devPersonas["rollup-test-staff-hidden"] = "test-staff-hidden"
	t.Cleanup(func() { delete(devPersonas, "rollup-test-staff-hidden") })
	return "rollup-test-staff-hidden"
}

// TestGridCalcMetricScopedCellsDoNotLeakHiddenFinerGrainDependency is the
// important regression: a caller with STAFF_A2 hidden (not DEPT_A itself)
// must see dept_total[DEPT_A]=100 (STAFF_A1 only), never the unscoped 150 —
// proving scopeCalcCells recomputes from already-scoped inputs rather than
// trusting calc_result's own (access-unaware) precomputed row for DEPT_A,
// whose dim_members never mentions STAFF_A2 at all. DEPT_B (unaffected by
// this rule) must still show its normal, unscoped 200.
func TestGridCalcMetricScopedCellsDoNotLeakHiddenFinerGrainDependency(t *testing.T) {
	f := setupRollupFixture(t)
	persona := f.setupStaffLevelHiddenPersona(t, f.staffA2ID)

	_, body := f.do(t, "GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.workingRevID, f.gridDeptTotalID), persona, nil)
	cells := body["cells"].(map[string]any)
	totals := body["totals"].(map[string]any)

	if v := cells[f.deptTotalMetricID+":DEPT_A"]; v != 100.0 {
		t.Errorf("dept_total[DEPT_A] with STAFF_A2 hidden = %v, want 100 (STAFF_A1 only) — a value of 150 would mean the hidden staff member's contribution leaked through calc_result's own unscoped row", v)
	}
	if v := cells[f.deptTotalMetricID+":DEPT_B"]; v != 200.0 {
		t.Errorf("dept_total[DEPT_B] (unaffected by the STAFF_A2 rule) = %v, want 200", v)
	}
	if v := totals[f.deptTotalMetricID]; v != 300.0 {
		t.Errorf("dept_total total with STAFF_A2 hidden = %v, want 300 (100+200)", v)
	}
}

// TestGridCalcMetricScopedTotalMatchesPerComboAggregationForNonLinearFormula
// proves the scoped total for a non-linear formula (dept_total_capped =
// IF(dept_total>120,120,dept_total)) matches "evaluate per combo, then
// combine" — the same model runtime.calc_result's own unscoped aggregate
// already uses — not "evaluate once against a scalar total", which the old
// scopeCalcTotals did and would give a different, wrong answer here: with
// STAFF_A2 hidden, the scalar dept_total total is 100+200=300 (still
// >120), so a once-against-scalar evaluation would yield capped=120 for the
// WHOLE total; the correct per-combo total is 100 (DEPT_A, since 100<=120,
// not capped) + 120 (DEPT_B, capped) = 220.
func TestGridCalcMetricScopedTotalMatchesPerComboAggregationForNonLinearFormula(t *testing.T) {
	f := setupRollupFixture(t)
	persona := f.setupStaffLevelHiddenPersona(t, f.staffA2ID)

	_, body := f.do(t, "GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.workingRevID, f.gridDeptTotalID), persona, nil)
	cells := body["cells"].(map[string]any)
	totals := body["totals"].(map[string]any)

	if v := cells[f.deptTotalCappedMetricID+":DEPT_A"]; v != 100.0 {
		t.Errorf("dept_total_capped[DEPT_A] with STAFF_A2 hidden = %v, want 100 (dept_total=100, not capped since 100<=120)", v)
	}
	if v := cells[f.deptTotalCappedMetricID+":DEPT_B"]; v != 120.0 {
		t.Errorf("dept_total_capped[DEPT_B] = %v, want 120 (dept_total=200, capped)", v)
	}
	if v := totals[f.deptTotalCappedMetricID]; v != 220.0 {
		t.Errorf("dept_total_capped scoped total = %v, want 220 (100+120, combined per-combo — NOT 120, which is what evaluating the formula once against the scalar 300 total would wrongly give)", v)
	}
}

// TestGridCalcMetricScopedCellsSurviveOnePerComboFailure is a regression
// test for a real bug found via manual verification against live seed data:
// budget_variance_pct (dividing by budget_target, only entered for some
// months) came back completely empty — every combo, not just the ones
// missing data — because the first implementation aborted a calc metric's
// ENTIRE resolution the moment any single combo's formula errored. quota is
// entered for DEPT_A only, so dept_ratio = dept_total/quota is a genuine
// #DIV/0! for DEPT_B specifically: DEPT_A's cell must still be present and
// correct, DEPT_B's must be absent (not zero, not present-with-garbage —
// simply missing, the same way a metric with literally no data at all would
// look), and DEPT_A's OTHER metrics on the same grid (dept_total,
// dept_total_capped) must be completely unaffected by DEPT_B's failure.
func TestGridCalcMetricScopedCellsSurviveOnePerComboFailure(t *testing.T) {
	f := setupRollupFixture(t)
	persona := f.setupStaffLevelHiddenPersona(t, f.staffA2ID)

	_, body := f.do(t, "GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.workingRevID, f.gridDeptTotalID), persona, nil)
	cells := body["cells"].(map[string]any)

	if v := cells[f.deptRatioMetricID+":DEPT_A"]; v != 2.0 {
		t.Errorf("dept_ratio[DEPT_A] = %v, want 2 (dept_total[DEPT_A]=100 with STAFF_A2 hidden, / quota=50)", v)
	}
	if _, present := cells[f.deptRatioMetricID+":DEPT_B"]; present {
		t.Error("dept_ratio[DEPT_B] must be absent (quota was never entered for DEPT_B, so dept_total/quota is #DIV/0! for that combo specifically) — a value here means the per-combo error was masked instead of skipped")
	}

	// DEPT_A's other calc metrics on the same grid must be entirely
	// unaffected by DEPT_B's dept_ratio failure — this is the actual
	// regression: the old implementation zeroed out EVERY combo of EVERY
	// metric sharing a resolution pass with a metric that hit any error.
	if v := cells[f.deptTotalMetricID+":DEPT_A"]; v != 100.0 {
		t.Errorf("dept_total[DEPT_A] = %v, want 100 (unaffected by dept_ratio's DEPT_B failure)", v)
	}
	if v := cells[f.deptTotalMetricID+":DEPT_B"]; v != 200.0 {
		t.Errorf("dept_total[DEPT_B] = %v, want 200 (unaffected by its own dept_ratio failure)", v)
	}
	if v := cells[f.deptTotalCappedMetricID+":DEPT_A"]; v != 100.0 {
		t.Errorf("dept_total_capped[DEPT_A] = %v, want 100 (unaffected)", v)
	}
	if v := cells[f.deptTotalCappedMetricID+":DEPT_B"]; v != 120.0 {
		t.Errorf("dept_total_capped[DEPT_B] = %v, want 120 (unaffected)", v)
	}
}

// TestGridCalcMetricScopedAverageRatioMatchesRatioOfSums is a regression
// test: scopeCalcCells (the hidden-member-restricted grid path) used to
// average dept_ratio_avg's per-combo ratios (DEPT_A=100/40=2.5,
// DEPT_B=200/60=3.333..., averaging to ~2.9167) instead of collapsing to a
// single ratio-of-sums evaluation the way internal/calculation's
// executePartition already does for any agg_rule="average" metric whose
// formula isn't itself dimension-conditional — producing (100+200)/(40+60)
// = 3.0. A hidden-member-restricted caller saw a genuinely different number
// than an unrestricted one for the exact same metric/revision.
func TestGridCalcMetricScopedAverageRatioMatchesRatioOfSums(t *testing.T) {
	f := setupRollupFixture(t)

	// Unrestricted persona: reads the synthetic calc_result ground-truth
	// row directly (3.0, the ratio-of-sums) — the bulk calc_result read
	// path was never the buggy one.
	_, unrestricted := f.do(t, "GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.workingRevID, f.gridDeptTotalID), "rollup-test-approver", nil)
	unrestrictedTotals := unrestricted["totals"].(map[string]any)
	if v := unrestrictedTotals[f.deptRatioAvgMetricID]; v != 3.0 {
		t.Fatalf("unrestricted dept_ratio_avg total = %v, want 3.0 (ratio-of-sums ground truth) — fixture assumption broken", v)
	}

	// Hidden-member-restricted persona (STAFF_A2 hidden, at a finer grain
	// than dept_ratio_avg's own [departments] dims) forces the
	// scopeCalcCells recomputation path — this must match the unrestricted
	// total exactly.
	persona := f.setupStaffLevelHiddenPersona(t, f.staffA2ID)
	_, scoped := f.do(t, "GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.workingRevID, f.gridDeptTotalID), persona, nil)
	scopedTotals := scoped["totals"].(map[string]any)
	if v := scopedTotals[f.deptRatioAvgMetricID]; v != 3.0 {
		t.Errorf("scoped dept_ratio_avg total = %v, want 3.0 to match the unrestricted ratio-of-sums — average-of-per-combo-ratios would wrongly give ~2.9167", v)
	}
}

// TestDeveloperModelSurfacesCalcError is a C1 regression test: a metric
// whose most recent calculation attempt failed (e.g. a formula referencing
// an identifier that no longer resolves after a rename) used to freeze
// silently — internal/calculation.Store.MarkError already persisted the
// failure onto runtime.metric_partition_state, but nothing in this package
// ever read it back out, so a developer had no way to discover why a value
// stopped updating. This seeds a partition_state row the same way MarkError
// would (the write path itself is calculation package territory, already
// covered there) and asserts GET /api/developer/model now surfaces it.
func TestDeveloperModelSurfacesCalcError(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	wantErr := "identifier \"old_metric_name\" not found"
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO runtime.metric_partition_state
		  (partition_key, model_id, metric_id, revision_id, time_partition, status, error, updated_at)
		VALUES ('test-pk-1', $1::uuid, $2::uuid, $3::uuid, '2026-08', 'error', $4, now())
	`, f.modelID, f.deptTotalMetricID, f.workingRevID, wantErr); err != nil {
		t.Fatalf("seed error partition state: %v", err)
	}

	status, body := f.do(t, "GET", "/api/developer/model?revision_id="+f.workingRevID, "test-approver", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/developer/model: status=%d, body=%v", status, body)
	}
	metrics, _ := body["metrics"].([]any)
	var found bool
	for _, raw := range metrics {
		mm := raw.(map[string]any)
		if mm["id"] != f.deptTotalMetricID {
			continue
		}
		found = true
		if mm["calc_error"] != wantErr {
			t.Errorf("dept_total.calc_error = %v, want %q", mm["calc_error"], wantErr)
		}
	}
	if !found {
		t.Fatalf("dept_total metric not found in response: %v", metrics)
	}

	// A healthy metric (no error partition row) must not carry a
	// calc_error field at all — the fix must not flag every metric.
	for _, raw := range metrics {
		mm := raw.(map[string]any)
		if mm["id"] == f.amountMetricID {
			if _, has := mm["calc_error"]; has {
				t.Errorf("amount.calc_error = %v, want field absent (this metric has no error partition row)", mm["calc_error"])
			}
		}
	}
}

// TestDeleteMetricRecalculatesDependents is a regression test:
// DELETE /api/developer/metrics/{id} used to only delete the row and call
// autoMigrate — never recalculating dependents. Worse than a simple
// staleness gap: the DELETE's own ON DELETE CASCADE removes the dependent's
// model.calc_dependency edge in the same statement, so even a blunt
// "recalc everything reachable from an input" pass (the pattern the
// sibling PATCH case already used) could never rediscover the dependent
// afterward — it would stay frozen at its last value forever with no error
// shown. developerMetricAction's DELETE case now captures the dependent set
// via a recursive CTE BEFORE deleting and force-recomputes it via
// Scheduler.RecalcSpecific afterward.
//
// This fixture (setupRollupFixture) writes synthetic calc_result rows
// directly rather than real model.calc_dependency edges (it exercises
// grid()'s read side, not the scheduler) — so this test adds its own real
// edge between two of the fixture's existing metrics before deleting one.
func TestDeleteMetricRecalculatesDependents(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid)
	`, f.deptTotalCappedMetricID, f.deptTotalMetricID); err != nil {
		t.Fatalf("insert calc_dependency: %v", err)
	}

	status, body := f.do(t, "DELETE", "/api/developer/metrics/"+f.deptTotalMetricID, "rollup-test-approver", nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE metric: status=%d, body=%v", status, body)
	}

	// Recalc is fire-and-forget from the handler's perspective (matches the
	// PATCH case's own synchronous-within-the-request-but-not-awaited-by-
	// the-test-server style) — poll briefly rather than assume it landed
	// before the response returned.
	var calcError string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = f.pool.QueryRow(ctx, `
			SELECT COALESCE(error, '') FROM runtime.metric_partition_state
			WHERE model_id=$1::uuid AND metric_id=$2::uuid AND revision_id=$3::uuid AND status='error'
			ORDER BY updated_at DESC LIMIT 1
		`, f.modelID, f.deptTotalCappedMetricID, f.workingRevID).Scan(&calcError)
		if calcError != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if calcError == "" {
		t.Fatal("dept_total_capped's partition never transitioned to error after dept_total was deleted — the dependent was left silently frozen instead of being recomputed")
	}
	if !strings.Contains(calcError, "dept_total") {
		t.Errorf("error message = %q, want it to reference the deleted dept_total identifier", calcError)
	}
}

// TestDeleteDimensionMemberTriggersRecalc is a regression test:
// DELETE /api/developer/dimensions/{dimId}/members/{memberId} never
// triggered a recalc at all, unlike the rename/create/reparent paths right
// above it in the same handler — an aggregate calc metric over that
// dimension stayed frozen at its last computed value indefinitely. This
// fixture writes synthetic calc_result rows directly rather than real
// model.calc_dependency edges, so this test adds its own real edge
// (dept_total depends on amount) before deleting a department member, then
// confirms dept_total's '{}' aggregate calc_result row gets a fresh
// calc_at timestamp — proof a recalc pass actually ran, not just that the
// stale row was left untouched.
func TestDeleteDimensionMemberTriggersRecalc(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid)
	`, f.deptTotalMetricID, f.amountMetricID); err != nil {
		t.Fatalf("insert calc_dependency: %v", err)
	}

	var before time.Time
	if err := f.pool.QueryRow(ctx, `
		SELECT calc_at FROM runtime.calc_result
		WHERE model_id=$1::uuid AND metric_id=$2::uuid AND dim_members='{}'
	`, f.modelID, f.deptTotalMetricID).Scan(&before); err != nil {
		t.Fatalf("query fixture's synthetic calc_result row: %v", err)
	}

	status, body := f.do(t, "DELETE", fmt.Sprintf("/api/developer/dimensions/%s/members/%s", f.deptsDimID, f.deptBID), "rollup-test-approver", nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE dimension member: status=%d, body=%v", status, body)
	}

	deadline := time.Now().Add(5 * time.Second)
	var after time.Time
	for time.Now().Before(deadline) {
		_ = f.pool.QueryRow(ctx, `
			SELECT calc_at FROM runtime.calc_result
			WHERE model_id=$1::uuid AND metric_id=$2::uuid AND dim_members='{}'
			ORDER BY calc_at DESC LIMIT 1
		`, f.modelID, f.deptTotalMetricID).Scan(&after)
		if after.After(before) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !after.After(before) {
		t.Fatalf("dept_total's calc_result row was never refreshed after deleting a department member (before=%v, after=%v) — no recalc was triggered", before, after)
	}
}

// TestDeveloperDimensionsFiltersOwnersOwnHiddenRule is a regression test:
// GET /api/developer/dimensions applied no identity.user_access_rule
// filtering at all, unlike publicDimensions/grid() — a developer whose own
// account carries a "hidden" rule (e.g. via a dual developer+business_admin
// account, like this fixture's approver) still saw every member through
// this endpoint. The response is a bare array (jsonOK(w, dims)), so this
// decodes it directly rather than through f.do's map[string]any helper.
func TestDeveloperDimensionsFiltersOwnersOwnHiddenRule(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'hidden')`,
		f.approverID, f.deptBID,
	); err != nil {
		t.Fatalf("seed hidden rule: %v", err)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+"/api/developer/dimensions?revision_id="+f.workingRevID, nil)
	req.Header.Set("X-Dev-User", "test-approver")
	req.Header.Set("X-App-Id", f.appID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var dims []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Members []struct {
			ID   string `json:"id"`
			Code string `json:"code"`
		} `json:"members"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dims); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, d := range dims {
		if d.ID != f.deptsDimID {
			continue
		}
		for _, m := range d.Members {
			if m.ID == f.deptBID {
				t.Errorf("departments dimension still lists DEPT_B (%s), hidden for this caller", m.ID)
			}
		}
		found := false
		for _, m := range d.Members {
			if m.ID == f.deptAID {
				found = true
			}
		}
		if !found {
			t.Error("departments dimension is missing DEPT_A, which is NOT hidden for this caller")
		}
	}
}

// TestBaAvailableFiltersAdminsOwnHiddenRule is a regression test: the
// business-admin console's own dimension-member/metric picker (used to
// configure OTHER users' access rules) was never filtered by the calling
// admin's own identity.user_access_rule — the same publicDimensions gap
// fixed earlier this session, not carried over here.
func TestBaAvailableFiltersAdminsOwnHiddenRule(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'hidden')`,
		f.approverID, f.deptBID,
	); err != nil {
		t.Fatalf("seed hidden dimension rule: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'metric', $2, 'hidden')`,
		f.approverID, f.amountMetricID,
	); err != nil {
		t.Fatalf("seed hidden metric rule: %v", err)
	}

	// jsonOK writes a bare array here, so this decodes it directly rather
	// than through f.do's map[string]any helper.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+"/api/business-admin/available?type=dimension_members", nil)
	req.Header.Set("X-Dev-User", "test-approver")
	req.Header.Set("X-App-Id", f.appID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var members []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, m := range members {
		if m.ID == f.deptBID {
			t.Errorf("dimension_members picker still lists DEPT_B (%s), hidden for this admin", m.ID)
		}
	}

	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+"/api/business-admin/available?type=metrics", nil)
	req2.Header.Set("X-Dev-User", "test-approver")
	req2.Header.Set("X-App-Id", f.appID)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp2.Body.Close() //nolint:errcheck
	var metrics []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&metrics); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, m := range metrics {
		if m.ID == f.amountMetricID {
			t.Errorf("metrics picker still lists amount (%s), hidden for this admin", m.ID)
		}
	}
}

// TestRedactHiddenContextRemovesOnlyHiddenDimensionMemberKeys is a
// regression test: tasks()/workflowMyHistory() used to return
// wi.context verbatim, with visibility gated purely by assignee_roles —
// a hidden-scoped assignee could still see the real dimension-member
// code in their own task list/history. DEPT_B is hidden for the
// manager-b persona (a real rule already seeded by this fixture); a
// context referencing DEPT_B must have that key redacted, while a
// same-shaped context referencing the visible DEPT_A must be untouched,
// and non-dimension-bound keys must never be touched either way.
func TestRedactHiddenContextRemovesOnlyHiddenDimensionMemberKeys(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	h := &handler{db: tenantdb.NewHandle(f.pool, nil), log: logger.New("test")}

	schema, err := json.Marshal([]contextVarSchema{
		{Key: "scope", DataType: "Dimension member", DimensionID: f.deptsDimID},
		{Key: "note", DataType: "Text"},
	})
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}

	// f.managerBID is hidden from DEPT_A (seeded by setupRollupFixture's
	// own hide(f.managerBID, f.deptAID) call) — reusing that real rule
	// rather than seeding a redundant one.
	hiddenCtx := map[string]any{"scope": "DEPT_A", "note": "reminder"}
	out := h.redactHiddenContext(ctx, f.managerBID, schema, hiddenCtx)
	if _, has := out["scope"]; has {
		t.Errorf("redactHiddenContext kept scope=%v for a user hidden from DEPT_A, want removed", out["scope"])
	}
	if out["note"] != "reminder" {
		t.Errorf("redactHiddenContext touched a non-dimension-bound key: note=%v", out["note"])
	}
	if _, has := hiddenCtx["scope"]; !has {
		t.Error("redactHiddenContext mutated its input map — must return a copy")
	}

	// Same shape, but DEPT_B (which managerB is NOT hidden from) — must
	// survive untouched.
	visibleCtx := map[string]any{"scope": "DEPT_B", "note": "reminder"}
	out2 := h.redactHiddenContext(ctx, f.managerBID, schema, visibleCtx)
	if out2["scope"] != "DEPT_B" {
		t.Errorf("redactHiddenContext removed scope=DEPT_B for a user NOT hidden from it: %v", out2)
	}
}

// TestDimensionMemberRenameReKeysFactAndCalcRows is a C2 regression test:
// runtime.fact_input/calc_result store dim_members as JSONB keyed by
// {dimension_id: member_CODE}, not member ID. Renaming a member's code used
// to leave every existing row referencing the old code permanently
// orphaned — not deleted, just unreachable via any current-code-based
// query. f.staffA1ID (code STAFF_A1) already has a real fact_input row
// (value 100) from the base fixture; this seeds a matching calc_result row
// too, then renames the member and confirms both are re-keyed to the new
// code and unreachable under the old one.
func TestDimensionMemberRenameReKeysFactAndCalcRows(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	dimMembers := fmt.Sprintf(`{"%s":"STAFF_A1"}`, f.staffDimID)
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO runtime.calc_result (model_id, revision_id, dim_members, metric_id, value, partition_key)
		VALUES ($1::uuid, $2::uuid, $3::jsonb, $4::uuid, 42, 'test-calc-pk-1')
	`, f.modelID, f.workingRevID, dimMembers, f.deptTotalMetricID); err != nil {
		t.Fatalf("seed calc_result: %v", err)
	}

	status, body := f.do(t, "PATCH",
		fmt.Sprintf("/api/developer/dimensions/%s/members/%s", f.staffDimID, f.staffA1ID),
		"test-approver",
		map[string]any{"code": "STAFF_A1_RENAMED", "label": "A1", "parent_member_id": f.deptAID},
	)
	if status != http.StatusOK {
		t.Fatalf("PATCH member rename: status=%d, body=%v", status, body)
	}

	var factCode, calcCode string
	if err := f.pool.QueryRow(ctx,
		`SELECT dim_members->>$1 FROM runtime.fact_input WHERE model_id=$2::uuid AND value=100`,
		f.staffDimID, f.modelID,
	).Scan(&factCode); err != nil {
		t.Fatalf("query re-keyed fact_input: %v", err)
	}
	if factCode != "STAFF_A1_RENAMED" {
		t.Errorf("fact_input dim_members[%s] = %q, want STAFF_A1_RENAMED", f.staffDimID, factCode)
	}

	if err := f.pool.QueryRow(ctx,
		`SELECT dim_members->>$1 FROM runtime.calc_result WHERE model_id=$2::uuid AND value=42`,
		f.staffDimID, f.modelID,
	).Scan(&calcCode); err != nil {
		t.Fatalf("query re-keyed calc_result: %v", err)
	}
	if calcCode != "STAFF_A1_RENAMED" {
		t.Errorf("calc_result dim_members[%s] = %q, want STAFF_A1_RENAMED", f.staffDimID, calcCode)
	}

	// Nothing should still be reachable under the old code.
	var staleCount int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND dim_members->>$2 = 'STAFF_A1'`,
		f.modelID, f.staffDimID,
	).Scan(&staleCount); err != nil {
		t.Fatalf("count stale fact_input rows: %v", err)
	}
	if staleCount != 0 {
		t.Errorf("%d fact_input row(s) still keyed under the old code STAFF_A1, want 0", staleCount)
	}
}

// TestGridHidesRestrictedMetricEverywhere is a regression test for a real
// leak: metricRules (rule_type='metric' access rules) used to only be
// applied to the final `metrics` list — never to all_metrics, cells, or
// totals — so a "hidden" metric's real value was still present in the
// /api/grid response a metric_kpi widget reads directly. amount is
// hidden from the bystander; deptTotal (a calc metric with no direct
// dependency on amount) must be completely unaffected.
func TestGridHidesRestrictedMetricEverywhere(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'metric', $2, 'hidden')`,
		f.approverID, f.amountMetricID,
	); err != nil {
		t.Fatalf("seed hidden metric rule: %v", err)
	}

	status, body := f.do(t, "GET", "/api/grid?revision_id="+f.workingRevID, "test-approver", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/grid: status=%d, body=%v", status, body)
	}

	metrics, _ := body["metrics"].([]any)
	for _, raw := range metrics {
		if raw.(map[string]any)["id"] == f.amountMetricID {
			t.Error("hidden metric still present in metrics list")
		}
	}
	allMetrics, _ := body["all_metrics"].([]any)
	for _, raw := range allMetrics {
		if raw.(map[string]any)["id"] == f.amountMetricID {
			t.Error("hidden metric still present in all_metrics — this is what MetricKpiWidget reads directly")
		}
	}
	totals, _ := body["totals"].(map[string]any)
	if _, has := totals[f.amountMetricID]; has {
		t.Errorf("hidden metric %s still present in totals: %v", f.amountMetricID, totals[f.amountMetricID])
	}
	cells, _ := body["cells"].(map[string]any)
	for k := range cells {
		if strings.HasPrefix(k, f.amountMetricID+":") {
			t.Errorf("hidden metric cell %q still present in cells", k)
		}
	}

	// An unrestricted calc metric with no dependency on the hidden metric
	// must be entirely unaffected — compare against its own unscoped
	// calc_result aggregate rather than a hand-derived expected number.
	var wantTotal float64
	if err := f.pool.QueryRow(ctx,
		`SELECT value::float8 FROM runtime.calc_result WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid AND dim_members='{}' ORDER BY calc_at DESC LIMIT 1`,
		f.modelID, f.workingRevID, f.deptTotalMetricID,
	).Scan(&wantTotal); err != nil {
		t.Fatalf("query deptTotal's own calc_result aggregate: %v", err)
	}
	if v, ok := totals[f.deptTotalMetricID]; !ok || v != wantTotal {
		t.Errorf("deptTotal total = %v (ok=%v), want %v (unaffected by amount's hidden rule)", v, ok, wantTotal)
	}
}

// TestContextDisplayResolvesNames: an approver reading "country: CA" cannot
// know they are approving Canada — contextDisplay must resolve dimension
// member codes to labels (with the owning dimension's name) and leave
// unresolvable or non-member values as-is, never dropping a key.
func TestContextDisplayResolvesNames(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	h := &handler{db: tenantdb.NewHandle(f.pool, nil), log: logger.New("test")}

	schema, err := json.Marshal([]contextVarSchema{
		{Key: "scope", DataType: "Dimension member", DimensionID: f.deptsDimID},
		{Key: "ghost", DataType: "Dimension member", DimensionID: f.deptsDimID},
		{Key: "note", DataType: "Text"},
	})
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}

	out := h.contextDisplay(ctx, schema, map[string]any{
		"scope":   "DEPT_A",
		"ghost":   "NO_SUCH_CODE",
		"note":    "reminder",
		"_hidden": "internal",
		"extra":   "unschemad",
	})

	byKey := map[string]taskContextEntry{}
	for _, e := range out {
		byKey[e.Key] = e
	}
	if e := byKey["scope"]; e.Display != "Dept A" || e.DimensionName != "departments" || e.Value != "DEPT_A" {
		t.Errorf("scope = %+v, want display 'Dept A' in dimension 'departments' with raw value DEPT_A", e)
	}
	if e := byKey["ghost"]; e.Display != "NO_SUCH_CODE" {
		t.Errorf("ghost = %+v, want raw code fallback when nothing resolves", e)
	}
	if e := byKey["note"]; e.Display != "reminder" || e.DimensionName != "" {
		t.Errorf("note = %+v, want raw text with no dimension", e)
	}
	if e := byKey["extra"]; e.Display != "unschemad" {
		t.Errorf("extra = %+v, want unschema'd keys kept with raw value", e)
	}
	if _, has := byKey["_hidden"]; has {
		t.Error("_hidden internal key must not appear on the card")
	}
	// Schema order first: scope, ghost, note (extra follows).
	if len(out) != 4 || out[0].Key != "scope" || out[1].Key != "ghost" || out[2].Key != "note" || out[3].Key != "extra" {
		keys := make([]string, len(out))
		for i, e := range out {
			keys[i] = e.Key
		}
		t.Errorf("order = %v, want schema order then sorted extras", keys)
	}
}
