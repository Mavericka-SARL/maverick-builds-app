// cmd/seed-budget — Budget Planning reference application demo.
//
// Creates a full revenue & cost budgeting scenario from scratch:
//
//	customer(reused) → workspace(Budget) → application → model →
//	dimensions(department,cost_type,budget_period) → metrics(revenue+cost) →
//	users(7 roles) → RACI+metric policies → grids(3 dashboards) →
//	workflow(2-step approval) → forms → automation rule → input data →
//	calculation → workflow instance → audit log
//
// Usage:
//
//	DATABASE_URL=postgres://... go run ./cmd/seed-budget
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
	scenarioName = "FY2026 Budget"
)

func main() {
	log := logger.New("seed-budget")
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultDSN
	}

	section("Budget Planning FY2026 — Demo Seed")

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

	// Reuse existing hierarchy when it already exists (idempotent re-runs).
	var customerID, workspaceID, appID, modelID string
	err = pool.QueryRow(ctx, `
		SELECT a.id::text, m.id::text, a.workspace_id::text, w.customer_id::text
		FROM core.application a
		JOIN core.model m ON m.application_id = a.id AND m.name = 'Budget Model'
		JOIN core.workspace w ON w.id = a.workspace_id
		WHERE a.name = 'Budget Planning FY2026'
		ORDER BY a.created_at DESC LIMIT 1
	`).Scan(&appID, &modelID, &workspaceID, &customerID)
	if err != nil {
		// Nothing exists yet — create the full hierarchy from scratch.
		err = pool.QueryRow(ctx, `SELECT id::text FROM core.customer LIMIT 1`).Scan(&customerID)
		if err != nil {
			customerID = mustScan(pool.QueryRow(ctx,
				`INSERT INTO core.customer (name, plan) VALUES ('Acme Corp', 'enterprise') RETURNING id::text`),
				"customer")
		}
		workspaceID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Finance & Budget') RETURNING id::text`,
			customerID), "workspace")
		appID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Budget Planning FY2026', 'planning') RETURNING id::text`,
			workspaceID, customerID), "application")
		modelID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Budget Model') RETURNING id::text`,
			appID), "model")
	}
	// Remove any stale duplicate budget apps and the empty workspaces they left behind.
	_, _ = pool.Exec(ctx, `
		DELETE FROM core.application
		WHERE name = 'Budget Planning FY2026' AND id != $1::uuid
	`, appID)
	_, _ = pool.Exec(ctx, `
		DELETE FROM core.workspace
		WHERE name = 'Finance & Budget' AND id != $1::uuid
		AND NOT EXISTS (SELECT 1 FROM core.application WHERE workspace_id = core.workspace.id)
	`, workspaceID)
	printf("  Customer   : %s\n", short(customerID))
	printf("  Workspace  : Finance & Budget (%s)\n", short(workspaceID))
	printf("  Application: Budget Planning FY2026 (%s)\n", short(appID))
	printf("  Model      : Budget Model (%s)\n", short(modelID))

	// ── Users — all roles ─────────────────────────────────────────────────────

	section("Creating demo users (all roles)")

	// Platform Admin — reuse the sub from the OPEX seed to avoid email conflicts across seeds
	platformAdminID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('demo-platform-admin-001', 'platform-admin@acme.com', 'Pat (Platform Admin)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id::text
	`, customerID), "platform admin")
	printf("  platform-admin@acme.com  (%s) — platform_admin\n", short(platformAdminID))

	// Tenant Admin — manages users and workspaces for Acme Corp
	tenantAdminID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('budget-tenant-admin-001', 'tenant-admin@acme.com', 'Charlie (Tenant Admin)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
		RETURNING id::text
	`, customerID), "tenant admin")
	printf("  tenant-admin@acme.com    (%s) — tenant_admin\n", short(tenantAdminID))

	// Developer — reuse the sub from the OPEX seed to avoid email conflicts across seeds
	devID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('demo-dev-001', 'dev@acme.com', 'Sam (Developer)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id::text
	`, customerID), "developer")
	printf("  dev@acme.com             (%s) — developer\n", short(devID))

	// CFO — final approver, can read/write all metrics, creates roles and access rules
	cfoID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('budget-cfo-001', 'cfo@acme.com', 'Dana (CFO)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
		RETURNING id::text
	`, customerID), "cfo")
	printf("  cfo@acme.com             (%s) — business_admin (CFO)\n", short(cfoID))

	// Finance Manager — reviewer, manages budget policies (role created by business admin)
	finMgrID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('budget-finmgr-001', 'finance.manager@acme.com', 'Riley (Finance Manager)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
		RETURNING id::text
	`, customerID), "finance manager")
	printf("  finance.manager@acme.com (%s) — business_admin (Finance Mgr)\n", short(finMgrID))

	// Dept Head — Engineering (budget owner for ENG cost center)
	deptEngID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('budget-depteng-001', 'dept.head.eng@acme.com', 'Alex (Eng Head)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
		RETURNING id::text
	`, customerID), "eng head")
	printf("  dept.head.eng@acme.com   (%s) — business_user (ENG budget owner)\n", short(deptEngID))

	// Dept Head — Sales (budget owner for SALES cost center + revenue owner)
	deptSalesID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('budget-deptsales-001', 'dept.head.sales@acme.com', 'Morgan (Sales Head)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
		RETURNING id::text
	`, customerID), "sales head")
	printf("  dept.head.sales@acme.com (%s) — business_user (SALES revenue+cost owner)\n", short(deptSalesID))

	// Dept Head — Operations (budget owner for OPS+GA+IT+MKTG cost centers)
	deptOpsID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('budget-deptops-001', 'dept.head.ops@acme.com', 'Jordan (Ops Head)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
		RETURNING id::text
	`, customerID), "ops head")
	printf("  dept.head.ops@acme.com   (%s) — business_user (OPS/GA/IT/MKTG cost owner)\n", short(deptOpsID))

	// ── Role assignments ──────────────────────────────────────────────────────

	// platform_admin has NULL workspace_id
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES ($1::uuid, 'platform_admin', NULL)
		ON CONFLICT (user_id, role, workspace_id) DO NOTHING
	`, platformAdminID)
	must(err, "platform_admin role")

	// tenant_admin and workspace-scoped roles
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES
		    ($1::uuid, 'tenant_admin',    $6::uuid),
		    ($2::uuid, 'developer',       $6::uuid),
		    ($3::uuid, 'business_admin',  $6::uuid),
		    ($4::uuid, 'business_admin',  $6::uuid),
		    ($5::uuid, 'business_user',   $6::uuid)
		ON CONFLICT (user_id, role, workspace_id) DO NOTHING
	`, tenantAdminID, devID, cfoID, finMgrID, deptEngID, workspaceID)
	must(err, "workspace role assignments (1)")

	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES
		    ($1::uuid, 'business_user', $2::uuid),
		    ($3::uuid, 'business_user', $2::uuid)
		ON CONFLICT (user_id, role, workspace_id) DO NOTHING
	`, deptSalesID, workspaceID, deptOpsID)
	must(err, "workspace role assignments (2)")
	printf("  Roles assigned in Finance & Budget workspace\n")

	// Cross-assign OPEX users to the Budget workspace so they can switch
	// models using the Apps & Models tab. We look users up by keycloak_sub
	// so this is safe to run even if the OPEX seed was not run first.
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		SELECT u.id, 'business_user', $1::uuid
		FROM identity.user u
		WHERE u.keycloak_sub IN ('demo-dept-head-001', 'demo-finance-001')
		ON CONFLICT DO NOTHING
	`, workspaceID)
	must(err, "cross-assign OPEX users to budget workspace")
	printf("  OPEX dept.head + finance also granted business_user in Finance & Budget\n")

	// ── Scenario + Version ────────────────────────────────────────────────────

	section("Defining model structure")

	scenarioID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.revision (model_id, name, description)
		VALUES ($1::uuid, $2, 'Annual revenue and cost budget for FY2026')
		ON CONFLICT (model_id, name) DO UPDATE SET description = EXCLUDED.description
		RETURNING id::text
	`, modelID, scenarioName), "scenario")
	_, err = pool.Exec(ctx, `
		UPDATE core.model
		SET active_revision_id=$1::uuid, active_revision_name=$2
		WHERE id=$3::uuid
	`, scenarioID, scenarioName, modelID)
	must(err, "set active revision")
	printf("  Scenario   : %s (%s)\n", scenarioName, short(scenarioID))

	// ── Dimensions ────────────────────────────────────────────────────────────

	// Department dimension
	deptDimID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, revision_id, name, properties)
		VALUES ($1::uuid, $2::uuid, 'department', '[{"name":"code","type":"text","required":true}]')
		ON CONFLICT (model_id, revision_id, name) WHERE revision_id IS NOT NULL
		DO UPDATE SET properties = EXCLUDED.properties
		RETURNING id::text
	`, modelID, scenarioID), "department dimension")
	printf("  Dimension: department (%s)\n", short(deptDimID))

	departments := []struct{ code, label string }{
		{"ENG", "Engineering"},
		{"SALES", "Sales"},
		{"MKTG", "Marketing"},
		{"GA", "G&A"},
		{"OPS", "Operations"},
		{"IT", "Information Technology"},
	}
	for _, d := range departments {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label)
			VALUES ($1::uuid, $2, $3)
			ON CONFLICT (dimension_id, code) DO UPDATE SET label = EXCLUDED.label
		`, deptDimID, d.code, d.label)
		must(err, "dept member "+d.code)
	}
	printf("  Members   : ENG, SALES, MKTG, GA, OPS, IT\n")

	// Cost type dimension
	costTypeDimID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, revision_id, name, properties)
		VALUES ($1::uuid, $2::uuid, 'cost_type', '[{"name":"code","type":"text","required":true}]')
		ON CONFLICT (model_id, revision_id, name) WHERE revision_id IS NOT NULL
		DO UPDATE SET properties = EXCLUDED.properties
		RETURNING id::text
	`, modelID, scenarioID), "cost_type dimension")
	printf("  Dimension: cost_type (%s)\n", short(costTypeDimID))

	costTypes := []struct{ code, label string }{
		{"HC", "Headcount"},
		{"SW", "Software & Tools"},
		{"TRAVEL", "Travel & Expenses"},
		{"FACI", "Facilities"},
		{"CAPEX", "Capital Expenditure"},
		{"OTHER", "Other"},
	}
	for _, c := range costTypes {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label)
			VALUES ($1::uuid, $2, $3)
			ON CONFLICT (dimension_id, code) DO UPDATE SET label = EXCLUDED.label
		`, costTypeDimID, c.code, c.label)
		must(err, "cost type member "+c.code)
	}
	printf("  Members   : HC, SW, TRAVEL, FACI, CAPEX, OTHER\n")

	// Budget period dimension
	periodDimID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, revision_id, name, properties)
		VALUES ($1::uuid, $2::uuid, 'budget_period', '[{"name":"code","type":"text","required":true}]')
		ON CONFLICT (model_id, revision_id, name) WHERE revision_id IS NOT NULL
		DO UPDATE SET properties = EXCLUDED.properties
		RETURNING id::text
	`, modelID, scenarioID), "budget_period dimension")
	printf("  Dimension: budget_period (%s)\n", short(periodDimID))

	for _, q := range []struct{ code, label string }{
		{"Q1", "Q1 FY2026 (Jan-Mar)"},
		{"Q2", "Q2 FY2026 (Apr-Jun)"},
		{"Q3", "Q3 FY2026 (Jul-Sep)"},
		{"Q4", "Q4 FY2026 (Oct-Dec)"},
	} {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label)
			VALUES ($1::uuid, $2, $3)
			ON CONFLICT (dimension_id, code) DO UPDATE SET label = EXCLUDED.label
		`, periodDimID, q.code, q.label)
		must(err, "period member "+q.code)
	}
	printf("  Members   : Q1, Q2, Q3, Q4\n")

	// ── Metrics ───────────────────────────────────────────────────────────────

	type metricDef struct {
		name    string
		label   string
		isInput bool
		formula string
	}
	metricDefs := []metricDef{
		// Revenue
		{"revenue_target", "Revenue Target", true, ""},
		{"cogs_budget", "Cost of Goods Sold (COGS)", true, ""},
		{"gross_profit", "Gross Profit", false, "{revenue_target} - {cogs_budget}"},
		{"gross_margin_pct", "Gross Margin %", false, "{gross_profit} / {revenue_target} * 100"},
		// OpEx costs
		{"headcount_cost", "Headcount Cost", true, ""},
		{"software_cost", "Software & Tools", true, ""},
		{"travel_cost", "Travel & Expenses", true, ""},
		{"facilities_cost", "Facilities Cost", true, ""},
		{"other_opex", "Other OpEx", true, ""},
		{"total_opex", "Total OpEx", false, "{headcount_cost} + {software_cost} + {travel_cost} + {facilities_cost} + {other_opex}"},
		// CapEx
		{"capex_budget", "Capital Expenditure", true, ""},
		// Bottom line
		{"ebitda", "EBITDA", false, "{gross_profit} - {total_opex}"},
		{"ebitda_margin_pct", "EBITDA Margin %", false, "{ebitda} / {revenue_target} * 100"},
		{"net_budget", "Net Budget", false, "{ebitda} - {capex_budget}"},
	}

	metrics := make(map[string]string)
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
		printf("  Metric [%s]: %-30s (%s)\n", flag, m.label, short(id))
	}

	// Calculation dependencies
	calcDeps := map[string][]string{
		"gross_profit":      {"revenue_target", "cogs_budget"},
		"gross_margin_pct":  {"gross_profit", "revenue_target"},
		"total_opex":        {"headcount_cost", "software_cost", "travel_cost", "facilities_cost", "other_opex"},
		"ebitda":            {"gross_profit", "total_opex"},
		"ebitda_margin_pct": {"ebitda", "revenue_target"},
		"net_budget":        {"ebitda", "capex_budget"},
	}
	for calc, deps := range calcDeps {
		for _, dep := range deps {
			_, err = pool.Exec(ctx, `
				INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id)
				VALUES ($1::uuid, $2::uuid)
				ON CONFLICT DO NOTHING
			`, metrics[calc], metrics[dep])
			must(err, "dep "+calc+"→"+dep)
		}
	}
	printf("  Dependencies: gross_profit, gross_margin_pct, total_opex, ebitda, ebitda_margin_pct, net_budget\n")

	// ── Grids (Dashboards) ────────────────────────────────────────────────────

	section("Creating dashboard grids")

	// Grid 1: Revenue & Gross Profit dashboard
	revenueGridID := getOrInsert(ctx, pool,
		`SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Revenue & Gross Profit' LIMIT 1`,
		`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Revenue & Gross Profit', $2::uuid) RETURNING id::text`,
		"revenue grid", modelID, scenarioID)
	for i, name := range []string{"revenue_target", "cogs_budget", "gross_profit", "gross_margin_pct"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.grid_metric (grid_id, metric_id, sort_order)
			VALUES ($1::uuid, $2::uuid, $3)
			ON CONFLICT DO NOTHING
		`, revenueGridID, metrics[name], i)
		must(err, "revenue grid metric "+name)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO model.grid_dimension (grid_id, dimension_id)
		VALUES ($1::uuid, $2::uuid)
		ON CONFLICT DO NOTHING
	`, revenueGridID, deptDimID)
	must(err, "revenue grid dimension")
	printf("  Grid 'Revenue & Gross Profit' (%s) — 4 metrics × department\n", short(revenueGridID))

	// Grid 2: OpEx Budget dashboard
	opexGridID := getOrInsert(ctx, pool,
		`SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='OpEx Budget' LIMIT 1`,
		`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'OpEx Budget', $2::uuid) RETURNING id::text`,
		"opex grid", modelID, scenarioID)
	for i, name := range []string{"headcount_cost", "software_cost", "travel_cost", "facilities_cost", "other_opex", "total_opex"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.grid_metric (grid_id, metric_id, sort_order)
			VALUES ($1::uuid, $2::uuid, $3)
			ON CONFLICT DO NOTHING
		`, opexGridID, metrics[name], i)
		must(err, "opex grid metric "+name)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO model.grid_dimension (grid_id, dimension_id)
		VALUES ($1::uuid, $2::uuid)
		ON CONFLICT DO NOTHING
	`, opexGridID, deptDimID)
	must(err, "opex grid dimension")
	printf("  Grid 'OpEx Budget' (%s) — 6 metrics × department\n", short(opexGridID))

	// Grid 3: Executive P&L Summary dashboard
	execGridID := getOrInsert(ctx, pool,
		`SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Executive P&L Summary' LIMIT 1`,
		`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Executive P&L Summary', $2::uuid) RETURNING id::text`,
		"exec grid", modelID, scenarioID)
	for i, name := range []string{"revenue_target", "cogs_budget", "gross_profit", "total_opex", "capex_budget", "ebitda", "ebitda_margin_pct", "net_budget"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.grid_metric (grid_id, metric_id, sort_order)
			VALUES ($1::uuid, $2::uuid, $3)
			ON CONFLICT DO NOTHING
		`, execGridID, metrics[name], i)
		must(err, "exec grid metric "+name)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO model.grid_dimension (grid_id, dimension_id)
		VALUES ($1::uuid, $2::uuid)
		ON CONFLICT DO NOTHING
	`, execGridID, deptDimID)
	must(err, "exec grid dimension")
	printf("  Grid 'Executive P&L Summary' (%s) — 8 metrics × department\n", short(execGridID))

	// ── Dashboard definitions (visible to business users) ─────────────────────

	section("Creating dashboards")

	type dashSpec struct {
		name   string
		tags   []string
		gridID string
		title  string
		sizeH  int
	}
	dashSpecs := []dashSpec{
		{"Revenue & Gross Profit", []string{"Revenue"}, revenueGridID, "Revenue & Gross Profit Grid", 320},
		{"OpEx Budget", []string{"Cost"}, opexGridID, "OpEx Budget Grid", 380},
		{"Executive P&L Summary", []string{"Executive", "P&L"}, execGridID, "P&L Summary Grid", 420},
	}
	for _, ds := range dashSpecs {
		var dashID string
		err = pool.QueryRow(ctx,
			`SELECT id::text FROM model.dashboard_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=$3 LIMIT 1`,
			modelID, scenarioID, ds.name,
		).Scan(&dashID)
		if err != nil {
			dashID = mustScan(pool.QueryRow(ctx,
				`INSERT INTO model.dashboard_def (model_id, name, tags, revision_id) VALUES ($1::uuid,$2,$3,$4::uuid) RETURNING id::text`,
				modelID, ds.name, ds.tags, scenarioID,
			), "dashboard "+ds.name)
		}
		var wc int
		_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.dashboard_widget WHERE dashboard_id=$1::uuid`, dashID).Scan(&wc)
		if wc == 0 {
			_, err = pool.Exec(ctx, `
				INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, title, show_title, pos_x, pos_y, size_w, size_h, sort_order)
				VALUES ($1::uuid, 'grid', $2, $3, true, 0, 0, 1200, $4, 0)
			`, dashID, ds.gridID, ds.title, ds.sizeH)
			must(err, "dashboard widget "+ds.name)
		} else {
			_, _ = pool.Exec(ctx, `
				DELETE FROM model.dashboard_widget
				WHERE dashboard_id=$1::uuid AND id NOT IN (
					SELECT id FROM model.dashboard_widget WHERE dashboard_id=$1::uuid ORDER BY created_at LIMIT 1
				)
			`, dashID)
		}
		printf("  Dashboard '%s' (%s)\n", ds.name, short(dashID))
	}

	// ── Security: RACI + metric access rules ─────────────────────────────────

	section("Configuring access rules (RACI + metric policies)")

	// RACI — who is accountable/responsible/consulted for budget resources
	_, err = pool.Exec(ctx, `
		INSERT INTO security.raci_rule (application_id, user_id, resource_pattern, raci_type)
		VALUES
		    ($1::uuid, $2::uuid, '*',       'accountable'),
		    ($1::uuid, $3::uuid, '*',       'consulted'),
		    ($1::uuid, $4::uuid, 'ENG.*',   'responsible'),
		    ($1::uuid, $5::uuid, 'SALES.*', 'responsible'),
		    ($1::uuid, $6::uuid, 'OPS.*',   'responsible')
		ON CONFLICT DO NOTHING
	`, appID, finMgrID, cfoID, deptEngID, deptSalesID, deptOpsID)
	must(err, "raci rules")
	printf("  finance.manager  → Accountable (all resources)\n")
	printf("  cfo              → Consulted (all resources)\n")
	printf("  dept.head.eng    → Responsible (ENG.*)\n")
	printf("  dept.head.sales  → Responsible (SALES.*)\n")
	printf("  dept.head.ops    → Responsible (OPS.*)\n")

	// CFO: full read+write on all metrics (role created by business admin)
	for _, name := range []string{"revenue_target", "cogs_budget", "headcount_cost", "software_cost", "travel_cost", "facilities_cost", "other_opex", "capex_budget"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO security.metric_policy (application_id, user_id, metric_id, can_read, can_write)
			VALUES ($1::uuid, $2::uuid, $3::uuid, true, true)
			ON CONFLICT DO NOTHING
		`, appID, cfoID, metrics[name])
		must(err, "cfo metric policy "+name)
	}
	printf("  cfo              → read+write all input metrics\n")

	// Finance Manager: full read+write on all metrics (role created by business admin)
	for _, name := range []string{"revenue_target", "cogs_budget", "headcount_cost", "software_cost", "travel_cost", "facilities_cost", "other_opex", "capex_budget"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO security.metric_policy (application_id, user_id, metric_id, can_read, can_write)
			VALUES ($1::uuid, $2::uuid, $3::uuid, true, true)
			ON CONFLICT DO NOTHING
		`, appID, finMgrID, metrics[name])
		must(err, "fin mgr metric policy "+name)
	}
	printf("  finance.manager  → read+write all input metrics\n")

	// ENG head: writes cost metrics for Engineering cost center
	for _, name := range []string{"headcount_cost", "software_cost", "travel_cost", "facilities_cost", "other_opex"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO security.metric_policy (application_id, user_id, metric_id, can_read, can_write)
			VALUES ($1::uuid, $2::uuid, $3::uuid, true, true)
			ON CONFLICT DO NOTHING
		`, appID, deptEngID, metrics[name])
		must(err, "eng metric policy "+name)
	}
	printf("  dept.head.eng    → read+write cost metrics (HC, SW, TRAVEL, FACI, OTHER)\n")

	// SALES head: writes revenue and cost metrics for Sales
	for _, name := range []string{"revenue_target", "cogs_budget", "headcount_cost", "software_cost", "travel_cost"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO security.metric_policy (application_id, user_id, metric_id, can_read, can_write)
			VALUES ($1::uuid, $2::uuid, $3::uuid, true, true)
			ON CONFLICT DO NOTHING
		`, appID, deptSalesID, metrics[name])
		must(err, "sales metric policy "+name)
	}
	printf("  dept.head.sales  → read+write revenue_target, cogs_budget, cost metrics\n")

	// OPS head: writes cost metrics for OPS/GA/MKTG/IT
	for _, name := range []string{"headcount_cost", "software_cost", "travel_cost", "facilities_cost", "other_opex", "capex_budget"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO security.metric_policy (application_id, user_id, metric_id, can_read, can_write)
			VALUES ($1::uuid, $2::uuid, $3::uuid, true, true)
			ON CONFLICT DO NOTHING
		`, appID, deptOpsID, metrics[name])
		must(err, "ops metric policy "+name)
	}
	printf("  dept.head.ops    → read+write all cost metrics + capex_budget\n")

	// ── Workflow ──────────────────────────────────────────────────────────────

	section("Creating budget approval workflow")

	wfStore := workflow.NewStore(pool)
	wfDef, err := wfStore.CreateWorkflowDef(ctx, appID, "Budget Approval", "budget.submitted",
		[]*workflowv1.WorkflowStepDef{
			{
				Id:            "step-finance-review",
				Name:          "Finance Manager Review",
				Type:          workflowv1.StepType_STEP_TYPE_APPROVAL,
				AssigneeRoles: []string{"business_admin"},
				SlaHours:      72,
				NextStepIds:   []string{"step-cfo-approval"},
			},
			{
				Id:            "step-cfo-approval",
				Name:          "CFO Approval",
				Type:          workflowv1.StepType_STEP_TYPE_APPROVAL,
				AssigneeRoles: []string{"business_admin"},
				SlaHours:      48,
				NextStepIds:   []string{"step-notify"},
			},
			{
				Id:          "step-notify",
				Name:        "Notify Budget Owner",
				Type:        workflowv1.StepType_STEP_TYPE_NOTIFICATION,
				NextStepIds: []string{},
			},
		},
	)
	must(err, "workflow def")
	printf("  Def: %q (%s)\n", wfDef.Name, short(wfDef.Id))
	printf("  Steps: Finance Manager Review (72h) → CFO Approval (48h) → Notify Budget Owner\n")

	// ── Forms ─────────────────────────────────────────────────────────────────

	section("Creating budget forms")

	crudStore := crudapp.NewStore(pool)

	// Budget Submission form — used by department heads to submit budget requests
	budgetFormDef, err := crudStore.CreateForm(ctx, modelID, scenarioID, "budget_submission", "Budget Submission",
		[]crudapp.FormField{
			{Name: "department", Label: "Department", Type: "select", Required: true,
				Options: []string{"ENG", "SALES", "MKTG", "GA", "OPS", "IT"}},
			{Name: "budget_period", Label: "Budget Period", Type: "select", Required: true,
				Options: []string{"Q1", "Q2", "Q3", "Q4", "Annual"}},
			{Name: "budget_type", Label: "Budget Type", Type: "select", Required: true,
				Options: []string{"OpEx", "CapEx", "Revenue"}},
			{Name: "amount", Label: "Amount (USD)", Type: "number", Required: true},
			{Name: "currency", Label: "Currency", Type: "select", Required: true,
				Options: []string{"USD", "EUR", "GBP"}},
			{Name: "cost_category", Label: "Cost Category", Type: "select", Required: false,
				Options: []string{"Headcount", "Software", "Travel", "Facilities", "Other"}},
			{Name: "justification", Label: "Business Justification", Type: "text", Required: true},
			{Name: "is_revised", Label: "Is Revised Budget?", Type: "boolean", Required: false},
		},
	)
	must(err, "budget submission form")
	printf("  Form: %q (%s) — 8 fields\n", budgetFormDef.Label, short(budgetFormDef.ID))

	// CapEx Request form — used by dept heads to request capital expenditures
	capexFormDef, err := crudStore.CreateForm(ctx, modelID, scenarioID, "capex_request", "CapEx Request",
		[]crudapp.FormField{
			{Name: "project_name", Label: "Project Name", Type: "text", Required: true},
			{Name: "department", Label: "Department", Type: "select", Required: true,
				Options: []string{"ENG", "SALES", "MKTG", "GA", "OPS", "IT"}},
			{Name: "category", Label: "Category", Type: "select", Required: true,
				Options: []string{"Hardware", "Software License", "Infrastructure", "R&D Equipment", "Other"}},
			{Name: "amount", Label: "Amount (USD)", Type: "number", Required: true},
			{Name: "start_date", Label: "Project Start Date", Type: "date", Required: true},
			{Name: "end_date", Label: "Expected Completion", Type: "date", Required: false},
			{Name: "priority", Label: "Priority", Type: "select", Required: true,
				Options: []string{"Critical", "High", "Medium", "Low"}},
			{Name: "justification", Label: "Business Justification", Type: "text", Required: true},
			{Name: "roi_months", Label: "Expected ROI (months)", Type: "number", Required: false},
		},
	)
	must(err, "capex request form")
	printf("  Form: %q (%s) — 9 fields\n", capexFormDef.Label, short(capexFormDef.ID))

	// ── Automation rule ───────────────────────────────────────────────────────

	section("Creating automation rules")

	budgetRule, err := wfStore.CreateAutomationRule(ctx, appID, scenarioID,
		"Auto-Route Budget Submissions",
		"Automatically start Budget Approval workflow when a budget submission is filed",
		"form_submit",
		"Budget Approval",
		"", budgetFormDef.ID, "", nil,
	)
	must(err, "budget automation rule")
	printf("  Rule: %q (%s) — trigger: form_submit\n", budgetRule.Name, short(budgetRule.ID))

	capexRule, err := wfStore.CreateAutomationRule(ctx, appID, scenarioID,
		"Auto-Route CapEx Requests",
		"Automatically start Budget Approval workflow for capital expenditure requests",
		"form_submit",
		"Budget Approval",
		"", capexFormDef.ID, "", nil,
	)
	must(err, "capex automation rule")
	printf("  Rule: %q (%s) — trigger: form_submit\n", capexRule.Name, short(capexRule.ID))

	// ── Input data — FY2026 Budget figures ────────────────────────────────────

	section("Writing FY2026 Budget input data")

	type deptBudget struct {
		dept        string
		ownerID     string
		revenuePlan float64
		cogs        float64
		headcount   float64
		software    float64
		travel      float64
		facilities  float64
		otherOpex   float64
		capex       float64
	}

	deptBudgets := []deptBudget{
		// dept,   owner,       rev,       cogs,     hc,        sw,      travel,   faci,    other,   capex
		{"ENG", deptEngID, 0, 0, 2_400_000, 500_000, 100_000, 200_000, 50_000, 800_000},
		{"SALES", deptSalesID, 6_000_000, 1_200_000, 1_200_000, 300_000, 400_000, 150_000, 100_000, 50_000},
		{"MKTG", deptOpsID, 2_000_000, 400_000, 800_000, 200_000, 150_000, 100_000, 80_000, 100_000},
		{"GA", deptOpsID, 0, 0, 600_000, 150_000, 50_000, 120_000, 60_000, 30_000},
		{"OPS", deptOpsID, 0, 200_000, 400_000, 100_000, 30_000, 300_000, 40_000, 400_000},
		{"IT", deptOpsID, 0, 0, 500_000, 800_000, 20_000, 50_000, 30_000, 600_000},
	}

	var totRevenue, totCOGS, totHC, totSW, totTravel, totFaci, totOther, totCapex float64
	for _, db := range deptBudgets {
		dimJSON := fmt.Sprintf(`{"%s": "%s"}`, deptDimID, db.dept)
		for metricName, value := range map[string]float64{
			"revenue_target":  db.revenuePlan,
			"cogs_budget":     db.cogs,
			"headcount_cost":  db.headcount,
			"software_cost":   db.software,
			"travel_cost":     db.travel,
			"facilities_cost": db.facilities,
			"other_opex":      db.otherOpex,
			"capex_budget":    db.capex,
		} {
			_, err = pool.Exec(ctx, `
				INSERT INTO runtime.fact_input
				    (model_id, revision_name, metric_id, dim_members, value, entered_by, revision_id)
				VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6::uuid, $7::uuid)
			`, modelID, scenarioName, metrics[metricName], dimJSON, value, db.ownerID, scenarioID)
			must(err, "fact "+db.dept+"."+metricName)
		}
		opex := db.headcount + db.software + db.travel + db.facilities + db.otherOpex
		printf("  %-6s  rev=%9.0f  cogs=%9.0f  opex=%9.0f  capex=%8.0f\n",
			db.dept, db.revenuePlan, db.cogs, opex, db.capex)
		totRevenue += db.revenuePlan
		totCOGS += db.cogs
		totHC += db.headcount
		totSW += db.software
		totTravel += db.travel
		totFaci += db.facilities
		totOther += db.otherOpex
		totCapex += db.capex
	}

	// Aggregate row (dim_members='{}') for the calculation engine
	for metricName, value := range map[string]float64{
		"revenue_target":  totRevenue,
		"cogs_budget":     totCOGS,
		"headcount_cost":  totHC,
		"software_cost":   totSW,
		"travel_cost":     totTravel,
		"facilities_cost": totFaci,
		"other_opex":      totOther,
		"capex_budget":    totCapex,
	} {
		_, err = pool.Exec(ctx, `
			INSERT INTO runtime.fact_input
			    (model_id, revision_name, metric_id, dim_members, value, entered_by, revision_id)
			VALUES ($1::uuid, $2, $3::uuid, '{}', $4, $5::uuid, $6::uuid)
		`, modelID, scenarioName, metrics[metricName], value, cfoID, scenarioID)
		must(err, "aggregate fact "+metricName)
	}
	totOpex := totHC + totSW + totTravel + totFaci + totOther
	printf("  ──────────────────────────────────────────────────────────────────\n")
	printf("  %-6s  rev=%9.0f  cogs=%9.0f  opex=%9.0f  capex=%8.0f\n",
		"TOTAL", totRevenue, totCOGS, totOpex, totCapex)

	// ── Calculation engine ────────────────────────────────────────────────────

	section("Running calculation engine")

	calcStore := calculation.NewStore(pool)
	scheduler := calculation.NewScheduler(log, calcStore, nil)
	changedInputs := []string{
		metrics["revenue_target"], metrics["cogs_budget"],
		metrics["headcount_cost"], metrics["software_cost"], metrics["travel_cost"],
		metrics["facilities_cost"], metrics["other_opex"], metrics["capex_budget"],
	}
	must(scheduler.RecalcAffected(ctx, modelID, scenarioID, changedInputs), "recalc")

	grossProfit, err := calcStore.GetCalcValue(ctx, modelID, scenarioID, metrics["gross_profit"], map[string]string{})
	must(err, "get gross_profit")
	ebitda, err := calcStore.GetCalcValue(ctx, modelID, scenarioID, metrics["ebitda"], map[string]string{})
	must(err, "get ebitda")
	netBudget, err := calcStore.GetCalcValue(ctx, modelID, scenarioID, metrics["net_budget"], map[string]string{})
	must(err, "get net_budget")

	expectedGross := totRevenue - totCOGS
	expectedEBITDA := expectedGross - totOpex

	grossOK := "✓"
	if grossProfit != expectedGross {
		grossOK = "✗ MISMATCH"
	}
	ebitdaOK := "✓"
	if ebitda != expectedEBITDA {
		ebitdaOK = "✗ MISMATCH"
	}
	printf("  gross_profit = %10.2f  %s\n", grossProfit, grossOK)
	printf("  total_opex   = %10.2f\n", totOpex)
	printf("  ebitda       = %10.2f  %s\n", ebitda, ebitdaOK)
	printf("  net_budget   = %10.2f\n", netBudget)
	grossMarginPct := grossProfit / totRevenue * 100
	ebitdaMarginPct := ebitda / totRevenue * 100
	printf("  gross margin = %9.1f%%\n", grossMarginPct)
	printf("  EBITDA margin= %9.1f%%\n", ebitdaMarginPct)

	// ── Sample form records ───────────────────────────────────────────────────

	section("Seeding budget submission records")

	type formSample struct {
		data   map[string]any
		status string
		userID string
	}
	budgetSamples := []formSample{
		{
			data: map[string]any{
				"department": "ENG", "budget_period": "Annual", "budget_type": "OpEx",
				"amount": 3_250_000, "currency": "USD", "cost_category": "Headcount",
				"justification": "Engineering headcount + tooling budget FY2026 — includes 4 new hires",
				"is_revised":    false,
			},
			status: "approved", userID: deptEngID,
		},
		{
			data: map[string]any{
				"department": "SALES", "budget_period": "Annual", "budget_type": "Revenue",
				"amount": 6_000_000, "currency": "USD", "cost_category": "",
				"justification": "FY2026 revenue plan — $6M ARR target based on pipeline analysis",
				"is_revised":    false,
			},
			status: "submitted", userID: deptSalesID,
		},
		{
			data: map[string]any{
				"department": "ENG", "budget_period": "Q1", "budget_type": "CapEx",
				"amount": 800_000, "currency": "USD", "cost_category": "Other",
				"justification": "Server infrastructure upgrade for new product line — Q1 deployment",
				"is_revised":    false,
			},
			status: "draft", userID: deptEngID,
		},
		{
			data: map[string]any{
				"department": "IT", "budget_period": "Annual", "budget_type": "OpEx",
				"amount": 1_400_000, "currency": "USD", "cost_category": "Software",
				"justification": "Annual software licences: AWS, Datadog, GitHub, Figma, Salesforce",
				"is_revised":    false,
			},
			status: "submitted", userID: deptOpsID,
		},
	}
	for _, r := range budgetSamples {
		rec, err := crudStore.CreateRecord(ctx, budgetFormDef.ID, r.userID, r.data)
		must(err, "create budget record")
		if r.status != "draft" {
			must(crudStore.UpdateRecord(ctx, rec.ID, r.status, r.data), "update budget record")
		}
		printf("  Budget (%s) dept=%-5s type=%-8s status=%s\n",
			short(rec.ID), r.data["department"], r.data["budget_type"], r.status)
	}

	capexSamples := []formSample{
		{
			data: map[string]any{
				"project_name": "Dev Infra Upgrade", "department": "ENG",
				"category": "Hardware", "amount": 450_000, "currency": "USD",
				"start_date": "2026-01-15", "end_date": "2026-03-31",
				"priority": "Critical", "roi_months": 18,
				"justification": "Replace aging CI/CD build servers — current throughput is a bottleneck",
			},
			status: "approved", userID: deptEngID,
		},
		{
			data: map[string]any{
				"project_name": "Data Center Migration", "department": "IT",
				"category": "Infrastructure", "amount": 600_000, "currency": "USD",
				"start_date": "2026-04-01", "end_date": "2026-09-30",
				"priority": "High", "roi_months": 24,
				"justification": "Migrate on-prem workloads to AWS — projected 30% infra cost reduction",
			},
			status: "submitted", userID: deptOpsID,
		},
	}
	for _, r := range capexSamples {
		rec, err := crudStore.CreateRecord(ctx, capexFormDef.ID, r.userID, r.data)
		must(err, "create capex record")
		if r.status != "draft" {
			must(crudStore.UpdateRecord(ctx, rec.ID, r.status, r.data), "update capex record")
		}
		printf("  CapEx  (%s) project=%-25s status=%s\n",
			short(rec.ID), r.data["project_name"], r.status)
	}

	// ── Workflow instance — full approval run ─────────────────────────────────

	section("Running budget approval workflow (ENG budget submission)")

	inst, err := wfStore.StartWorkflow(ctx, wfDef.Id, deptEngID, map[string]string{
		"revision":     scenarioName,
		"model_id":     modelID,
		"department":   "ENG",
		"submitted_by": "dept.head.eng@acme.com",
	})
	must(err, "start workflow")
	printf("  Instance: %s (status: %s)\n", short(inst.Id), inst.Status)

	_, steps, err := wfStore.GetWorkflowInstance(ctx, inst.Id)
	must(err, "get instance")

	// Finance Manager Review step
	var financeStepID string
	for _, s := range steps {
		if s.StepDefId == "step-finance-review" {
			financeStepID = s.Id
			break
		}
	}
	if financeStepID == "" {
		fatalf("finance-review step not found")
	}
	_, err = wfStore.CompleteStep(ctx, financeStepID, finMgrID, "approve",
		"ENG budget reviewed — headcount plan aligns with hiring roadmap. Forwarding to CFO.")
	must(err, "complete finance review")
	printf("  Finance Manager Review: approved by Riley\n")

	// CFO Approval step
	_, updatedSteps, err := wfStore.GetWorkflowInstance(ctx, inst.Id)
	must(err, "get instance after finance review")
	var cfoStepID string
	for _, s := range updatedSteps {
		if s.StepDefId == "step-cfo-approval" {
			cfoStepID = s.Id
			break
		}
	}
	if cfoStepID != "" {
		_, err = wfStore.CompleteStep(ctx, cfoStepID, cfoID, "approve",
			"ENG budget approved for FY2026. Budget code CAPEX-ENG-2026 allocated.")
		must(err, "complete cfo approval")
		printf("  CFO Approval: approved by Dana (CFO)\n")
	}

	// Auto-complete notification step
	_, finalSteps, _ := wfStore.GetWorkflowInstance(ctx, inst.Id)
	for _, s := range finalSteps {
		if s.StepDefId == "step-notify" {
			_, _ = wfStore.CompleteStep(ctx, s.Id, cfoID, "sent",
				"Budget owner Alex notified of approval via email")
		}
	}

	finalInst, _, err := wfStore.GetWorkflowInstance(ctx, inst.Id)
	must(err, "final instance")
	printf("  Workflow status: %s\n", finalInst.Status)

	// ── Audit log ─────────────────────────────────────────────────────────────

	section("Seeding audit log")

	auditEvents := []struct {
		category, eventType, actorID, actorRole, resourceType, resourceID, revisionID, metadata string
	}{
		// User provisioning (platform admin)
		{"admin", "user.created", platformAdminID, "platform_admin", "user", cfoID, "", `{"email":"cfo@acme.com","role":"business_admin","title":"CFO"}`},
		{"admin", "user.created", platformAdminID, "platform_admin", "user", finMgrID, "", `{"email":"finance.manager@acme.com","role":"business_admin","title":"Finance Manager"}`},
		{"admin", "user.created", platformAdminID, "platform_admin", "user", deptEngID, "", `{"email":"dept.head.eng@acme.com","role":"business_user","title":"Eng Head"}`},
		{"admin", "user.created", platformAdminID, "platform_admin", "user", deptSalesID, "", `{"email":"dept.head.sales@acme.com","role":"business_user","title":"Sales Head"}`},
		{"admin", "user.created", platformAdminID, "platform_admin", "user", deptOpsID, "", `{"email":"dept.head.ops@acme.com","role":"business_user","title":"Ops Head"}`},
		// Access rule configuration (business admin — CFO configures access policies)
		{"admin", "raci_rule.created", cfoID, "business_admin", "raci_rule", appID, "", `{"type":"accountable","user":"finance.manager@acme.com","pattern":"*"}`},
		{"admin", "metric_policy.created", cfoID, "business_admin", "metric_policy", metrics["revenue_target"], "", `{"user":"dept.head.sales@acme.com","can_write":true}`},
		{"admin", "metric_policy.created", cfoID, "business_admin", "metric_policy", metrics["capex_budget"], "", `{"user":"dept.head.ops@acme.com","can_write":true}`},
		// Model configuration (developer)
		{"model_change", "metric.created", devID, "developer", "metric", metrics["revenue_target"], scenarioID, `{"name":"revenue_target","type":"input"}`},
		{"model_change", "metric.created", devID, "developer", "metric", metrics["ebitda"], scenarioID, `{"name":"ebitda","type":"calc","formula":"{gross_profit}-{total_opex}"}`},
		{"model_change", "grid.created", devID, "developer", "grid_def", execGridID, scenarioID, `{"name":"Executive P&L Summary","metrics":8}`},
		// Budget data entry (dept heads)
		{"data_change", "cell.written", deptEngID, "business_user", "fact_input", modelID, scenarioID, `{"metric":"headcount_cost","dept":"ENG","value":2400000}`},
		{"data_change", "cell.written", deptSalesID, "business_user", "fact_input", modelID, scenarioID, `{"metric":"revenue_target","dept":"SALES","value":6000000}`},
		{"data_change", "cell.written", deptOpsID, "business_user", "fact_input", modelID, scenarioID, `{"metric":"capex_budget","dept":"IT","value":600000}`},
		// Workflow events
		{"data_change", "budget.submitted", deptEngID, "business_user", "workflow_instance", inst.Id, scenarioID, `{"revision":"FY2026 Budget","department":"ENG"}`},
		{"data_change", "budget.approved", finMgrID, "business_admin", "workflow_step", financeStepID, scenarioID, `{"decision":"approve","step":"Finance Manager Review"}`},
		{"data_change", "budget.approved", cfoID, "business_admin", "workflow_step", cfoStepID, scenarioID, `{"decision":"approve","step":"CFO Approval"}`},
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

	// ── Summary ───────────────────────────────────────────────────────────────

	section("Demo complete — summary")
	printf("  Application  : Budget Planning FY2026 (%s)\n", short(appID))
	printf("  Model ID     : %s\n", short(modelID))
	printf("  Scenario     : %s\n", scenarioName)
	printf("\n")
	printf("  Users (8):\n")
	printf("    platform_admin   : platform-admin@acme.com\n")
	printf("    tenant_admin     : tenant-admin@acme.com\n")
	printf("    developer        : dev@acme.com\n")
	printf("    business_admin   : cfo@acme.com (CFO — final approver)\n")
	printf("    business_admin   : finance.manager@acme.com (Finance Manager — reviewer)\n")
	printf("    business_user    : dept.head.eng@acme.com (ENG budget owner)\n")
	printf("    business_user    : dept.head.sales@acme.com (SALES revenue+cost owner)\n")
	printf("    business_user    : dept.head.ops@acme.com (OPS/GA/MKTG/IT budget owner)\n")
	printf("\n")
	printf("  Dimensions   : department (6), cost_type (6), budget_period (4)\n")
	printf("  Metrics      : 8 input + 6 calculated\n")
	printf("  Dashboards   : Revenue & Gross Profit | OpEx Budget | Executive P&L Summary\n")
	printf("\n")
	printf("  FY2026 Budget:\n")
	printf("    Revenue Target : %12.0f\n", totRevenue)
	printf("    COGS           : %12.0f\n", totCOGS)
	printf("    Gross Profit   : %12.0f  (%.1f%%)\n", grossProfit, grossMarginPct)
	printf("    Total OpEx     : %12.0f\n", totOpex)
	printf("    EBITDA         : %12.0f  (%.1f%%)\n", ebitda, ebitdaMarginPct)
	printf("    CapEx          : %12.0f\n", totCapex)
	printf("    Net Budget     : %12.0f\n", netBudget)
	printf("\n")
	printf("  Workflow     : %s → %s\n", wfDef.Name, finalInst.Status)
	printf("  Automation   : %s | %s\n", budgetRule.Name, capexRule.Name)
	printf("  DB ready     : %s\n", dsn)
	fmt.Println()

	_ = time.Now
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
