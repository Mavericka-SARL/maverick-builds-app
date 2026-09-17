package workflow_test

import (
	"context"
	"embed"
	"encoding/json"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

//go:embed testdata/*.sql
var testMigrations embed.FS

func setupDB(t *testing.T) (*workflow.Store, func()) {
	t.Helper()
	ctx := context.Background()

	pgc, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("mavericks"),
		tcpostgres.WithUsername("mavericks"),
		tcpostgres.WithPassword("mavericks"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}

	dsn, err := pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = pgc.Terminate(ctx)
		t.Fatalf("connection string: %v", err)
	}

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		_ = pgc.Terminate(ctx)
		t.Fatalf("connect: %v", err)
	}

	if err := migrate.Run(ctx, pool, testMigrations, "testdata"); err != nil {
		pool.Close()
		_ = pgc.Terminate(ctx)
		t.Fatalf("migrate: %v", err)
	}

	return workflow.NewStore(pool), func() {
		pool.Close()
		_ = pgc.Terminate(ctx)
	}
}

func insertFixtures(t *testing.T, store *workflow.Store) (userID, appID string) {
	t.Helper()
	ctx := context.Background()

	err := store.Pool().QueryRow(ctx, `
		INSERT INTO identity.user (email) VALUES ('test@example.com') RETURNING id::text
	`).Scan(&userID)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	var custID string
	err = store.Pool().QueryRow(ctx, `
		INSERT INTO core.customer (name) VALUES ('ACME') RETURNING id::text
	`).Scan(&custID)
	if err != nil {
		t.Fatalf("insert customer: %v", err)
	}

	var wsID string
	err = store.Pool().QueryRow(ctx, `
		INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws1') RETURNING id::text
	`, custID).Scan(&wsID)
	if err != nil {
		t.Fatalf("insert workspace: %v", err)
	}

	err = store.Pool().QueryRow(ctx, `
		INSERT INTO core.application (workspace_id, name) VALUES ($1::uuid, 'app1') RETURNING id::text
	`, wsID).Scan(&appID)
	if err != nil {
		t.Fatalf("insert application: %v", err)
	}

	return userID, appID
}

func TestCreateAndStartWorkflow(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	userID, appID := insertFixtures(t, store)

	steps := []*workflowv1.WorkflowStepDef{
		{Id: "step-1", Name: "Submit", Type: workflowv1.StepType_STEP_TYPE_TASK, SlaHours: 24},
		{Id: "step-2", Name: "Approve", Type: workflowv1.StepType_STEP_TYPE_APPROVAL},
	}

	def, err := store.CreateWorkflowDef(ctx, appID, "Budget Approval", "budget.submitted", steps)
	if err != nil {
		t.Fatalf("CreateWorkflowDef: %v", err)
	}
	if def.Id == "" {
		t.Fatal("expected non-empty def ID")
	}

	inst, err := store.StartWorkflow(ctx, def.Id, userID, map[string]string{"period": "2026-Q1"})
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if inst.Status != workflowv1.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
		t.Errorf("instance status = %v, want RUNNING", inst.Status)
	}

	fetchedInst, fetchedSteps, err := store.GetWorkflowInstance(ctx, inst.Id)
	if err != nil {
		t.Fatalf("GetWorkflowInstance: %v", err)
	}
	if fetchedInst.Id != inst.Id {
		t.Errorf("instance ID mismatch")
	}
	if len(fetchedSteps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(fetchedSteps))
	}
	if fetchedSteps[0].Status != workflowv1.StepStatus_STEP_STATUS_IN_PROGRESS {
		t.Errorf("first step status = %v, want IN_PROGRESS", fetchedSteps[0].Status)
	}
	if fetchedSteps[1].Status != workflowv1.StepStatus_STEP_STATUS_PENDING {
		t.Errorf("second step status = %v, want PENDING", fetchedSteps[1].Status)
	}
}

func TestCompleteStepAdvancesWorkflow(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	userID, appID := insertFixtures(t, store)

	steps := []*workflowv1.WorkflowStepDef{
		{Id: "step-a", Name: "Task", Type: workflowv1.StepType_STEP_TYPE_TASK},
		{Id: "step-b", Name: "Approve", Type: workflowv1.StepType_STEP_TYPE_APPROVAL},
	}
	def, _ := store.CreateWorkflowDef(ctx, appID, "Test Flow", "test.event", steps)
	inst, _ := store.StartWorkflow(ctx, def.Id, userID, nil)

	_, fetchedSteps, _ := store.GetWorkflowInstance(ctx, inst.Id)
	firstStepID := fetchedSteps[0].Id

	// Complete first step
	completedStep, err := store.CompleteStep(ctx, firstStepID, userID, "done", "looks good")
	if err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}
	if completedStep.Status != workflowv1.StepStatus_STEP_STATUS_COMPLETED {
		t.Errorf("step status = %v, want COMPLETED", completedStep.Status)
	}

	// Second step should now be in_progress
	_, updatedSteps, _ := store.GetWorkflowInstance(ctx, inst.Id)
	if updatedSteps[1].Status != workflowv1.StepStatus_STEP_STATUS_IN_PROGRESS {
		t.Errorf("second step status = %v, want IN_PROGRESS", updatedSteps[1].Status)
	}
}

func TestCompleteAllStepsClosesInstance(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	userID, appID := insertFixtures(t, store)

	steps := []*workflowv1.WorkflowStepDef{
		{Id: "only-step", Name: "Solo", Type: workflowv1.StepType_STEP_TYPE_TASK},
	}
	def, _ := store.CreateWorkflowDef(ctx, appID, "Single Step", "solo.event", steps)
	inst, _ := store.StartWorkflow(ctx, def.Id, userID, nil)

	_, fetchedSteps, _ := store.GetWorkflowInstance(ctx, inst.Id)
	_, err := store.CompleteStep(ctx, fetchedSteps[0].Id, userID, "approve", "")
	if err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}

	finalInst, _, _ := store.GetWorkflowInstance(ctx, inst.Id)
	if finalInst.Status != workflowv1.WorkflowStatus_WORKFLOW_STATUS_COMPLETED {
		t.Errorf("instance status = %v, want COMPLETED", finalInst.Status)
	}
}

func TestGetStepApplicationID(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	userID, appID := insertFixtures(t, store)

	steps := []*workflowv1.WorkflowStepDef{{Id: "s1", Name: "Step", Type: workflowv1.StepType_STEP_TYPE_TASK}}
	def, _ := store.CreateWorkflowDef(ctx, appID, "App ID Test", "evt", steps)
	inst, _ := store.StartWorkflow(ctx, def.Id, userID, nil)
	_, fetchedSteps, _ := store.GetWorkflowInstance(ctx, inst.Id)

	gotAppID, err := store.GetStepApplicationID(ctx, fetchedSteps[0].Id)
	if err != nil {
		t.Fatalf("GetStepApplicationID: %v", err)
	}
	if gotAppID != appID {
		t.Errorf("application_id = %q, want %q", gotAppID, appID)
	}
}

func TestGetPendingTasks(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	userID, appID := insertFixtures(t, store)

	steps := []*workflowv1.WorkflowStepDef{
		{Id: "t1", Name: "Task 1", Type: workflowv1.StepType_STEP_TYPE_TASK},
		{Id: "t2", Name: "Task 2", Type: workflowv1.StepType_STEP_TYPE_TASK},
	}
	def, _ := store.CreateWorkflowDef(ctx, appID, "Tasks", "trigger", steps)
	_, _ = store.StartWorkflow(ctx, def.Id, userID, nil)

	tasks, err := store.GetPendingTasks(ctx, "", appID)
	if err != nil {
		t.Fatalf("GetPendingTasks: %v", err)
	}
	if len(tasks) == 0 {
		t.Error("expected at least one pending task")
	}
}

func TestCreateAndListAutomationRules(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	_, appID := insertFixtures(t, store)

	rule, err := store.CreateAutomationRule(ctx, appID, "", "auto-pr", "Auto PR approval", "manual", "Budget Approval", "", "", "", nil)
	if err != nil {
		t.Fatalf("CreateAutomationRule: %v", err)
	}
	if rule.ID == "" {
		t.Error("expected non-empty rule ID")
	}
	if !rule.Enabled {
		t.Error("expected rule to be enabled by default")
	}

	rules, err := store.ListAutomationRules(ctx, appID, "")
	if err != nil {
		t.Fatalf("ListAutomationRules: %v", err)
	}
	if len(rules) != 1 {
		t.Errorf("len(rules) = %d, want 1", len(rules))
	}
	if rules[0].WorkflowName != "Budget Approval" {
		t.Errorf("workflow_name = %q, want Budget Approval", rules[0].WorkflowName)
	}
}

func TestTriggerRule(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	// Create a workflow def first
	steps := []*workflowv1.WorkflowStepDef{
		{Id: "review", Name: "Review", Type: workflowv1.StepType_STEP_TYPE_APPROVAL, SlaHours: 24},
	}
	wf, err := store.CreateWorkflowDef(ctx, appID, "Purchase Approval", "manual", steps)
	if err != nil {
		t.Fatalf("CreateWorkflowDef: %v", err)
	}
	if _, err := store.PublishWorkflowDef(ctx, wf.Id, userID); err != nil {
		t.Fatalf("PublishWorkflowDef: %v", err)
	}

	rule, err := store.CreateAutomationRule(ctx, appID, "", "trigger-test", "", "manual", "Purchase Approval", "", "", "", nil)
	if err != nil {
		t.Fatalf("CreateAutomationRule: %v", err)
	}

	exec, err := store.TriggerRule(ctx, rule.ID, userID, map[string]string{"ref": "PR-001"})
	if err != nil {
		t.Fatalf("TriggerRule: %v", err)
	}
	// The execution mirrors the instance lifecycle: the review step is still
	// waiting on a human, so the execution must report 'running' (it flips to
	// completed/cancelled when the instance closes).
	if exec.Status != "running" {
		t.Errorf("status = %q, want running", exec.Status)
	}
	if exec.InstanceID == "" {
		t.Error("expected non-empty instance_id after trigger")
	}
	if exec.TriggerPayload["ref"] != "PR-001" {
		t.Errorf("payload[ref] = %q, want PR-001", exec.TriggerPayload["ref"])
	}
}

// ── Developer workflow def management ────────────────────────────────────────

func TestCreateWorkflowDefFull(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	def, err := store.CreateWorkflowDefFull(ctx, appID, "", "Budget Approval", "Approves budgets", "manual", userID)
	if err != nil {
		t.Fatalf("CreateWorkflowDefFull: %v", err)
	}
	if def.ID == "" {
		t.Error("expected non-empty id")
	}
	if def.Status != "draft" {
		t.Errorf("status = %q, want draft", def.Status)
	}
	if def.Name != "Budget Approval" {
		t.Errorf("name = %q, want Budget Approval", def.Name)
	}
}

func TestListWorkflowDefs(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	_, _ = store.CreateWorkflowDefFull(ctx, appID, "", "WF 1", "", "manual", userID)
	_, _ = store.CreateWorkflowDefFull(ctx, appID, "", "WF 2", "", "form.submit", userID)

	list, err := store.ListWorkflowDefs(ctx, appID, "")
	if err != nil {
		t.Fatalf("ListWorkflowDefs: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("len = %d, want 2", len(list))
	}
}

func TestUpdateWorkflowDefFull(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	def, _ := store.CreateWorkflowDefFull(ctx, appID, "", "Old Name", "", "manual", userID)

	stepsJSON := []byte(`[{"id":"s1","name":"Approve","type":"approval"}]`)
	updated, err := store.UpdateWorkflowDefFull(ctx, def.ID, "New Name", "Desc", "form.submit", "", userID, stepsJSON, nil, nil)
	if err != nil {
		t.Fatalf("UpdateWorkflowDefFull: %v", err)
	}
	if updated.Name != "New Name" {
		t.Errorf("name = %q, want New Name", updated.Name)
	}
	if updated.TriggerEvent != "form.submit" {
		t.Errorf("trigger_event = %q, want form.submit", updated.TriggerEvent)
	}
}

func TestPublishWorkflowDef(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	def, _ := store.CreateWorkflowDefFull(ctx, appID, "", "Approval", "", "manual", userID)
	if def.Status != "draft" {
		t.Fatalf("expected draft, got %q", def.Status)
	}

	pub, err := store.PublishWorkflowDef(ctx, def.ID, userID)
	if err != nil {
		t.Fatalf("PublishWorkflowDef: %v", err)
	}
	if pub.Status != "published" {
		t.Errorf("status = %q, want published", pub.Status)
	}
	if pub.PublishedAt == nil {
		t.Error("expected published_at to be set")
	}
}

func TestArchiveWorkflowDef(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	def, _ := store.CreateWorkflowDefFull(ctx, appID, "", "To Archive", "", "manual", userID)
	archived, err := store.ArchiveWorkflowDef(ctx, def.ID, userID)
	if err != nil {
		t.Fatalf("ArchiveWorkflowDef: %v", err)
	}
	if archived.Status != "archived" {
		t.Errorf("status = %q, want archived", archived.Status)
	}
	if archived.ArchivedAt == nil {
		t.Error("expected archived_at to be set")
	}

	// Second archive should fail (already archived)
	_, err = store.ArchiveWorkflowDef(ctx, def.ID, userID)
	if err == nil {
		t.Error("expected error archiving already-archived workflow")
	}
}

func TestDeleteWorkflowDef(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	def, _ := store.CreateWorkflowDefFull(ctx, appID, "", "Deletable", "", "manual", userID)

	if err := store.DeleteWorkflowDef(ctx, def.ID); err != nil {
		t.Fatalf("DeleteWorkflowDef: %v", err)
	}

	list, _ := store.ListWorkflowDefs(ctx, appID, "")
	if len(list) != 0 {
		t.Errorf("expected 0 workflows after delete, got %d", len(list))
	}
}

func TestDeleteWorkflowDefBlockedWithInstances(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	// Use legacy CreateWorkflowDef which populates steps correctly for StartWorkflow
	steps := []*workflowv1.WorkflowStepDef{{Id: "s1", Name: "Step", Type: workflowv1.StepType_STEP_TYPE_TASK}}
	def, _ := store.CreateWorkflowDef(ctx, appID, "Has Instance", "manual", steps)
	_, _ = store.StartWorkflow(ctx, def.Id, userID, nil)

	err := store.DeleteWorkflowDef(ctx, def.Id)
	if err == nil {
		t.Error("expected error deleting workflow with existing instances")
	}
}

func TestDuplicateWorkflowDef(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	stepsJSON := []byte(`[{"id":"s1","name":"Approve","type":"approval"}]`)
	orig, _ := store.CreateWorkflowDefFull(ctx, appID, "", "Original", "desc", "manual", userID)
	_, _ = store.UpdateWorkflowDefFull(ctx, orig.ID, "Original", "desc", "manual", "", userID, stepsJSON, nil, nil)

	dup, err := store.DuplicateWorkflowDef(ctx, orig.ID, "Original (copy)", userID)
	if err != nil {
		t.Fatalf("DuplicateWorkflowDef: %v", err)
	}
	if dup.Name != "Original (copy)" {
		t.Errorf("dup name = %q, want Original (copy)", dup.Name)
	}
	if dup.Status != "draft" {
		t.Errorf("dup status = %q, want draft", dup.Status)
	}
	if dup.ID == orig.ID {
		t.Error("duplicate should have a different id")
	}
}

func TestGetWorkflowDefUsage(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	def, _ := store.CreateWorkflowDefFull(ctx, appID, "", "Usage WF", "", "manual", userID)

	// No automation rules yet
	usage, err := store.GetWorkflowDefUsage(ctx, def.ID)
	if err != nil {
		t.Fatalf("GetWorkflowDefUsage: %v", err)
	}
	if len(usage) != 0 {
		t.Errorf("expected 0 usage, got %d", len(usage))
	}

	// Create a rule that references this workflow by name
	_, err = store.CreateAutomationRule(ctx, appID, "", "rule-1", "", "manual", def.Name, "", "", "", nil)
	if err != nil {
		t.Fatalf("CreateAutomationRule: %v", err)
	}

	usage, err = store.GetWorkflowDefUsage(ctx, def.ID)
	if err != nil {
		t.Fatalf("GetWorkflowDefUsage after rule: %v", err)
	}
	if len(usage) != 1 {
		t.Errorf("expected 1 usage, got %d", len(usage))
	}
	if usage[0].RuleName != "rule-1" {
		t.Errorf("rule name = %q, want rule-1", usage[0].RuleName)
	}
}

func TestUpdateAutomationRuleEnabled(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	_, appID := insertFixtures(t, store)

	rule, _ := store.CreateAutomationRule(ctx, appID, "", "toggle-test", "", "manual", "WF", "", "", "", nil)
	if !rule.Enabled {
		t.Fatal("expected enabled by default")
	}

	f := false
	updated, err := store.UpdateAutomationRule(ctx, rule.ID, "", "", "", "", "", "", "", &f, nil)
	if err != nil {
		t.Fatalf("UpdateAutomationRule: %v", err)
	}
	if updated.Enabled {
		t.Error("expected rule to be disabled after update")
	}

	tr := true
	updated, err = store.UpdateAutomationRule(ctx, rule.ID, "", "", "", "", "", "", "", &tr, nil)
	if err != nil {
		t.Fatalf("UpdateAutomationRule re-enable: %v", err)
	}
	if !updated.Enabled {
		t.Error("expected rule to be enabled again")
	}
}

func TestTriggerDisabledRuleFails(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	steps := []*workflowv1.WorkflowStepDef{{Id: "s1", Name: "Step", Type: workflowv1.StepType_STEP_TYPE_TASK}}
	_, _ = store.CreateWorkflowDef(ctx, appID, "Disabled WF", "manual", steps)

	rule, _ := store.CreateAutomationRule(ctx, appID, "", "disabled-rule", "", "manual", "Disabled WF", "", "", "", nil)
	f := false
	_, _ = store.UpdateAutomationRule(ctx, rule.ID, "", "", "", "", "", "", "", &f, nil)

	_, err := store.TriggerRule(ctx, rule.ID, userID, nil)
	if err == nil {
		t.Error("expected error triggering a disabled rule")
	}
}

func TestApprovalRouteAdvancesWorkflow(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	steps := []*workflowv1.WorkflowStepDef{
		{Id: "approve", Name: "Manager Approval", Type: workflowv1.StepType_STEP_TYPE_APPROVAL},
		{Id: "notify", Name: "Notify", Type: workflowv1.StepType_STEP_TYPE_NOTIFICATION},
	}
	def, _ := store.CreateWorkflowDef(ctx, appID, "Approval Flow", "manual", steps)
	inst, _ := store.StartWorkflow(ctx, def.Id, userID, map[string]string{"amount": "50000"})

	_, fetchedSteps, _ := store.GetWorkflowInstance(ctx, inst.Id)
	if fetchedSteps[0].StepDefId != "approve" {
		t.Fatalf("expected first step to be 'approve', got %q", fetchedSteps[0].StepDefId)
	}

	completedStep, err := store.CompleteStep(ctx, fetchedSteps[0].Id, userID, "approve", "Looks good")
	if err != nil {
		t.Fatalf("CompleteStep: %v", err)
	}
	if completedStep.Status != workflowv1.StepStatus_STEP_STATUS_COMPLETED {
		t.Errorf("step status = %v, want COMPLETED", completedStep.Status)
	}

	_, updatedSteps, _ := store.GetWorkflowInstance(ctx, inst.Id)
	if updatedSteps[1].Status != workflowv1.StepStatus_STEP_STATUS_IN_PROGRESS {
		t.Errorf("notify step status = %v, want IN_PROGRESS", updatedSteps[1].Status)
	}
}

func TestRejectionClosesWorkflow(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	steps := []*workflowv1.WorkflowStepDef{
		{Id: "approve", Name: "Approval", Type: workflowv1.StepType_STEP_TYPE_APPROVAL},
	}
	def, _ := store.CreateWorkflowDef(ctx, appID, "Reject Flow", "manual", steps)
	inst, _ := store.StartWorkflow(ctx, def.Id, userID, nil)

	_, fetchedSteps, _ := store.GetWorkflowInstance(ctx, inst.Id)
	_, err := store.CompleteStep(ctx, fetchedSteps[0].Id, userID, "reject", "Not approved")
	if err != nil {
		t.Fatalf("CompleteStep reject: %v", err)
	}

	// A rejection is a decided outcome, not an aborted run: the instance
	// COMPLETES and the "reject" decision lives on the step. CANCELLED is
	// reserved for runs stopped before finishing.
	finalInst, finalSteps, _ := store.GetWorkflowInstance(ctx, inst.Id)
	if finalInst.Status != workflowv1.WorkflowStatus_WORKFLOW_STATUS_COMPLETED {
		t.Errorf("instance status = %v, want COMPLETED after rejection (the decision lives on the step)", finalInst.Status)
	}
	if len(finalSteps) == 0 || finalSteps[0].Decision != "reject" {
		t.Errorf("step decision = %v, want reject preserved on the step", finalSteps)
	}
}

func TestAutomationTriggerStartsWorkflowInstance(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	steps := []*workflowv1.WorkflowStepDef{
		{Id: "review", Name: "Review", Type: workflowv1.StepType_STEP_TYPE_APPROVAL, SlaHours: 48},
	}
	wf, _ := store.CreateWorkflowDef(ctx, appID, "Purchase Approval", "manual", steps)
	if _, err := store.PublishWorkflowDef(ctx, wf.Id, userID); err != nil {
		t.Fatalf("PublishWorkflowDef: %v", err)
	}

	rule, _ := store.CreateAutomationRule(ctx, appID, "", "pr-approval", "", "manual", "Purchase Approval", "", "", "", nil)

	exec, err := store.TriggerRule(ctx, rule.ID, userID, map[string]string{
		"purchase_request_id": "PR-2026-001",
		"amount":              "15000",
	})
	if err != nil {
		t.Fatalf("TriggerRule: %v", err)
	}
	if exec.InstanceID == "" {
		t.Error("expected workflow instance to be created")
	}
	if exec.TriggerPayload["amount"] != "15000" {
		t.Errorf("payload amount = %q, want 15000", exec.TriggerPayload["amount"])
	}

	inst, fetchedSteps, err := store.GetWorkflowInstance(ctx, exec.InstanceID)
	if err != nil {
		t.Fatalf("GetWorkflowInstance: %v", err)
	}
	if inst.Status != workflowv1.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
		t.Errorf("instance status = %v, want RUNNING", inst.Status)
	}
	if len(fetchedSteps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(fetchedSteps))
	}
	if fetchedSteps[0].Status != workflowv1.StepStatus_STEP_STATUS_IN_PROGRESS {
		t.Errorf("step status = %v, want IN_PROGRESS", fetchedSteps[0].Status)
	}
	if fetchedSteps[0].DueAt == nil {
		t.Error("expected due_at to be set from SLA hours")
	}
}

func TestStartWorkflowFromDeveloperStringStepTypes(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	def, err := store.CreateWorkflowDefFull(ctx, appID, "", "String Step Flow", "Created by Workflows tab", "manual", userID)
	if err != nil {
		t.Fatalf("CreateWorkflowDefFull: %v", err)
	}

	stepsJSON := json.RawMessage(`[
		{"id":"approve","name":"Manager Approval","type":"approval","assignee_roles":["finance"],"sla_hours":24,"routes":{"approve":"notify","reject":"end-rejected"}},
		{"id":"notify","name":"Notify Requester","type":"notification","routes":{"next":"end-completed"}}
	]`)
	if _, err := store.UpdateWorkflowDefFull(ctx, def.ID, def.Name, def.Description, def.TriggerEvent, "", userID, stepsJSON, nil, nil); err != nil {
		t.Fatalf("UpdateWorkflowDefFull: %v", err)
	}

	inst, err := store.StartWorkflow(ctx, def.ID, userID, map[string]string{"source": "developer-test"})
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}

	_, steps, err := store.GetWorkflowInstance(ctx, inst.Id)
	if err != nil {
		t.Fatalf("GetWorkflowInstance: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("expected 2 workflow steps, got %d", len(steps))
	}
	if steps[0].StepDefId != "approve" {
		t.Fatalf("first step = %q, want approve", steps[0].StepDefId)
	}
	if steps[0].Status != workflowv1.StepStatus_STEP_STATUS_IN_PROGRESS {
		t.Errorf("first step status = %v, want IN_PROGRESS", steps[0].Status)
	}
	if steps[0].DueAt == nil {
		t.Error("expected due_at from string-typed approval step SLA")
	}
}

func TestAutomationTriggerStartsDeveloperWorkflowWithStringStepTypes(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	def, err := store.CreateWorkflowDefFull(ctx, appID, "", "Developer String Automation WF", "", "manual", userID)
	if err != nil {
		t.Fatalf("CreateWorkflowDefFull: %v", err)
	}
	stepsJSON := json.RawMessage(`[
		{"id":"review","name":"Review","type":"approval","assignee_roles":["finance"],"sla_hours":48,"routes":{"approve":"end-completed","reject":"end-rejected"}}
	]`)
	if _, err := store.UpdateWorkflowDefFull(ctx, def.ID, def.Name, def.Description, def.TriggerEvent, "", userID, stepsJSON, nil, nil); err != nil {
		t.Fatalf("UpdateWorkflowDefFull: %v", err)
	}
	if _, err := store.PublishWorkflowDef(ctx, def.ID, userID); err != nil {
		t.Fatalf("PublishWorkflowDef: %v", err)
	}

	rule, err := store.CreateAutomationRule(ctx, appID, "", "developer-string-rule", "", "manual", def.Name, "", "", "", nil)
	if err != nil {
		t.Fatalf("CreateAutomationRule: %v", err)
	}
	exec, err := store.TriggerRule(ctx, rule.ID, userID, map[string]string{"source": "dashboard-button"})
	if err != nil {
		t.Fatalf("TriggerRule: %v", err)
	}
	if exec.InstanceID == "" {
		t.Fatal("expected workflow instance from automation trigger")
	}

	_, steps, err := store.GetWorkflowInstance(ctx, exec.InstanceID)
	if err != nil {
		t.Fatalf("GetWorkflowInstance: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("expected 1 workflow step, got %d", len(steps))
	}
	if steps[0].StepDefId != "review" {
		t.Errorf("step_def_id = %q, want review", steps[0].StepDefId)
	}
	if steps[0].Status != workflowv1.StepStatus_STEP_STATUS_IN_PROGRESS {
		t.Errorf("step status = %v, want IN_PROGRESS", steps[0].Status)
	}
}

func TestListExecutions(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, appID := insertFixtures(t, store)

	steps := []*workflowv1.WorkflowStepDef{
		{Id: "s1", Name: "Step", Type: workflowv1.StepType_STEP_TYPE_APPROVAL, SlaHours: 24},
	}
	wf, err := store.CreateWorkflowDef(ctx, appID, "Exec WF", "manual", steps)
	if err != nil {
		t.Fatalf("CreateWorkflowDef: %v", err)
	}
	if _, err := store.PublishWorkflowDef(ctx, wf.Id, userID); err != nil {
		t.Fatalf("PublishWorkflowDef: %v", err)
	}
	rule, err := store.CreateAutomationRule(ctx, appID, "", "list-exec-rule", "", "manual", "Exec WF", "", "", "", nil)
	if err != nil {
		t.Fatalf("CreateAutomationRule: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := store.TriggerRule(ctx, rule.ID, userID, nil); err != nil {
			t.Fatalf("TriggerRule %d: %v", i, err)
		}
	}

	execs, err := store.ListExecutions(ctx, appID, 10)
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(execs) != 3 {
		t.Errorf("len(execs) = %d, want 3", len(execs))
	}
}
