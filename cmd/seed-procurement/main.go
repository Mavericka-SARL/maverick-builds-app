// cmd/seed-procurement — Procurement/CPO reference application demo.
//
// Creates a full procurement planning scenario from scratch:
//
//	customer(reused) → workspace(Procurement) → application → model →
//	dimensions(supplier,category) → metrics → workflow → form →
//	automation rule → sample records
//
// Run after migrations are already applied (or standalone on empty DB).
//
// Usage:
//
//	DATABASE_URL=postgres://... go run ./cmd/seed-procurement
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/migrate"

	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
)

const (
	defaultDSN   = "postgres://mavericks:mavericks@localhost:5432/mavericks?sslmode=disable"
	scenarioName = "Procurement FY2026"
)

func main() {
	log := logger.New("seed-procurement")
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultDSN
	}

	section("Procurement/CPO — Demo Seed")

	step("Connecting to PostgreSQL")
	pool, err := db.Connect(ctx, dsn)
	must(err, "connect")
	defer pool.Close()
	ok()

	step("Running migrations")
	must(migrate.Run(ctx, pool, migrationfs.FS, "."), "migrate")
	ok()

	// ── Customer (reuse or create) ─────────────────────────────────────────────

	section("Building org hierarchy")

	// Reuse existing customer if present, otherwise create GlobalCorp
	var customerID string
	err = pool.QueryRow(ctx, `SELECT id::text FROM core.customer LIMIT 1`).Scan(&customerID)
	if err != nil {
		customerID = mustScan(pool.QueryRow(ctx, `
			INSERT INTO core.customer (name, plan)
			VALUES ('GlobalCorp', 'enterprise')
			RETURNING id::text
		`), "customer")
		printf("  Customer   : GlobalCorp (new, %s)\n", short(customerID))
	} else {
		printf("  Customer   : existing (%s)\n", short(customerID))
	}

	workspaceID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO core.workspace (customer_id, name)
		VALUES ($1::uuid, 'Procurement')
		RETURNING id::text
	`, customerID), "workspace")
	printf("  Workspace  : Procurement (%s)\n", short(workspaceID))

	appID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO core.application (workspace_id, name, mode)
		VALUES ($1::uuid, 'Procurement Planning 2026', 'crud')
		RETURNING id::text
	`, workspaceID), "application")
	printf("  Application: Procurement Planning 2026 (%s)\n", short(appID))

	modelID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO core.model (application_id, name)
		VALUES ($1::uuid, 'Procurement Model')
		RETURNING id::text
	`, appID), "model")
	printf("  Model      : Procurement Model (%s)\n", short(modelID))

	// ── Users (reuse via ON CONFLICT) ─────────────────────────────────────────

	section("Creating / reusing demo users")

	// ON CONFLICT DO UPDATE with no-op ensures RETURNING always fires
	deptHeadID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('demo-dept-head-001', 'dept.head@acme.com', 'Alex (Dept Head)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
		RETURNING id::text
	`, customerID), "dept head user")
	printf("  dept.head@acme.com (%s)\n", short(deptHeadID))

	financeID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('demo-finance-001', 'finance@acme.com', 'Jordan (Finance)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
		RETURNING id::text
	`, customerID), "finance user")
	printf("  finance@acme.com   (%s)\n", short(financeID))

	devID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('demo-dev-001', 'dev@acme.com', 'Sam (Developer)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
		RETURNING id::text
	`, customerID), "developer user")
	printf("  dev@acme.com       (%s)\n", short(devID))

	adminID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('demo-admin-001', 'admin@acme.com', 'Pat (Platform Admin)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
		RETURNING id::text
	`, customerID), "platform admin user")
	printf("  admin@acme.com     (%s)\n", short(adminID))

	// Role assignments for the new workspace (UNIQUE on user_id+role+workspace_id)
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES
		    ($1::uuid, 'business_user',  $4::uuid),
		    ($2::uuid, 'business_admin', $4::uuid),
		    ($3::uuid, 'developer',      $4::uuid)
		ON CONFLICT (user_id, role, workspace_id) DO NOTHING
	`, deptHeadID, financeID, devID, workspaceID)
	must(err, "role assignments")
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES ($1::uuid, 'platform_admin', NULL)
		ON CONFLICT (user_id, role, workspace_id) DO NOTHING
	`, adminID)
	must(err, "platform_admin role")
	printf("  Roles assigned in Procurement workspace\n")
	_ = devID
	_ = adminID

	// ── Dimensions ────────────────────────────────────────────────────────────

	section("Defining model structure")

	supplierDimID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, name, properties)
		VALUES ($1::uuid, 'supplier', '[{"name":"code","type":"text","required":true}]')
		RETURNING id::text
	`, modelID), "supplier dimension")
	printf("  Dimension: supplier (%s)\n", short(supplierDimID))

	suppliers := []struct{ code, label string }{
		{"SUP001", "AcmeTech"},
		{"SUP002", "GlobalParts"},
		{"SUP003", "OfficePro"},
		{"SUP004", "CloudVendor"},
	}
	for _, s := range suppliers {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label)
			VALUES ($1::uuid, $2, $3)
		`, supplierDimID, s.code, s.label)
		must(err, "supplier member "+s.code)
	}
	printf("  Members   : SUP001-AcmeTech, SUP002-GlobalParts, SUP003-OfficePro, SUP004-CloudVendor\n")

	categoryDimID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, name, properties)
		VALUES ($1::uuid, 'category', '[{"name":"code","type":"text","required":true}]')
		RETURNING id::text
	`, modelID), "category dimension")
	printf("  Dimension: category (%s)\n", short(categoryDimID))

	categories := []struct{ code, label string }{
		{"IT", "Information Technology"},
		{"FACI", "Facilities"},
		{"MKT", "Marketing"},
		{"OPS", "Operations"},
	}
	for _, c := range categories {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label)
			VALUES ($1::uuid, $2, $3)
		`, categoryDimID, c.code, c.label)
		must(err, "category member "+c.code)
	}
	printf("  Members   : IT, FACI, MKT, OPS\n")

	// ── Metrics ───────────────────────────────────────────────────────────────

	metricDefs := []struct {
		name    string
		label   string
		isInput bool
		formula string
	}{
		{"budget", "Approved Budget", true, ""},
		{"committed_spend", "Committed Spend", true, ""},
		{"actual_spend", "Actual Spend", true, ""},
		{"savings", "Savings vs Budget", false, "{budget} - {actual_spend}"},
		{"utilization_pct", "Budget Utilisation %", false, "{actual_spend} / {budget} * 100"},
	}

	metrics := make(map[string]string)
	for _, m := range metricDefs {
		id := mustScan(pool.QueryRow(ctx, `
			INSERT INTO model.metric_def (model_id, name, formula, is_input)
			VALUES ($1::uuid, $2, NULLIF($3,''), $4)
			RETURNING id::text
		`, modelID, m.name, m.formula, m.isInput), "metric "+m.name)
		metrics[m.name] = id
		flag := "INPUT"
		if !m.isInput {
			flag = "CALC "
		}
		printf("  Metric [%s]: %-26s (%s)\n", flag, m.label, short(id))
	}

	// Dependencies
	for _, dep := range []string{"budget", "actual_spend"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id)
			VALUES ($1::uuid, $2::uuid)
		`, metrics["savings"], metrics[dep])
		must(err, "dep savings→"+dep)
	}
	for _, dep := range []string{"actual_spend", "budget"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id)
			VALUES ($1::uuid, $2::uuid)
		`, metrics["utilization_pct"], metrics[dep])
		must(err, "dep utilization_pct→"+dep)
	}
	printf("  Dependencies: savings ← budget - actual_spend | utilization_pct ← actual_spend / budget\n")

	// Scenario
	scenarioID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.revision (model_id, name, description)
		VALUES ($1::uuid, $2, 'Annual procurement spend plan')
		RETURNING id::text
	`, modelID, scenarioName), "scenario")
	printf("  Scenario   : %s (%s)\n", scenarioName, short(scenarioID))

	// ── Security ──────────────────────────────────────────────────────────────

	section("Configuring RACI policies")

	_, err = pool.Exec(ctx, `
		INSERT INTO security.raci_rule (application_id, user_id, resource_pattern, raci_type)
		VALUES
		    ($1::uuid, $2::uuid, '*', 'responsible'),
		    ($1::uuid, $3::uuid, '*', 'accountable')
	`, appID, deptHeadID, financeID)
	must(err, "raci rules")

	for _, name := range []string{"budget", "committed_spend", "actual_spend"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO security.metric_policy
			    (application_id, user_id, metric_id, can_read, can_write)
			VALUES ($1::uuid, $2::uuid, $3::uuid, true, true)
		`, appID, deptHeadID, metrics[name])
		must(err, "metric policy "+name)
	}
	printf("  dept.head can write: budget, committed_spend, actual_spend\n")

	// ── Input data ────────────────────────────────────────────────────────────

	section("Writing procurement spend data (by category)")

	type catFact struct {
		cat    string
		budget float64
		commit float64
		actual float64
	}
	catFacts := []catFact{
		{"IT", 850_000, 780_000, 720_000},
		{"FACI", 200_000, 190_000, 185_000},
		{"MKT", 300_000, 270_000, 255_000},
		{"OPS", 150_000, 140_000, 135_000},
	}

	var totBudget, totCommit, totActual float64
	for _, cf := range catFacts {
		dimJSON := fmt.Sprintf(`{"%s": "%s"}`, categoryDimID, cf.cat)
		for metric, value := range map[string]float64{
			"budget": cf.budget, "committed_spend": cf.commit, "actual_spend": cf.actual,
		} {
			_, err = pool.Exec(ctx, `
				INSERT INTO runtime.fact_input
				    (model_id, revision_name, metric_id, dim_members, value, entered_by)
				VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6::uuid)
			`, modelID, scenarioName, metrics[metric], dimJSON, value, deptHeadID)
			must(err, "fact "+cf.cat+"."+metric)
		}
		printf("  %-6s  budget=%8.0f  commit=%8.0f  actual=%8.0f  savings=%7.0f\n",
			cf.cat, cf.budget, cf.commit, cf.actual, cf.budget-cf.actual)
		totBudget += cf.budget
		totCommit += cf.commit
		totActual += cf.actual
	}

	for metric, value := range map[string]float64{
		"budget": totBudget, "committed_spend": totCommit, "actual_spend": totActual,
	} {
		_, err = pool.Exec(ctx, `
			INSERT INTO runtime.fact_input
			    (model_id, revision_name, metric_id, dim_members, value, entered_by)
			VALUES ($1::uuid, $2, $3::uuid, '{}', $4, $5::uuid)
		`, modelID, scenarioName, metrics[metric], value, deptHeadID)
		must(err, "aggregate fact "+metric)
	}
	printf("  TOTAL   budget=%8.0f  commit=%8.0f  actual=%8.0f  savings=%7.0f\n",
		totBudget, totCommit, totActual, totBudget-totActual)

	// ── Calculation ───────────────────────────────────────────────────────────

	section("Running calculation engine")

	calcStore := calculation.NewStore(pool)
	scheduler := calculation.NewScheduler(log, calcStore, nil)
	changedInputs := []string{metrics["budget"], metrics["committed_spend"], metrics["actual_spend"]}
	must(scheduler.RecalcAffected(ctx, modelID, scenarioID, changedInputs), "recalc")

	savingsResult, err := calcStore.GetCalcValue(ctx, modelID, scenarioID, metrics["savings"], map[string]string{})
	must(err, "get savings")
	printf("  savings = %.2f (expected %.2f)\n", savingsResult, totBudget-totActual)

	// ── Workflow ──────────────────────────────────────────────────────────────

	section("Creating Purchase Request Approval workflow")

	wfStore := workflow.NewStore(pool)
	wfDef, err := wfStore.CreateWorkflowDef(ctx, appID, "Purchase Request Approval", "purchase_request.submitted",
		[]*workflowv1.WorkflowStepDef{
			{
				Id:            "step-manager-review",
				Name:          "Manager Review",
				Type:          workflowv1.StepType_STEP_TYPE_APPROVAL,
				AssigneeRoles: []string{"business_user"},
				SlaHours:      24,
				NextStepIds:   []string{"step-finance-approval"},
			},
			{
				Id:            "step-finance-approval",
				Name:          "Finance Approval",
				Type:          workflowv1.StepType_STEP_TYPE_APPROVAL,
				AssigneeRoles: []string{"business_admin"},
				SlaHours:      48,
				NextStepIds:   []string{"step-notify"},
			},
			{
				Id:          "step-notify",
				Name:        "Notify Requester",
				Type:        workflowv1.StepType_STEP_TYPE_NOTIFICATION,
				NextStepIds: []string{},
			},
		},
	)
	must(err, "workflow def")
	printf("  Def: %q (%s)\n", wfDef.Name, short(wfDef.Id))
	printf("  Steps: Manager Review (24h) → Finance Approval (48h) → Notify Requester\n")

	// ── Form definition ───────────────────────────────────────────────────────

	section("Creating Purchase Request form")

	crudStore := crudapp.NewStore(pool)
	formDef, err := crudStore.CreateForm(ctx, modelID, scenarioID, "purchase_request", "Purchase Request",
		[]crudapp.FormField{
			{Name: "vendor", Label: "Vendor Name", Type: "text", Required: true},
			{Name: "category", Label: "Category", Type: "select", Required: true,
				Options: []string{"IT", "Facilities", "Marketing", "Operations"}},
			{Name: "amount", Label: "Amount (USD)", Type: "number", Required: true},
			{Name: "currency", Label: "Currency", Type: "select", Required: true,
				Options: []string{"USD", "EUR", "GBP", "JPY"}},
			{Name: "justification", Label: "Business Justification", Type: "text", Required: true},
			{Name: "required_by", Label: "Required By", Type: "date", Required: false},
			{Name: "is_urgent", Label: "Urgent Request", Type: "boolean", Required: false},
		},
	)
	must(err, "create form")
	printf("  Form: %q (%s)\n", formDef.Label, short(formDef.ID))
	printf("  Fields: vendor, category, amount, currency, justification, required_by, is_urgent\n")

	// ── Sample records ────────────────────────────────────────────────────────

	section("Seeding sample purchase requests")

	sampleRecords := []struct {
		data   map[string]any
		status string
	}{
		{
			data: map[string]any{
				"vendor": "CloudVendor Inc", "category": "IT", "amount": 45000,
				"currency": "USD", "justification": "Annual SaaS subscription renewal",
				"required_by": "2026-03-01", "is_urgent": false,
			},
			status: "approved",
		},
		{
			data: map[string]any{
				"vendor": "OfficePro Ltd", "category": "Facilities", "amount": 12500,
				"currency": "USD", "justification": "Office furniture for new hires Q1",
				"required_by": "2026-02-15", "is_urgent": false,
			},
			status: "submitted",
		},
		{
			data: map[string]any{
				"vendor": "AcmeTech Solutions", "category": "IT", "amount": 8750,
				"currency": "EUR", "justification": "Emergency hardware replacement — dev lab server failure",
				"required_by": "2026-01-28", "is_urgent": true,
			},
			status: "draft",
		},
	}

	for _, r := range sampleRecords {
		rec, err := crudStore.CreateRecord(ctx, formDef.ID, deptHeadID, r.data)
		must(err, "create record")
		if r.status != "draft" {
			must(crudStore.UpdateRecord(ctx, rec.ID, r.status, r.data), "update record to "+r.status)
		}
		printf("  PR (%s) vendor=%-22s status=%s\n", short(rec.ID), fmt.Sprintf("%q", r.data["vendor"]), r.status)
	}

	// ── Automation rule ───────────────────────────────────────────────────────

	section("Creating automation rule")

	rule, err := wfStore.CreateAutomationRule(ctx, appID, scenarioID,
		"Auto-Route Purchase Requests",
		"Automatically start an approval workflow when a purchase request is submitted",
		"form_submit",
		"Purchase Request Approval",
		"", formDef.ID, "", nil,
	)
	must(err, "create automation rule")
	printf("  Rule: %q (%s)\n", rule.Name, short(rule.ID))
	printf("  Trigger: form_submit → Purchase Request Approval\n")

	// ── Audit events ──────────────────────────────────────────────────────────

	section("Seeding audit log")

	auditEvents := []struct {
		category, eventType, actorID, actorRole, resourceType, resourceID, metadata string
	}{
		{"model_change", "form.created", devID, "developer", "form_def", formDef.ID, `{"name":"purchase_request","fields":7}`},
		{"model_change", "automation_rule.created", devID, "developer", "automation_rule", rule.ID, `{"trigger":"form_submit"}`},
		{"data_change", "purchase_request.created", deptHeadID, "business_user", "form_record", formDef.ID, `{"vendor":"CloudVendor Inc","amount":45000,"status":"approved"}`},
		{"data_change", "purchase_request.created", deptHeadID, "business_user", "form_record", formDef.ID, `{"vendor":"OfficePro Ltd","amount":12500,"status":"submitted"}`},
		{"data_change", "purchase_request.created", deptHeadID, "business_user", "form_record", formDef.ID, `{"vendor":"AcmeTech Solutions","amount":8750,"status":"draft"}`},
	}

	for _, e := range auditEvents {
		_, err = pool.Exec(ctx, `
			INSERT INTO audit.audit_event
			    (category, event_type, actor_user_id, actor_role, workspace_id, resource_type, resource_id, metadata)
			VALUES ($1::audit.event_category, $2, $3::uuid, $4, $5::uuid, $6, $7, $8::jsonb)
		`, e.category, e.eventType, e.actorID, e.actorRole, workspaceID, e.resourceType, e.resourceID, e.metadata)
		must(err, "audit "+e.eventType)
	}
	printf("  %d audit events inserted\n", len(auditEvents))

	// ── Workflow instance (demo: submit the approved PR through workflow) ──────

	section("Running demo workflow: approved purchase request")

	inst, err := wfStore.StartWorkflow(ctx, wfDef.Id, deptHeadID, map[string]string{
		"form_id":  formDef.ID,
		"vendor":   "CloudVendor Inc",
		"amount":   "45000",
		"category": "IT",
	})
	must(err, "start workflow")
	printf("  Instance: %s (status: %s)\n", short(inst.Id), inst.Status)

	_, steps, err := wfStore.GetWorkflowInstance(ctx, inst.Id)
	must(err, "get instance")

	// Complete manager review step
	var managerStepID string
	for _, s := range steps {
		if s.StepDefId == "step-manager-review" {
			managerStepID = s.Id
			break
		}
	}
	if managerStepID == "" {
		fatalf("manager-review step not found")
	}
	_, err = wfStore.CompleteStep(ctx, managerStepID, deptHeadID, "approve", "Vendor is pre-approved. Budget is available.")
	must(err, "complete manager review")
	printf("  Manager Review: approved\n")

	// Complete finance approval step
	_, updatedSteps, err := wfStore.GetWorkflowInstance(ctx, inst.Id)
	must(err, "get instance after manager approval")
	var financeStepID string
	for _, s := range updatedSteps {
		if s.StepDefId == "step-finance-approval" {
			financeStepID = s.Id
			break
		}
	}
	if financeStepID != "" {
		_, err = wfStore.CompleteStep(ctx, financeStepID, financeID, "approve", "Confirmed — budget code IT-Q1-2026 allocated.")
		must(err, "complete finance approval")
		printf("  Finance Approval: approved\n")
	}

	// Auto-complete notification step
	_, finalSteps, _ := wfStore.GetWorkflowInstance(ctx, inst.Id)
	for _, s := range finalSteps {
		if s.StepDefId == "step-notify" {
			_, _ = wfStore.CompleteStep(ctx, s.Id, financeID, "sent", "Requester notified via email")
		}
	}

	finalInst, _, err := wfStore.GetWorkflowInstance(ctx, inst.Id)
	must(err, "final instance")

	// ── Summary ───────────────────────────────────────────────────────────────

	section("Demo complete — summary")
	printf("  Application : Procurement Planning 2026 (%s)\n", short(appID))
	printf("  Model ID    : %s\n", short(modelID))
	printf("  Scenario    : %s\n", scenarioName)
	printf("  Dimensions  : supplier (%d members), category (%d members)\n", len(suppliers), len(categories))
	printf("  Metrics     : 3 input + 2 calculated\n")
	printf("  Total Budget: %.0f | Actual Spend: %.0f | Savings: %.2f\n", totBudget, totActual, savingsResult)
	printf("  Form        : Purchase Request (%d fields, %d sample records)\n", 7, len(sampleRecords))
	printf("  Workflow    : %s → %s\n", wfDef.Name, finalInst.Status)
	printf("  Automation  : %s (%s)\n", rule.Name, rule.TriggerType)
	printf("  DB ready    : %s\n", dsn)
	fmt.Println()

	_ = json.Marshal // keep import
	_ = time.Now()
}

// ── output helpers ─────────────────────────────────────────────────────────────

func section(title string)              { fmt.Printf("\n\033[1;34m▶ %s\033[0m\n", title) }
func step(msg string)                   { fmt.Printf("  %s... ", msg) }
func ok()                               { fmt.Println("\033[32mok\033[0m") }
func printf(format string, args ...any) { fmt.Printf(format, args...) }
func short(uuid string) string {
	if len(uuid) >= 8 {
		return uuid[:8]
	}
	return uuid
}
func must(err error, context string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n\033[31mERROR [%s]: %v\033[0m\n", context, err)
		os.Exit(1)
	}
}
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n\033[31mERROR: %s\033[0m\n", fmt.Sprintf(format, args...))
	os.Exit(1)
}
func mustScan(row interface{ Scan(dest ...any) error }, context string) string {
	var id string
	if err := row.Scan(&id); err != nil {
		fmt.Fprintf(os.Stderr, "\n\033[31mERROR [%s]: %v\033[0m\n", context, err)
		os.Exit(1)
	}
	return id
}
