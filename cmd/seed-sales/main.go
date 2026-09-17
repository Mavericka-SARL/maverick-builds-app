// cmd/seed-sales — Sales Tracker reference application demo.
//
// Creates the full scenario from scratch:
//
//	customer → workspace → application → model → dimension → metrics →
//	grids → forms → dashboard → workflows (×3) → triggers (×3) → facts → calc
//
// The model exercises all metric format types (number, currency, percentage)
// and all three trigger types (form_submit, api, manual) to enable full UI/UX
// testing of the Triggers, Workflows, and Dashboard tabs.
//
// Usage:
//
//	DATABASE_URL=postgres://... go run ./cmd/seed-sales
package main

import (
	"context"
	"fmt"
	"os"

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
	scenarioName = "Q1 2026"
)

func main() {
	log := logger.New("seed-sales")
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultDSN
	}

	section("Sales Tracker — Demo Seed")

	step("Connecting to PostgreSQL")
	pool, err := db.Connect(ctx, dsn)
	must(err, "connect")
	defer pool.Close()
	ok()

	step("Running migrations")
	must(migrate.Run(ctx, pool, migrationfs.FS, "."), "migrate")
	ok()

	// ── Org hierarchy ─────────────────────────────────────────────────────────

	section("Building org hierarchy")

	var appID, modelID, workspaceID, customerID string
	err = pool.QueryRow(ctx, `
		SELECT a.id::text, m.id::text, a.workspace_id::text, w.customer_id::text
		FROM core.application a
		JOIN core.model m ON m.application_id = a.id AND m.name = 'Sales Model'
		JOIN core.workspace w ON w.id = a.workspace_id
		WHERE a.name = 'Sales Tracker'
		ORDER BY a.created_at DESC LIMIT 1
	`).Scan(&appID, &modelID, &workspaceID, &customerID)
	if err != nil {
		customerID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO core.customer (name, plan) VALUES ('TechFlow Inc', 'enterprise') RETURNING id::text`),
			"customer")
		workspaceID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Revenue Operations') RETURNING id::text`,
			customerID), "workspace")
		appID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Sales Tracker', 'planning') RETURNING id::text`,
			workspaceID, customerID), "application")
		modelID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Sales Model') RETURNING id::text`,
			appID), "model")
	}
	_, _ = pool.Exec(ctx, `
		DELETE FROM core.application WHERE name = 'Sales Tracker' AND id != $1::uuid
	`, appID)

	printf("  Customer   : TechFlow Inc (%s)\n", short(customerID))
	printf("  Workspace  : Revenue Operations (%s)\n", short(workspaceID))
	printf("  Application: Sales Tracker (%s)\n", short(appID))
	printf("  Model      : Sales Model (%s)\n", short(modelID))

	// ── Users ─────────────────────────────────────────────────────────────────

	section("Creating demo users")

	salesRepID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('sales-rep-001', 'alex@techflow.com', 'Alex (Sales Rep)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id::text
	`, customerID), "sales rep")
	printf("  alex@techflow.com    (%s) — business_user\n", short(salesRepID))

	salesMgrID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('sales-mgr-001', 'jordan@techflow.com', 'Jordan (Sales Manager)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id::text
	`, customerID), "sales manager")
	printf("  jordan@techflow.com  (%s) — business_admin\n", short(salesMgrID))

	devID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('sales-dev-001', 'sam@techflow.com', 'Sam (Developer)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id::text
	`, customerID), "developer")
	printf("  sam@techflow.com     (%s) — developer\n", short(devID))

	adminID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('sales-admin-001', 'pat@techflow.com', 'Pat (Admin)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id::text
	`, customerID), "tenant admin")
	printf("  pat@techflow.com     (%s) — tenant_admin\n", short(adminID))

	platformAdminID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('sales-platform-001', 'platform@techflow.com', 'Platform Admin', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id::text
	`, customerID), "platform admin")
	printf("  platform@techflow.com (%s) — platform_admin\n", short(platformAdminID))

	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES
		    ($1::uuid, 'business_user',  $4::uuid),
		    ($2::uuid, 'business_admin', $4::uuid),
		    ($3::uuid, 'developer',      $4::uuid)
		ON CONFLICT DO NOTHING
	`, salesRepID, salesMgrID, devID, workspaceID)
	must(err, "role assignments")
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES ($1::uuid, 'tenant_admin'::identity.user_role, $2::uuid)
		ON CONFLICT DO NOTHING
	`, adminID, workspaceID)
	must(err, "tenant_admin role")
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES ($1::uuid, 'platform_admin', NULL)
		ON CONFLICT DO NOTHING
	`, platformAdminID)
	must(err, "platform_admin role")
	printf("  Roles assigned\n")
	_ = devID

	// Remove stale Acme Corp cross-assignments (non-developer business users
	// should not appear in TechFlow's workspace). demo-dev-001 is intentionally
	// kept — developers need cross-customer visibility.
	_, _ = pool.Exec(ctx, `
		DELETE FROM identity.role_assignment
		WHERE workspace_id = $1::uuid
		  AND user_id IN (
		      SELECT id FROM identity.user
		      WHERE keycloak_sub IN (
		          'demo-dept-head-001','demo-finance-001',
		          'demo-admin-001','demo-platform-admin-001',
		          'budget-tenant-admin-001','budget-cfo-001','budget-finmgr-001',
		          'budget-depteng-001','budget-deptsales-001','budget-deptops-001'
		      )
		  )
	`, workspaceID)
	printf("  Stale Acme Corp cross-assignments removed from Revenue Operations\n")

	// Grant Acme's developer (Sam, demo-dev-001) access to TechFlow's workspace
	// so he can see Sales Tracker and Sales Model across customers.
	_, _ = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		SELECT id, 'developer', $1::uuid
		FROM identity.user WHERE keycloak_sub = 'demo-dev-001'
		ON CONFLICT DO NOTHING
	`, workspaceID)
	printf("  Sam (dev@acme.com) granted developer access to Revenue Operations\n")

	// Clear stale manual app/model whitelist grants for demo users — seeded
	// state must reflect workspace membership, not one-off UI grants.
	_, _ = pool.Exec(ctx, `
		DELETE FROM identity.user_app_access
		WHERE user_id IN (SELECT id FROM identity.user WHERE keycloak_sub = 'demo-dev-001')
	`)
	_, _ = pool.Exec(ctx, `
		DELETE FROM identity.user_model_access
		WHERE user_id IN (SELECT id FROM identity.user WHERE keycloak_sub = 'demo-dev-001')
	`)
	printf("  Stale app/model whitelist grants cleared for Sam\n")

	// ── Scenario ──────────────────────────────────────────────────────────────

	section("Defining model structure")

	scenarioID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.revision (model_id, name, description)
		VALUES ($1::uuid, $2, 'Q1 2026 sales performance targets and actuals')
		ON CONFLICT (model_id, name) DO UPDATE SET description = EXCLUDED.description
		RETURNING id::text
	`, modelID, scenarioName), "scenario")
	_, err = pool.Exec(ctx, `
		UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name=$2 WHERE id=$3::uuid
	`, scenarioID, scenarioName, modelID)
	must(err, "set active revision")
	printf("  Scenario : %s (%s)\n", scenarioName, short(scenarioID))

	// ── Dimension ─────────────────────────────────────────────────────────────

	dimID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, revision_id, name, properties)
		VALUES ($1::uuid, $2::uuid, 'region', '[{"name":"code","type":"text","required":true}]')
		ON CONFLICT (model_id, revision_id, name) WHERE revision_id IS NOT NULL
		DO UPDATE SET properties = EXCLUDED.properties
		RETURNING id::text
	`, modelID, scenarioID), "region dimension")
	printf("  Dimension: region (%s)\n", short(dimID))

	for _, r := range []struct{ code, label string }{
		{"NORTH", "North"},
		{"SOUTH", "South"},
		{"EAST", "East"},
		{"WEST", "West"},
	} {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label)
			VALUES ($1::uuid, $2, $3)
			ON CONFLICT (dimension_id, code) DO UPDATE SET label = EXCLUDED.label
		`, dimID, r.code, r.label)
		must(err, "region member "+r.code)
	}
	printf("  Members  : NORTH, SOUTH, EAST, WEST\n")

	// ── Metrics ───────────────────────────────────────────────────────────────

	type metricSpec struct {
		name     string
		label    string
		isInput  bool
		formula  string
		format   string
		decimals int
		currency string
	}
	metricSpecs := []metricSpec{
		{"leads_in", "Leads In", true, "", "number", 0, ""},
		{"qualified_leads", "Qualified Leads", true, "", "number", 0, ""},
		{"deals_won", "Deals Won", true, "", "number", 0, ""},
		{"revenue", "Revenue", true, "", "currency", 0, "$"},
		{"cost_of_sales", "Cost of Sales", true, "", "currency", 0, "$"},
		{"win_rate", "Win Rate", true, "", "percentage", 1, ""},
		{"gross_profit", "Gross Profit", false, "{revenue} - {cost_of_sales}", "currency", 0, "$"},
	}

	metrics := make(map[string]string) // name → id
	for _, m := range metricSpecs {
		id := mustScan(pool.QueryRow(ctx, `
			INSERT INTO model.metric_def
			    (model_id, revision_id, name, formula, is_input, format, format_decimals, format_currency)
			VALUES ($1::uuid, $2::uuid, $3, NULLIF($4,''), $5, $6, $7, $8)
			ON CONFLICT (model_id, revision_id, name) WHERE revision_id IS NOT NULL
			DO UPDATE SET formula         = EXCLUDED.formula,
			              format          = EXCLUDED.format,
			              format_decimals = EXCLUDED.format_decimals,
			              format_currency = EXCLUDED.format_currency
			RETURNING id::text
		`, modelID, scenarioID, m.name, m.formula, m.isInput, m.format, m.decimals, m.currency), "metric "+m.name)
		metrics[m.name] = id
		kind := "INPUT"
		if !m.isInput {
			kind = "CALC "
		}
		printf("  Metric [%s] %-12s %s (%s)\n", kind, "("+m.format+")", m.label, short(id))
	}

	// gross_profit depends on revenue and cost_of_sales
	for _, dep := range []string{"revenue", "cost_of_sales"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id)
			VALUES ($1::uuid, $2::uuid)
			ON CONFLICT DO NOTHING
		`, metrics["gross_profit"], metrics[dep])
		must(err, "dependency gross_profit→"+dep)
	}
	printf("  Dependencies: gross_profit ← revenue - cost_of_sales\n")

	// ── Grid ──────────────────────────────────────────────────────────────────

	gridID := getOrInsert(ctx, pool,
		`SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Sales Performance' LIMIT 1`,
		`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Sales Performance', $2::uuid) RETURNING id::text`,
		"grid def", modelID, scenarioID)
	for i, name := range []string{"leads_in", "qualified_leads", "deals_won", "revenue", "cost_of_sales", "win_rate", "gross_profit"} {
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
	printf("  Grid     : 'Sales Performance' (%s) — 7 metrics × region\n", short(gridID))

	// ── Forms ─────────────────────────────────────────────────────────────────

	section("Creating forms")

	dealFormID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'deal_registration', 'Deal Registration', '[
		    {"name":"company",    "label":"Company",           "type":"text",    "required":true},
		    {"name":"deal_name",  "label":"Deal Name",         "type":"text",    "required":true},
		    {"name":"region",     "label":"Region",            "type":"select",  "required":true,  "options":["North","South","East","West"]},
		    {"name":"deal_value", "label":"Deal Value (USD)",  "type":"number",  "required":true},
		    {"name":"stage",      "label":"Stage",             "type":"select",  "required":true,  "options":["Prospect","Qualified","Proposal","Closed Won"]},
		    {"name":"notes",      "label":"Notes",             "type":"text",    "required":false}
		]')
		ON CONFLICT (model_id, revision_id, name) DO UPDATE SET label = EXCLUDED.label
		RETURNING id::text
	`, modelID, scenarioID), "deal registration form")
	printf("  Form: Deal Registration (%s)\n", short(dealFormID))

	leadFormID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'lead_intake', 'Lead Intake', '[
		    {"name":"company",         "label":"Company",              "type":"text",    "required":true},
		    {"name":"contact_name",    "label":"Contact Name",         "type":"text",    "required":true},
		    {"name":"source",          "label":"Lead Source",          "type":"select",  "required":true,  "options":["Web","Referral","Outbound","Event","Partner"]},
		    {"name":"region",          "label":"Region",               "type":"select",  "required":true,  "options":["North","South","East","West"]},
		    {"name":"estimated_value", "label":"Estimated Value (USD)","type":"number",  "required":false},
		    {"name":"is_enterprise",   "label":"Enterprise Account?",  "type":"boolean", "required":false}
		]')
		ON CONFLICT (model_id, revision_id, name) DO UPDATE SET label = EXCLUDED.label
		RETURNING id::text
	`, modelID, scenarioID), "lead intake form")
	printf("  Form: Lead Intake (%s)\n", short(leadFormID))

	// Seed form records
	_, err = pool.Exec(ctx, `
		INSERT INTO runtime.form_record (form_id, data, status, created_by) VALUES
		($1::uuid, '{"company":"Meridian Corp","deal_name":"Enterprise License","region":"North","deal_value":85000,"stage":"Proposal","notes":"Decision expected end of quarter"}', 'submitted', $2::uuid),
		($1::uuid, '{"company":"Atlas Systems","deal_name":"SaaS Subscription","region":"East","deal_value":24000,"stage":"Qualified","notes":""}', 'draft', $2::uuid),
		($1::uuid, '{"company":"Vertex AI","deal_name":"Professional Services","region":"West","deal_value":120000,"stage":"Closed Won","notes":"Signed 2026-01-15"}', 'submitted', $2::uuid)
		ON CONFLICT DO NOTHING
	`, dealFormID, salesRepID)
	must(err, "deal form records")

	_, err = pool.Exec(ctx, `
		INSERT INTO runtime.form_record (form_id, data, status, created_by) VALUES
		($1::uuid, '{"company":"NovaTech","contact_name":"Sarah Chen","source":"Web","region":"East","estimated_value":45000,"is_enterprise":true}', 'submitted', $2::uuid),
		($1::uuid, '{"company":"BlueSky Ltd","contact_name":"Marcus Williams","source":"Referral","region":"North","estimated_value":18000,"is_enterprise":false}', 'draft', $2::uuid)
		ON CONFLICT DO NOTHING
	`, leadFormID, salesRepID)
	must(err, "lead form records")
	printf("  Records  : 3 deal registrations + 2 lead intake records seeded\n")

	// ── Dashboards ────────────────────────────────────────────────────────────

	section("Creating dashboards")

	// Wipe all dashboards for this revision and rebuild from scratch so the
	// seed is fully idempotent regardless of prior runs.
	_, _ = pool.Exec(ctx,
		`DELETE FROM model.dashboard_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`,
		modelID, scenarioID)

	repDashID := mustScan(pool.QueryRow(ctx,
		`INSERT INTO model.dashboard_def (model_id, name, tags, revision_id) VALUES ($1::uuid, 'My Pipeline', '{Sales,Q1}', $2::uuid) RETURNING id::text`,
		modelID, scenarioID), "rep dashboard")
	mgrDashID := mustScan(pool.QueryRow(ctx,
		`INSERT INTO model.dashboard_def (model_id, name, tags, revision_id) VALUES ($1::uuid, 'Team Performance', '{Sales,Q1}', $2::uuid) RETURNING id::text`,
		modelID, scenarioID), "mgr dashboard")

	printf("  My Pipeline    (%s) — Sales Rep\n", short(repDashID))
	printf("  Team Performance (%s) — Sales Manager\n", short(mgrDashID))

	// ── Workflows ─────────────────────────────────────────────────────────────

	section("Creating workflows")

	wfStore := workflow.NewStore(pool)

	// 1. Deal Approval — triggered when a deal registration form is submitted
	wfDeal, err := wfStore.CreateWorkflowDef(ctx, appID, "Deal Approval", "form.submit",
		[]*workflowv1.WorkflowStepDef{
			{
				Id:            "step-manager-review",
				Name:          "Sales Manager Review",
				Type:          workflowv1.StepType_STEP_TYPE_APPROVAL,
				AssigneeRoles: []string{"business_admin"},
				SlaHours:      24,
				NextStepIds:   []string{"step-crm-update"},
			},
			{
				Id:          "step-crm-update",
				Name:        "CRM Update Notification",
				Type:        workflowv1.StepType_STEP_TYPE_NOTIFICATION,
				NextStepIds: []string{},
			},
		},
	)
	must(err, "deal approval workflow")
	printf("  Workflow : %q [form.submit] (%s)\n", wfDeal.Name, short(wfDeal.Id))
	printf("             Sales Manager Review (approval 24h SLA) → CRM Update Notification\n")

	// 2. Forecast Review — triggered by an external API call at month close
	wfForecast, err := wfStore.CreateWorkflowDef(ctx, appID, "Forecast Review", "api.workflow.start",
		[]*workflowv1.WorkflowStepDef{
			{
				Id:            "step-cfo-review",
				Name:          "CFO Review",
				Type:          workflowv1.StepType_STEP_TYPE_APPROVAL,
				AssigneeRoles: []string{"business_admin"},
				SlaHours:      48,
				NextStepIds:   []string{"step-exec-notify"},
			},
			{
				Id:          "step-exec-notify",
				Name:        "Executive Notification",
				Type:        workflowv1.StepType_STEP_TYPE_NOTIFICATION,
				NextStepIds: []string{},
			},
		},
	)
	must(err, "forecast review workflow")
	printf("  Workflow : %q [api.workflow.start] (%s)\n", wfForecast.Name, short(wfForecast.Id))
	printf("             CFO Review (approval 48h SLA) → Executive Notification\n")

	// 3. Lead Assignment — triggered manually by a sales manager
	wfLead, err := wfStore.CreateWorkflowDef(ctx, appID, "Lead Assignment", "manual",
		[]*workflowv1.WorkflowStepDef{
			{
				Id:          "step-assign-notify",
				Name:        "Assignment Notification",
				Type:        workflowv1.StepType_STEP_TYPE_NOTIFICATION,
				NextStepIds: []string{},
			},
		},
	)
	must(err, "lead assignment workflow")
	printf("  Workflow : %q [manual] (%s)\n", wfLead.Name, short(wfLead.Id))
	printf("             Assignment Notification\n")

	// ── Triggers ──────────────────────────────────────────────────────────────

	section("Creating triggers")

	ruleSubmit, err := wfStore.CreateAutomationRule(ctx, appID, scenarioID,
		"new_deal_submitted",
		"Starts Deal Approval whenever a deal registration form is submitted",
		"form_submit",
		"Deal Approval",
		wfDeal.Id,
		dealFormID,
		"",
		nil,
	)
	must(err, "new deal trigger")
	printf("  Trigger  : %q [form_submit] (%s)\n", ruleSubmit.Name, short(ruleSubmit.ID))

	ruleAPI, err := wfStore.CreateAutomationRule(ctx, appID, scenarioID,
		"monthly_forecast",
		"Triggers the Forecast Review workflow via API call at month close",
		"api",
		"Forecast Review",
		wfForecast.Id,
		"",
		"",
		nil,
	)
	must(err, "monthly forecast trigger")
	printf("  Trigger  : %q [api] (%s)\n", ruleAPI.Name, short(ruleAPI.ID))

	ruleManual, err := wfStore.CreateAutomationRule(ctx, appID, scenarioID,
		"lead_assignment_request",
		"Manually assigns a batch of leads to sales reps",
		"manual",
		"Lead Assignment",
		wfLead.Id,
		"",
		"",
		nil,
	)
	must(err, "lead assignment trigger")
	printf("  Trigger  : %q [manual] (%s)\n", ruleManual.Name, short(ruleManual.ID))

	ruleForecastManual, err := wfStore.CreateAutomationRule(ctx, appID, scenarioID,
		"start_forecast_review",
		"Manually starts the month-end CFO forecast sign-off",
		"manual",
		"Forecast Review",
		wfForecast.Id,
		"",
		"",
		nil,
	)
	must(err, "start forecast review trigger")
	printf("  Trigger  : %q [manual] (%s)\n", ruleForecastManual.Name, short(ruleForecastManual.ID))

	// ── Dashboard widgets ─────────────────────────────────────────────────────
	// Inserted here so wfForecast.Id, ruleManual.ID, and form IDs are defined.

	section("Populating dashboard widgets")

	type wSpec struct {
		dashID, wtype, refID, title, content, props string
		showTitle                                   bool
		posX, posY, sizeW, sizeH, sort              int
	}
	insertWidget := func(w wSpec) {
		var refArg, propsArg, contentArg interface{}
		if w.refID != "" {
			refArg = w.refID
		}
		if w.props != "" {
			propsArg = w.props
		}
		if w.content != "" {
			contentArg = w.content
		}
		_, err = pool.Exec(ctx, `
			INSERT INTO model.dashboard_widget
			    (dashboard_id, widget_type, ref_id, title, show_title, content, pos_x, pos_y, size_w, size_h, sort_order, widget_props)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb)
		`, w.dashID, w.wtype, refArg, nilStr(w.title), w.showTitle, contentArg,
			w.posX, w.posY, w.sizeW, w.sizeH, w.sort, propsArg)
		must(err, "widget "+w.wtype)
		printf("  [%s] %s %q\n", w.dashID[:8], w.wtype, w.title)
	}

	// ── My Pipeline (Sales Rep) ────────────────────────────────────────────
	// Row 0 (y=  0): instructions — full width
	// Row 1 (y=100): Assign Leads button — full width
	// Row 2 (y=200): Deal Registration form — full width
	// Row 3 (y=700): Lead Intake form — full width
	printf("\n  My Pipeline widgets:\n")
	insertWidget(wSpec{
		dashID: repDashID, wtype: "text", showTitle: false,
		content: "Log new deals and leads as they come in.\n" +
			"• Fill in Deal Registration to register a new opportunity — this automatically starts the Deal Approval workflow.\n" +
			"• Fill in Lead Intake to record incoming leads from campaigns or events.\n" +
			"• Click Assign Leads to batch-route unassigned leads to the right rep.",
		props: `{"font_size":13,"color":"#374151"}`,
		posX:  0, posY: 0, sizeW: 1200, sizeH: 72, sort: 0,
	})
	insertWidget(wSpec{
		dashID: repDashID, wtype: "automation_button", refID: ruleManual.ID,
		title: "Assign Leads", showTitle: true,
		content: "Assign Leads",
		props:   `{"button_color":"#0ea5e9"}`,
		posX:    0, posY: 100, sizeW: 1200, sizeH: 56, sort: 1,
	})
	insertWidget(wSpec{
		dashID: repDashID, wtype: "form", refID: dealFormID,
		title: "Deal Registration", showTitle: true,
		posX: 0, posY: 200, sizeW: 1200, sizeH: 460, sort: 2,
	})
	insertWidget(wSpec{
		dashID: repDashID, wtype: "form", refID: leadFormID,
		title: "Lead Intake", showTitle: true,
		posX: 0, posY: 700, sizeW: 1200, sizeH: 400, sort: 3,
	})

	// ── Team Performance (Sales Manager) ──────────────────────────────────
	// Row 0 (y=  0): instructions — full width
	// Row 1 (y=100): Start Forecast Review button — full width
	// Row 2 (y=200): Sales Performance Grid — full width
	printf("\n  Team Performance widgets:\n")
	insertWidget(wSpec{
		dashID: mgrDashID, wtype: "text", showTitle: false,
		content: "Monitor team performance and manage the pipeline.\n" +
			"• Review the Sales Performance Grid to track regional results.\n" +
			"• Click Start Forecast Review to initiate the month-end CFO sign-off.\n" +
			"• Use Workflow Inbox to approve pending deal registrations from your team.",
		props: `{"font_size":13,"color":"#374151"}`,
		posX:  0, posY: 0, sizeW: 1200, sizeH: 72, sort: 0,
	})
	insertWidget(wSpec{
		dashID: mgrDashID, wtype: "automation_button", refID: ruleForecastManual.ID,
		title: "Start Forecast Review", showTitle: true,
		content: "Start Forecast Review",
		props:   `{"button_color":"#6366f1"}`,
		posX:    0, posY: 100, sizeW: 1200, sizeH: 56, sort: 1,
	})
	insertWidget(wSpec{
		dashID: mgrDashID, wtype: "grid", refID: gridID,
		title: "Sales Performance Grid", showTitle: true,
		posX: 0, posY: 200, sizeW: 1200, sizeH: 480, sort: 2,
	})

	// ── Business roles ────────────────────────────────────────────────────
	// Wipe and recreate so roles stay in sync with the dashboards above.
	section("Configuring business roles")

	_, _ = pool.Exec(ctx,
		`DELETE FROM identity.business_role WHERE workspace_id=$1::uuid AND name IN ('Sales Rep', 'Sales Manager')`,
		workspaceID)

	repRoleID := mustScan(pool.QueryRow(ctx,
		`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Sales Rep') RETURNING id::text`,
		workspaceID), "sales rep role")
	mgrRoleID := mustScan(pool.QueryRow(ctx,
		`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Sales Manager') RETURNING id::text`,
		workspaceID), "sales manager role")

	// Link dashboards to roles
	_, err = pool.Exec(ctx,
		`INSERT INTO identity.business_role_dashboard VALUES ($1::uuid, $2::uuid)`, repRoleID, repDashID)
	must(err, "rep dash link")
	_, err = pool.Exec(ctx,
		`INSERT INTO identity.business_role_dashboard VALUES ($1::uuid, $2::uuid)`, mgrRoleID, mgrDashID)
	must(err, "mgr dash link")

	// Assign users to roles
	_, err = pool.Exec(ctx,
		`INSERT INTO identity.business_role_member VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`, repRoleID, salesRepID)
	must(err, "rep member")
	_, err = pool.Exec(ctx,
		`INSERT INTO identity.business_role_member VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`, mgrRoleID, salesMgrID)
	must(err, "mgr member")

	printf("  Sales Rep    role → Alex  → My Pipeline\n")
	printf("  Sales Manager role → Jordan → Team Performance\n")

	// ── Input data ─────────────────────────────────────────────────────────────

	section("Seeding Q1 2026 input data (per region)")

	type regionFact struct {
		code        string
		leadsIn     float64
		qualified   float64
		dealsWon    float64
		revenue     float64
		costOfSales float64
		winRate     float64
	}
	regionFacts := []regionFact{
		{"NORTH", 45, 20, 8, 320_000, 95_000, 38.1},
		{"SOUTH", 38, 15, 6, 210_000, 72_000, 37.5},
		{"EAST", 52, 28, 12, 480_000, 140_000, 42.9},
		{"WEST", 30, 12, 5, 175_000, 52_000, 41.7},
	}

	var totalLeads, totalQualified, totalDeals, totalRevenue, totalCOGS float64
	for _, rf := range regionFacts {
		dimJSON := fmt.Sprintf(`{"%s": "%s"}`, dimID, rf.code)
		for _, f := range []struct {
			metric string
			value  float64
		}{
			{"leads_in", rf.leadsIn},
			{"qualified_leads", rf.qualified},
			{"deals_won", rf.dealsWon},
			{"revenue", rf.revenue},
			{"cost_of_sales", rf.costOfSales},
			{"win_rate", rf.winRate},
		} {
			_, err = pool.Exec(ctx, `
				INSERT INTO runtime.fact_input
				    (model_id, revision_name, metric_id, dim_members, value, entered_by, revision_id)
				VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6::uuid, $7::uuid)
			`, modelID, scenarioName, metrics[f.metric], dimJSON, f.value, salesRepID, scenarioID)
			must(err, "fact "+rf.code+"."+f.metric)
		}
		printf("  %-6s leads=%2.0f  qual=%2.0f  won=%2.0f  rev=$%9.0f  gp=$%8.0f  wr=%.1f%%\n",
			rf.code, rf.leadsIn, rf.qualified, rf.dealsWon,
			rf.revenue, rf.revenue-rf.costOfSales, rf.winRate)
		totalLeads += rf.leadsIn
		totalQualified += rf.qualified
		totalDeals += rf.dealsWon
		totalRevenue += rf.revenue
		totalCOGS += rf.costOfSales
	}

	// Aggregate rows (dim_members='{}') for the total row
	totalWinRate := totalDeals / totalLeads * 100
	for _, f := range []struct {
		metric string
		value  float64
	}{
		{"leads_in", totalLeads},
		{"qualified_leads", totalQualified},
		{"deals_won", totalDeals},
		{"revenue", totalRevenue},
		{"cost_of_sales", totalCOGS},
		{"win_rate", totalWinRate},
	} {
		_, err = pool.Exec(ctx, `
			INSERT INTO runtime.fact_input
			    (model_id, revision_name, metric_id, dim_members, value, entered_by, revision_id)
			VALUES ($1::uuid, $2, $3::uuid, '{}', $4, $5::uuid, $6::uuid)
		`, modelID, scenarioName, metrics[f.metric], f.value, salesRepID, scenarioID)
		must(err, "aggregate fact "+f.metric)
	}
	printf("  %-6s leads=%2.0f  qual=%2.0f  won=%2.0f  rev=$%9.0f  gp=$%8.0f  wr=%.1f%%\n",
		"TOTAL", totalLeads, totalQualified, totalDeals,
		totalRevenue, totalRevenue-totalCOGS, totalWinRate)

	// ── Calculation ─────────────────────────────────────────────────────────────

	section("Running calculation engine")

	calcStore := calculation.NewStore(pool)
	scheduler := calculation.NewScheduler(log, calcStore, nil)
	must(scheduler.RecalcAffected(ctx, modelID, scenarioID, []string{metrics["revenue"], metrics["cost_of_sales"]}), "recalc")

	result, err := calcStore.GetCalcValue(ctx, modelID, scenarioID, metrics["gross_profit"], map[string]string{})
	must(err, "get calc value")
	expected := totalRevenue - totalCOGS
	check := "✓"
	if result != expected {
		check = fmt.Sprintf("✗ MISMATCH (expected %.2f)", expected)
	}
	printf("  gross_profit (total) = $%.2f  %s\n", result, check)

	// ── Workflow instance ─────────────────────────────────────────────────────

	section("Starting a Deal Approval workflow instance")

	inst, err := wfStore.StartWorkflow(ctx, wfDeal.Id, salesRepID, map[string]string{
		"deal":   "Meridian Corp — Enterprise License",
		"value":  "85000",
		"region": "North",
	})
	must(err, "start deal approval instance")
	printf("  Instance : %s (status: %s)\n", short(inst.Id), inst.Status)

	_, steps, err := wfStore.GetWorkflowInstance(ctx, inst.Id)
	must(err, "get workflow instance")
	for _, s := range steps {
		if s.StepDefId == "step-manager-review" {
			printf("  Step     : 'Sales Manager Review' %s — awaiting Jordan's approval\n", short(s.Id))
			break
		}
	}

	// ── RACI ──────────────────────────────────────────────────────────────────

	section("Configuring RACI policies")

	_, err = pool.Exec(ctx, `
		INSERT INTO security.raci_rule (application_id, user_id, resource_pattern, raci_type)
		VALUES
		    ($1::uuid, $2::uuid, '*', 'responsible'),
		    ($1::uuid, $3::uuid, '*', 'accountable')
		ON CONFLICT DO NOTHING
	`, appID, salesRepID, salesMgrID)
	must(err, "raci rules")
	printf("  alex@techflow.com   → Responsible\n")
	printf("  jordan@techflow.com → Accountable\n")

	// Metric write policy: sales rep can write all input metrics
	for _, name := range []string{"leads_in", "qualified_leads", "deals_won", "revenue", "cost_of_sales", "win_rate"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO security.metric_policy (application_id, user_id, metric_id, can_read, can_write)
			VALUES ($1::uuid, $2::uuid, $3::uuid, true, true)
			ON CONFLICT DO NOTHING
		`, appID, salesRepID, metrics[name])
		must(err, "metric policy "+name)
	}
	printf("  alex can write: all 6 input metrics\n")

	// ── Summary ───────────────────────────────────────────────────────────────

	section("Demo complete — summary")
	printf("  Customer     : TechFlow Inc\n")
	printf("  Application  : Sales Tracker\n")
	printf("  Model ID     : %s\n", short(modelID))
	printf("  Scenario     : %s\n", scenarioName)
	printf("  Metrics      : 7 (number ×3, currency ×3, percentage ×1)\n")
	printf("  Workflows    : 3 (Deal Approval, Forecast Review, Lead Assignment)\n")
	printf("  Triggers     : 4 (form_submit, api, manual ×2)\n")
	printf("  Forms        : 2 (Deal Registration, Lead Intake)\n")
	printf("  gross_profit : $%.2f\n", result)
	printf("  DB           : %s\n", dsn)
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

func nilStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func must(err error, context string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n\033[31mERROR [%s]: %v\033[0m\n", context, err)
		os.Exit(1)
	}
}

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
