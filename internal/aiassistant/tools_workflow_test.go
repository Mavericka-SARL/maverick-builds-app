package aiassistant_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// The workflow/form parity tools (programme item 5). Each test pins one
// behaviour the developer console has and the assistant lacked.

func approvalSteps(role string) []map[string]any {
	return []map[string]any{
		{"id": "review", "name": "Review", "type": "approval", "assignee_roles": []string{role},
			"routes": map[string]string{"approve": "done", "reject": "end-rejected"}},
		{"id": "done", "name": "Notify", "type": "notification",
			"notification": map[string]any{"recipient_type": "requester", "subject": "Done", "message": "Done."},
			"routes":       map[string]string{"next": "end-completed"}},
	}
}

func TestGetWorkflow_ShowsStepsRulesAndTheValidateVerdict(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	userID := seedActor(t, pool)
	ctx := context.Background()
	writer := aiassistant.NewWriteExecutorWithActor(pool, modelID, revID, userID)
	reader := aiassistant.NewToolExecutor(pool, modelID, revID)

	_, wfID, err := writer.Execute(ctx, "create_workflow_def", mustJSON(t, map[string]any{"name": "Manager Approval"}))
	if err != nil {
		t.Fatal(err)
	}
	// list_workflows says how many steps; only get_workflow says which.
	out, err := reader.Execute(ctx, "get_workflow", mustJSON(t, map[string]any{"workflow_def_id": wfID}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"steps": []`, "Validation: 1 issue", "Workflow has no steps", "No automation rule starts this workflow yet"} {
		if !strings.Contains(out, want) {
			t.Errorf("get_workflow lacks %q:\n%s", want, out)
		}
	}

	res, _, err := writer.Execute(ctx, "update_workflow_def", mustJSON(t, map[string]any{
		"workflow_def_id": "Manager Approval", // by name, as models do
		"steps":           approvalSteps("business_admin"), "single_active_instance": false,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "validation: OK") || !strings.Contains(res, "single_active_instance: false") {
		t.Errorf("update result = %q", res)
	}
	var single bool
	_ = pool.QueryRow(ctx, `SELECT single_active_instance FROM workflow.workflow_def WHERE id=$1::uuid`, wfID).Scan(&single)
	if single {
		t.Error("single_active_instance was not switched off")
	}

	if _, _, err := writer.Execute(ctx, "create_automation_rule", mustJSON(t, map[string]any{
		"name": "Start it", "trigger_type": "manual", "workflow_name": "Manager Approval",
	})); err != nil {
		t.Fatal(err)
	}
	out, _ = reader.Execute(ctx, "get_workflow", mustJSON(t, map[string]any{"workflow_def_id": wfID}))
	for _, want := range []string{`"id": "review"`, `"assignee_roles"`, "Validation: OK", `"name": "Start it"`, `"trigger_type": "manual"`} {
		if !strings.Contains(out, want) {
			t.Errorf("get_workflow lacks %q:\n%s", want, out)
		}
	}
}

func TestValidateWorkflow_IsTheDevelopersValidateButton(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	reader := aiassistant.NewToolExecutor(pool, modelID, revID)
	ctx := context.Background()

	bad := []map[string]any{
		{"id": "a", "name": "Approve", "type": "approval", "routes": map[string]string{"approve": "nowhere"}},
	}
	out, err := reader.Execute(ctx, "validate_workflow", mustJSON(t, map[string]any{"name": "X", "steps": bad}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"no approver role", "missing reject route", `points to unknown step "nowhere"`} {
		if !strings.Contains(out, want) {
			t.Errorf("validate_workflow lacks %q:\n%s", want, out)
		}
	}
	out, _ = reader.Execute(ctx, "validate_workflow", mustJSON(t, map[string]any{"name": "X", "steps": approvalSteps("business_user")}))
	if !strings.Contains(out, "is valid") {
		t.Errorf("a good definition reported: %s", out)
	}
}

func TestCreateAutomationRule_BindsByNameUntilPublished(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	userID := seedActor(t, pool)
	ctx := context.Background()
	writer := aiassistant.NewWriteExecutorWithActor(pool, modelID, revID, userID)
	reader := aiassistant.NewToolExecutor(pool, modelID, revID)

	_, wfID, _ := writer.Execute(ctx, "create_workflow_def", mustJSON(t, map[string]any{"name": "Expense Approval"}))
	if _, _, err := writer.Execute(ctx, "update_workflow_def", mustJSON(t, map[string]any{"workflow_def_id": wfID, "steps": approvalSteps("business_admin")})); err != nil {
		t.Fatal(err)
	}

	// A draft cannot be bound by id (the store refuses), and the assistant
	// cannot publish; the rule is stored by name and says so.
	res, ruleID, err := writer.Execute(ctx, "create_automation_rule", mustJSON(t, map[string]any{
		"name": "Manual start", "trigger_type": "manual", "workflow_def_id": wfID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "fires once a developer publishes") {
		t.Errorf("result = %q", res)
	}
	var boundID *string
	var boundName string
	_ = pool.QueryRow(ctx, `SELECT workflow_def_id::text, workflow_name FROM workflow.automation_rule WHERE id=$1::uuid`, ruleID).Scan(&boundID, &boundName)
	if boundID != nil || boundName != "Expense Approval" {
		t.Errorf("draft binding: id=%v name=%q, want by name only", boundID, boundName)
	}

	// Once a developer publishes, the same call binds by id.
	if _, err := workflow.NewStore(pool).PublishWorkflowDef(ctx, wfID, userID); err != nil {
		t.Fatal(err)
	}
	res, rule2, err := writer.Execute(ctx, "create_automation_rule", mustJSON(t, map[string]any{
		"name": "Form start", "trigger_type": "form_submit", "workflow_name": "Expense Approval",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res, "fires once") {
		t.Errorf("published workflow still reported as pending: %q", res)
	}
	_ = pool.QueryRow(ctx, `SELECT workflow_def_id::text FROM workflow.automation_rule WHERE id=$1::uuid`, rule2).Scan(&boundID)
	if boundID == nil || *boundID != wfID {
		t.Errorf("published binding id = %v, want %s", boundID, wfID)
	}

	// What the developer's screen would refuse, the tool refuses.
	for name, params := range map[string]map[string]any{
		"unknown trigger":     {"name": "x", "trigger_type": "on_tuesdays", "workflow_name": "Expense Approval"},
		"schedule w/o cron":   {"name": "x", "trigger_type": "schedule", "workflow_name": "Expense Approval"},
		"unknown source form": {"name": "x", "trigger_type": "form_submit", "workflow_name": "Expense Approval", "source_form_id": "no-such-form"},
		"unknown workflow":    {"name": "x", "trigger_type": "manual", "workflow_name": "Nope"},
		"no workflow":         {"name": "x", "trigger_type": "manual"},
	} {
		if _, _, err := writer.Execute(ctx, "create_automation_rule", mustJSON(t, params)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	out, _ := reader.Execute(ctx, "list_automation_rules", nil)
	for _, want := range []string{"Manual start", "by name", "Form start", "by id", "trigger:form_submit"} {
		if !strings.Contains(out, want) {
			t.Errorf("list_automation_rules lacks %q:\n%s", want, out)
		}
	}

	// update: only what is supplied changes; delete removes.
	res, _, err = writer.Execute(ctx, "update_automation_rule", mustJSON(t, map[string]any{
		"automation_rule_id": ruleID, "enabled": false, "description": "paused",
	}))
	if err != nil || !strings.Contains(res, "disabled") {
		t.Fatalf("update: %v %q", err, res)
	}
	var trig, desc string
	var enabled bool
	_ = pool.QueryRow(ctx, `SELECT trigger_type::text, COALESCE(description,''), enabled FROM workflow.automation_rule WHERE id=$1::uuid`, ruleID).Scan(&trig, &desc, &enabled)
	if trig != "manual" || desc != "paused" || enabled {
		t.Errorf("after update: trigger=%s desc=%q enabled=%v", trig, desc, enabled)
	}
	if _, _, err := writer.Execute(ctx, "delete_automation_rule", mustJSON(t, map[string]any{"automation_rule_id": ruleID})); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM workflow.automation_rule WHERE id=$1::uuid`, ruleID).Scan(&n)
	if n != 0 {
		t.Error("rule not deleted")
	}
}

func TestBusinessRoles_CreateAndList(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	userID := seedActor(t, pool)
	ctx := context.Background()
	writer := aiassistant.NewWriteExecutorWithActor(pool, modelID, revID, userID)
	reader := aiassistant.NewToolExecutor(pool, modelID, revID)

	out, _ := reader.Execute(ctx, "list_workflow_roles", nil)
	if !strings.Contains(out, "business_user, business_admin, developer, platform_admin") || !strings.Contains(out, "none yet") {
		t.Errorf("empty listing: %s", out)
	}
	res, roleID, err := writer.Execute(ctx, "create_business_role", mustJSON(t, map[string]any{"name": " Finance Review "}))
	if err != nil || roleID == "" {
		t.Fatalf("create: %v %q", err, res)
	}
	var wsID, appWS string
	_ = pool.QueryRow(ctx, `SELECT workspace_id::text FROM identity.business_role WHERE id=$1::uuid`, roleID).Scan(&wsID)
	_ = pool.QueryRow(ctx, `SELECT a.workspace_id::text FROM core.model m JOIN core.application a ON a.id=m.application_id WHERE m.id=$1::uuid`, modelID).Scan(&appWS)
	if wsID != appWS {
		t.Errorf("role created in workspace %s, app's is %s", wsID, appWS)
	}
	// Same name again is not an error and not a duplicate.
	_, again, err := writer.Execute(ctx, "create_business_role", mustJSON(t, map[string]any{"name": "finance review"}))
	if err != nil || again != roleID {
		t.Errorf("second create: %v id=%s want %s", err, again, roleID)
	}
	out, _ = reader.Execute(ctx, "list_workflow_roles", nil)
	if !strings.Contains(out, "Finance Review (id:"+roleID+", 0 member(s))") {
		t.Errorf("listing after create: %s", out)
	}
}

func TestFormIntegration_Lifecycle(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	userID := seedActor(t, pool)
	ctx := context.Background()
	writer := aiassistant.NewWriteExecutorWithActor(pool, modelID, revID, userID)
	reader := aiassistant.NewToolExecutor(pool, modelID, revID)

	_, metricID, err := writer.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "expense", "is_input": true, "agg_rule": "sum", "format": "number"}))
	if err != nil {
		t.Fatal(err)
	}
	_, dimID, err := writer.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{"name": "department"}))
	if err != nil {
		t.Fatal(err)
	}
	_, formID, err := writer.Execute(ctx, "create_form_def", mustJSON(t, map[string]any{
		"name": "expense_request", "label": "Expense request",
		"fields": []map[string]any{
			{"name": "amount", "label": "Amount", "type": "number", "required": true},
			{"name": "dept", "label": "Department", "type": "dimension", "dimension_id": dimID, "required": true},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}

	// Mistakes the console's pickers make impossible are refused, named.
	for name, params := range map[string]map[string]any{
		"unknown source field": {"form_id": formID, "name": "x", "source_field": "total", "target_metric_id": "expense"},
		"unknown mapped field": {"form_id": formID, "name": "x", "source_field": "amount", "target_metric_id": "expense", "dimension_mappings": map[string]string{"department": "region"}},
		"unknown dimension":    {"form_id": formID, "name": "x", "source_field": "amount", "target_metric_id": "expense", "dimension_mappings": map[string]string{"geography": "dept"}},
		"unknown metric":       {"form_id": formID, "name": "x", "source_field": "amount", "target_metric_id": "revenue"},
	} {
		if _, _, err := writer.Execute(ctx, "create_form_integration", mustJSON(t, params)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// Names where the schema says id, the way models write them.
	res, intID, err := writer.Execute(ctx, "create_form_integration", mustJSON(t, map[string]any{
		"form_id": "expense_request", "name": "Post expenses", "source_field": "amount", "target_metric_id": "expense",
		"dimension_mappings": map[string]string{"department": "dept"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "posts to metric 'expense'") || !strings.Contains(res, "status approved") {
		t.Errorf("result = %q", res)
	}
	var target string
	var dimRaw []byte
	_ = pool.QueryRow(ctx, `SELECT target_metric_id::text, dimension_mappings FROM model.form_metric_mapping WHERE id=$1::uuid`, intID).Scan(&target, &dimRaw)
	if target != metricID || string(dimRaw) != `{"`+dimID+`": "dept"}` {
		t.Errorf("stored target=%s mappings=%s", target, dimRaw)
	}

	out, _ := reader.Execute(ctx, "get_form", mustJSON(t, map[string]any{"form_id": formID}))
	for _, want := range []string{`"name": "amount"`, `"dimension_id": "` + dimID, "Post expenses (id:" + intID, "0 saved record(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("get_form lacks %q:\n%s", want, out)
		}
	}
	out, _ = reader.Execute(ctx, "list_form_integrations", nil)
	if !strings.Contains(out, "department←dept") || !strings.Contains(out, "form 'expense_request' field 'amount' → metric 'expense'") {
		t.Errorf("list_form_integrations: %s", out)
	}

	if _, _, err := writer.Execute(ctx, "update_form_integration", mustJSON(t, map[string]any{
		"form_integration_id": intID, "posting_statuses": []string{"submitted", "approved"}, "live_posting": false,
	})); err != nil {
		t.Fatal(err)
	}
	var statuses []string
	var live bool
	var keptField string
	_ = pool.QueryRow(ctx, `SELECT posting_statuses, live_posting, source_field FROM model.form_metric_mapping WHERE id=$1::uuid`, intID).Scan(&statuses, &live, &keptField)
	if len(statuses) != 2 || live || keptField != "amount" {
		t.Errorf("after update: statuses=%v live=%v field=%s", statuses, live, keptField)
	}
	if _, _, err := writer.Execute(ctx, "delete_form_integration", mustJSON(t, map[string]any{"form_integration_id": intID})); err != nil {
		t.Fatal(err)
	}
	res, _, err = writer.Execute(ctx, "delete_form_def", mustJSON(t, map[string]any{"form_id": "expense_request"}))
	if err != nil || !strings.Contains(res, "deleted") {
		t.Fatalf("delete form: %v %q", err, res)
	}
}

func TestUpdateFormDef_RemapsIntoTheWorkingRevision(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revA := seedRevision(t, pool, modelID, "Rev A")
	revB := seedRevision(t, pool, modelID, "Rev B")
	userID := seedActor(t, pool)
	ctx := context.Background()
	fields := []map[string]any{{"name": "amount", "label": "Amount", "type": "number"}}
	_, formA, err := aiassistant.NewWriteExecutorWithActor(pool, modelID, revA, userID).Execute(ctx, "create_form_def", mustJSON(t, map[string]any{"name": "req", "label": "Old", "fields": fields}))
	if err != nil {
		t.Fatal(err)
	}
	writerB := aiassistant.NewWriteExecutorWithActor(pool, modelID, revB, userID)
	_, formB, err := writerB.Execute(ctx, "create_form_def", mustJSON(t, map[string]any{"name": "req", "label": "Old", "fields": fields}))
	if err != nil {
		t.Fatal(err)
	}
	// An id read before the draft existed edits the draft's copy, never
	// the other revision's row — update_workflow_def already did this,
	// update_form_def refused instead.
	if _, _, err := writerB.Execute(ctx, "update_form_def", mustJSON(t, map[string]any{"form_id": formA, "label": "New"})); err != nil {
		t.Fatalf("update by the other revision's id: %v", err)
	}
	var labelA, labelB string
	_ = pool.QueryRow(ctx, `SELECT label FROM model.form_def WHERE id=$1::uuid`, formA).Scan(&labelA)
	_ = pool.QueryRow(ctx, `SELECT label FROM model.form_def WHERE id=$1::uuid`, formB).Scan(&labelB)
	if labelA != "Old" || labelB != "New" {
		t.Errorf("labels: A=%q B=%q — want A untouched, B edited", labelA, labelB)
	}
	// Fields survive a label-only update (whole-list replace only when supplied).
	var n int
	_ = pool.QueryRow(ctx, `SELECT jsonb_array_length(fields) FROM model.form_def WHERE id=$1::uuid`, formB).Scan(&n)
	if n != 1 {
		t.Errorf("fields after label-only update = %d, want 1", n)
	}
}

func TestDeleteWorkflowDef_DraftsOnly(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	userID := seedActor(t, pool)
	ctx := context.Background()
	writer := aiassistant.NewWriteExecutorWithActor(pool, modelID, revID, userID)

	_, draft, _ := writer.Execute(ctx, "create_workflow_def", mustJSON(t, map[string]any{"name": "Draft"}))
	_, pub, _ := writer.Execute(ctx, "create_workflow_def", mustJSON(t, map[string]any{"name": "Live"}))
	if _, _, err := writer.Execute(ctx, "update_workflow_def", mustJSON(t, map[string]any{"workflow_def_id": pub, "steps": approvalSteps("business_admin")})); err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.NewStore(pool).PublishWorkflowDef(ctx, pub, userID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writer.Execute(ctx, "delete_workflow_def", mustJSON(t, map[string]any{"workflow_def_id": "Draft"})); err != nil {
		t.Fatalf("delete draft: %v", err)
	}
	_, _, err := writer.Execute(ctx, "delete_workflow_def", mustJSON(t, map[string]any{"workflow_def_id": pub}))
	if err == nil || !strings.Contains(err.Error(), "only a draft can be deleted") {
		t.Errorf("published delete: %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM workflow.workflow_def WHERE id IN ($1::uuid, $2::uuid)`, draft, pub).Scan(&n)
	if n != 1 {
		t.Errorf("%d definitions remain, want 1 (the published one)", n)
	}
}

// Every tool definition goes to the provider on every chat call. A malformed
// parameter schema on one tool breaks the whole assistant at runtime, and no
// other test sends the schemas anywhere — the scripted providers ignore
// them — so this is the only place the JSON is ever parsed.
func TestToolDefinitionsAreValidJSONSchemas(t *testing.T) {
	seen := map[string]bool{}
	for _, def := range aiassistant.AllTools() {
		if seen[def.Name] {
			t.Errorf("tool %q is defined twice", def.Name)
		}
		seen[def.Name] = true
		var schema struct {
			Type       string         `json:"type"`
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		if err := json.Unmarshal(def.Parameters, &schema); err != nil {
			t.Errorf("tool %q: parameters are not valid JSON: %v", def.Name, err)
			continue
		}
		if schema.Type != "object" || schema.Properties == nil || schema.Required == nil {
			t.Errorf("tool %q: schema must be an object with properties and required, got %s", def.Name, def.Parameters)
		}
		for _, r := range schema.Required {
			if _, ok := schema.Properties[r]; !ok {
				t.Errorf("tool %q: required %q is not a property", def.Name, r)
			}
		}
	}
	for _, want := range []string{"get_workflow", "validate_workflow", "list_workflow_roles", "list_automation_rules", "get_form", "list_form_integrations", "propose_actions"} {
		if !seen[want] {
			t.Errorf("tool %q is not offered to the model", want)
		}
	}
}
