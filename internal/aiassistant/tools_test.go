package aiassistant_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

func TestValidateFormulas_AllValid(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	writer := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	if _, _, err := writer.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "revenue", "is_input": true})); err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	if _, _, err := writer.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "headcount", "is_input": true})); err != nil {
		t.Fatalf("create headcount: %v", err)
	}
	if _, _, err := writer.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "revenue_per_head", "is_input": false, "formula": "revenue / headcount",
	})); err != nil {
		t.Fatalf("create revenue_per_head: %v", err)
	}

	reader := aiassistant.NewToolExecutor(pool, modelID, revID)
	result, err := reader.Execute(ctx, "validate_formulas", nil)
	if err != nil {
		t.Fatalf("validate_formulas: %v", err)
	}
	if !strings.Contains(result, "All 1 calculated metric(s) have valid formulas") {
		t.Fatalf("unexpected result: %q", result)
	}
}

func TestValidateFormulas_NoCalculatedMetrics(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	writer := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	if _, _, err := writer.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "revenue", "is_input": true})); err != nil {
		t.Fatalf("create revenue: %v", err)
	}

	reader := aiassistant.NewToolExecutor(pool, modelID, revID)
	result, err := reader.Execute(ctx, "validate_formulas", nil)
	if err != nil {
		t.Fatalf("validate_formulas: %v", err)
	}
	if !strings.Contains(result, "All 0 calculated metric(s) have valid formulas") {
		t.Fatalf("unexpected result: %q", result)
	}
}

// TestValidateFormulas_DetectsBrokenReference exercises a real gap: delete_metric
// has no guard against deleting a metric other formulas still depend on, so a
// formula can go broken after creation without any single write step failing.
// validate_formulas is exactly the tool meant to catch that after the fact.
func TestValidateFormulas_DetectsBrokenReference(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	writer := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	if _, _, err := writer.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "revenue", "is_input": true})); err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	_, headcountID, err := writer.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "headcount", "is_input": true}))
	if err != nil {
		t.Fatalf("create headcount: %v", err)
	}
	if _, _, err := writer.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "revenue_per_head", "is_input": false, "formula": "revenue / headcount",
	})); err != nil {
		t.Fatalf("create revenue_per_head: %v", err)
	}

	if _, _, err := writer.Execute(ctx, "delete_metric", mustJSON(t, map[string]any{"metric_id": headcountID})); err != nil {
		t.Fatalf("delete headcount: %v", err)
	}

	reader := aiassistant.NewToolExecutor(pool, modelID, revID)
	result, err := reader.Execute(ctx, "validate_formulas", nil)
	if err != nil {
		t.Fatalf("validate_formulas: %v", err)
	}
	if !strings.Contains(result, "Found 1 of 1 calculated metric(s) with invalid formulas") {
		t.Fatalf("expected exactly 1 broken metric reported, got: %q", result)
	}
	// The reason comes from internal/metricformula now, so the diagnostic
	// reports exactly what the developer role's own save would have said.
	if !strings.Contains(result, "revenue_per_head:") ||
		!strings.Contains(result, `unknown metric or dimension "headcount"`) {
		t.Fatalf("expected revenue_per_head to be reported as missing headcount, got: %q", result)
	}
}

func TestValidateFormulas_UnknownToolStillRejected(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")

	reader := aiassistant.NewToolExecutor(pool, modelID, revID)
	_, err := reader.Execute(context.Background(), "not_a_real_tool", nil)
	if err == nil {
		t.Fatal("expected an error for an unknown read tool")
	}
}

func TestCheckGridCompleteness_NoGrids(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")

	reader := aiassistant.NewToolExecutor(pool, modelID, revID)
	result, err := reader.Execute(context.Background(), "check_grid_completeness", nil)
	if err != nil {
		t.Fatalf("check_grid_completeness: %v", err)
	}
	if !strings.Contains(result, "All 0 grid(s) have at least one metric and one dimension configured") {
		t.Fatalf("unexpected result: %q", result)
	}
}

func TestCheckGridCompleteness_AllComplete(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	writer := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, metricID, err := writer.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "revenue", "is_input": true}))
	if err != nil {
		t.Fatalf("create metric: %v", err)
	}
	_, dimID, err := writer.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{"name": "Department"}))
	if err != nil {
		t.Fatalf("create dimension: %v", err)
	}
	if _, _, err := writer.Execute(ctx, "create_grid", mustJSON(t, map[string]any{
		"name": "Sales Grid", "metric_ids": []string{metricID}, "dimension_ids": []string{dimID},
	})); err != nil {
		t.Fatalf("create grid: %v", err)
	}

	reader := aiassistant.NewToolExecutor(pool, modelID, revID)
	result, err := reader.Execute(ctx, "check_grid_completeness", nil)
	if err != nil {
		t.Fatalf("check_grid_completeness: %v", err)
	}
	if !strings.Contains(result, "All 1 grid(s) have at least one metric and one dimension configured") {
		t.Fatalf("unexpected result: %q", result)
	}
}

func TestCheckGridCompleteness_ReportsMissingPieces(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	writer := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()

	_, metricID, err := writer.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "revenue", "is_input": true}))
	if err != nil {
		t.Fatalf("create metric: %v", err)
	}
	_, dimID, err := writer.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{"name": "Department"}))
	if err != nil {
		t.Fatalf("create dimension: %v", err)
	}

	// Missing dimensions only.
	if _, _, err := writer.Execute(ctx, "create_grid", mustJSON(t, map[string]any{
		"name": "Metrics Only", "metric_ids": []string{metricID},
	})); err != nil {
		t.Fatalf("create grid 1: %v", err)
	}
	// Missing metrics only.
	if _, _, err := writer.Execute(ctx, "create_grid", mustJSON(t, map[string]any{
		"name": "Dims Only", "dimension_ids": []string{dimID},
	})); err != nil {
		t.Fatalf("create grid 2: %v", err)
	}
	// Missing both.
	if _, _, err := writer.Execute(ctx, "create_grid", mustJSON(t, map[string]any{
		"name": "Empty Grid",
	})); err != nil {
		t.Fatalf("create grid 3: %v", err)
	}

	reader := aiassistant.NewToolExecutor(pool, modelID, revID)
	result, err := reader.Execute(ctx, "check_grid_completeness", nil)
	if err != nil {
		t.Fatalf("check_grid_completeness: %v", err)
	}
	if !strings.Contains(result, "Found 3 of 3 grid(s) with missing configuration") {
		t.Fatalf("expected all 3 grids flagged, got: %q", result)
	}
	if !strings.Contains(result, "Metrics Only") || !strings.Contains(result, "missing dimensions") {
		t.Fatalf("expected 'Metrics Only' flagged as missing dimensions, got: %q", result)
	}
	if !strings.Contains(result, "Dims Only") || !strings.Contains(result, "missing metrics") {
		t.Fatalf("expected 'Dims Only' flagged as missing metrics, got: %q", result)
	}
	if !strings.Contains(result, "Empty Grid") || !strings.Contains(result, "missing metrics and dimensions") {
		t.Fatalf("expected 'Empty Grid' flagged as missing both, got: %q", result)
	}
}

func TestListWorkflows_Empty(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")

	reader := aiassistant.NewToolExecutor(pool, modelID, revID)
	result, err := reader.Execute(context.Background(), "list_workflows", nil)
	if err != nil {
		t.Fatalf("list_workflows: %v", err)
	}
	if result != "No workflows defined yet." {
		t.Fatalf("unexpected result: %q", result)
	}
}

func TestListWorkflows_ReportsNameTriggerStatusAndStepCount(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	userID := seedActor(t, pool)
	ctx := context.Background()

	writer := aiassistant.NewWriteExecutorWithActor(pool, modelID, revID, userID)
	_, wfID, err := writer.Execute(ctx, "create_workflow_def", mustJSON(t, map[string]any{
		"name": "Manager Approval", "trigger_event": "manual",
	}))
	if err != nil {
		t.Fatalf("create_workflow_def: %v", err)
	}
	steps := []map[string]any{
		{"id": "s1", "name": "Approve", "type": "approval", "routes": map[string]string{"approve": "end-completed"}},
	}
	if _, _, err := writer.Execute(ctx, "update_workflow_def", mustJSON(t, map[string]any{
		"workflow_def_id": wfID, "steps": steps,
	})); err != nil {
		t.Fatalf("update_workflow_def: %v", err)
	}

	reader := aiassistant.NewToolExecutor(pool, modelID, revID)
	result, err := reader.Execute(ctx, "list_workflows", nil)
	if err != nil {
		t.Fatalf("list_workflows: %v", err)
	}
	if !strings.Contains(result, "Manager Approval") || !strings.Contains(result, "trigger:manual") ||
		!strings.Contains(result, "status:draft") || !strings.Contains(result, "steps:1") {
		t.Fatalf("unexpected result: %q", result)
	}
}

func TestListForms_Empty(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")

	reader := aiassistant.NewToolExecutor(pool, modelID, revID)
	result, err := reader.Execute(context.Background(), "list_forms", nil)
	if err != nil {
		t.Fatalf("list_forms: %v", err)
	}
	if result != "No forms defined yet." {
		t.Fatalf("unexpected result: %q", result)
	}
}

func TestListForms_ReportsNameLabelAndFieldCount(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	ctx := context.Background()

	writer := aiassistant.NewWriteExecutor(pool, modelID, revID)
	if _, _, err := writer.Execute(ctx, "create_form_def", mustJSON(t, map[string]any{
		"name": "expense_request", "label": "Expense Request",
		"fields": []map[string]any{
			{"name": "amount", "label": "Amount", "type": "number", "required": true},
		},
	})); err != nil {
		t.Fatalf("create_form_def: %v", err)
	}

	reader := aiassistant.NewToolExecutor(pool, modelID, revID)
	result, err := reader.Execute(ctx, "list_forms", nil)
	if err != nil {
		t.Fatalf("list_forms: %v", err)
	}
	if !strings.Contains(result, "expense_request (id:") || !strings.Contains(result, "Expense Request) — 1 field(s)") {
		t.Fatalf("unexpected result: %q", result)
	}
}
