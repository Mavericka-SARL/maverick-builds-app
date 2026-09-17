// Tests for CompleteStep's on-approve fact-copy path: atomicity with the
// decision itself, the per-(metric,dim_members) copy fix, the
// revision-ownership validation, and the concurrency guard. Unlike
// store_test.go's minimal testdata/*.sql schema (which deliberately has no
// model.revision/model.dimension_def/runtime.fact_input — see
// testdata/007_revision_isolation.sql), these tests need the real
// model/runtime schema, so they run against the full migration set, the
// same way internal/gateway's generic_rollup_workflow_test.go does.
//
// Every test here drives workflow.Store.CompleteStep directly, with no HTTP
// server involved at all — exactly the path internal/workflow/server.go's
// gRPC WorkflowService and every cmd/seed*/main.go script use. Before this
// fix, the on-approve fact copy only ever ran from the gateway's taskAction
// HTTP handler; every test in this file is itself proof that gap is closed.
package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// onApproveFixture seeds one department-scope member (DEPT_A) with one
// child "staff" member under it (STAFF_A1, via a cross-dimension
// parent_dimension_id/parent_member_id link — the same shape
// generic_rollup_workflow_test.go's rollup fixture uses), two input metrics
// and one metric calculated from both, and a published-or-not (StartWorkflow
// doesn't require publish at this level — see store_test.go's own tests)
// single-step approval workflow whose on_approve config copies facts scoped
// to DEPT_A into a second revision.
type onApproveFixture struct {
	pool  *pgxpool.Pool
	store *workflow.Store

	modelID, otherModelID                   string
	workingRevID, targetRevID, foreignRevID string
	metric1ID, metric2ID, totalMetricID     string
	userID, appID, wfDefID                  string
}

func setupOnApproveFixture(t *testing.T) *onApproveFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

	f := &onApproveFixture{pool: pool, store: workflow.NewStore(pool)}
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
	otherAppID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Other App', 'planning') RETURNING id::text`, wsID, custID)
	f.otherModelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Other') RETURNING id::text`, otherAppID)

	f.workingRevID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, f.modelID)
	f.targetRevID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Target') RETURNING id::text`, f.modelID)
	f.foreignRevID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Foreign') RETURNING id::text`, f.otherModelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid WHERE id=$2::uuid`, f.workingRevID, f.modelID)

	deptsDimID := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'departments') RETURNING id::text`, f.modelID, f.workingRevID)
	deptAID := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'DEPT_A', 'Dept A') RETURNING id::text`, deptsDimID)

	staffDimID := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, parent_dimension_id) VALUES ($1::uuid, $2::uuid, 'staff', $3::uuid) RETURNING id::text`, f.modelID, f.workingRevID, deptsDimID)
	_ = q(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id) VALUES ($1::uuid, 'STAFF_A1', 'A1', $2::uuid) RETURNING id::text`, staffDimID, deptAID)

	f.metric1ID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'metric1', true) RETURNING id::text`, f.modelID, f.workingRevID)
	f.metric2ID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'metric2', true) RETURNING id::text`, f.modelID, f.workingRevID)
	f.totalMetricID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula) VALUES ($1::uuid, $2::uuid, 'total', false, 'metric1 + metric2') RETURNING id::text`, f.modelID, f.workingRevID)
	exec(`INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid)`, f.totalMetricID, f.metric1ID)
	exec(`INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid)`, f.totalMetricID, f.metric2ID)

	f.userID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-approver', 'approver@t.com', 'Approver', $1::uuid) RETURNING id::text`, custID)

	dimMembers := fmt.Sprintf(`{"%s":"STAFF_A1"}`, staffDimID)
	exec(`INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, $4::jsonb, 100, $5::uuid)`,
		f.modelID, f.workingRevID, f.metric1ID, dimMembers, f.userID)
	exec(`INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, $4::jsonb, 200, $5::uuid)`,
		f.modelID, f.workingRevID, f.metric2ID, dimMembers, f.userID)

	wfDef, err := f.store.CreateWorkflowDef(ctx, f.appID, "Approval", "manual", []*workflowv1.WorkflowStepDef{
		{Id: "step-1", Name: "Approve", Type: workflowv1.StepType_STEP_TYPE_APPROVAL},
	})
	if err != nil {
		t.Fatalf("create workflow def: %v", err)
	}
	f.wfDefID = wfDef.Id

	type contextVarDef struct {
		Key         string `json:"key"`
		DataType    string `json:"data_type"`
		DimensionID string `json:"dimension_id,omitempty"`
	}
	schema, _ := json.Marshal([]contextVarDef{
		{Key: "scope", DataType: "Dimension member", DimensionID: deptsDimID},
		{Key: "revision_id", DataType: "Text"},
		{Key: "target_revision_id", DataType: "Text"},
	})
	exec(`UPDATE workflow.workflow_def SET context_schema=$1::jsonb WHERE id=$2::uuid`, schema, f.wfDefID)
	onApprove, _ := json.Marshal(map[string]string{"copy_facts_to_context_key": "target_revision_id", "scope_context_key": "scope"})
	exec(`UPDATE workflow.workflow_def SET steps = jsonb_set(steps, '{0,on_approve}', $1::jsonb) WHERE id=$2::uuid`, onApprove, f.wfDefID)

	return f
}

// startInstance starts a fresh instance scoped to DEPT_A with
// target_revision_id pointed at targetRevID, and returns the resulting
// instance and its single step's ID.
func (f *onApproveFixture) startInstance(t *testing.T, targetRevID string) (instanceID, stepID string) {
	t.Helper()
	ctx := context.Background()
	inst, err := f.store.StartWorkflow(ctx, f.wfDefID, f.userID, map[string]string{
		"model_id":           f.modelID,
		"revision_id":        f.workingRevID,
		"target_revision_id": targetRevID,
		"scope":              "DEPT_A",
	})
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	_, steps, err := f.store.GetWorkflowInstance(ctx, inst.Id)
	if err != nil || len(steps) == 0 {
		t.Fatalf("get instance steps: %v (steps=%d)", err, len(steps))
	}
	return inst.Id, steps[0].Id
}

func TestCompleteStepDirectCallCopiesBothMetricsAtSameIntersection(t *testing.T) {
	f := setupOnApproveFixture(t)
	ctx := context.Background()

	_, stepID := f.startInstance(t, f.targetRevID)
	completed, err := f.store.CompleteStep(ctx, stepID, f.userID, "approve", "looks good")
	if err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}
	if completed.Status != workflowv1.StepStatus_STEP_STATUS_COMPLETED {
		t.Fatalf("step status = %v, want COMPLETED", completed.Status)
	}

	// Both metric1 and metric2 must have been copied — before the fix,
	// DISTINCT ON (dim_members) (missing metric_id) collapsed both rows at
	// this one dimension intersection down to a single, arbitrarily-chosen
	// metric, silently dropping the other.
	var count int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM runtime.fact_input
		WHERE revision_id=$1::uuid AND metric_id IN ($2::uuid, $3::uuid)
	`, f.targetRevID, f.metric1ID, f.metric2ID).Scan(&count); err != nil {
		t.Fatalf("count copied facts: %v", err)
	}
	if count != 2 {
		t.Errorf("copied fact rows = %d, want 2 (one per metric) — the DISTINCT ON (dim_members) bug would collapse this to 1", count)
	}

	// The dependent "total" metric's partition must have been marked dirty
	// atomically with the copy (MarkDirtyTx), regardless of whether the
	// post-commit RecalcAffected pass that follows went on to compute it
	// successfully.
	var dirtyRows int
	_ = f.pool.QueryRow(ctx, `
		SELECT count(*) FROM runtime.metric_partition_state
		WHERE model_id=$1::uuid AND metric_id=$2::uuid AND revision_id=$3::uuid
	`, f.modelID, f.totalMetricID, f.targetRevID).Scan(&dirtyRows)
	if dirtyRows == 0 {
		t.Error("expected the dependent 'total' metric's partition to have a runtime.metric_partition_state row after the on-approve copy")
	}
}

// TestCompleteStepRejectsOnApproveCopyForHiddenMember is a regression test
// for a real writeguard bypass: runOnApproveCopy used to write straight
// into runtime.fact_input with no access check at all — the exact write
// the interactive grid (writeguard.CheckWrite) would reject as hidden
// succeeded unconditionally through workflow approval. STAFF_A1 (the
// member the fixture's fact rows are keyed by) is hidden from the
// approver; CompleteStep must fail and nothing must land in the target
// revision.
func TestCompleteStepRejectsOnApproveCopyForHiddenMember(t *testing.T) {
	f := setupOnApproveFixture(t)
	ctx := context.Background()

	var staffA1ID string
	if err := f.pool.QueryRow(ctx,
		`SELECT id::text FROM model.dimension_member WHERE code='STAFF_A1'`,
	).Scan(&staffA1ID); err != nil {
		t.Fatalf("find STAFF_A1: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'dimension_member', $2, 'hidden')`,
		f.userID, staffA1ID,
	); err != nil {
		t.Fatalf("seed hidden rule: %v", err)
	}

	_, stepID := f.startInstance(t, f.targetRevID)
	if _, err := f.store.CompleteStep(ctx, stepID, f.userID, "approve", "looks good"); err == nil {
		t.Fatal("CompleteStep: want an error (approver is hidden from STAFF_A1), got nil")
	} else if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("CompleteStep error = %q, want it to mention access denied", err.Error())
	}

	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM runtime.fact_input WHERE revision_id=$1::uuid`, f.targetRevID,
	).Scan(&n); err != nil {
		t.Fatalf("count target revision facts: %v", err)
	}
	if n != 0 {
		t.Errorf("target revision has %d fact row(s) after a rejected approval, want 0", n)
	}
}

// TestCompleteStepRejectsOnApproveCopyForHiddenMetric is the metric-level
// counterpart: metric1 is hidden from the approver, so the copy (which
// would otherwise copy both metric1 and metric2 at the STAFF_A1
// intersection) must be rejected once metric1 shows up among the metrics
// the copy actually found — not just silently drop that one metric.
func TestCompleteStepRejectsOnApproveCopyForHiddenMetric(t *testing.T) {
	f := setupOnApproveFixture(t)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'metric', $2, 'hidden')`,
		f.userID, f.metric1ID,
	); err != nil {
		t.Fatalf("seed hidden rule: %v", err)
	}

	_, stepID := f.startInstance(t, f.targetRevID)
	if _, err := f.store.CompleteStep(ctx, stepID, f.userID, "approve", "looks good"); err == nil {
		t.Fatal("CompleteStep: want an error (approver is hidden from metric1), got nil")
	} else if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("CompleteStep error = %q, want it to mention access denied", err.Error())
	}

	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM runtime.fact_input WHERE revision_id=$1::uuid`, f.targetRevID,
	).Scan(&n); err != nil {
		t.Fatalf("count target revision facts: %v", err)
	}
	if n != 0 {
		t.Errorf("target revision has %d fact row(s) after a rejected approval, want 0", n)
	}
}

func TestCompleteStepRejectsOnApproveTargetRevisionFromWrongModel(t *testing.T) {
	f := setupOnApproveFixture(t)
	ctx := context.Background()

	// foreignRevID belongs to otherModelID, not f.modelID — a stale or
	// tampered context value.
	_, stepID := f.startInstance(t, f.foreignRevID)
	if _, err := f.store.CompleteStep(ctx, stepID, f.userID, "approve", "looks good"); err == nil {
		t.Fatal("expected CompleteStep to fail when the on-approve target revision belongs to a different model")
	}

	// The step must NOT have been left completed: decision, validation, and
	// copy are one transaction, so a validation failure rolls back the
	// step-completion UPDATE too, instead of silently approving without
	// copying.
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status::text FROM workflow.workflow_step WHERE id=$1::uuid`, stepID).Scan(&status); err != nil {
		t.Fatalf("query step status: %v", err)
	}
	if status != "in_progress" {
		t.Errorf("step status = %q, want still in_progress after a rolled-back on-approve validation failure", status)
	}

	var count int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input WHERE revision_id=$1::uuid`, f.foreignRevID).Scan(&count)
	if count != 0 {
		t.Errorf("fact_input rows copied into the foreign revision = %d, want 0", count)
	}
}

func TestCompleteStepConcurrentCompletionsOnlyOneSucceeds(t *testing.T) {
	f := setupOnApproveFixture(t)
	ctx := context.Background()

	_, stepID := f.startInstance(t, f.targetRevID)

	var wg sync.WaitGroup
	results := make([]error, 2)
	wg.Add(2)
	for i := range results {
		go func(i int) {
			defer wg.Done()
			_, results[i] = f.store.CompleteStep(ctx, stepID, f.userID, "approve", "concurrent")
		}(i)
	}
	wg.Wait()

	var successes, conflicts int
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case strings.Contains(err.Error(), "cannot complete"):
			conflicts++
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("expected exactly one success and one conflict, got successes=%d conflicts=%d (errors: %v)", successes, conflicts, results)
	}

	// Exactly one copy of each metric must exist. Before the fix, the
	// step-completion UPDATE's WHERE clause didn't check status at all
	// (only an earlier, separate SELECT did) — a classic check-then-act
	// race that could let both goroutines pass the Go-level check and both
	// run the copy.
	var count int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM runtime.fact_input
		WHERE revision_id=$1::uuid AND metric_id IN ($2::uuid, $3::uuid)
	`, f.targetRevID, f.metric1ID, f.metric2ID).Scan(&count); err != nil {
		t.Fatalf("count copied facts: %v", err)
	}
	if count != 2 {
		t.Errorf("copied fact rows after concurrent completion = %d, want exactly 2 (no double-copy)", count)
	}
}
