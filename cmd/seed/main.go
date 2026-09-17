// cmd/seed — OPEX Planning reference application demo.
//
// Creates the full scenario from scratch:
//
//	customer → workspace → application → model → dimensions → metrics →
//	users → facts → auto-recalc → workflow → approval
//
// Usage:
//
//	DATABASE_URL=postgres://... go run ./cmd/seed
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/migrate"

	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
)

const (
	defaultDSN   = "postgres://mavericks:mavericks@localhost:5432/mavericks?sslmode=disable"
	scenarioName = "FY2026 Budget"
)

func main() {
	log := logger.New("seed")
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultDSN
	}

	section("OPEX Planning — Demo Seed")

	step("Connecting to PostgreSQL")
	pool, err := db.Connect(ctx, dsn)
	must(err, "connect")
	defer pool.Close()
	ok()

	step("Running migrations")
	must(migrate.Run(ctx, pool, migrationfs.FS, "."), "migrate")
	ok()

	// ── Org hierarchy ─────────────────────────────────────────────────────────
	// Resolve by finding the application directly — avoids creating duplicates
	// when multiple "Acme Corp" customers exist from old seed runs.

	section("Building org hierarchy")

	var appID, modelID, workspaceID, customerID string
	err = pool.QueryRow(ctx, `
		SELECT a.id::text, m.id::text, a.workspace_id::text, w.customer_id::text
		FROM core.application a
		JOIN core.model m ON m.application_id = a.id AND m.name = 'OPEX Model'
		JOIN core.workspace w ON w.id = a.workspace_id
		WHERE a.name = 'OPEX Planning 2026'
		ORDER BY a.created_at DESC LIMIT 1
	`).Scan(&appID, &modelID, &workspaceID, &customerID)
	if err != nil {
		// Nothing exists yet — create the full hierarchy from scratch.
		customerID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO core.customer (name, plan) VALUES ('Acme Corp', 'enterprise') RETURNING id::text`),
			"customer")
		workspaceID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Finance') RETURNING id::text`,
			customerID), "workspace")
		appID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'OPEX Planning 2026', 'planning') RETURNING id::text`,
			workspaceID, customerID), "application")
		modelID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'OPEX Model') RETURNING id::text`,
			appID), "model")
	}
	// Delete any duplicate "OPEX Planning 2026" applications that aren't this one.
	// These accumulate from old broken seed runs and cause the wrong model to be served.
	_, _ = pool.Exec(ctx, `
		DELETE FROM core.application
		WHERE name = 'OPEX Planning 2026' AND id != $1::uuid
	`, appID)

	printf("  Customer   : Acme Corp (%s)\n", short(customerID))
	printf("  Workspace  : Finance (%s)\n", short(workspaceID))
	printf("  Application: OPEX Planning 2026 (%s)\n", short(appID))
	printf("  Model      : OPEX Model (%s)\n", short(modelID))

	// ── Users ─────────────────────────────────────────────────────────────────

	section("Creating demo users")

	deptHeadID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('demo-dept-head-001', 'dept.head@acme.com', 'Alex (Dept Head)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id::text
	`, customerID), "dept head user")
	printf("  dept.head@acme.com (%s) — business_user\n", short(deptHeadID))

	financeID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('demo-finance-001', 'finance@acme.com', 'Jordan (Finance)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id::text
	`, customerID), "finance user")
	printf("  finance@acme.com   (%s) — business_admin\n", short(financeID))

	devID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('demo-dev-001', 'dev@acme.com', 'Sam (Developer)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id::text
	`, customerID), "developer user")
	printf("  dev@acme.com       (%s) — developer\n", short(devID))

	tenantAdminID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('demo-admin-001', 'admin@acme.com', 'Pat (Tenant Admin)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id::text
	`, customerID), "tenant admin user")
	printf("  admin@acme.com          (%s) — tenant_admin\n", short(tenantAdminID))

	platformAdminID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('demo-platform-admin-001', 'platform-admin@acme.com', 'Pat (Platform Admin)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id::text
	`, customerID), "platform admin user")
	printf("  platform-admin@acme.com (%s) — platform_admin\n", short(platformAdminID))

	// Role assignments (platform_admin has NULL workspace_id per schema convention)
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES
		    ($1::uuid, 'business_user',  $4::uuid),
		    ($2::uuid, 'business_admin', $4::uuid),
		    ($3::uuid, 'developer',      $4::uuid)
		ON CONFLICT DO NOTHING
	`, deptHeadID, financeID, devID, workspaceID)
	must(err, "role assignments")
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES ($1::uuid, 'tenant_admin'::identity.user_role, $2::uuid)
		ON CONFLICT DO NOTHING
	`, tenantAdminID, workspaceID)
	must(err, "tenant_admin role assignment")
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES ($1::uuid, 'platform_admin', NULL)
		ON CONFLICT DO NOTHING
	`, platformAdminID)
	must(err, "platform_admin role assignment")
	printf("  Roles assigned\n")
	_ = devID

	// Cross-assign Budget users to the OPEX workspace so they can switch
	// models via the Apps & Models tab. Safe to run even if budget seed hasn't run yet.
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		SELECT u.id, 'business_user', $1::uuid
		FROM identity.user u
		WHERE u.keycloak_sub IN ('budget-cfo-001','budget-finmgr-001','budget-depteng-001','budget-deptsales-001','budget-deptops-001')
		ON CONFLICT DO NOTHING
	`, workspaceID)
	must(err, "cross-assign budget users to OPEX workspace")
	printf("  Budget business users also granted business_user in OPEX workspace\n")

	// ── Scenario + Version (created first so revision_id is available for all entities) ──

	section("Defining model structure")

	scenarioID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.revision (model_id, name, description)
		VALUES ($1::uuid, $2, 'Annual operating expense budget')
		ON CONFLICT (model_id, name) DO UPDATE SET description = EXCLUDED.description
		RETURNING id::text
	`, modelID, scenarioName), "scenario")
	_, err = pool.Exec(ctx, `
		UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name=$2 WHERE id=$3::uuid
	`, scenarioID, scenarioName, modelID)
	must(err, "set active revision")
	printf("  Scenario   : %s (%s)\n", scenarioName, short(scenarioID))

	// Dimension: department
	dimID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, revision_id, name, properties)
		VALUES ($1::uuid, $2::uuid, 'department', '[{"name":"code","type":"text","required":true}]')
		ON CONFLICT (model_id, revision_id, name) WHERE revision_id IS NOT NULL
		DO UPDATE SET properties = EXCLUDED.properties
		RETURNING id::text
	`, modelID, scenarioID), "dimension")
	printf("  Dimension: department (%s)\n", short(dimID))

	departments := []struct{ code, label string }{
		{"ENG", "Engineering"},
		{"SALES", "Sales"},
		{"MKTG", "Marketing"},
		{"GA", "G&A"},
	}
	for _, d := range departments {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label)
			VALUES ($1::uuid, $2, $3)
			ON CONFLICT (dimension_id, code) DO UPDATE SET label = EXCLUDED.label
		`, dimID, d.code, d.label)
		must(err, "dimension member "+d.code)
	}
	printf("  Members   : ENG, SALES, MKTG, G&A\n")

	// Metrics — three inputs + one calculated
	metricDefs := []struct {
		name    string
		label   string
		isInput bool
		formula string
	}{
		{"hc_cost", "Headcount Cost", true, ""},
		{"hr_cost", "HR Cost", true, ""},
		{"software_cost", "Software Cost", true, ""},
		{"travel_cost", "Travel & Expenses", true, ""},
		{"total_opex", "Total OPEX", false, "{hc_cost} + {hr_cost} + {software_cost} + {travel_cost}"},
	}

	metrics := make(map[string]string) // name → id
	for _, m := range metricDefs {
		id := mustScan(pool.QueryRow(ctx, `
			INSERT INTO model.metric_def (model_id, revision_id, name, formula, is_input)
			VALUES ($1::uuid, $2::uuid, $3, NULLIF($4,''), $5)
			ON CONFLICT (model_id, revision_id, name) WHERE revision_id IS NOT NULL
			DO UPDATE SET formula = EXCLUDED.formula
			RETURNING id::text
		`, modelID, scenarioID, m.name, m.formula, m.isInput), "metric "+m.name)
		metrics[m.name] = id
		flag := "INPUT"
		if !m.isInput {
			flag = "CALC "
		}
		printf("  Metric [%s]: %-20s (%s)\n", flag, m.label, short(id))
	}

	// total_opex depends on all four inputs
	for _, dep := range []string{"hc_cost", "hr_cost", "software_cost", "travel_cost"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id)
			VALUES ($1::uuid, $2::uuid)
			ON CONFLICT DO NOTHING
		`, metrics["total_opex"], metrics[dep])
		must(err, "dependency total_opex→"+dep)
	}
	printf("  Dependencies: total_opex ← hc_cost + hr_cost + software_cost + travel_cost\n")

	// Default grid: all metrics × department dimension
	gridID := getOrInsert(ctx, pool,
		`SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='OPEX Budget' LIMIT 1`,
		`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'OPEX Budget', $2::uuid) RETURNING id::text`,
		"grid def", modelID, scenarioID)
	for i, name := range []string{"hc_cost", "hr_cost", "software_cost", "travel_cost", "total_opex"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.grid_metric (grid_id, metric_id, sort_order)
			VALUES ($1::uuid, $2::uuid, $3)
			ON CONFLICT DO NOTHING
		`, gridID, metrics[name], i)
		must(err, "grid metric "+name)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO model.grid_dimension (grid_id, dimension_id)
		VALUES ($1::uuid, $2::uuid)
		ON CONFLICT DO NOTHING
	`, gridID, dimID)
	must(err, "grid dimension")
	printf("  Grid: 'OPEX Budget' (%s) — 5 metrics × department\n", short(gridID))

	// ── Dashboard definition (visible to business users) ──────────────────────

	var dashID string
	err = pool.QueryRow(ctx,
		`SELECT id::text FROM model.dashboard_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='OPEX Planning' LIMIT 1`,
		modelID, scenarioID,
	).Scan(&dashID)
	if err != nil {
		dashID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO model.dashboard_def (model_id, name, tags, revision_id) VALUES ($1::uuid,'OPEX Planning','{OPEX}',$2::uuid) RETURNING id::text`,
			modelID, scenarioID,
		), "dashboard def")
	}
	var wCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.dashboard_widget WHERE dashboard_id=$1::uuid`, dashID).Scan(&wCount)
	if wCount == 0 {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, title, show_title, pos_x, pos_y, size_w, size_h, sort_order)
			VALUES ($1::uuid, 'grid', $2, 'OPEX Budget Grid', true, 0, 0, 1200, 360, 0)
		`, dashID, gridID)
		must(err, "dashboard widget")
	} else {
		// Keep exactly one widget; remove any duplicates from repeated seed runs.
		_, _ = pool.Exec(ctx, `
			DELETE FROM model.dashboard_widget
			WHERE dashboard_id=$1::uuid AND id NOT IN (
				SELECT id FROM model.dashboard_widget WHERE dashboard_id=$1::uuid ORDER BY created_at LIMIT 1
			)
		`, dashID)
	}
	printf("  Dashboard: 'OPEX Planning' (%s)\n", short(dashID))

	// ── Security ──────────────────────────────────────────────────────────────

	section("Configuring RACI policies")

	_, err = pool.Exec(ctx, `
		INSERT INTO security.raci_rule (application_id, user_id, resource_pattern, raci_type)
		VALUES
		    ($1::uuid, $2::uuid, '*', 'responsible'),
		    ($1::uuid, $3::uuid, '*', 'accountable')
		ON CONFLICT DO NOTHING
	`, appID, deptHeadID, financeID)
	must(err, "raci rules")
	printf("  dept.head@acme.com → Responsible\n")
	printf("  finance@acme.com   → Accountable\n")

	// Metric write policy: dept head can write input metrics
	for _, name := range []string{"hc_cost", "software_cost", "travel_cost"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO security.metric_policy
			    (application_id, user_id, metric_id, can_read, can_write)
			VALUES ($1::uuid, $2::uuid, $3::uuid, true, true)
			ON CONFLICT DO NOTHING
		`, appID, deptHeadID, metrics[name])
		must(err, "metric policy "+name)
	}
	printf("  dept.head can write: hc_cost, software_cost, travel_cost\n")

	// ── Workflow definition ───────────────────────────────────────────────────

	section("Creating approval workflow")

	wfStore := workflow.NewStore(pool)
	wfDef, err := wfStore.CreateWorkflowDefFull(ctx, appID, "",
		"Budget Approval",
		"Finance approval for the FY2026 OPEX budget. Started from the OPEX Planning dashboard's 'Submit for Approval' button.",
		"budget.submitted", financeID)
	must(err, "workflow def")

	// Steps in the Developer Console designer's serialization — string step
	// types, approve/reject routes, instructions — so the Workflows canvas
	// shows real branch targets and the inbox card tells the approver what
	// to do. Approve routes to the notify step; reject ends the workflow.
	steps := json.RawMessage(`[
		{"id":"step-finance-review","name":"Finance Review","type":"approval",
		 "assignee_roles":["business_admin"],"sla_hours":72,"required_comment":true,
		 "instructions":"Review the submitted FY2026 OPEX budget on the OPEX Planning dashboard. Approve to notify the submitter, or reject to end the workflow. A comment is required either way.",
		 "routes":{"approve":"step-notify","reject":"end-rejected"}},
		{"id":"step-notify","name":"Notify Submitter","type":"notification",
		 "notification":{"recipient_type":"requester","subject":"FY2026 OPEX budget approved",
		 "message":"Jordan approved the FY2026 OPEX budget you submitted. It is now finalized on the OPEX Planning dashboard."},
		 "routes":{"next":"end-completed"}}
	]`)
	_, err = wfStore.UpdateWorkflowDefFull(ctx, wfDef.ID, wfDef.Name, wfDef.Description, wfDef.TriggerEvent, "", financeID, steps, nil, nil)
	must(err, "workflow steps")
	printf("  Def: %q (%s)\n", wfDef.Name, short(wfDef.ID))
	printf("  Steps: Finance Review (approval, 72h SLA) —approve→ Notify Submitter | —reject→ End\n")

	// Publish so the manual automation rule created below can actually fire
	// it — TriggerRule/ResolveStartContext refuse unpublished workflows, and
	// new defs start in 'draft'.
	_, err = wfStore.PublishWorkflowDef(ctx, wfDef.ID, financeID)
	must(err, "publish workflow def")

	// ── Input data — per department ───────────────────────────────────────────

	section("Writing FY2026 Budget input data (per department)")

	type deptFact struct {
		dept   string
		hcCost float64
		swCost float64
		trCost float64
	}
	deptFacts := []deptFact{
		{"ENG", 120_000, 30_000, 15_000},
		{"SALES", 40_000, 10_000, 8_000},
		{"MKTG", 25_000, 7_000, 5_000},
		{"GA", 15_000, 3_000, 2_000},
	}

	type deptMetricFact struct {
		metric string
		value  float64
	}

	var totalHC, totalSW, totalTR float64
	for _, df := range deptFacts {
		dimJSON := fmt.Sprintf(`{"%s": "%s"}`, dimID, df.dept)
		for _, f := range []deptMetricFact{
			{"hc_cost", df.hcCost},
			{"software_cost", df.swCost},
			{"travel_cost", df.trCost},
		} {
			_, err = pool.Exec(ctx, `
				INSERT INTO runtime.fact_input
				    (model_id, revision_name, metric_id, dim_members, value, entered_by, revision_id)
				VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6::uuid, $7::uuid)
			`, modelID, scenarioName, metrics[f.metric], dimJSON, f.value, deptHeadID, scenarioID)
			must(err, "fact "+df.dept+"."+f.metric)
		}
		printf("  %-6s  hc=%8.0f  sw=%7.0f  tr=%7.0f  opex=%9.0f\n",
			df.dept, df.hcCost, df.swCost, df.trCost, df.hcCost+df.swCost+df.trCost)
		totalHC += df.hcCost
		totalSW += df.swCost
		totalTR += df.trCost
	}

	// Aggregate row (dim_members='{}') — used by the calculation engine
	for _, f := range []deptMetricFact{
		{"hc_cost", totalHC},
		{"software_cost", totalSW},
		{"travel_cost", totalTR},
	} {
		_, err = pool.Exec(ctx, `
			INSERT INTO runtime.fact_input
			    (model_id, revision_name, metric_id, dim_members, value, entered_by, revision_id)
			VALUES ($1::uuid, $2, $3::uuid, '{}', $4, $5::uuid, $6::uuid)
		`, modelID, scenarioName, metrics[f.metric], f.value, deptHeadID, scenarioID)
		must(err, "aggregate fact "+f.metric)
	}
	printf("  %s\n", strings.Repeat("─", 55))
	printf("  %-6s  hc=%8.0f  sw=%7.0f  tr=%7.0f  opex=%9.0f\n",
		"TOTAL", totalHC, totalSW, totalTR, totalHC+totalSW+totalTR)

	// ── Calculation ───────────────────────────────────────────────────────────

	section("Running calculation engine")

	calcStore := calculation.NewStore(pool)
	scheduler := calculation.NewScheduler(log, calcStore, nil) // nil NATS: direct call
	changedInputs := []string{metrics["hc_cost"], metrics["software_cost"], metrics["travel_cost"]}
	must(scheduler.RecalcAffected(ctx, modelID, scenarioID, changedInputs), "recalc")

	result, err := calcStore.GetCalcValue(ctx, modelID, scenarioID, metrics["total_opex"], map[string]string{})
	must(err, "get calc value")

	match := "✓"
	if result != 280_000 {
		match = "✗ MISMATCH"
	}
	printf("  total_opex = %.2f  %s\n", result, match)

	// ── Workflow instance ─────────────────────────────────────────────────────

	section("Starting budget approval workflow")

	inst, err := wfStore.StartWorkflow(ctx, wfDef.ID, deptHeadID, map[string]string{
		"revision": scenarioName,
		"model_id": modelID,
	})
	must(err, "start workflow")
	printf("  Instance: %s (status: %s)\n", short(inst.Id), inst.Status)

	// Find the first step (Finance Review)
	_, instSteps, err := wfStore.GetWorkflowInstance(ctx, inst.Id)
	must(err, "get instance")

	var financeStepID string
	for _, s := range instSteps {
		if s.StepDefId == "step-finance-review" {
			financeStepID = s.Id
			break
		}
	}
	if financeStepID == "" {
		fatalf("finance review step not found")
	}
	printf("  Finance Review step: %s (status: %s)\n", short(financeStepID), instSteps[0].Status)

	section("Jordan (Finance) approves the budget")
	printf("  Completing step as finance@acme.com...\n")
	completedStep, err := wfStore.CompleteStep(ctx, financeStepID, financeID, "approve", "Looks good, approved for FY2026.")
	must(err, "complete step")
	printf("  Decision: %q\n", completedStep.Decision)
	printf("  Comment : %q\n", completedStep.Comment)

	// Auto-complete the notification step (in production this fires automatically)
	_, notifySteps, err := wfStore.GetWorkflowInstance(ctx, inst.Id)
	must(err, "get instance after approval")
	for _, s := range notifySteps {
		if s.StepDefId == "step-notify" && (s.Status == workflowv1.StepStatus_STEP_STATUS_IN_PROGRESS || s.Status == workflowv1.StepStatus_STEP_STATUS_PENDING) {
			_, _ = wfStore.CompleteStep(ctx, s.Id, financeID, "sent", "Notification dispatched")
		}
	}

	// Final instance status
	finalInst, _, err := wfStore.GetWorkflowInstance(ctx, inst.Id)
	must(err, "final instance")

	// ── Audit events ──────────────────────────────────────────────────────────

	section("Seeding audit log")

	auditEvents := []struct {
		category     string
		eventType    string
		actorID      string
		actorRole    string
		resourceType string
		resourceID   string
		revisionID   string
		metadata     string
	}{
		{"admin", "user.created", platformAdminID, "platform_admin", "user", deptHeadID, "", `{"email":"dept.head@acme.com","role":"business_user"}`},
		{"admin", "user.created", platformAdminID, "platform_admin", "user", financeID, "", `{"email":"finance@acme.com","role":"business_admin"}`},
		{"admin", "user.created", platformAdminID, "platform_admin", "user", devID, "", `{"email":"dev@acme.com","role":"developer"}`},
		{"model_change", "metric.created", devID, "developer", "metric", metrics["hc_cost"], scenarioID, `{"name":"hc_cost","type":"input"}`},
		{"model_change", "metric.created", devID, "developer", "metric", metrics["software_cost"], scenarioID, `{"name":"software_cost","type":"input"}`},
		{"model_change", "metric.created", devID, "developer", "metric", metrics["travel_cost"], scenarioID, `{"name":"travel_cost","type":"input"}`},
		{"model_change", "metric.created", devID, "developer", "metric", metrics["total_opex"], scenarioID, `{"name":"total_opex","type":"calc","formula":"{hc_cost}+{hr_cost}+{software_cost}+{travel_cost}"}`},
		{"data_change", "cell.written", deptHeadID, "business_user", "fact_input", modelID, scenarioID, `{"metric":"hc_cost","dept":"ENG","value":120000}`},
		{"data_change", "cell.written", deptHeadID, "business_user", "fact_input", modelID, scenarioID, `{"metric":"hc_cost","dept":"SALES","value":40000}`},
		{"data_change", "cell.written", deptHeadID, "business_user", "fact_input", modelID, scenarioID, `{"metric":"hc_cost","dept":"MKTG","value":25000}`},
		{"data_change", "cell.written", deptHeadID, "business_user", "fact_input", modelID, scenarioID, `{"metric":"hc_cost","dept":"GA","value":15000}`},
		{"data_change", "budget.submitted", deptHeadID, "business_user", "workflow_instance", inst.Id, scenarioID, `{"revision":"FY2026 Budget"}`},
		{"data_change", "budget.approved", financeID, "business_admin", "workflow_step", financeStepID, scenarioID, `{"decision":"approve","comment":"Looks good, approved for FY2026."}`},
	}

	for _, e := range auditEvents {
		var revArg *string
		if e.revisionID != "" {
			revArg = &e.revisionID
		}
		_, err = pool.Exec(ctx, `
			INSERT INTO audit.audit_event
			    (category, event_type, actor_user_id, actor_role, workspace_id, resource_type, resource_id, revision_id, metadata)
			VALUES ($1::audit.event_category, $2, $3::uuid, $4, $5::uuid, $6, $7, $8::uuid, $9::jsonb)
		`, e.category, e.eventType, e.actorID, e.actorRole, workspaceID, e.resourceType, e.resourceID, revArg, e.metadata)
		must(err, "audit event "+e.eventType)
	}
	printf("  %d audit events inserted\n", len(auditEvents))

	// ── Automation rule ───────────────────────────────────────────────────────

	section("Creating automation rule")

	// Linked by workflow_def_id (not just name) so the trigger survives
	// workflow renames and shows up as a hard link in the Workflows tab's
	// Usage panel and the Triggers tab.
	autoRule, err := wfStore.CreateAutomationRule(ctx, appID, scenarioID,
		"budget_approval_trigger",
		"Triggers the budget approval workflow on manual request",
		"manual",
		"Budget Approval",
		wfDef.ID, "", "", nil,
	)
	must(err, "create automation rule")
	printf("  Rule: %q (%s) → workflow %s\n", autoRule.Name, short(autoRule.ID), short(wfDef.ID))

	// Give business users a self-service way to start it — without this, the
	// rule is only reachable from the Developer Console's Automation tab.
	var buttonCount int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM model.dashboard_widget WHERE dashboard_id=$1::uuid AND widget_type='automation_button'`,
		dashID,
	).Scan(&buttonCount)
	if buttonCount == 0 {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, title, show_title, pos_x, pos_y, size_w, size_h, sort_order)
			VALUES ($1::uuid, 'automation_button', $2, 'Submit for Approval', true, 0, 360, 300, 60, 1)
		`, dashID, autoRule.ID)
		must(err, "dashboard automation button widget")
	}
	printf("  Dashboard button: 'Submit for Approval' on 'OPEX Planning' → rule %s\n", short(autoRule.ID))

	// Text widget beside the button so business users can see who approves
	// and where, without asking a developer.
	var textCount int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM model.dashboard_widget WHERE dashboard_id=$1::uuid AND widget_type='text'`,
		dashID,
	).Scan(&textCount)
	if textCount == 0 {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.dashboard_widget (dashboard_id, widget_type, content, title, show_title, pos_x, pos_y, size_w, size_h, sort_order)
			VALUES ($1::uuid, 'text',
			        'How approval works: pressing "Submit for Approval" starts the Budget Approval workflow. Jordan (Finance, business admin) reviews it in Business Console → Workflow Inbox and approves or rejects with a comment. Approval notifies the submitter; rejection ends the workflow. Track progress under My History.',
			        'Approval process', true, 320, 360, 500, 120, 2)
		`, dashID)
		must(err, "dashboard approval explainer widget")
	}

	// ── CRUD Forms ────────────────────────────────────────────────────────────

	section("Seeding purchase request form")

	formID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'purchase_request', 'Purchase Request', '[
			{"name":"vendor","label":"Vendor","type":"text","required":true},
			{"name":"category","label":"Category","type":"select","required":true,"options":["IT","Marketing","Travel","Other"]},
			{"name":"amount","label":"Amount (USD)","type":"number","required":true},
			{"name":"currency","label":"Currency","type":"text","required":false},
			{"name":"justification","label":"Justification","type":"text","required":true},
			{"name":"is_urgent","label":"Urgent?","type":"boolean","required":false}
		]')
		ON CONFLICT (model_id, revision_id, name) DO UPDATE SET label = EXCLUDED.label
		RETURNING id::text
	`, modelID, scenarioID), "form_def")
	printf("  Form: Purchase Request (%s)\n", short(formID))

	// Form integration: Purchase Request → hc_cost (sum, approved, dept dimension)
	dimMappingJSON := fmt.Sprintf(`{"%s": "dept"}`, dimID)
	integrationID := getOrInsert(ctx, pool,
		`SELECT id::text FROM model.form_metric_mapping
		 WHERE model_id=$1::uuid AND name='HC Budget Requests → hc_cost' LIMIT 1`,
		`INSERT INTO model.form_metric_mapping
		   (model_id, form_id, grid_id, name, source_field, target_metric_id,
		    aggregation, posting_statuses, dimension_mappings, live_posting)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, 'HC Budget Requests → hc_cost', 'amount', $4::uuid,
		   'sum', ARRAY['approved'], $5::jsonb, true)
		 RETURNING id::text`,
		"form integration", modelID, formID, gridID, metrics["hc_cost"], dimMappingJSON)
	printf("  Integration: Purchase Request → hc_cost (%s)\n", short(integrationID))

	_, err = pool.Exec(ctx, `
		INSERT INTO runtime.form_record (form_id, data, status, created_by) VALUES
		($1::uuid, '{"vendor":"Acme Supplies Ltd","category":"IT","amount":15000,"currency":"USD","justification":"Annual software licence renewal","is_urgent":false}', 'submitted', $2::uuid),
		($1::uuid, '{"vendor":"TechCo GmbH","category":"IT","amount":4500,"currency":"USD","justification":"Hardware for new engineer","is_urgent":true}', 'draft', $2::uuid)
		ON CONFLICT DO NOTHING
	`, formID, deptHeadID)
	must(err, "form records")
	printf("  2 seeded purchase requests\n")

	// ── Summary ───────────────────────────────────────────────────────────────

	section("Demo complete — summary")
	printf("  Customer   : Acme Corp\n")
	printf("  Application: OPEX Planning 2026\n")
	printf("  Model ID   : %s\n", short(modelID))
	printf("  Scenario   : %s\n", scenarioName)
	printf("  total_opex : %.2f\n", result)
	printf("  Workflow   : %s → %s\n", wfDef.Name, finalInst.Status)
	printf("  DB ready   : %s\n", dsn)
	fmt.Println()
}

// ── output helpers ────────────────────────────────────────────────────────────

func section(title string) {
	fmt.Printf("\n\033[1;34m▶ %s\033[0m\n", title)
}

func step(msg string) {
	fmt.Printf("  %s... ", msg)
}

func ok() {
	fmt.Println("\033[32mok\033[0m")
}

func printf(format string, args ...any) {
	fmt.Printf(format, args...)
}

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

// mustScan runs a QueryRow scan for a single string column and exits on error.
func mustScan(row interface {
	Scan(dest ...any) error
}, context string) string {
	var id string
	if err := row.Scan(&id); err != nil {
		fmt.Fprintf(os.Stderr, "\n\033[31mERROR [%s]: %v\033[0m\n", context, err)
		os.Exit(1)
	}
	return id
}

// getOrInsert tries selectSQL first; if it returns no rows, runs insertSQL.
// Makes the seed safe to re-run when data already exists.
func getOrInsert(ctx context.Context, pool *pgxpool.Pool, selectSQL, insertSQL, label string, args ...any) string {
	var id string
	if err := pool.QueryRow(ctx, selectSQL, args...).Scan(&id); err == nil {
		return id
	}
	if err := pool.QueryRow(ctx, insertSQL, args...).Scan(&id); err != nil {
		fmt.Fprintf(os.Stderr, "\n\033[31mERROR [%s]: %v\033[0m\n", label, err)
		os.Exit(1)
	}
	return id
}

// keep time import used (time.Time used implicitly via timestamppb in workflow store)
var _ = time.Now
