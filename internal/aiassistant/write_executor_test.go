package aiassistant_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// setupDB spins up a real Postgres container and applies the full, real
// migration set (not a hand-curated subset) — write_executor touches enough
// of the model.* schema (metrics, dimensions, grids, dashboards, revisions)
// that a partial schema would risk silently drifting from production, as
// happened with the model.scenario -> model.revision rename in migration 045.
func setupWriteExecutorDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool := testdb.New(t, migrationfs.FS, ".")
	return pool
}

// seedModel creates the customer -> workspace -> application -> model chain
// and returns the model ID.
func seedModel(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()

	var customerID, workspaceID, appID, modelID string
	if err := pool.QueryRow(ctx, `INSERT INTO core.customer (name) VALUES ('Test Customer') RETURNING id::text`).Scan(&customerID); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Test Workspace') RETURNING id::text`, customerID).Scan(&workspaceID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO core.application (workspace_id, name, mode) VALUES ($1::uuid, 'Test App', 'planning') RETURNING id::text`, workspaceID).Scan(&appID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Test Model') RETURNING id::text`, appID).Scan(&modelID); err != nil {
		t.Fatalf("seed model: %v", err)
	}
	return modelID
}

// seedRevision creates a working revision (model.revision — the renamed
// model.scenario table, see migration 045) and returns its ID.
func seedRevision(t *testing.T, pool *pgxpool.Pool, modelID, name string) string {
	t.Helper()
	var revID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO model.revision (model_id, name, description) VALUES ($1::uuid, $2, '') RETURNING id::text
	`, modelID, name).Scan(&revID); err != nil {
		t.Fatalf("seed revision %q: %v", name, err)
	}
	return revID
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return b
}

// ── create_metric / update_metric / delete_metric ──────────────────────────

func TestCreateMetric_Input(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)

	result, createdID, err := exec.Execute(context.Background(), "create_metric", mustJSON(t, map[string]any{
		"name": "headcount", "is_input": true, "format": "number", "agg_rule": "sum",
	}))
	if err != nil {
		t.Fatalf("create_metric: %v", err)
	}
	if createdID == "" {
		t.Fatal("expected a created metric ID")
	}
	if result == "" {
		t.Fatal("expected a non-empty result message")
	}

	var name string
	var isInput bool
	var formula *string
	if err := pool.QueryRow(context.Background(), `SELECT name, is_input, formula FROM model.metric_def WHERE id=$1::uuid`, createdID).
		Scan(&name, &isInput, &formula); err != nil {
		t.Fatalf("verify metric row: %v", err)
	}
	if name != "headcount" || !isInput || formula != nil {
		t.Fatalf("unexpected metric row: name=%q is_input=%v formula=%v", name, isInput, formula)
	}
}

func TestCreateMetric_CalculatedRequiresFormula(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)

	_, _, err := exec.Execute(context.Background(), "create_metric", mustJSON(t, map[string]any{
		"name": "gross_profit", "is_input": false,
	}))
	if err == nil {
		t.Fatal("expected an error when a calculated metric has no formula")
	}
}

func TestCreateMetric_CalculatedMissingDependencyRejected(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)

	// "headcount" does not exist yet — the executor must reject this before
	// inserting, not create an orphaned formula (this was a real bug fixed
	// earlier: the metric got created anyway and silently evaluated to 0).
	_, _, err := exec.Execute(context.Background(), "create_metric", mustJSON(t, map[string]any{
		"name": "revenue_per_head", "is_input": false, "formula": "revenue / headcount",
	}))
	if err == nil {
		t.Fatal("expected an error for a formula referencing a nonexistent metric")
	}

	var count int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid`, modelID).Scan(&count)
	if count != 0 {
		t.Fatalf("expected no metric row to be created, found %d", count)
	}
}

// TestCreateMetric_FormulaCanReferenceADimension is a regression test for a
// real gap: create_metric validated formula references against
// model.metric_def only, so any formula referencing a dimension for
// rollup (e.g. "revenue / Department") was rejected even though it resolves
// fine at calc time — the manual developerMetrics handler validates against
// metrics AND dimensions via a UNION ALL, and the AI tool now matches it.
func TestCreateMetric_FormulaCanReferenceADimension(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "revenue", "is_input": true})); err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	if _, _, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{"name": "Department"})); err != nil {
		t.Fatalf("create dimension: %v", err)
	}

	_, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "revenue_per_dept", "is_input": false, "formula": "revenue / Department",
	}))
	if err != nil {
		t.Fatalf("create_metric with a dimension reference: %v (dimension refs must validate, not just metric refs)", err)
	}
}

func TestCreateMetric_WiresCalcDependency(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, revenueID, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "revenue", "is_input": true, "format": "currency",
	}))
	if err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	_, headcountID, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "headcount", "is_input": true, "format": "number",
	}))
	if err != nil {
		t.Fatalf("create headcount: %v", err)
	}
	_, calcID, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "revenue_per_head", "is_input": false, "formula": "revenue / headcount",
	}))
	if err != nil {
		t.Fatalf("create revenue_per_head: %v", err)
	}

	rows, err := pool.Query(ctx, `SELECT depends_on_metric_id::text FROM model.calc_dependency WHERE metric_id=$1::uuid`, calcID)
	if err != nil {
		t.Fatalf("query calc_dependency: %v", err)
	}
	defer rows.Close()
	deps := map[string]bool{}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		deps[id] = true
	}
	if !deps[revenueID] || !deps[headcountID] {
		t.Fatalf("expected calc_dependency rows for both revenue and headcount, got: %v", deps)
	}
}

func TestUpdateMetric_RewiresChangedFormula(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, aID, _ := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "a", "is_input": true}))
	_, bID, _ := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "b", "is_input": true}))
	_, calcID, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "calc", "is_input": false, "formula": "a",
	}))
	if err != nil {
		t.Fatalf("create calc: %v", err)
	}

	_, _, err = exec.Execute(ctx, "update_metric", mustJSON(t, map[string]any{
		"metric_id": calcID, "name": "calc", "formula": "b",
	}))
	if err != nil {
		t.Fatalf("update_metric: %v", err)
	}

	rows, err := pool.Query(ctx, `SELECT depends_on_metric_id::text FROM model.calc_dependency WHERE metric_id=$1::uuid`, calcID)
	if err != nil {
		t.Fatalf("query calc_dependency: %v", err)
	}
	defer rows.Close()
	var deps []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		deps = append(deps, id)
	}
	if len(deps) != 1 || deps[0] != bID {
		t.Fatalf("expected calc_dependency to be re-wired to b (%s) only, got: %v (a=%s)", bID, deps, aID)
	}
}

// TestUpdateMetric_RejectsMissingDependency is a regression test for a real
// gap: update_metric previously ran NO pre-flight formula validation at
// all (only create_metric did), so editing a formula to reference a
// nonexistent metric/dimension went live silently and only surfaced later
// via validate_formulas.
func TestUpdateMetric_RejectsMissingDependency(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "a", "is_input": true}))
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	_, calcID, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "calc", "is_input": false, "formula": "a",
	}))
	if err != nil {
		t.Fatalf("create calc: %v", err)
	}

	_, _, err = exec.Execute(ctx, "update_metric", mustJSON(t, map[string]any{
		"metric_id": calcID, "name": "calc", "formula": "nonexistent_metric",
	}))
	if err == nil {
		t.Fatal("expected an error updating a formula to reference a nonexistent metric")
	}

	var formula string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(formula,'') FROM model.metric_def WHERE id=$1::uuid`, calcID).Scan(&formula); err != nil {
		t.Fatalf("query metric: %v", err)
	}
	if formula != "a" {
		t.Errorf("formula = %q, want unchanged 'a' — a rejected update must not partially apply", formula)
	}
}

// TestUpdateMetric_FormulaCanReferenceADimension mirrors
// TestCreateMetric_FormulaCanReferenceADimension for the update path — the
// same UNION ALL fix applies to the new pre-flight check added above.
func TestUpdateMetric_FormulaCanReferenceADimension(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "revenue", "is_input": true}))
	if err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	if _, _, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{"name": "Department"})); err != nil {
		t.Fatalf("create dimension: %v", err)
	}
	_, calcID, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "calc", "is_input": false, "formula": "revenue",
	}))
	if err != nil {
		t.Fatalf("create calc: %v", err)
	}

	_, _, err = exec.Execute(ctx, "update_metric", mustJSON(t, map[string]any{
		"metric_id": calcID, "name": "calc", "formula": "revenue / Department",
	}))
	if err != nil {
		t.Fatalf("update_metric with a dimension reference: %v (dimension refs must validate, not just metric refs)", err)
	}
}

func TestDeleteMetric(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, metricID, _ := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "temp", "is_input": true}))

	if _, _, err := exec.Execute(ctx, "delete_metric", mustJSON(t, map[string]any{"metric_id": metricID})); err != nil {
		t.Fatalf("delete_metric: %v", err)
	}

	var count int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.metric_def WHERE id=$1::uuid`, metricID).Scan(&count)
	if count != 0 {
		t.Fatal("expected metric row to be gone after delete")
	}
}

// ── create_dimension / add_dimension_member ─────────────────────────────────

func TestCreateDimension_SameDimensionHierarchy(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, dimID, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{
		"name": "Department",
		"members": []map[string]any{
			{"code": "ENG", "label": "Engineering"},
			{"code": "ENG_BE", "label": "Backend", "parent_code": "ENG"},
		},
	}))
	if err != nil {
		t.Fatalf("create_dimension: %v", err)
	}

	var parentCode *string
	if err := pool.QueryRow(ctx, `
		SELECT pm.code FROM model.dimension_member m
		LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE m.dimension_id=$1::uuid AND m.code='ENG_BE'
	`, dimID).Scan(&parentCode); err != nil {
		t.Fatalf("verify member hierarchy: %v", err)
	}
	if parentCode == nil || *parentCode != "ENG" {
		t.Fatalf("expected ENG_BE's parent to be ENG, got %v", parentCode)
	}
}

// TestCreateDimension_CrossDimensionHierarchy is a regression test for the
// cross-dimension dimension-hierarchy feature (Cabinet child of Department):
// a new dimension's parent_dimension_id must be set, and its members' parent
// codes must resolve against the DECLARED PARENT dimension's members, not
// its own (empty) member set.
func TestCreateDimension_CrossDimensionHierarchy(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, deptID, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{
		"name":    "Department",
		"members": []map[string]any{{"code": "ENG", "label": "Engineering"}},
	}))
	if err != nil {
		t.Fatalf("create Department: %v", err)
	}

	_, cabinetID, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{
		"name":                  "Cabinet",
		"parent_dimension_name": "Department",
		"members": []map[string]any{
			{"code": "C1", "label": "Cabinet 1", "parent_code": "ENG"},
		},
	}))
	if err != nil {
		t.Fatalf("create Cabinet: %v", err)
	}

	var parentDimID *string
	if err := pool.QueryRow(ctx, `SELECT parent_dimension_id::text FROM model.dimension_def WHERE id=$1::uuid`, cabinetID).Scan(&parentDimID); err != nil {
		t.Fatalf("query parent_dimension_id: %v", err)
	}
	if parentDimID == nil || *parentDimID != deptID {
		t.Fatalf("expected Cabinet.parent_dimension_id=%s, got %v", deptID, parentDimID)
	}

	var memberParentCode *string
	if err := pool.QueryRow(ctx, `
		SELECT pm.code FROM model.dimension_member m
		LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE m.dimension_id=$1::uuid AND m.code='C1'
	`, cabinetID).Scan(&memberParentCode); err != nil {
		t.Fatalf("verify member cross-dim parent: %v", err)
	}
	if memberParentCode == nil || *memberParentCode != "ENG" {
		t.Fatalf("expected C1's parent to resolve to Department member ENG, got %v", memberParentCode)
	}
}

func TestAddDimensionMember_CrossDimensionParentResolution(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, _, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{
		"name":    "Department",
		"members": []map[string]any{{"code": "SALES", "label": "Sales"}},
	}))
	if err != nil {
		t.Fatalf("create Department: %v", err)
	}
	_, cabinetID, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{
		"name": "Cabinet", "parent_dimension_name": "Department",
	}))
	if err != nil {
		t.Fatalf("create Cabinet: %v", err)
	}

	// Adding a member after dimension creation must still resolve parent_code
	// against Department, not Cabinet's own (empty) member list.
	_, memberID, err := exec.Execute(ctx, "add_dimension_member", mustJSON(t, map[string]any{
		"dimension_id": cabinetID, "code": "C2", "label": "Cabinet 2", "parent_code": "SALES",
	}))
	if err != nil {
		t.Fatalf("add_dimension_member: %v", err)
	}
	if memberID == "" {
		t.Fatal("expected a created member ID")
	}

	// An unresolvable parent_code must be rejected, not silently create an
	// unparented member.
	_, _, err = exec.Execute(ctx, "add_dimension_member", mustJSON(t, map[string]any{
		"dimension_id": cabinetID, "code": "C3", "label": "Cabinet 3", "parent_code": "DOES_NOT_EXIST",
	}))
	if err == nil {
		t.Fatal("expected an error for an unresolvable parent_code")
	}
}

// TestAddDimensionMember_AutoCreatesMissingSameDimensionParent is a
// regression test for a file-import proposal (create dimension + add N
// members) that references a root/group value — e.g. "total" — as many
// members' parent without any step in the plan ever creating "total"
// itself, because the source file never lists it as its own row (it only
// ever appears in the parent column). Every add_dimension_member step
// referencing it used to fail with a raw "no rows in result set", and
// whether the whole batch worked was purely down to whether the LLM
// happened to also emit a create step for the parent — non-deterministic
// across otherwise-identical runs. Same-dimension parent lookups now
// auto-create the missing parent as a top-level member instead.
func TestAddDimensionMember_AutoCreatesMissingSameDimensionParent(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, dimID, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{"name": "dimension_1"}))
	if err != nil {
		t.Fatalf("create dimension: %v", err)
	}

	// "total" is never created — only ever referenced as a parent, exactly
	// like the failing run's plan.
	result, s1ID, err := exec.Execute(ctx, "add_dimension_member", mustJSON(t, map[string]any{
		"dimension_id": dimID, "code": "s1", "label": "s1", "parent_code": "total",
	}))
	if err != nil {
		t.Fatalf("add s1 under undeclared parent 'total': %v", err)
	}
	if s1ID == "" {
		t.Fatal("expected a created member ID for s1")
	}
	if !strings.Contains(result, "total") {
		t.Errorf("result message should mention the auto-created parent, got %q", result)
	}

	var totalCode string
	var totalParent *string
	if err := pool.QueryRow(ctx, `
		SELECT m.code, pm.code FROM model.dimension_member m
		LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE m.dimension_id=$1::uuid AND m.code='total'
	`, dimID).Scan(&totalCode, &totalParent); err != nil {
		t.Fatalf("'total' was not auto-created: %v", err)
	}
	if totalParent != nil {
		t.Errorf("auto-created 'total' should be top-level, got parent %v", *totalParent)
	}

	// A second member referencing the SAME now-existing parent must resolve
	// normally — not create a duplicate "total".
	_, s2ID, err := exec.Execute(ctx, "add_dimension_member", mustJSON(t, map[string]any{
		"dimension_id": dimID, "code": "s2", "label": "s2", "parent_code": "total",
	}))
	if err != nil {
		t.Fatalf("add s2 under the now-existing 'total': %v", err)
	}
	if s2ID == "" {
		t.Fatal("expected a created member ID for s2")
	}

	var totalCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='total'`, dimID).Scan(&totalCount); err != nil {
		t.Fatalf("count 'total' members: %v", err)
	}
	if totalCount != 1 {
		t.Errorf("'total' member count = %d, want exactly 1 (no duplicate auto-creation)", totalCount)
	}

	var s1Parent, s2Parent string
	if err := pool.QueryRow(ctx, `
		SELECT pm.code FROM model.dimension_member m JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE m.dimension_id=$1::uuid AND m.code='s1'`, dimID).Scan(&s1Parent); err != nil {
		t.Fatalf("s1 parent: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT pm.code FROM model.dimension_member m JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE m.dimension_id=$1::uuid AND m.code='s2'`, dimID).Scan(&s2Parent); err != nil {
		t.Fatalf("s2 parent: %v", err)
	}
	if s1Parent != "total" || s2Parent != "total" {
		t.Errorf("s1/s2 parents = %q/%q, want both 'total'", s1Parent, s2Parent)
	}
}

// ── create_grid / add_grid_metric / add_grid_dimension ──────────────────────

// A cross-revision metric reference with NO same-named counterpart in the
// working revision must be refused — there is nothing to remap it onto. (A
// reference WITH a counterpart is remapped instead; see
// TestCrossRevisionReferences_RemapIntoWorkingRevision.) The foreign metric
// is seeded via SQL because the create tools themselves now clamp to the
// executor's revision — an executor can no longer be talked into creating
// rows outside its own scope.
func TestAddGridMetric_RevisionMismatchRejected(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revA := seedRevision(t, pool, modelID, "Rev A")
	revB := seedRevision(t, pool, modelID, "Rev B")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revA)
	ctx := context.Background()

	_, gridID, err := exec.Execute(ctx, "create_grid", mustJSON(t, map[string]any{"name": "Grid A", "revision_id": revA}))
	if err != nil {
		t.Fatalf("create_grid: %v", err)
	}
	var metricID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, name, is_input, revision_id, agg_rule)
		VALUES ($1::uuid, 'rev_b_only', true, $2::uuid, 'sum') RETURNING id::text
	`, modelID, revB).Scan(&metricID); err != nil {
		t.Fatalf("seed metric in rev B: %v", err)
	}

	_, _, err = exec.Execute(ctx, "add_grid_metric", mustJSON(t, map[string]any{"grid_id": gridID, "metric_id": metricID}))
	if err == nil {
		t.Fatal("expected an error adding a metric from a different revision with no counterpart in the working revision")
	}
	if !strings.Contains(err.Error(), "no counterpart") {
		t.Fatalf("error should name the missing counterpart, got: %v", err)
	}
}

// The create tools clamp to the executor's revision: a proposal step naming
// another revision (the LLM echoes whatever revision IDs it has seen) must
// not create rows outside the session draft — that is exactly the isolation
// aiProposalConfirm promises. Observed live 2026-08-25: honoring caller
// revision IDs let a proposal mutate the ACTIVE revision.
func TestCreateMetric_ClampsToExecutorRevision(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revA := seedRevision(t, pool, modelID, "Rev A")
	revB := seedRevision(t, pool, modelID, "Rev B")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revA)
	ctx := context.Background()

	_, metricID, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "clamped", "is_input": true, "revision_id": revB,
	}))
	if err != nil {
		t.Fatalf("create_metric: %v", err)
	}
	var gotRev string
	if err := pool.QueryRow(ctx, `SELECT revision_id::text FROM model.metric_def WHERE id=$1::uuid`, metricID).Scan(&gotRev); err != nil {
		t.Fatalf("load metric: %v", err)
	}
	if gotRev != revA {
		t.Fatalf("metric landed in revision %s, want the executor's %s — the caller's revision_id must not win", gotRev, revA)
	}
}

// Cross-revision references WITH a counterpart resolve onto the working
// revision's copy instead of writing into the foreign revision. This is the
// draft-flow regression test for the 2026-08-25 incident: a draft is copied
// from the active revision, the LLM's proposal references the ACTIVE
// revision's UUIDs (that is what its read tools showed before the draft
// existed), and executing those references must mutate the DRAFT.
func TestCrossRevisionReferences_RemapIntoWorkingRevision(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revA := seedRevision(t, pool, modelID, "Rev A")
	execA := aiassistant.NewWriteExecutor(pool, modelID, revA)
	ctx := context.Background()

	_, gridA, err := execA.Execute(ctx, "create_grid", mustJSON(t, map[string]any{"name": "Plan"}))
	if err != nil {
		t.Fatalf("create_grid: %v", err)
	}
	_, metricA, err := execA.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "revenue", "is_input": true}))
	if err != nil {
		t.Fatalf("create_metric: %v", err)
	}

	// The draft: a copy of rev A, exactly as aiProposalConfirm creates it.
	_, revB, err := execA.Execute(ctx, "create_revision", mustJSON(t, map[string]any{
		"name": "Draft", "source_revision_id": revA,
	}))
	if err != nil {
		t.Fatalf("create_revision: %v", err)
	}
	execB := aiassistant.NewWriteExecutor(pool, modelID, revB)

	// Reference rev A's grid and metric from the draft-scoped executor.
	if _, _, err := execB.Execute(ctx, "add_grid_metric", mustJSON(t, map[string]any{
		"grid_id": gridA, "metric_id": metricA,
	})); err != nil {
		t.Fatalf("add_grid_metric with source-revision ids: %v", err)
	}

	// The membership must exist on the DRAFT's grid, and rev A must be
	// untouched — the exact opposite of the pre-fix behavior.
	var draftCount, sourceCount int
	_ = pool.QueryRow(ctx, `
		SELECT count(*) FROM model.grid_metric gm
		JOIN model.grid_def g ON g.id = gm.grid_id WHERE g.revision_id=$1::uuid
	`, revB).Scan(&draftCount)
	_ = pool.QueryRow(ctx, `
		SELECT count(*) FROM model.grid_metric gm
		JOIN model.grid_def g ON g.id = gm.grid_id WHERE g.revision_id=$1::uuid
	`, revA).Scan(&sourceCount)
	if draftCount != 1 {
		t.Errorf("draft grid memberships = %d, want 1 (the remapped add)", draftCount)
	}
	if sourceCount != 0 {
		t.Errorf("source grid memberships = %d, want 0 — the add must not leak into the source revision", sourceCount)
	}

	// Widget refs get the same treatment: a metric_kpi created with rev A's
	// metric UUID must store the draft's copy, or the widget renders blank.
	if _, _, err := execB.Execute(ctx, "create_dashboard", mustJSON(t, map[string]any{"name": "Board"})); err != nil {
		t.Fatalf("create_dashboard: %v", err)
	}
	var dashID string
	_ = pool.QueryRow(ctx, `SELECT id::text FROM model.dashboard_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Board'`, modelID, revB).Scan(&dashID)
	if _, _, err := execB.Execute(ctx, "add_dashboard_widget", mustJSON(t, map[string]any{
		"dashboard_id": dashID, "widget_type": "metric_kpi", "ref_id": metricA,
	})); err != nil {
		t.Fatalf("add_dashboard_widget with source-revision metric: %v", err)
	}
	var refRev string
	_ = pool.QueryRow(ctx, `
		SELECT COALESCE(m.revision_id::text,'') FROM model.dashboard_widget w
		JOIN model.metric_def m ON m.id::text = w.ref_id
		WHERE w.dashboard_id=$1::uuid
	`, dashID).Scan(&refRev)
	if refRev != revB {
		t.Errorf("widget ref resolves to revision %q, want the draft %s", refRev, revB)
	}
}

// The AI draft copy must keep rate metrics computable: agg operands are
// remapped by name onto the new revision's copies, mirroring Step A3 of the
// developer-console duplicate handler. Before 2026-08-25 they were dropped
// entirely, and every copied rate metric failed every recalculation.
func TestCreateRevision_RemapsRateOperands(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revA := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revA)
	ctx := context.Background()

	_, revenueA, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "revenue", "is_input": true}))
	if err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	_, unitsA, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "units", "is_input": true}))
	if err != nil {
		t.Fatalf("create units: %v", err)
	}
	if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "avg_price", "is_input": false, "formula": "{revenue} / {units}", "agg_rule": "rate",
		"agg_numerator_metric_id": revenueA, "agg_denominator_metric_id": unitsA,
	})); err != nil {
		t.Fatalf("create avg_price: %v", err)
	}

	_, revB, err := exec.Execute(ctx, "create_revision", mustJSON(t, map[string]any{
		"name": "Copy", "source_revision_id": revA,
	}))
	if err != nil {
		t.Fatalf("create_revision: %v", err)
	}

	var numName, denName string
	err = pool.QueryRow(ctx, `
		SELECT num.name, den.name
		FROM model.metric_def m
		JOIN model.metric_def num ON num.id = m.agg_numerator_metric_id
		JOIN model.metric_def den ON den.id = m.agg_denominator_metric_id
		WHERE m.model_id=$1::uuid AND m.revision_id=$2::uuid AND m.name='avg_price'
		  AND num.revision_id=$2::uuid AND den.revision_id=$2::uuid
	`, modelID, revB).Scan(&numName, &denName)
	if err != nil {
		t.Fatalf("copied avg_price has no in-revision rate operands (they were dropped or left pointing at the source revision): %v", err)
	}
	if numName != "revenue" || denName != "units" {
		t.Fatalf("rate operands remapped to (%s, %s), want (revenue, units)", numName, denName)
	}
}

// TestAddGridMetric_OneGridPerMetricRule is a regression test for the "a
// metric can only belong to one grid" constraint added this session.
func TestAddGridMetric_OneGridPerMetricRule(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, grid1, _ := exec.Execute(ctx, "create_grid", mustJSON(t, map[string]any{"name": "Grid 1", "revision_id": revID}))
	_, grid2, _ := exec.Execute(ctx, "create_grid", mustJSON(t, map[string]any{"name": "Grid 2", "revision_id": revID}))
	_, metricID, _ := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "m", "is_input": true, "revision_id": revID}))

	if _, _, err := exec.Execute(ctx, "add_grid_metric", mustJSON(t, map[string]any{"grid_id": grid1, "metric_id": metricID})); err != nil {
		t.Fatalf("add to grid1: %v", err)
	}
	_, _, err := exec.Execute(ctx, "add_grid_metric", mustJSON(t, map[string]any{"grid_id": grid2, "metric_id": metricID}))
	if err == nil {
		t.Fatal("expected an error adding a metric to a second grid")
	}

	var count int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.grid_metric WHERE metric_id=$1::uuid`, metricID).Scan(&count)
	if count != 1 {
		t.Fatalf("expected the metric to remain in exactly 1 grid, found %d", count)
	}
}

func TestCreateGrid_WithMetricsAndDimensions(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, metricID, _ := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "m", "is_input": true, "revision_id": revID}))
	_, dimID, _ := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{"name": "Department", "revision_id": revID}))

	_, gridID, err := exec.Execute(ctx, "create_grid", mustJSON(t, map[string]any{
		"name": "Sales Grid", "revision_id": revID,
		"metric_ids": []string{metricID}, "dimension_ids": []string{dimID},
	}))
	if err != nil {
		t.Fatalf("create_grid: %v", err)
	}

	var metricCount, dimCount int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.grid_metric WHERE grid_id=$1::uuid`, gridID).Scan(&metricCount)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.grid_dimension WHERE grid_id=$1::uuid`, gridID).Scan(&dimCount)
	if metricCount != 1 || dimCount != 1 {
		t.Fatalf("expected 1 grid_metric and 1 grid_dimension row, got %d and %d", metricCount, dimCount)
	}
}

// TestCreateGrid_SkipsMetricAlreadyInAnotherGrid is a regression test: a
// metric already belonging to another grid used to be silently dropped from
// the new grid's INSERT (ON CONFLICT DO NOTHING against grid_metric's
// UNIQUE(metric_id)) while the returned message still reported every
// requested metric as attached. It must now report the real attached count
// and name what was skipped, without corrupting the metric's existing
// grid membership.
func TestCreateGrid_SkipsMetricAlreadyInAnotherGrid(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, existingGrid, _ := exec.Execute(ctx, "create_grid", mustJSON(t, map[string]any{"name": "Existing Grid", "revision_id": revID}))
	_, metricID, _ := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "m", "is_input": true, "revision_id": revID}))
	_, freeMetricID, _ := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "m2", "is_input": true, "revision_id": revID}))
	if _, _, err := exec.Execute(ctx, "add_grid_metric", mustJSON(t, map[string]any{"grid_id": existingGrid, "metric_id": metricID})); err != nil {
		t.Fatalf("seed metric into existing grid: %v", err)
	}

	msg, newGridID, err := exec.Execute(ctx, "create_grid", mustJSON(t, map[string]any{
		"name": "New Grid", "revision_id": revID,
		"metric_ids": []string{metricID, freeMetricID},
	}))
	if err != nil {
		t.Fatalf("create_grid: %v", err)
	}
	if !strings.Contains(msg, "1/2 metrics attached") {
		t.Errorf("message = %q, want it to report 1/2 metrics attached", msg)
	}
	if !strings.Contains(msg, "Existing Grid") {
		t.Errorf("message = %q, want it to name the grid the conflicting metric already belongs to", msg)
	}

	var newGridCount, existingGridCount int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.grid_metric WHERE grid_id=$1::uuid AND metric_id=$2::uuid`, newGridID, metricID).Scan(&newGridCount)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.grid_metric WHERE grid_id=$1::uuid AND metric_id=$2::uuid`, existingGrid, metricID).Scan(&existingGridCount)
	if newGridCount != 0 {
		t.Errorf("conflicting metric landed in the new grid too (count=%d), want 0 — one-grid-per-metric violated", newGridCount)
	}
	if existingGridCount != 1 {
		t.Errorf("conflicting metric no longer in its original grid (count=%d), want 1", existingGridCount)
	}

	var freeMetricCount int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.grid_metric WHERE grid_id=$1::uuid AND metric_id=$2::uuid`, newGridID, freeMetricID).Scan(&freeMetricCount)
	if freeMetricCount != 1 {
		t.Errorf("the non-conflicting metric wasn't attached (count=%d), want 1", freeMetricCount)
	}
}

// ── create_dashboard / add_dashboard_widget ─────────────────────────────────

func TestCreateDashboardAndAddWidget(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, dashID, err := exec.Execute(ctx, "create_dashboard", mustJSON(t, map[string]any{"name": "Exec Summary", "revision_id": revID}))
	if err != nil {
		t.Fatalf("create_dashboard: %v", err)
	}

	_, widgetID, err := exec.Execute(ctx, "add_dashboard_widget", mustJSON(t, map[string]any{
		"dashboard_id": dashID, "widget_type": "text", "content": "Hello",
	}))
	if err != nil {
		t.Fatalf("add_dashboard_widget: %v", err)
	}
	if widgetID == "" {
		t.Fatal("expected a created widget ID")
	}

	var count int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.dashboard_widget WHERE dashboard_id=$1::uuid`, dashID).Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 widget row, got %d", count)
	}
}

// ── create_revision ───────────────────────────────────────────────────────

// TestCreateRevision_CopiesMetricsAndCrossDimensionHierarchy is a regression
// test for a real bug found this session: within a single multi-CTE SQL
// statement, an UPDATE cannot see rows an earlier INSERT in the SAME
// statement just created (Postgres statement-level snapshot isolation), so
// the dimension-hierarchy remap must run as a separate Exec after the
// structural copy commits. This test would have caught that bug.
func TestCreateRevision_CopiesMetricsAndCrossDimensionHierarchy(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	srcRev := seedRevision(t, pool, modelID, "Source")
	exec := aiassistant.NewWriteExecutor(pool, modelID, srcRev)
	ctx := context.Background()

	// Build: Department (dim) -> Cabinet (child dim) with a member under ENG,
	// plus a metric and a grid, all in the source revision.
	_, _, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{
		"name": "Department", "revision_id": srcRev,
		"members": []map[string]any{{"code": "ENG", "label": "Engineering"}},
	}))
	if err != nil {
		t.Fatalf("create Department: %v", err)
	}
	_, _, err = exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{
		"name": "Cabinet", "revision_id": srcRev, "parent_dimension_name": "Department",
		"members": []map[string]any{{"code": "C1", "label": "Cabinet 1", "parent_code": "ENG"}},
	}))
	if err != nil {
		t.Fatalf("create Cabinet: %v", err)
	}
	if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "headcount", "is_input": true, "revision_id": srcRev,
	})); err != nil {
		t.Fatalf("create metric: %v", err)
	}

	// Duplicate the revision.
	_, newRevID, err := exec.Execute(ctx, "create_revision", mustJSON(t, map[string]any{
		"name": "Copy", "source_revision_id": srcRev,
	}))
	if err != nil {
		t.Fatalf("create_revision: %v", err)
	}
	if newRevID == srcRev {
		t.Fatal("expected a distinct new revision ID")
	}

	// The metric must have been copied into the new revision.
	var metricCount int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='headcount'`,
		modelID, newRevID).Scan(&metricCount)
	if metricCount != 1 {
		t.Fatalf("expected headcount to be copied into the new revision, found %d rows", metricCount)
	}

	// The cross-dimension hierarchy must survive: Cabinet's parent_dimension_id
	// must point at the NEW revision's Department row (not stay null, and not
	// point at the old revision's Department).
	var newCabinetID, newDeptID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Department'`,
		modelID, newRevID).Scan(&newDeptID); err != nil {
		t.Fatalf("find copied Department: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Cabinet'`,
		modelID, newRevID).Scan(&newCabinetID); err != nil {
		t.Fatalf("find copied Cabinet: %v", err)
	}
	var parentDimID *string
	if err := pool.QueryRow(ctx, `SELECT parent_dimension_id::text FROM model.dimension_def WHERE id=$1::uuid`, newCabinetID).Scan(&parentDimID); err != nil {
		t.Fatalf("query copied parent_dimension_id: %v", err)
	}
	if parentDimID == nil {
		t.Fatal("expected copied Cabinet.parent_dimension_id to be set, got NULL — this is the exact bug found and fixed this session")
	}
	if *parentDimID != newDeptID {
		t.Fatalf("expected copied Cabinet.parent_dimension_id=%s (new Department), got %s", newDeptID, *parentDimID)
	}

	// And the member-level cross-dimension parent must also be remapped.
	var memberParentCode *string
	if err := pool.QueryRow(ctx, `
		SELECT pm.code FROM model.dimension_member m
		LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE m.dimension_id=$1::uuid AND m.code='C1'
	`, newCabinetID).Scan(&memberParentCode); err != nil {
		t.Fatalf("verify copied member hierarchy: %v", err)
	}
	if memberParentCode == nil || *memberParentCode != "ENG" {
		t.Fatalf("expected copied C1's parent to resolve to ENG in the new revision, got %v", memberParentCode)
	}
}

// TestCreateRevision_CopiesCalcDependencies is a regression test for a real
// bug found this session: create_revision (used by the AI Assistant to spin
// up every isolated draft) copied model.metric_def rows but never copied
// model.calc_dependency — so a copied calc metric kept its formula text but
// lost every dependency edge. The visible symptom was the Developer
// Console's Dependency Graph tab rendering a calc metric with no lines to
// the inputs its own formula clearly referenced, in any revision that had
// ever been created via the AI Assistant.
func TestCreateRevision_CopiesCalcDependencies(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	srcRev := seedRevision(t, pool, modelID, "Source")
	exec := aiassistant.NewWriteExecutor(pool, modelID, srcRev)
	ctx := context.Background()

	_, revenueID, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "revenue", "is_input": true, "revision_id": srcRev,
	}))
	if err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	_, headcountID, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "headcount", "is_input": true, "revision_id": srcRev,
	}))
	if err != nil {
		t.Fatalf("create headcount: %v", err)
	}
	if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "revenue_per_head", "is_input": false, "formula": "revenue / headcount", "revision_id": srcRev,
	})); err != nil {
		t.Fatalf("create revenue_per_head: %v", err)
	}

	_, newRevID, err := exec.Execute(ctx, "create_revision", mustJSON(t, map[string]any{
		"name": "Copy", "source_revision_id": srcRev,
	}))
	if err != nil {
		t.Fatalf("create_revision: %v", err)
	}

	var newCalcID, newRevenueID, newHeadcountID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='revenue_per_head'`,
		modelID, newRevID).Scan(&newCalcID); err != nil {
		t.Fatalf("find copied revenue_per_head: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='revenue'`,
		modelID, newRevID).Scan(&newRevenueID); err != nil {
		t.Fatalf("find copied revenue: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='headcount'`,
		modelID, newRevID).Scan(&newHeadcountID); err != nil {
		t.Fatalf("find copied headcount: %v", err)
	}

	rows, err := pool.Query(ctx, `SELECT depends_on_metric_id::text FROM model.calc_dependency WHERE metric_id=$1::uuid`, newCalcID)
	if err != nil {
		t.Fatalf("query calc_dependency for copied metric: %v", err)
	}
	defer rows.Close()
	deps := map[string]bool{}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		deps[id] = true
	}
	if !deps[newRevenueID] || !deps[newHeadcountID] {
		t.Fatalf("copied revenue_per_head has no dependency edges to the copied revenue/headcount metrics (got deps: %v) — "+
			"the source revision's own dependency edges must not leak across (they reference old-revision IDs)", deps)
	}

	// The edges must point at the NEW revision's metric IDs, not the source
	// revision's — a leaked old-revision edge would silently break once the
	// source revision is later archived/deleted.
	oldRevenueID := revenueID
	oldHeadcountID := headcountID
	if deps[oldRevenueID] || deps[oldHeadcountID] {
		t.Fatalf("copied metric's dependency edges still reference the SOURCE revision's metric IDs: %v", deps)
	}
}

// TestCreateRevision_CopiesFormsAndWorkflows is a regression test for a real
// gap found this session: create_revision copied metrics/dimensions/grids/
// dashboards into a new draft but never forms or workflow defs (+ their
// automation rules) — so a hypothetical update tool for either could only
// ever find rows created earlier in the same AI session, never ones that
// predated it. Seeds a form and a published workflow+rule directly via the
// same stores the manual developer-console handlers use (no AI write tool
// for either exists yet at this point in the plan), then asserts both are
// present — by name, with the new revision's ID — after create_revision.
func TestCreateRevision_CopiesFormsAndWorkflows(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	srcRev := seedRevision(t, pool, modelID, "Source")
	ctx := context.Background()

	var appID string
	if err := pool.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID); err != nil {
		t.Fatalf("resolve application id: %v", err)
	}

	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email) VALUES ('seed-user', 'seed@test.dev') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	form, err := crudapp.NewStore(pool).CreateForm(ctx, modelID, srcRev, "expenses", "Expenses", nil)
	if err != nil {
		t.Fatalf("seed form: %v", err)
	}

	ws := workflow.NewStore(pool)
	wfDef, err := ws.CreateWorkflowDefFull(ctx, appID, srcRev, "Approve Expense", "", "manual", userID)
	if err != nil {
		t.Fatalf("seed workflow def: %v", err)
	}
	steps, _ := json.Marshal([]map[string]any{
		{"id": "s1", "name": "Notify", "type": "notification", "config": map[string]any{"subject": "hi", "message": "hi"}},
	})
	if _, err := ws.UpdateWorkflowDefFull(ctx, wfDef.ID, wfDef.Name, wfDef.Description, wfDef.TriggerEvent, "none", userID, steps, []byte("[]"), []byte("{}")); err != nil {
		t.Fatalf("set workflow steps: %v", err)
	}
	if _, err := ws.PublishWorkflowDef(ctx, wfDef.ID, userID); err != nil {
		t.Fatalf("publish workflow def: %v", err)
	}
	if _, err := ws.CreateAutomationRule(ctx, appID, srcRev, "Auto-approve", "", "manual", "Approve Expense", wfDef.ID, form.ID, "", nil); err != nil {
		t.Fatalf("seed automation rule: %v", err)
	}

	exec := aiassistant.NewWriteExecutor(pool, modelID, srcRev)
	_, newRevID, err := exec.Execute(ctx, "create_revision", mustJSON(t, map[string]any{
		"name": "Copy", "source_revision_id": srcRev,
	}))
	if err != nil {
		t.Fatalf("create_revision: %v", err)
	}

	var formCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model.form_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='expenses'`,
		modelID, newRevID).Scan(&formCount); err != nil {
		t.Fatalf("query copied form: %v", err)
	}
	if formCount != 1 {
		t.Fatalf("expected the source form to be copied into the new revision, found %d rows", formCount)
	}

	var newWfID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM workflow.workflow_def WHERE application_id=$1::uuid AND revision_id=$2::uuid AND name='Approve Expense'`,
		appID, newRevID).Scan(&newWfID); err != nil {
		t.Fatalf("query copied workflow def: %v", err)
	}

	var ruleWorkflowDefID, ruleSourceFormID *string
	if err := pool.QueryRow(ctx, `
		SELECT workflow_def_id::text, source_form_id::text FROM workflow.automation_rule
		WHERE application_id=$1::uuid AND revision_id=$2::uuid AND name='Auto-approve'
	`, appID, newRevID).Scan(&ruleWorkflowDefID, &ruleSourceFormID); err != nil {
		t.Fatalf("query copied automation rule: %v", err)
	}
	if ruleWorkflowDefID == nil || *ruleWorkflowDefID != newWfID {
		t.Fatalf("copied automation rule's workflow_def_id = %v, want the copied workflow def %s", ruleWorkflowDefID, newWfID)
	}
	if ruleSourceFormID == nil {
		t.Fatal("copied automation rule's source_form_id is NULL, want it remapped to the copied form")
	}
}

// TestCreateRevision_CopiesFactsPropertiesIntegrationsAndFolders is a
// parity test for the "eliminate real gaps" pass: this AI-draft path used
// to never copy runtime.fact_input, model.dimension_property, or
// model.integration_def at all, never copied dashboard folders or
// dashboard.category/folder_id, dropped dimension_def.source_property and
// grid_dimension.display_level on copy, never remapped
// rollup_source_grid_id, and copied dashboard_widget without title/
// show_title or a ref_id remap — all mirrored from the (now also fixed)
// developer-console duplicateRevision handler in this same pass.
func TestCreateRevision_CopiesFactsPropertiesIntegrationsAndFolders(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	srcRev := seedRevision(t, pool, modelID, "Source")
	ctx := context.Background()

	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email) VALUES ('seed-user-2', 'seed2@test.dev') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	var dimID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, name, agg_rule, revision_id, source_property)
		VALUES ($1::uuid, 'Department', 'sum', $2::uuid, 'dept_code') RETURNING id::text
	`, modelID, srcRev).Scan(&dimID); err != nil {
		t.Fatalf("seed dimension: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model.dimension_property (dimension_id, name, data_type) VALUES ($1::uuid, 'region', 'text')`, dimID); err != nil {
		t.Fatalf("seed dimension property: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'APAC', 'Asia Pacific')`, dimID); err != nil {
		t.Fatalf("seed dimension member: %v", err)
	}

	var metricID string
	if err := pool.QueryRow(ctx, `INSERT INTO model.metric_def (model_id, name, is_input, agg_rule, revision_id) VALUES ($1::uuid, 'Revenue', true, 'sum', $2::uuid) RETURNING id::text`,
		modelID, srcRev).Scan(&metricID); err != nil {
		t.Fatalf("seed metric: %v", err)
	}

	var gridID string
	if err := pool.QueryRow(ctx, `INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Revenue Grid', $2::uuid) RETURNING id::text`,
		modelID, srcRev).Scan(&gridID); err != nil {
		t.Fatalf("seed grid: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, 0)`, gridID, metricID); err != nil {
		t.Fatalf("seed grid metric: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model.grid_dimension (grid_id, dimension_id, display_level) VALUES ($1::uuid, $2::uuid, 2)`, gridID, dimID); err != nil {
		t.Fatalf("seed grid dimension: %v", err)
	}

	var rollupGridID string
	if err := pool.QueryRow(ctx, `INSERT INTO model.grid_def (model_id, name, revision_id, rollup_source_grid_id) VALUES ($1::uuid, 'Rollup Grid', $2::uuid, $3::uuid) RETURNING id::text`,
		modelID, srcRev, gridID).Scan(&rollupGridID); err != nil {
		t.Fatalf("seed rollup grid: %v", err)
	}

	sourceRefID := "11111111-1111-1111-1111-111111111111"
	var enteredAt time.Time
	if err := pool.QueryRow(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_name, metric_id, dim_members, value, entered_by, revision_id, source_ref, entered_at)
		VALUES ($1::uuid, 'Source', $2::uuid, jsonb_build_object($3::text, 'APAC'), 100, $4::uuid, $5::uuid, $6::uuid, now() - interval '2 days')
		RETURNING entered_at
	`, modelID, metricID, dimID, userID, srcRev, sourceRefID).Scan(&enteredAt); err != nil {
		t.Fatalf("seed fact_input: %v", err)
	}

	var intID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO model.integration_def (model_id, name, type, target_type, target_id, config, revision_id)
		VALUES ($1::uuid, 'CSV Import', 'csv_import', 'grid', $2::uuid, '{}'::jsonb, $3::uuid) RETURNING id::text
	`, modelID, gridID, srcRev).Scan(&intID); err != nil {
		t.Fatalf("seed integration: %v", err)
	}

	var folderID string
	if err := pool.QueryRow(ctx, `INSERT INTO model.dashboard_folder (model_id, name, revision_id) VALUES ($1::uuid, 'Reports', $2::uuid) RETURNING id::text`,
		modelID, srcRev).Scan(&folderID); err != nil {
		t.Fatalf("seed folder: %v", err)
	}

	var dashID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO model.dashboard_def (model_id, name, category, revision_id, folder_id)
		VALUES ($1::uuid, 'Revenue Overview', 'finance', $2::uuid, $3::uuid) RETURNING id::text
	`, modelID, srcRev, folderID).Scan(&dashID); err != nil {
		t.Fatalf("seed dashboard: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, title, show_title, sort_order)
		VALUES ($1::uuid, 'chart', $2, 'Revenue Chart', true, 0),
		       ($1::uuid, 'metric_kpi', $3, 'Revenue KPI', false, 1),
		       ($1::uuid, 'integration_button', $4, 'Import Revenue', true, 2)
	`, dashID, gridID, metricID, intID); err != nil {
		t.Fatalf("seed widgets: %v", err)
	}

	exec := aiassistant.NewWriteExecutor(pool, modelID, srcRev)
	_, newRevID, err := exec.Execute(ctx, "create_revision", mustJSON(t, map[string]any{
		"name": "Copy", "source_revision_id": srcRev,
	}))
	if err != nil {
		t.Fatalf("create_revision: %v", err)
	}

	var newDimID string
	var newSourceProperty *string
	if err := pool.QueryRow(ctx, `SELECT id::text, source_property FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Department'`,
		modelID, newRevID).Scan(&newDimID, &newSourceProperty); err != nil {
		t.Fatalf("query copied dimension: %v", err)
	}
	if newSourceProperty == nil || *newSourceProperty != "dept_code" {
		t.Errorf("copied dimension source_property = %v, want dept_code", newSourceProperty)
	}
	var propCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model.dimension_property WHERE dimension_id=$1::uuid AND name='region'`, newDimID).Scan(&propCount); err != nil {
		t.Fatalf("query copied dimension property: %v", err)
	}
	if propCount != 1 {
		t.Errorf("dimension_property not copied, count=%d, want 1", propCount)
	}

	var newGridID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Revenue Grid'`, modelID, newRevID).Scan(&newGridID); err != nil {
		t.Fatalf("query copied grid: %v", err)
	}
	var displayLevel int
	if err := pool.QueryRow(ctx, `SELECT display_level FROM model.grid_dimension WHERE grid_id=$1::uuid AND dimension_id=$2::uuid`, newGridID, newDimID).Scan(&displayLevel); err != nil {
		t.Fatalf("query copied grid_dimension: %v", err)
	}
	if displayLevel != 2 {
		t.Errorf("copied grid_dimension.display_level = %d, want 2", displayLevel)
	}

	var newRollupSourceID *string
	if err := pool.QueryRow(ctx, `SELECT rollup_source_grid_id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Rollup Grid'`, modelID, newRevID).Scan(&newRollupSourceID); err != nil {
		t.Fatalf("query copied rollup grid: %v", err)
	}
	if newRollupSourceID == nil || *newRollupSourceID != newGridID {
		t.Errorf("copied rollup grid's rollup_source_grid_id = %v, want the copied grid %s (not the source revision's grid)", newRollupSourceID, newGridID)
	}

	var newMetricID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Revenue'`, modelID, newRevID).Scan(&newMetricID); err != nil {
		t.Fatalf("query copied metric: %v", err)
	}
	var copiedSourceRef *string
	var copiedEnteredAt time.Time
	if err := pool.QueryRow(ctx, `SELECT source_ref::text, entered_at FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid`,
		modelID, newRevID, newMetricID).Scan(&copiedSourceRef, &copiedEnteredAt); err != nil {
		t.Fatalf("query copied fact_input: %v", err)
	}
	if copiedSourceRef == nil || *copiedSourceRef != sourceRefID {
		t.Errorf("copied fact_input.source_ref = %v, want %s", copiedSourceRef, sourceRefID)
	}
	if !copiedEnteredAt.Equal(enteredAt) {
		t.Errorf("copied fact_input.entered_at = %v, want %v", copiedEnteredAt, enteredAt)
	}

	var newIntID, newIntTargetID string
	if err := pool.QueryRow(ctx, `SELECT id::text, target_id::text FROM model.integration_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='CSV Import'`,
		modelID, newRevID).Scan(&newIntID, &newIntTargetID); err != nil {
		t.Fatalf("query copied integration: %v", err)
	}
	if newIntTargetID != newGridID {
		t.Errorf("copied integration target_id = %s, want the copied grid %s", newIntTargetID, newGridID)
	}

	var newDashID, newDashCategory string
	var newFolderID *string
	if err := pool.QueryRow(ctx, `SELECT id::text, category, folder_id::text FROM model.dashboard_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Revenue Overview'`,
		modelID, newRevID).Scan(&newDashID, &newDashCategory, &newFolderID); err != nil {
		t.Fatalf("query copied dashboard: %v", err)
	}
	if newDashCategory != "finance" {
		t.Errorf("copied dashboard category = %q, want finance", newDashCategory)
	}
	if newFolderID == nil {
		t.Fatal("copied dashboard folder_id is NULL, want it remapped to the copied folder")
	}
	var newFolderName string
	if err := pool.QueryRow(ctx, `SELECT name FROM model.dashboard_folder WHERE id=$1::uuid`, *newFolderID).Scan(&newFolderName); err != nil {
		t.Fatalf("query copied folder: %v", err)
	}
	if newFolderName != "Reports" {
		t.Errorf("copied dashboard's folder name = %q, want Reports", newFolderName)
	}

	type widgetRow struct {
		widgetType string
		title      *string
		showTitle  bool
		refID      *string
	}
	rows, err := pool.Query(ctx, `SELECT widget_type, title, show_title, ref_id FROM model.dashboard_widget WHERE dashboard_id=$1::uuid ORDER BY sort_order`, newDashID)
	if err != nil {
		t.Fatalf("query copied widgets: %v", err)
	}
	var widgets []widgetRow
	for rows.Next() {
		var w widgetRow
		if err := rows.Scan(&w.widgetType, &w.title, &w.showTitle, &w.refID); err != nil {
			rows.Close()
			t.Fatalf("scan widget: %v", err)
		}
		widgets = append(widgets, w)
	}
	rows.Close()
	if len(widgets) != 3 {
		t.Fatalf("expected 3 copied widgets, got %d", len(widgets))
	}
	if widgets[0].title == nil || *widgets[0].title != "Revenue Chart" || !widgets[0].showTitle {
		t.Errorf("chart widget title/show_title not copied: %+v", widgets[0])
	}
	if widgets[0].refID == nil || *widgets[0].refID != newGridID {
		t.Errorf("chart widget ref_id = %v, want remapped to copied grid %s", widgets[0].refID, newGridID)
	}
	if widgets[1].refID == nil || *widgets[1].refID != newMetricID {
		t.Errorf("metric_kpi widget ref_id = %v, want remapped to copied metric %s", widgets[1].refID, newMetricID)
	}
	if widgets[2].refID == nil || *widgets[2].refID != newIntID {
		t.Errorf("integration_button widget ref_id = %v, want remapped to copied integration %s", widgets[2].refID, newIntID)
	}
}

// TestCreateRevision_CopiesFormWithScalarFields is a regression test for a
// real bug the previous test's first run actually hit: crudapp.Store.CreateForm/
// UpdateForm marshal a nil []FormField to the JSON scalar `null`, not `[]` —
// a valid (non-SQL-NULL) jsonb value that the copy step's
// jsonb_array_elements(f.fields) then chokes on ("cannot extract elements
// from a scalar"), breaking create_revision for ANY model with a
// still-fieldless form. Fixed at the source (CreateForm/UpdateForm now
// normalize nil to []) and defensively in the copy query itself (a
// jsonb_typeof guard), since a raw INSERT bypassing the Go stores — or a
// row written before this fix shipped — could still carry the scalar form.
// This test targets the defensive guard specifically: it bypasses CreateForm
// with a raw insert to prove the copy step tolerates a pre-existing scalar
// value, not just the now-fixed application-level write path.
func TestCreateRevision_CopiesFormWithScalarFields(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	srcRev := seedRevision(t, pool, modelID, "Source")
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'legacy_form', 'Legacy Form', 'null'::jsonb)
	`, modelID, srcRev); err != nil {
		t.Fatalf("seed form with scalar fields: %v", err)
	}

	exec := aiassistant.NewWriteExecutor(pool, modelID, srcRev)
	_, newRevID, err := exec.Execute(ctx, "create_revision", mustJSON(t, map[string]any{
		"name": "Copy", "source_revision_id": srcRev,
	}))
	if err != nil {
		t.Fatalf("create_revision: %v (the jsonb_typeof defensive guard should tolerate a scalar fields value)", err)
	}

	var copiedFields []byte
	if err := pool.QueryRow(ctx, `SELECT fields FROM model.form_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='legacy_form'`,
		modelID, newRevID).Scan(&copiedFields); err != nil {
		t.Fatalf("query copied form: %v", err)
	}
	if string(copiedFields) != "[]" {
		t.Errorf("copied legacy form's fields = %s, want the scalar normalized to []", copiedFields)
	}
}

// TestCreateRevision_RollsBackOnMidCopyFailure is a regression test for a
// real gap found while adding the forms/workflows copy steps above:
// createRevision was a bare sequence of independent pool.Exec calls (several
// explicitly "best-effort"), so a failure partway through the copy — now a
// real possibility with more copy steps than before — could leave a
// genuinely inconsistent draft (some entities copied, others not) rather
// than no draft at all. Forces a deterministic real Postgres error in the
// LAST copy step (workflow context_schema must be a JSON array;
// jsonb_array_elements errors on a scalar) and asserts nothing from any
// EARLIER step — including the revision row itself — survives.
func TestCreateRevision_RollsBackOnMidCopyFailure(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	srcRev := seedRevision(t, pool, modelID, "Source")
	ctx := context.Background()

	var appID string
	if err := pool.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID); err != nil {
		t.Fatalf("resolve application id: %v", err)
	}

	exec := aiassistant.NewWriteExecutor(pool, modelID, srcRev)
	if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "headcount", "is_input": true, "revision_id": srcRev,
	})); err != nil {
		t.Fatalf("create metric: %v", err)
	}

	// Bypass CreateWorkflowDefFull (which always writes a valid JSON array)
	// with a raw insert carrying a malformed context_schema — a JSON string
	// instead of an array — so the copy step's own
	// jsonb_array_elements(wd.context_schema) genuinely fails in Postgres.
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, context_schema, revision_id)
		VALUES ($1::uuid, 'Broken', 'manual', '[]'::jsonb, '"not an array"'::jsonb, $2::uuid)
	`, appID, srcRev); err != nil {
		t.Fatalf("seed malformed workflow def: %v", err)
	}

	_, _, err := exec.Execute(ctx, "create_revision", mustJSON(t, map[string]any{
		"name": "Copy", "source_revision_id": srcRev,
	}))
	if err == nil {
		t.Fatal("expected create_revision to fail on the malformed workflow context_schema")
	}

	var newRevCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model.revision WHERE model_id=$1::uuid AND name='Copy'`, modelID).Scan(&newRevCount); err != nil {
		t.Fatalf("count revisions named Copy: %v", err)
	}
	if newRevCount != 0 {
		t.Fatalf("expected the failed create_revision to leave NO 'Copy' revision behind (transaction should roll back), found %d", newRevCount)
	}

	// Belt-and-suspenders: the metric copy (an earlier, otherwise-successful
	// step in the same transaction) must not have landed anywhere either.
	var strandedMetricCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid AND name='headcount' AND revision_id != $2::uuid
	`, modelID, srcRev).Scan(&strandedMetricCount); err != nil {
		t.Fatalf("count stranded metric copies: %v", err)
	}
	if strandedMetricCount != 0 {
		t.Fatalf("expected no copied 'headcount' metric to survive the rolled-back transaction, found %d", strandedMetricCount)
	}
}

// ── create_workflow_def / update_workflow_def / create_form_def / update_form_def ──

// seedActor creates an identity.user row and returns its ID.
// create_workflow_def/update_workflow_def need a real actor UUID
// (NewWriteExecutorWithActor) — CreateWorkflowDefFull/UpdateWorkflowDefFull
// cast created_by/updated_by directly to ::uuid with no NULLIF.
func seedActor(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var userID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO identity.user (keycloak_sub, email) VALUES ($1, $2) RETURNING id::text
	`, "actor-"+t.Name(), t.Name()+"@test.dev").Scan(&userID); err != nil {
		t.Fatalf("seed actor: %v", err)
	}
	return userID
}

func TestCreateWorkflowDef(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	userID := seedActor(t, pool)
	ctx := context.Background()

	exec := aiassistant.NewWriteExecutorWithActor(pool, modelID, revID, userID)
	_, newID, err := exec.Execute(ctx, "create_workflow_def", mustJSON(t, map[string]any{
		"name": "Manager Approval", "trigger_event": "manual",
	}))
	if err != nil {
		t.Fatalf("create_workflow_def: %v", err)
	}
	if newID == "" {
		t.Fatal("expected a created workflow def id")
	}

	var status, gotRevID, createdBy string
	if err := pool.QueryRow(ctx, `
		SELECT status, COALESCE(revision_id::text,''), COALESCE(created_by::text,'') FROM workflow.workflow_def WHERE id=$1::uuid
	`, newID).Scan(&status, &gotRevID, &createdBy); err != nil {
		t.Fatalf("query created workflow: %v", err)
	}
	if status != "draft" {
		t.Errorf("status = %q, want draft — a brand-new AI-created workflow must be inert until a human publishes it", status)
	}
	if gotRevID != revID {
		t.Errorf("revision_id = %q, want %q", gotRevID, revID)
	}
	if createdBy != userID {
		t.Errorf("created_by = %q, want %q", createdBy, userID)
	}
}

func TestCreateWorkflowDef_RequiresName(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	userID := seedActor(t, pool)

	exec := aiassistant.NewWriteExecutorWithActor(pool, modelID, revID, userID)
	_, _, err := exec.Execute(context.Background(), "create_workflow_def", mustJSON(t, map[string]any{}))
	if err == nil {
		t.Fatal("expected an error for a missing name")
	}
}

func TestUpdateWorkflowDef_PreservesUnsuppliedFieldsAndSetsSteps(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	userID := seedActor(t, pool)
	ctx := context.Background()

	exec := aiassistant.NewWriteExecutorWithActor(pool, modelID, revID, userID)
	_, wfID, err := exec.Execute(ctx, "create_workflow_def", mustJSON(t, map[string]any{
		"name": "Manager Approval", "description": "orig desc", "trigger_event": "manual",
	}))
	if err != nil {
		t.Fatalf("create_workflow_def: %v", err)
	}

	steps := []map[string]any{
		{"id": "s1", "name": "Approve", "type": "approval", "routes": map[string]string{"approve": "end-completed", "reject": "end-rejected"}},
	}
	if _, _, err := exec.Execute(ctx, "update_workflow_def", mustJSON(t, map[string]any{
		"workflow_def_id": wfID, "steps": steps,
	})); err != nil {
		t.Fatalf("update_workflow_def: %v", err)
	}

	var name, description, stepsJSON string
	if err := pool.QueryRow(ctx, `
		SELECT name, COALESCE(description,''), steps::text FROM workflow.workflow_def WHERE id=$1::uuid
	`, wfID).Scan(&name, &description, &stepsJSON); err != nil {
		t.Fatalf("query updated workflow: %v", err)
	}
	if name != "Manager Approval" {
		t.Errorf("name = %q, want unchanged 'Manager Approval' (not resupplied in the update call)", name)
	}
	if description != "orig desc" {
		t.Errorf("description = %q, want unchanged 'orig desc' (not resupplied in the update call)", description)
	}
	var gotSteps []map[string]any
	_ = json.Unmarshal([]byte(stepsJSON), &gotSteps)
	if len(gotSteps) != 1 {
		t.Fatalf("steps = %s, want 1 step applied", stepsJSON)
	}
}

func TestUpdateWorkflowDef_RejectsOutOfRevisionScope(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	otherRev := seedRevision(t, pool, modelID, "Other Rev")
	sessionRev := seedRevision(t, pool, modelID, "Session Rev")
	userID := seedActor(t, pool)
	ctx := context.Background()

	otherExec := aiassistant.NewWriteExecutorWithActor(pool, modelID, otherRev, userID)
	_, wfID, err := otherExec.Execute(ctx, "create_workflow_def", mustJSON(t, map[string]any{"name": "Other Rev Workflow"}))
	if err != nil {
		t.Fatalf("create_workflow_def in other revision: %v", err)
	}

	sessionExec := aiassistant.NewWriteExecutorWithActor(pool, modelID, sessionRev, userID)
	_, _, err = sessionExec.Execute(ctx, "update_workflow_def", mustJSON(t, map[string]any{
		"workflow_def_id": wfID, "description": "hijacked",
	}))
	if err == nil {
		t.Fatal("expected an error updating a workflow that belongs to a different revision")
	}
}

func TestCreateFormDef(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	ctx := context.Background()

	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	_, newID, err := exec.Execute(ctx, "create_form_def", mustJSON(t, map[string]any{
		"name": "expense_request", "label": "Expense Request",
		"fields": []map[string]any{
			{"name": "amount", "label": "Amount", "type": "number", "required": true},
		},
	}))
	if err != nil {
		t.Fatalf("create_form_def: %v", err)
	}
	if newID == "" {
		t.Fatal("expected a created form id")
	}

	var gotRevID string
	var fieldsJSON []byte
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(revision_id::text,''), fields FROM model.form_def WHERE id=$1::uuid
	`, newID).Scan(&gotRevID, &fieldsJSON); err != nil {
		t.Fatalf("query created form: %v", err)
	}
	if gotRevID != revID {
		t.Errorf("revision_id = %q, want %q", gotRevID, revID)
	}
	var fields []map[string]any
	_ = json.Unmarshal(fieldsJSON, &fields)
	if len(fields) != 1 {
		t.Fatalf("fields = %s, want 1 field", fieldsJSON)
	}
}

func TestUpdateFormDef_PreservesUnsuppliedFields(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	ctx := context.Background()

	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	_, formID, err := exec.Execute(ctx, "create_form_def", mustJSON(t, map[string]any{
		"name": "expense_request", "label": "Expense Request",
		"fields": []map[string]any{
			{"name": "amount", "label": "Amount", "type": "number", "required": true},
		},
	}))
	if err != nil {
		t.Fatalf("create_form_def: %v", err)
	}

	// Rename only — omit "fields" entirely, must not wipe the field list
	// (crudapp.UpdateForm has no COALESCE, unlike UpdateWorkflowDefFull).
	if _, _, err := exec.Execute(ctx, "update_form_def", mustJSON(t, map[string]any{
		"form_id": formID, "label": "Expense Claim",
	})); err != nil {
		t.Fatalf("update_form_def: %v", err)
	}

	var name, label string
	var fieldsJSON []byte
	if err := pool.QueryRow(ctx, `SELECT name, label, fields FROM model.form_def WHERE id=$1::uuid`, formID).Scan(&name, &label, &fieldsJSON); err != nil {
		t.Fatalf("query updated form: %v", err)
	}
	if name != "expense_request" {
		t.Errorf("name = %q, want unchanged 'expense_request'", name)
	}
	if label != "Expense Claim" {
		t.Errorf("label = %q, want 'Expense Claim'", label)
	}
	var fields []map[string]any
	_ = json.Unmarshal(fieldsJSON, &fields)
	if len(fields) != 1 {
		t.Fatalf("fields = %s, want the original field preserved (1 field)", fieldsJSON)
	}
}

func TestUpdateFormDef_RejectsOutOfRevisionScope(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	otherRev := seedRevision(t, pool, modelID, "Other Rev")
	sessionRev := seedRevision(t, pool, modelID, "Session Rev")
	ctx := context.Background()

	otherExec := aiassistant.NewWriteExecutor(pool, modelID, otherRev)
	_, formID, err := otherExec.Execute(context.Background(), "create_form_def", mustJSON(t, map[string]any{"name": "other_rev_form", "label": "L"}))
	if err != nil {
		t.Fatalf("create_form_def in other revision: %v", err)
	}

	sessionExec := aiassistant.NewWriteExecutor(pool, modelID, sessionRev)
	_, _, err = sessionExec.Execute(ctx, "update_form_def", mustJSON(t, map[string]any{
		"form_id": formID, "label": "hijacked",
	}))
	if err == nil {
		t.Fatal("expected an error updating a form that belongs to a different revision")
	}
}

// ── misc ─────────────────────────────────────────────────────────────────

func TestExecute_UnknownTool(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)

	_, _, err := exec.Execute(context.Background(), "not_a_real_tool", mustJSON(t, map[string]any{}))
	if err == nil {
		t.Fatal("expected an error for an unknown tool name")
	}
}

// The assistant runs as the developer who opened it, and that developer's
// access was checked once — against the MODEL. Every tool ID after that arrives
// from the language model, so a tool naming a UUID from somewhere else must be
// refused here, exactly as requireResourceAccess refuses it on the HTTP
// endpoint behind the same action.
//
// update_metric and delete_metric were plain "WHERE id=$1" with no scoping at
// all: the assistant could edit or delete a metric in any model in any tenant,
// which is strictly more than the developer role it acts as.
func TestWriteExecutorRefusesResourcesFromAnotherModel(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	ctx := context.Background()

	// Two models in separate tenants — seedModel builds its own customer each
	// time, so "another model" here is genuinely another tenant's.
	mine := seedModel(t, pool)
	myRev := seedRevision(t, pool, mine, "Working")
	theirs := seedModel(t, pool)
	theirRev := seedRevision(t, pool, theirs, "Working")

	// One of each resource kind in the other tenant's model.
	seed := func(query string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, query, args...).Scan(&id); err != nil {
			t.Fatalf("seed: %v", err)
		}
		return id
	}
	theirMetric := seed(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid,$2::uuid,'secret_revenue',true) RETURNING id::text`, theirs, theirRev)
	theirDim := seed(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'secret_region') RETURNING id::text`, theirs, theirRev)
	theirGrid := seed(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'Secret Grid') RETURNING id::text`, theirs, theirRev)
	theirDash := seed(`INSERT INTO model.dashboard_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'Secret Dashboard') RETURNING id::text`, theirs, theirRev)

	// The executor is scoped to MY model, the way the handler builds it after
	// resolving the caller's access.
	e := aiassistant.NewWriteExecutor(pool, mine, myRev)

	for _, c := range []struct {
		tool   string
		params any
	}{
		{"update_metric", map[string]any{"metric_id": theirMetric, "name": "pwned"}},
		{"delete_metric", map[string]any{"metric_id": theirMetric}},
		{"add_dimension_member", map[string]any{"dimension_id": theirDim, "code": "X", "label": "X"}},
		{"add_grid_metric", map[string]any{"grid_id": theirGrid, "metric_id": theirMetric}},
		{"add_grid_dimension", map[string]any{"grid_id": theirGrid, "dimension_id": theirDim}},
		{"add_dashboard_widget", map[string]any{"dashboard_id": theirDash, "widget_type": "text", "content": "x"}},
	} {
		t.Run(c.tool, func(t *testing.T) {
			if _, _, err := e.Execute(ctx, c.tool, mustJSON(t, c.params)); err == nil {
				t.Fatalf("%s accepted a resource from another tenant's model", c.tool)
			}
		})
	}

	// Nothing was touched: the refusals are refusals, not partial writes.
	var name string
	if err := pool.QueryRow(ctx, `SELECT name FROM model.metric_def WHERE id=$1::uuid`, theirMetric).Scan(&name); err != nil {
		t.Fatalf("the other tenant's metric was deleted: %v", err)
	}
	if name != "secret_revenue" {
		t.Errorf("the other tenant's metric was renamed to %q", name)
	}
	for _, q := range []struct{ label, query string }{
		{"grid_metric", `SELECT count(*) FROM model.grid_metric WHERE grid_id=$1::uuid`},
		{"grid_dimension", `SELECT count(*) FROM model.grid_dimension WHERE grid_id=$1::uuid`},
	} {
		var n int
		if err := pool.QueryRow(ctx, q.query, theirGrid).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", q.label, err)
		}
		if n != 0 {
			t.Errorf("%d %s row(s) were written into another tenant's grid", n, q.label)
		}
	}
	var members, widgets int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.dimension_member WHERE dimension_id=$1::uuid`, theirDim).Scan(&members)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.dashboard_widget WHERE dashboard_id=$1::uuid`, theirDash).Scan(&widgets)
	if members != 0 {
		t.Errorf("%d member(s) were added to another tenant's dimension", members)
	}
	if widgets != 0 {
		t.Errorf("%d widget(s) were added to another tenant's dashboard", widgets)
	}
}

// The mirror image: the same tools must still work on this model's own
// resources. A guard that refused everything would pass the test above and
// break the product.
func TestWriteExecutorStillAcceptsItsOwnModel(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	ctx := context.Background()

	mine := seedModel(t, pool)
	myRev := seedRevision(t, pool, mine, "Working")
	e := aiassistant.NewWriteExecutor(pool, mine, myRev)

	metricID, _, err := e.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "revenue", "is_input": true,
	}))
	_ = metricID
	if err != nil {
		t.Fatalf("create_metric on own model: %v", err)
	}
	var ownMetric string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND name='revenue'`, mine).Scan(&ownMetric); err != nil {
		t.Fatalf("read back own metric: %v", err)
	}
	if _, _, err := e.Execute(ctx, "update_metric", mustJSON(t, map[string]any{
		"metric_id": ownMetric, "name": "revenue_renamed",
	})); err != nil {
		t.Fatalf("update_metric on own model: %v", err)
	}
	if _, _, err := e.Execute(ctx, "delete_metric", mustJSON(t, map[string]any{
		"metric_id": ownMetric,
	})); err != nil {
		t.Fatalf("delete_metric on own model: %v", err)
	}
}

// The AI Developer's formula checking must match the developer role's exactly.
// It used to be its own hand-rolled thing that split a formula on operators
// and treated each piece as a metric name, which was wrong in both directions
// at once: a developer could save formulas the AI rejected (anything with a
// function call or a legacy {reference}, since IFERROR and {revenue} are not
// metric names), and the AI could save formulas a developer is refused
// (unparseable ones, unknown functions, self-reference), because a formula
// that fails to parse yields no references and so passes a
// "do all references exist" loop by having none.
//
// Both now run internal/metricformula. These tests pin that, because the two
// paths drifting apart again is invisible until someone builds with the AI.
func TestAIFormulaRulesMatchTheDeveloperRole(t *testing.T) {
	ctx := context.Background()
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)

	for _, name := range []string{"revenue", "cost"} {
		if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
			"name": name, "is_input": true,
		})); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	t.Run("accepts what a developer can write", func(t *testing.T) {
		accepted := []struct{ name, formula string }{
			{"margin_plain", "=revenue - cost"},
			{"margin_braced", "={revenue} - {cost}"},
			{"margin_fn", "=IFERROR({revenue} / {cost}, 0)"},
			{"margin_nested", "=ROUND(ABS({revenue} - {cost}) / 2, 1)"},
		}
		for _, c := range accepted {
			t.Run(c.name, func(t *testing.T) {
				id, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
					"name": c.name, "is_input": false, "formula": c.formula,
				}))
				if err != nil {
					t.Fatalf("%s rejected %q: %v", c.name, c.formula, err)
				}
				_ = id
				// The dependency edges must be wired too — a metric that
				// saves with no edges recalculates in the wrong order and is
				// wrong in a way nothing reports.
				var edges int
				if err := pool.QueryRow(ctx, `
					SELECT count(*) FROM model.calc_dependency cd
					JOIN model.metric_def m ON m.id = cd.metric_id
					WHERE m.name=$1 AND m.model_id=$2::uuid`, c.name, modelID).Scan(&edges); err != nil {
					t.Fatalf("count edges: %v", err)
				}
				if edges != 2 {
					t.Errorf("%s has %d dependency edge(s), want 2 (revenue, cost)", c.name, edges)
				}
			})
		}
	})

	t.Run("refuses what a developer is refused", func(t *testing.T) {
		refused := []struct{ name, formula, because string }{
			{"unparseable", "=revenue - / cost", "does not parse"},
			{"unknown_fn", "=TOTAL(revenue)", "calls a function that does not exist"},
			{"self_ref", "=self_ref + revenue", "references the metric it defines"},
			{"unknown_ref", "=revenue - nonexistent_thing", "references a name that is not a metric or dimension"},
			// The same two without the leading "=". These are the cases that
			// prove the AI is not MORE permissive than a developer: the old
			// check rejected the "=" forms only by accident, because it read
			// "=TOTAL" as one unknown name. Strip the "=" and every token is
			// a real metric, so the old check passed them both.
			{"unparseable_bare", "revenue - / cost", "does not parse"},
			{"unknown_fn_bare", "TOTAL(revenue)", "calls a function that does not exist"},
		}
		for _, c := range refused {
			t.Run(c.name, func(t *testing.T) {
				_, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
					"name": c.name, "is_input": false, "formula": c.formula,
				}))
				if err == nil {
					t.Fatalf("accepted %q, but it %s — the developer role refuses this", c.formula, c.because)
				}
				var n int
				_ = pool.QueryRow(ctx,
					`SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid AND name=$2`,
					modelID, c.name).Scan(&n)
				if n != 0 {
					t.Errorf("%q was rejected but %d row(s) were written anyway", c.name, n)
				}
			})
		}
	})

	// Aggregation rules were written straight to the column with no check of
	// any kind, so the AI could save a rule the engine does not implement and
	// a "rate" with nothing to divide. Neither fails on save: the first
	// silently sums, and the second fails in the scheduler on every
	// recalculation, a long way from where it was authored.
	t.Run("aggregation rules match the developer role", func(t *testing.T) {
		_, revenueID, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
			"name": "agg_revenue", "is_input": true,
		}))
		if err != nil {
			t.Fatalf("seed agg_revenue: %v", err)
		}
		_, unitsID, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
			"name": "agg_units", "is_input": true,
		}))
		if err != nil {
			t.Fatalf("seed agg_units: %v", err)
		}

		refused := []struct {
			name    string
			params  map[string]any
			because string
		}{
			{"unknown_rule", map[string]any{
				"name": "unknown_rule", "is_input": true, "agg_rule": "median",
			}, "median is not a rule the engine implements"},
			{"rate_without_operands", map[string]any{
				"name": "rate_without_operands", "is_input": true, "agg_rule": "rate",
			}, "a rate has nothing to divide without both operands"},
			{"rate_by_itself", map[string]any{
				"name": "rate_by_itself", "is_input": true, "agg_rule": "rate",
				"agg_numerator_metric_id": revenueID, "agg_denominator_metric_id": revenueID,
			}, "dividing a metric by itself is always 1"},
			{"formula_rule_on_input", map[string]any{
				"name": "formula_rule_on_input", "is_input": true, "agg_rule": "formula",
			}, "an input metric has no formula to re-evaluate at the total level"},
		}
		for _, c := range refused {
			t.Run(c.name, func(t *testing.T) {
				if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, c.params)); err == nil {
					t.Fatalf("accepted %s: %s — the developer role refuses this", c.name, c.because)
				}
				var n int
				_ = pool.QueryRow(ctx,
					`SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid AND name=$2`,
					modelID, c.name).Scan(&n)
				if n != 0 {
					t.Errorf("%s was rejected but %d row(s) were written anyway", c.name, n)
				}
			})
		}

		// And the legitimate case still works, with both operands stored.
		t.Run("accepts a well-formed rate", func(t *testing.T) {
			_, id, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
				"name": "agg_avg_price", "is_input": false, "formula": "=agg_revenue / agg_units",
				"agg_rule": "rate", "agg_numerator_metric_id": revenueID,
				"agg_denominator_metric_id": unitsID,
			}))
			if err != nil {
				t.Fatalf("rejected a well-formed rate metric: %v", err)
			}
			var num, den string
			if err := pool.QueryRow(ctx, `
				SELECT COALESCE(agg_numerator_metric_id::text,''), COALESCE(agg_denominator_metric_id::text,'')
				FROM model.metric_def WHERE id=$1::uuid`, id).Scan(&num, &den); err != nil {
				t.Fatalf("read operands: %v", err)
			}
			if num != revenueID || den != unitsID {
				t.Errorf("operands stored as %q / %q, want %q / %q", num, den, revenueID, unitsID)
			}
		})
	})

	// A cycle needs two metrics that already exist, so it does not fit the
	// table above. update_metric is also the path that never validated at all.
	t.Run("refuses a cycle through update_metric", func(t *testing.T) {
		_, aID, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
			"name": "cyc_a", "is_input": false, "formula": "=revenue * 2",
		}))
		if err != nil {
			t.Fatalf("create cyc_a: %v", err)
		}
		if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
			"name": "cyc_b", "is_input": false, "formula": "=cyc_a + 1",
		})); err != nil {
			t.Fatalf("create cyc_b: %v", err)
		}
		// cyc_b depends on cyc_a; pointing cyc_a at cyc_b closes the loop.
		_, _, err = exec.Execute(ctx, "update_metric", mustJSON(t, map[string]any{
			"metric_id": aID, "name": "cyc_a", "formula": "=cyc_b + 1",
		}))
		if err == nil {
			t.Fatal("accepted a formula that closes a dependency cycle")
		}
		if !strings.Contains(err.Error(), "cycle") {
			t.Errorf("error does not mention the cycle: %v", err)
		}
	})
}

// TestNameBasedReferenceResolution: LLMs routinely pass a resource NAME
// where the tool schema says id ("dimension_id": "products" — seen live,
// failing 'not found in this model'). requireInModel now resolves non-UUID
// references by name within the working revision; unknown names still fail
// with a pointed message.
func TestNameBasedReferenceResolution(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, _, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{"name": "products"}))
	if err != nil {
		t.Fatalf("create products: %v", err)
	}

	// The exact live failure: dimension referenced by NAME.
	_, memberID, err := exec.Execute(ctx, "add_dimension_member", mustJSON(t, map[string]any{
		"dimension_id": "products", "code": "TOTAL", "label": "TOTAL",
	}))
	if err != nil {
		t.Fatalf("add_dimension_member by dimension NAME: %v", err)
	}
	if memberID == "" {
		t.Fatal("expected a created member ID")
	}

	// Unknown name still fails, with the name echoed for the model to fix.
	_, _, err = exec.Execute(ctx, "add_dimension_member", mustJSON(t, map[string]any{
		"dimension_id": "no_such_dimension", "code": "X", "label": "X",
	}))
	if err == nil {
		t.Fatal("unknown dimension name must be rejected")
	}
}

// ── update_dimension_member ──────────────────────────────────────────────────

// TestUpdateDimensionMember_ReparentByCategoryProperty is the exact live
// request that exposed the gap ("in product dimension if member category is
// hardware or software move this member as child of the appropriate member"):
// the AI had add_dimension_member but no way to MOVE an existing member, and
// list_dimensions never showed properties, so it couldn't even see the
// category. This covers the whole loop: properties visible in the read tool,
// then a re-parent + property merge via the new write tool.
func TestUpdateDimensionMember_ReparentByCategoryProperty(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, dimID, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{
		"name": "Products",
		"members": []map[string]any{
			{"code": "HARDWARE", "label": "Hardware"},
			{"code": "SOFTWARE", "label": "Software"},
			{"code": "ITEM_1", "label": "Item 1"},
		},
	}))
	if err != nil {
		t.Fatalf("create Products: %v", err)
	}

	// Give ITEM_1 its category property via the new tool's merge path.
	if _, _, err := exec.Execute(ctx, "update_dimension_member", mustJSON(t, map[string]any{
		"dimension_id": dimID, "code": "ITEM_1", "properties": map[string]string{"category": "hardware"},
	})); err != nil {
		t.Fatalf("merge properties: %v", err)
	}

	// The read tool must show the property — this is what lets the AI decide
	// which parent each member belongs under.
	reader := aiassistant.NewToolExecutor(pool, modelID, revID)
	listing, err := reader.Execute(ctx, "list_dimensions", nil)
	if err != nil {
		t.Fatalf("list_dimensions: %v", err)
	}
	if !strings.Contains(listing, "{category=hardware}") {
		t.Fatalf("list_dimensions must render member properties; got:\n%s", listing)
	}

	// Re-parent by that category.
	result, _, err := exec.Execute(ctx, "update_dimension_member", mustJSON(t, map[string]any{
		"dimension_id": dimID, "code": "ITEM_1", "parent_code": "HARDWARE",
	}))
	if err != nil {
		t.Fatalf("re-parent: %v", err)
	}
	if !strings.Contains(result, "parent → HARDWARE") {
		t.Fatalf("unexpected result: %q", result)
	}
	var parentCode string
	if err := pool.QueryRow(ctx, `
		SELECT pm.code FROM model.dimension_member m
		JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE m.dimension_id=$1::uuid AND m.code='ITEM_1'
	`, dimID).Scan(&parentCode); err != nil || parentCode != "HARDWARE" {
		t.Fatalf("ITEM_1's parent = %q (err %v), want HARDWARE", parentCode, err)
	}

	// And the listing reflects the move.
	listing, _ = reader.Execute(ctx, "list_dimensions", nil)
	if !strings.Contains(listing, "ITEM_1 (Item 1) → parent: HARDWARE {category=hardware}") {
		t.Fatalf("listing must show the new parent and the property; got:\n%s", listing)
	}
}

func TestUpdateDimensionMember_RejectsCycleAndMissingParent(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, dimID, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{
		"name": "Products",
		"members": []map[string]any{
			{"code": "ALL", "label": "All"},
			{"code": "HW", "label": "Hardware", "parent_code": "ALL"},
			{"code": "LAPTOP", "label": "Laptop", "parent_code": "HW"},
		},
	}))
	if err != nil {
		t.Fatalf("create Products: %v", err)
	}

	// Moving ALL under its own grandchild must be refused.
	if _, _, err := exec.Execute(ctx, "update_dimension_member", mustJSON(t, map[string]any{
		"dimension_id": dimID, "code": "ALL", "parent_code": "LAPTOP",
	})); err == nil {
		t.Fatal("expected a cycle error moving ALL under its own descendant")
	}

	// Unknown parent is an error (no auto-create on update).
	if _, _, err := exec.Execute(ctx, "update_dimension_member", mustJSON(t, map[string]any{
		"dimension_id": dimID, "code": "LAPTOP", "parent_code": "NOPE",
	})); err == nil {
		t.Fatal("expected an error for an unknown parent_code")
	}

	// clear_parent makes it top-level.
	if _, _, err := exec.Execute(ctx, "update_dimension_member", mustJSON(t, map[string]any{
		"dimension_id": dimID, "code": "LAPTOP", "clear_parent": true,
	})); err != nil {
		t.Fatalf("clear_parent: %v", err)
	}
	var hasParent bool
	if err := pool.QueryRow(ctx, `
		SELECT parent_member_id IS NOT NULL FROM model.dimension_member
		WHERE dimension_id=$1::uuid AND code='LAPTOP'
	`, dimID).Scan(&hasParent); err != nil || hasParent {
		t.Fatalf("LAPTOP still has a parent after clear_parent (err %v)", err)
	}

	// A no-op call (nothing to change) is rejected loudly, not silently OK.
	if _, _, err := exec.Execute(ctx, "update_dimension_member", mustJSON(t, map[string]any{
		"dimension_id": dimID, "code": "LAPTOP",
	})); err == nil {
		t.Fatal("expected an error when no change is requested")
	}
}
