// cmd/seed-payroll — Salary Budgeting Demo (examples/budgeting-demo).
//
// Creates a full cost-center salary planning + approval scenario, built
// entirely from generic platform primitives — no bespoke endpoints or
// widget types:
//
//	customer(reused) → workspace(Demo Workspace) → application(Budgeting Demo) →
//	model(Salary Budget Model) → dimensions(cost_centers, regions, employees
//	[cross-dimension parent → cost_centers, region property], months [same-
//	dimension parent → year]) → metrics(salary input, social_tax calc) →
//	grid 1(employees × months, editable) → grid 2/3(cost_centers × months,
//	regions × years — rollup_source_grid_id = grid 1, no grid_metric rows
//	of their own) → users(7 roles) → RACI + dimension-member access rules →
//	business roles + dashboards(grid + automation_button widgets only) →
//	workflow("Salary Budget Submission", context_schema RACI-scoped +
//	on_approve copy-to-Annual config) → input data → calculation
//
// See examples/budgeting-demo/README.md for the full write-up, including
// why this is a Go program rather than a YAML config tree, and why
// "Working" / "Annual" are two model.revision rows rather than two
// versions of one.
//
// Behavioral correctness (RACI-scoped submit, write locks while a workflow
// is in flight, approval copying facts into Annual, cross-dimension/
// property-based rollup) is verified two other ways, not scripted here:
// internal/gateway's DATABASE_URL-guarded tests exercise the real HTTP
// handlers directly, and the README documents a manual demo script. This
// program's own checks below are structural only (did the seed itself
// produce a correctly wired model), since the behavior it would otherwise
// script lives in generic gateway code, not a reusable Store package.
//
// Usage:
//
//	DATABASE_URL=postgres://... go run ./cmd/seed-payroll
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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
	defaultDSN = "postgres://mavericks:mavericks@localhost:5432/mavericks?sslmode=disable"

	// Context point 2 (plan): the spec's "Budget / Working" and
	// "Budget / AnnualBudget" map onto two model.revision rows — this
	// platform partitions runtime.fact_input / calc_result by revision_id,
	// not by a separate in-revision version axis.
	workingRevisionName = "Budget (Working)"
	annualRevisionName  = "Budget (Annual)"

	socialTaxRate = 0.25
)

var checks []checkResult

type checkResult struct {
	label string
	pass  bool
	note  string
}

type ccManager struct {
	sub, email, name, costCenter, ccLabel string
	userID                                string
}

func main() {
	log := logger.New("seed-payroll")
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultDSN
	}

	section("Salary Budgeting Demo — Seed")

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

	var customerID, workspaceID, appID, modelID string
	err = pool.QueryRow(ctx, `
		SELECT a.id::text, m.id::text, a.workspace_id::text, w.customer_id::text
		FROM core.application a
		JOIN core.model m ON m.application_id = a.id AND m.name = 'Salary Budget Model'
		JOIN core.workspace w ON w.id = a.workspace_id
		WHERE a.name = 'Budgeting Demo'
		ORDER BY a.created_at DESC LIMIT 1
	`).Scan(&appID, &modelID, &workspaceID, &customerID)
	if err != nil {
		err = pool.QueryRow(ctx, `SELECT id::text FROM core.customer LIMIT 1`).Scan(&customerID)
		if err != nil {
			customerID = mustScan(pool.QueryRow(ctx,
				`INSERT INTO core.customer (name, plan) VALUES ('Acme Corp', 'enterprise') RETURNING id::text`),
				"customer")
		}
		workspaceID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Demo Workspace') RETURNING id::text`,
			customerID), "workspace")
		appID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Budgeting Demo', 'planning') RETURNING id::text`,
			workspaceID, customerID), "application")
		modelID = mustScan(pool.QueryRow(ctx,
			`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Salary Budget Model') RETURNING id::text`,
			appID), "model")
	}
	printf("  Customer   : %s\n", short(customerID))
	printf("  Workspace  : Demo Workspace (%s)\n", short(workspaceID))
	printf("  Application: Budgeting Demo (%s)\n", short(appID))
	printf("  Model      : Salary Budget Model (%s)\n", short(modelID))

	// ── Users ─────────────────────────────────────────────────────────────────

	section("Creating demo users")

	// Platform admin + developer are reused across every seed (same
	// keycloak_sub as cmd/seed and cmd/seed-budget) to avoid duplicate
	// accounts cluttering the Users table across demos.
	platformAdminID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('demo-platform-admin-001', 'platform-admin@acme.com', 'Pat (Platform Admin)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id::text
	`, customerID), "platform admin")
	printf("  platform_admin    : platform-admin@acme.com     (%s)\n", short(platformAdminID))

	devID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('demo-dev-001', 'dev@acme.com', 'Sam (Developer)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id::text
	`, customerID), "developer")
	printf("  demo_developer    : dev@acme.com                (%s)\n", short(devID))

	tenantAdminID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('payroll-tenant-admin-001', 'tenant-admin+payroll@acme.com', 'Charlie (Tenant Admin)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
		RETURNING id::text
	`, customerID), "tenant admin")
	printf("  tenant_admin      : tenant-admin+payroll@acme.com (%s)\n", short(tenantAdminID))

	generalManagerID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('payroll-gm-001', 'general.manager@acme.com', 'Taylor (General Manager)', $1::uuid)
		ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
		RETURNING id::text
	`, customerID), "general manager")
	printf("  general_manager   : general.manager@acme.com    (%s)\n", short(generalManagerID))

	managers := []*ccManager{
		{sub: "payroll-ccm-sales-001", email: "cc.manager.sales@acme.com", name: "Morgan (Sales CC Manager)", costCenter: "CC_SALES", ccLabel: "Sales"},
		{sub: "payroll-ccm-ops-001", email: "cc.manager.ops@acme.com", name: "Jordan (Ops CC Manager)", costCenter: "CC_OPS", ccLabel: "Operations"},
		{sub: "payroll-ccm-ga-001", email: "cc.manager.ga@acme.com", name: "Riley (G&A CC Manager)", costCenter: "CC_GA", ccLabel: "General & Admin"},
	}
	for _, m := range managers {
		m.userID = mustScan(pool.QueryRow(ctx, `
			INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
			VALUES ($1, $2, $3, $4::uuid)
			ON CONFLICT (keycloak_sub) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub
			RETURNING id::text
		`, m.sub, m.email, m.name, customerID), "cc manager "+m.costCenter)
		printf("  cc_manager_%-6s : %-28s (%s)\n", ccSuffix(m.costCenter), m.email, short(m.userID))
	}

	// ── Role assignments ──────────────────────────────────────────────────────

	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES ($1::uuid, 'platform_admin', NULL)
		ON CONFLICT (user_id, role, workspace_id) DO NOTHING
	`, platformAdminID)
	must(err, "platform_admin role")

	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES
		    ($1::uuid, 'tenant_admin',   $5::uuid),
		    ($2::uuid, 'developer',      $5::uuid),
		    ($3::uuid, 'business_admin', $5::uuid),
		    ($4::uuid, 'business_user',  $5::uuid)
		ON CONFLICT (user_id, role, workspace_id) DO NOTHING
	`, tenantAdminID, devID, generalManagerID, managers[0].userID, workspaceID)
	must(err, "role assignments (1)")
	_, err = pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES ($1::uuid, 'business_user', $3::uuid), ($2::uuid, 'business_user', $3::uuid)
		ON CONFLICT (user_id, role, workspace_id) DO NOTHING
	`, managers[1].userID, managers[2].userID, workspaceID)
	must(err, "role assignments (2)")
	printf("  Roles assigned in Demo Workspace\n")

	// ── RACI: each cost-center manager is "responsible" for their own cost
	// center's resources; general_manager is "accountable" for everything.
	// This is the same RACI grant internal/workflow's generic
	// ResolveStartContext (source_hint "raci_responsible", shared by the
	// direct HTTP start endpoint and every automation_button/TriggerRule
	// click) reads to auto-scope the shared "Submit Plan" trigger per caller.

	_, err = pool.Exec(ctx, `
		INSERT INTO security.raci_rule (application_id, user_id, resource_pattern, raci_type)
		VALUES ($1::uuid, $2::uuid, '*', 'accountable')
		ON CONFLICT DO NOTHING
	`, appID, generalManagerID)
	must(err, "raci gm")
	for _, m := range managers {
		_, err = pool.Exec(ctx, `
			INSERT INTO security.raci_rule (application_id, user_id, resource_pattern, raci_type)
			VALUES ($1::uuid, $2::uuid, $3, 'responsible')
			ON CONFLICT DO NOTHING
		`, appID, m.userID, m.costCenter+".*")
		must(err, "raci "+m.costCenter)
	}
	printf("  general_manager   → accountable (*)\n")
	printf("  cc_manager_*      → responsible (own cost center)\n")

	// ── Revisions ─────────────────────────────────────────────────────────────

	section("Defining revisions")

	workingRevID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.revision (model_id, name, description)
		VALUES ($1::uuid, $2, 'Editable salary planning cycle')
		ON CONFLICT (model_id, name) DO UPDATE SET description = EXCLUDED.description
		RETURNING id::text
	`, modelID, workingRevisionName), "working revision")
	annualRevID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.revision (model_id, name, description, system_managed)
		VALUES ($1::uuid, $2, 'Approved salary plan — populated by workflow approval, never directly edited', true)
		ON CONFLICT (model_id, name) DO UPDATE SET description = EXCLUDED.description, system_managed = true
		RETURNING id::text
	`, modelID, annualRevisionName), "annual revision")
	_, err = pool.Exec(ctx, `
		UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name=$2 WHERE id=$3::uuid
	`, workingRevID, workingRevisionName, modelID)
	must(err, "set active revision")
	printf("  %s (%s) — editable, active\n", workingRevisionName, short(workingRevID))
	printf("  %s (%s) — system_managed (read-only via cells())\n", annualRevisionName, short(annualRevID))

	// ── Dimensions ────────────────────────────────────────────────────────────

	section("Defining dimensions")

	costCentersDimID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, revision_id, name)
		VALUES ($1::uuid, $2::uuid, 'cost_centers')
		ON CONFLICT (model_id, revision_id, name) WHERE revision_id IS NOT NULL DO UPDATE SET name = EXCLUDED.name
		RETURNING id::text
	`, modelID, workingRevID), "cost_centers dimension")
	costCenterMemberID := map[string]string{}
	for _, cc := range []struct{ code, label string }{
		{"CC_SALES", "Sales"}, {"CC_OPS", "Operations"}, {"CC_GA", "General & Admin"},
	} {
		id := mustScan(pool.QueryRow(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label)
			VALUES ($1::uuid, $2, $3)
			ON CONFLICT (dimension_id, code) DO UPDATE SET label = EXCLUDED.label
			RETURNING id::text
		`, costCentersDimID, cc.code, cc.label), "cost center "+cc.code)
		costCenterMemberID[cc.code] = id
	}
	printf("  cost_centers (%s): CC_SALES, CC_OPS, CC_GA\n", short(costCentersDimID))

	regionsDimID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, revision_id, name)
		VALUES ($1::uuid, $2::uuid, 'regions')
		ON CONFLICT (model_id, revision_id, name) WHERE revision_id IS NOT NULL DO UPDATE SET name = EXCLUDED.name
		RETURNING id::text
	`, modelID, workingRevID), "regions dimension")
	for _, r := range []struct{ code, label string }{
		{"LUX", "Luxembourg"}, {"BE", "Belgium"}, {"DE", "Germany"}, {"FR", "France"},
	} {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label)
			VALUES ($1::uuid, $2, $3) ON CONFLICT (dimension_id, code) DO UPDATE SET label = EXCLUDED.label
		`, regionsDimID, r.code, r.label)
		must(err, "region "+r.code)
	}
	printf("  regions (%s): LUX, BE, DE, FR\n", short(regionsDimID))

	// employees: cross-dimension parent -> cost_centers (parent_dimension_id),
	// each member's parent_member_id points at its cost-center member. region
	// is stored in dimension_member.properties JSONB — the platform's real
	// per-member value store (model.dimension_property only stores property
	// name/type *definitions*, no per-member value table).
	employeesDimID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, revision_id, name, parent_dimension_id)
		VALUES ($1::uuid, $2::uuid, 'employees', $3::uuid)
		ON CONFLICT (model_id, revision_id, name) WHERE revision_id IS NOT NULL
		DO UPDATE SET parent_dimension_id = EXCLUDED.parent_dimension_id
		RETURNING id::text
	`, modelID, workingRevID, costCentersDimID), "employees dimension")

	// regions' members are a grouping of employees' members by their
	// "region" property — the generic property-derived-dimension mechanism
	// (grid()/resolveCrossDimensionValue), the property-based counterpart
	// to employees' structural parent_dimension_id link above (a dimension
	// can only have one structural parent, and employees already uses that
	// slot for cost_centers).
	_, err = pool.Exec(ctx, `
		UPDATE model.dimension_def SET source_dimension_id=$1::uuid, source_property='region' WHERE id=$2::uuid
	`, employeesDimID, regionsDimID)
	must(err, "regions source_dimension_id/source_property")

	regionCycle := []string{"LUX", "BE", "DE", "FR"}
	type employeeSeed struct {
		code, label, costCenter, region string
		monthlySalary2026               float64
	}
	var employees []employeeSeed
	names := map[string][]string{
		"CC_SALES": {"Ana Costa", "Ben Weber", "Chloe Martin", "Dries Peeters"},
		"CC_OPS":   {"Elin Novak", "Finn Kruger", "Greta Muller", "Hugo Rossi"},
		"CC_GA":    {"Ines Dubois", "Jan Larsen", "Kara Schmidt", "Liam Bakker"},
	}
	empIdx := 0
	for _, cc := range []string{"CC_SALES", "CC_OPS", "CC_GA"} {
		for i := 0; i < 4; i++ {
			code := fmt.Sprintf("E-%s-%02d", ccSuffixUpper(cc), i+1)
			employees = append(employees, employeeSeed{
				code:              code,
				label:             names[cc][i],
				costCenter:        cc,
				region:            regionCycle[empIdx%len(regionCycle)],
				monthlySalary2026: 4800 + float64((empIdx%6)*450), // spread of realistic monthly salaries
			})
			empIdx++
		}
	}
	for _, e := range employees {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, properties)
			VALUES ($1::uuid, $2, $3, $4::uuid, $5::jsonb)
			ON CONFLICT (dimension_id, code) DO UPDATE
			SET label = EXCLUDED.label, parent_member_id = EXCLUDED.parent_member_id, properties = EXCLUDED.properties
		`, employeesDimID, e.code, e.label, costCenterMemberID[e.costCenter],
			fmt.Sprintf(`{"region":"%s"}`, e.region))
		must(err, "employee "+e.code)
	}
	printf("  employees (%s): 12 members, 4 per cost center, parent_dimension_id -> cost_centers\n", short(employeesDimID))

	// months: same-dimension hierarchy (leaf month -> year parent), matching
	// the OPEX seed's department tree pattern exactly. No separate "years"
	// dimension per the spec.
	monthsDimID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, revision_id, name)
		VALUES ($1::uuid, $2::uuid, 'months')
		ON CONFLICT (model_id, revision_id, name) WHERE revision_id IS NOT NULL DO UPDATE SET name = EXCLUDED.name
		RETURNING id::text
	`, modelID, workingRevID), "months dimension")
	yearMemberID := map[string]string{}
	for _, y := range []string{"2026", "2027"} {
		id := mustScan(pool.QueryRow(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label)
			VALUES ($1::uuid, $2, $2)
			ON CONFLICT (dimension_id, code) DO UPDATE SET label = EXCLUDED.label
			RETURNING id::text
		`, monthsDimID, y), "year "+y)
		yearMemberID[y] = id
	}
	monthNames := []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	for _, y := range []string{"2026", "2027"} {
		for i, mn := range monthNames {
			code := fmt.Sprintf("%s-%02d", y, i+1)
			_, err = pool.Exec(ctx, `
				INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id)
				VALUES ($1::uuid, $2, $3, $4::uuid)
				ON CONFLICT (dimension_id, code) DO UPDATE SET label = EXCLUDED.label, parent_member_id = EXCLUDED.parent_member_id
			`, monthsDimID, code, mn+" "+y, yearMemberID[y])
			must(err, "month "+code)
		}
	}
	printf("  months (%s): 2026, 2027 (years) + 24 months, parent_member_id -> year\n", short(monthsDimID))

	// ── Metrics ───────────────────────────────────────────────────────────────

	section("Defining metrics")

	salaryMetricID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format, format_decimals, format_currency)
		VALUES ($1::uuid, $2::uuid, 'salary', true, 'currency', 0, '$')
		ON CONFLICT (model_id, revision_id, name) WHERE revision_id IS NOT NULL DO UPDATE SET is_input = EXCLUDED.is_input
		RETURNING id::text
	`, modelID, workingRevID), "salary metric")
	socialTaxFormula := fmt.Sprintf("{salary} * %g", socialTaxRate)
	socialTaxMetricID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula, format, format_decimals, format_currency)
		VALUES ($1::uuid, $2::uuid, 'social_tax', false, $3, 'currency', 0, '$')
		ON CONFLICT (model_id, revision_id, name) WHERE revision_id IS NOT NULL DO UPDATE SET formula = EXCLUDED.formula
		RETURNING id::text
	`, modelID, workingRevID, socialTaxFormula), "social_tax metric")
	_, err = pool.Exec(ctx, `
		INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id)
		VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING
	`, socialTaxMetricID, salaryMetricID)
	must(err, "calc dependency")
	printf("  salary     [INPUT] (%s)\n", short(salaryMetricID))
	printf("  social_tax [CALC ] (%s) formula: %s\n", short(socialTaxMetricID), socialTaxFormula)

	// ── Grid 1: Salary by Employee & Month (editable) ───────────────────────────

	section("Creating Grid 1 (employees x months, editable)")

	grid1ID := getOrInsert(ctx, pool,
		`SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Salary by Employee & Month' LIMIT 1`,
		`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Salary by Employee & Month', $2::uuid) RETURNING id::text`,
		"grid1", modelID, workingRevID)
	for _, dimID := range []string{employeesDimID, monthsDimID} {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING
		`, grid1ID, dimID)
		must(err, "grid1 dimension")
	}
	for i, metricID := range []string{salaryMetricID, socialTaxMetricID} {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, $3) ON CONFLICT DO NOTHING
		`, grid1ID, metricID, i)
		must(err, "grid1 metric")
	}
	printf("  Grid 'Salary by Employee & Month' (%s)\n", short(grid1ID))

	// ── Grid 2 & 3: read-only rollups via rollup_source_grid_id ────────────────
	// Neither gets its own grid_metric rows — both mirror Grid 1's metrics,
	// computed via the generic cross-dimension/property/hierarchy rollup in
	// grid()+resolveCrossDimensionValue. This is what makes them ordinary
	// `grid` dashboard widgets instead of a bespoke endpoint+widget pair.

	section("Creating Grid 2 (cost_centers x months, rollup)")

	grid2ID := getOrInsert(ctx, pool,
		`SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Cost Center Summary' LIMIT 1`,
		`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Cost Center Summary', $2::uuid) RETURNING id::text`,
		"grid2", modelID, workingRevID)
	_, err = pool.Exec(ctx, `UPDATE model.grid_def SET rollup_source_grid_id=$1::uuid WHERE id=$2::uuid`, grid1ID, grid2ID)
	must(err, "grid2 rollup_source_grid_id")
	for _, dimID := range []string{costCentersDimID, monthsDimID} {
		_, err = pool.Exec(ctx, `
			INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING
		`, grid2ID, dimID)
		must(err, "grid2 dimension")
	}
	printf("  Grid 'Cost Center Summary' (%s) — rollup_source_grid_id=Grid1\n", short(grid2ID))

	section("Creating Grid 3 (regions x years, rollup)")

	grid3ID := getOrInsert(ctx, pool,
		`SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Region Summary' LIMIT 1`,
		`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Region Summary', $2::uuid) RETURNING id::text`,
		"grid3", modelID, workingRevID)
	_, err = pool.Exec(ctx, `UPDATE model.grid_def SET rollup_source_grid_id=$1::uuid WHERE id=$2::uuid`, grid1ID, grid3ID)
	must(err, "grid3 rollup_source_grid_id")
	_, err = pool.Exec(ctx, `
		INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING
	`, grid3ID, regionsDimID)
	must(err, "grid3 regions dimension")
	_, err = pool.Exec(ctx, `
		INSERT INTO model.grid_dimension (grid_id, dimension_id, display_level) VALUES ($1::uuid, $2::uuid, 0)
		ON CONFLICT (grid_id, dimension_id) DO UPDATE SET display_level = EXCLUDED.display_level
	`, grid3ID, monthsDimID)
	must(err, "grid3 months dimension (year level)")
	printf("  Grid 'Region Summary' (%s) — rollup_source_grid_id=Grid1, months@year-level\n", short(grid3ID))

	// ── Workflow ──────────────────────────────────────────────────────────────

	section("Creating salary budget submission workflow")

	wfStore := workflow.NewStore(pool)
	wfDef, err := wfStore.CreateWorkflowDef(ctx, appID, "Salary Budget Submission", "manual",
		[]*workflowv1.WorkflowStepDef{
			{
				Id:            "step-gm-approval",
				Name:          "General Manager Approval",
				Type:          workflowv1.StepType_STEP_TYPE_APPROVAL,
				AssigneeRoles: []string{"business_admin"},
				SlaHours:      72,
				NextStepIds:   []string{},
			},
		},
	)
	must(err, "workflow def")

	// context_schema: "department" is server-resolved from the caller's
	// RACI "responsible" grant (internal/workflow's ResolveStartContext,
	// source_hint "raci_responsible") — never client-supplied — and is also
	// what cells()'s generic write-lock matches a running instance's scope
	// against. "revision_id" is always populated by AutomationButtonWidget's
	// baseline context; "target_revision_id" comes from the Submit Plan
	// dashboard widget's static widget_props.context (set below).
	type contextVarDef struct {
		Key         string `json:"key"`
		Label       string `json:"label"`
		DataType    string `json:"data_type"`
		Required    bool   `json:"required"`
		SourceHint  string `json:"source_hint,omitempty"`
		DimensionID string `json:"dimension_id,omitempty"`
	}
	contextSchema, err := json.Marshal([]contextVarDef{
		{Key: "department", Label: "Cost Center", DataType: "Dimension member", Required: true, SourceHint: "raci_responsible", DimensionID: costCentersDimID},
		{Key: "revision_id", Label: "Working Revision", DataType: "Text", Required: true},
		{Key: "target_revision_id", Label: "Target (Annual) Revision", DataType: "Text", Required: true},
	})
	must(err, "marshal context_schema")
	_, err = pool.Exec(ctx, `UPDATE workflow.workflow_def SET context_schema=$1::jsonb WHERE id=$2::uuid`, contextSchema, wfDef.Id)
	must(err, "set context_schema")

	// on_approve: attached to the approval step's raw JSON, the same
	// precedent as stepDefRouted's "routes" field (internal/workflow/store.go)
	// — extra config beyond the typed WorkflowStepDef proto, stored directly
	// in workflow_def.steps. Executed generically by the gateway's
	// runStepApprovalActions (taskAction) on final approval.
	onApprove, err := json.Marshal(map[string]string{
		"copy_facts_to_context_key": "target_revision_id",
		"scope_context_key":         "department",
	})
	must(err, "marshal on_approve")
	_, err = pool.Exec(ctx, `UPDATE workflow.workflow_def SET steps = jsonb_set(steps, '{0,on_approve}', $1::jsonb) WHERE id=$2::uuid`, onApprove, wfDef.Id)
	must(err, "set on_approve")

	// required_comment: an existing generic field (surfaced to the UI by
	// tasks(), now enforced server-side by taskAction) — blanket, so both
	// reject and approve carry a rationale (satisfies "cannot reject
	// without comment" and matches this platform's own existing semantics
	// for the flag, which isn't decision-specific).
	_, err = pool.Exec(ctx, `UPDATE workflow.workflow_def SET steps = jsonb_set(steps, '{0,required_comment}', 'true'::jsonb) WHERE id=$1::uuid`, wfDef.Id)
	must(err, "set required_comment")

	var wfDefStatus string
	must(pool.QueryRow(ctx, `SELECT status FROM workflow.workflow_def WHERE id=$1::uuid`, wfDef.Id).Scan(&wfDefStatus), "check workflow def status")
	if wfDefStatus != "published" {
		_, err = wfStore.PublishWorkflowDef(ctx, wfDef.Id, platformAdminID)
		must(err, "publish workflow def")
	}

	// Reset any workflow instances left over from prior interactive testing
	// (browser/curl) so a re-seed always starts from a clean, fully-open
	// demo state. This program exclusively owns this workflow_def_id, so
	// this only ever touches its own demo data.
	_, err = pool.Exec(ctx, `DELETE FROM workflow.workflow_instance WHERE workflow_def_id=$1::uuid`, wfDef.Id)
	must(err, "reset workflow instances")

	// Reset Annual — it's system_managed and populated ONLY by an approval's
	// on_approve fact-copy (never by this seed), so a re-seed must clear
	// whatever a prior interactive approval left there or the "Annual starts
	// empty" acceptance criterion silently stops holding after the first
	// approve click anyone ever does against this demo.
	_, err = pool.Exec(ctx, `DELETE FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id=$2::uuid`, modelID, annualRevID)
	must(err, "reset annual facts")
	_, err = pool.Exec(ctx, `DELETE FROM runtime.calc_result WHERE model_id=$1::uuid AND revision_id=$2::uuid`, modelID, annualRevID)
	must(err, "reset annual calc results")

	printf("  Def: %q (%s) — published, context_schema RACI-scoped, on_approve configured\n", wfDef.Name, short(wfDef.Id))

	// ── Triggers ──────────────────────────────────────────────────────────────
	// A manual automation_rule, not a bare workflow_def_id on the dashboard
	// widget below — this is what makes "Submit Plan" a first-class Trigger:
	// listed/fireable/logged in the Developer console's Triggers tab, and
	// shown in "Salary Budget Submission"'s own Usage section in the
	// Workflows tab (internal/workflow/store.go GetWorkflowDefUsage).

	section("Creating triggers")

	submitRule, err := wfStore.CreateAutomationRule(ctx, appID, workingRevID,
		"submit_salary_plan",
		"Submits the cost center's Working salary plan for General Manager approval",
		"manual",
		wfDef.Name,
		wfDef.Id,
		"",
		"",
		nil,
	)
	must(err, "submit salary plan trigger")
	printf("  Trigger  : %q [manual] (%s)\n", submitRule.Name, short(submitRule.ID))

	// ── Dashboards ────────────────────────────────────────────────────────────
	// Generic widget types only: grid (Grid 1/2/3) and automation_button
	// (Submit Plan) — no payroll-specific widget_type values.

	section("Creating dashboards")

	ccDashID := getOrInsert(ctx, pool,
		`SELECT id::text FROM model.dashboard_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Cost Center Manager Dashboard' LIMIT 1`,
		`INSERT INTO model.dashboard_def (model_id, name, tags, revision_id) VALUES ($1::uuid, 'Cost Center Manager Dashboard', '{"Payroll"}', $2::uuid) RETURNING id::text`,
		"cc dashboard", modelID, workingRevID)
	gmDashID := getOrInsert(ctx, pool,
		`SELECT id::text FROM model.dashboard_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='General Manager Dashboard' LIMIT 1`,
		`INSERT INTO model.dashboard_def (model_id, name, tags, revision_id) VALUES ($1::uuid, 'General Manager Dashboard', '{"Payroll"}', $2::uuid) RETURNING id::text`,
		"gm dashboard", modelID, workingRevID)

	submitPlanProps, err := json.Marshal(map[string]any{
		"button_color": "var(--color-success)",
		"context":      map[string]string{"target_revision_id": annualRevID},
	})
	must(err, "marshal submit-plan widget_props")

	replaceWidgets(ctx, pool, ccDashID, []widgetSpec{
		{widgetType: "automation_button", refID: submitRule.ID, title: "Submit Plan", content: "Submit Plan", sizeH: 90, widgetProps: submitPlanProps},
		{widgetType: "grid", refID: grid1ID, title: "Salary by Employee & Month", sizeH: 420},
		{widgetType: "grid", refID: grid2ID, title: "Cost Center Summary", sizeH: 320},
		{widgetType: "grid", refID: grid3ID, title: "Region / Year Summary", sizeH: 320},
	})
	printf("  Cost Center Manager Dashboard (%s) — 4 widgets\n", short(ccDashID))

	replaceWidgets(ctx, pool, gmDashID, []widgetSpec{
		{widgetType: "grid", refID: grid2ID, title: "Cost Center Summary (All)", sizeH: 380},
		{widgetType: "grid", refID: grid3ID, title: "Region / Year Summary (All)", sizeH: 380},
	})
	printf("  General Manager Dashboard (%s) — 2 widgets (status via Workflow Inbox/History)\n", short(gmDashID))

	// ── Business roles: dashboard visibility ────────────────────────────────

	section("Assigning business roles (dashboard visibility)")

	ccRoleID := getOrInsert(ctx, pool,
		`SELECT id::text FROM identity.business_role WHERE workspace_id=$1::uuid AND name='Cost Center Managers' LIMIT 1`,
		`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Cost Center Managers') RETURNING id::text`,
		"cc business role", workspaceID)
	gmRoleID := getOrInsert(ctx, pool,
		`SELECT id::text FROM identity.business_role WHERE workspace_id=$1::uuid AND name='General Managers' LIMIT 1`,
		`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'General Managers') RETURNING id::text`,
		"gm business role", workspaceID)
	_, err = pool.Exec(ctx, `INSERT INTO identity.business_role_dashboard (role_id, dashboard_id) VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`, ccRoleID, ccDashID)
	must(err, "cc role dashboard")
	_, err = pool.Exec(ctx, `INSERT INTO identity.business_role_dashboard (role_id, dashboard_id) VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`, gmRoleID, gmDashID)
	must(err, "gm role dashboard")
	for _, m := range managers {
		_, err = pool.Exec(ctx, `INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`, ccRoleID, m.userID)
		must(err, "cc role member")
	}
	_, err = pool.Exec(ctx, `INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`, gmRoleID, generalManagerID)
	must(err, "gm role member")
	printf("  Cost Center Managers (%s) -> Cost Center Manager Dashboard, 3 members\n", short(ccRoleID))
	printf("  General Managers (%s) -> General Manager Dashboard, 1 member\n", short(gmRoleID))

	// ── Access rules: row-level scoping on Grid 1 ────────────────────────────
	// Only the cost_centers members outside a manager's own scope need a
	// rule — the employees dimension is declared a child of cost_centers
	// (parent_dimension_id/parent_member_id, see the employees section
	// above), so hiding a cost center now automatically cascades to every
	// employee that rolls up to it (writeguard.ExpandHidden on the read
	// side, walking AncestorChain on the write side — see internal/
	// writeguard/cascade.go). Grid 3 (regions) needs no rule of its own
	// either: the cascade also removes a hidden employee from
	// AllDimensions and every fact row referencing it — including the
	// region grouping Grid 3 derives from employees.properties.region — so
	// this one cost-center-level rule set is sufficient for Grid 1, 2, AND
	// 3 to all be manager-scoped.

	section("Configuring access rules (cost-center row scoping)")

	for _, m := range managers {
		// This program exclusively owns each manager's access rules — reset
		// before recreating so a reseed against a database left over from an
		// older version of this script (e.g. one that also hid employees
		// individually) doesn't accumulate stale rows the current code no
		// longer creates.
		_, err = pool.Exec(ctx, `DELETE FROM identity.user_access_rule WHERE user_id=$1::uuid`, m.userID)
		must(err, "reset access rules for "+m.costCenter)
		for cc, memberID := range costCenterMemberID {
			if cc == m.costCenter {
				continue
			}
			_, err = pool.Exec(ctx, `
				INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access)
				VALUES ($1::uuid, 'dimension_member', $2, 'hidden')
				ON CONFLICT (user_id, rule_type, ref_id) DO UPDATE SET access = EXCLUDED.access
			`, m.userID, memberID)
			must(err, "hide "+cc+" from "+m.costCenter)
		}
		printf("  cc_manager_%-6s: 2 other cost centers hidden (8 employees cascade automatically)\n", ccSuffix(m.costCenter))
	}
	printf("  general_manager: no restrictions (sees all 12 employees, all cost centers)\n")

	// ── Input data — Working revision salary facts ───────────────────────────

	section("Writing salary input data (Working revision)")

	for _, e := range employees {
		for _, y := range []string{"2026", "2027"} {
			monthly := e.monthlySalary2026
			if y == "2027" {
				monthly = math.Round(monthly * 1.05) // a modest year-over-year raise
			}
			for i := 0; i < 12; i++ {
				code := fmt.Sprintf("%s-%02d", y, i+1)
				dimJSON := fmt.Sprintf(`{"%s":"%s","%s":"%s"}`, employeesDimID, e.code, monthsDimID, code)
				_, err = pool.Exec(ctx, `
					INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by)
					VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5::jsonb, $6, $7::uuid)
				`, modelID, workingRevID, workingRevisionName, salaryMetricID, dimJSON, monthly, managerFor(managers, e.costCenter))
				must(err, "fact "+e.code+"."+code)
			}
		}
	}
	printf("  %d employees x 24 months salary facts written\n", len(employees))

	// ── Calculation engine ────────────────────────────────────────────────────

	section("Running calculation engine (Working revision)")

	calcStore := calculation.NewStore(pool)
	scheduler := calculation.NewScheduler(log, calcStore, nil)
	must(scheduler.RecalcAffected(ctx, modelID, workingRevID, []string{salaryMetricID}), "recalc working")
	printf("  social_tax recalculated for the Working revision\n")

	// ── Audit log ─────────────────────────────────────────────────────────────

	section("Seeding audit log")
	auditEvents := []struct {
		category, eventType, actorID, actorRole, resourceType, resourceID string
	}{
		{"admin", "user.created", platformAdminID, "platform_admin", "user", generalManagerID},
		{"admin", "user.created", platformAdminID, "platform_admin", "user", managers[0].userID},
		{"model_change", "metric.created", devID, "developer", "metric", salaryMetricID},
		{"model_change", "metric.created", devID, "developer", "metric", socialTaxMetricID},
		{"admin", "access_rule.created", tenantAdminID, "tenant_admin", "user_access_rule", managers[0].userID},
	}
	// audit_event is an append-only log with no natural unique key to
	// ON CONFLICT against — delete this seed's own rows (identified by
	// workspace_id + the exact event_type/resource_id pairs it owns) before
	// reinserting, the same "this program exclusively owns this data, so
	// reset-then-recreate is safe" reasoning as the workflow-instance reset
	// above. Without this, every re-seed silently duplicated all 5 rows.
	for _, e := range auditEvents {
		_, err = pool.Exec(ctx, `
			DELETE FROM audit.audit_event
			WHERE workspace_id=$1::uuid AND event_type=$2 AND resource_type=$3 AND resource_id=$4
		`, workspaceID, e.eventType, e.resourceType, e.resourceID)
		must(err, "reset audit "+e.eventType)
	}
	for _, e := range auditEvents {
		_, err = pool.Exec(ctx, `
			INSERT INTO audit.audit_event (category, event_type, actor_user_id, actor_role, workspace_id, resource_type, resource_id, revision_id)
			VALUES ($1::audit.event_category, $2, $3::uuid, $4, $5::uuid, $6, $7, $8::uuid)
		`, e.category, e.eventType, e.actorID, e.actorRole, workspaceID, e.resourceType, e.resourceID, workingRevID)
		must(err, "audit "+e.eventType)
	}
	printf("  %d audit events inserted (reset-then-recreate, no duplicates on reseed)\n", len(auditEvents))

	// ── Structural verification ──────────────────────────────────────────────
	// Behavioral correctness (RACI-scoped submit, write locks, approval
	// copy, rollup math) lives in generic gateway code and is verified by
	// internal/gateway's DATABASE_URL-guarded tests and the README's manual
	// demo script — not scripted here, since there's no longer a reusable
	// Store package for this program to call into directly.

	section("Structural verification")

	check := func(label string, pass bool, note string) {
		checks = append(checks, checkResult{label, pass, note})
	}

	// -- seven users, each with exactly their expected role --------------------
	wantRole := map[string]string{
		platformAdminID:    "platform_admin",
		devID:              "developer",
		tenantAdminID:      "tenant_admin",
		generalManagerID:   "business_admin",
		managers[0].userID: "business_user",
		managers[1].userID: "business_user",
		managers[2].userID: "business_user",
	}
	rightRoleCount := 0
	for userID, role := range wantRole {
		var has bool
		must(pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM identity.role_assignment WHERE user_id=$1::uuid AND role=$2::identity.user_role)`, userID, role).Scan(&has), "role check")
		if has {
			rightRoleCount++
		}
	}
	check("Seven users, each with their expected role", rightRoleCount == 7, fmt.Sprintf("%d/7 roles confirmed", rightRoleCount))

	// -- exactly three cost centers ---------------------------------------------
	var ccCount int
	must(pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid`, costCentersDimID).Scan(&ccCount), "cost center count")
	check("Exactly three cost centers", ccCount == 3, fmt.Sprintf("%d members", ccCount))

	// -- 12 employees, four per cost center --------------------------------------
	var empCount int
	must(pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid`, employeesDimID).Scan(&empCount), "employee count")
	allFourEach := true
	for _, cc := range []string{"CC_SALES", "CC_OPS", "CC_GA"} {
		var n int
		must(pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid AND parent_member_id=$2::uuid`, employeesDimID, costCenterMemberID[cc]).Scan(&n), "employees per cc")
		if n != 4 {
			allFourEach = false
		}
	}
	check("12 employees, four per cost center", empCount == 12 && allFourEach, fmt.Sprintf("%d employees, 4/cc=%v", empCount, allFourEach))

	// -- every employee has a valid region property ------------------------------
	var empWithRegion int
	must(pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM model.dimension_member
		WHERE dimension_id=$1::uuid AND properties->>'region' = ANY($2::text[])
	`, employeesDimID, regionCycle).Scan(&empWithRegion), "employees with valid region")
	check("Every employee has a valid region property", empWithRegion == 12, fmt.Sprintf("%d/12", empWithRegion))

	// -- two year parents and 24 month leaves -------------------------------------
	var yearCount, leafCount int
	must(pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid AND parent_member_id IS NULL`, monthsDimID).Scan(&yearCount), "year count")
	must(pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid AND parent_member_id IS NOT NULL`, monthsDimID).Scan(&leafCount), "month leaf count")
	check("Two year parents and 24 month leaves", yearCount == 2 && leafCount == 24, fmt.Sprintf("years=%d leaves=%d", yearCount, leafCount))

	// -- metric formulas and dependencies ------------------------------------------
	var salaryIsInput bool
	var socialTaxFormulaDB *string
	must(pool.QueryRow(ctx, `SELECT is_input FROM model.metric_def WHERE id=$1::uuid`, salaryMetricID).Scan(&salaryIsInput), "salary is_input")
	must(pool.QueryRow(ctx, `SELECT formula FROM model.metric_def WHERE id=$1::uuid`, socialTaxMetricID).Scan(&socialTaxFormulaDB), "social_tax formula")
	var depExists bool
	must(pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model.calc_dependency WHERE metric_id=$1::uuid AND depends_on_metric_id=$2::uuid)`, socialTaxMetricID, salaryMetricID).Scan(&depExists), "calc dependency exists")
	check("salary is an input metric, social_tax formula + dependency wired", salaryIsInput && socialTaxFormulaDB != nil && *socialTaxFormulaDB == socialTaxFormula && depExists, "ok")

	// -- all three grid definitions exist -----------------------------------------
	var gridDefCount int
	must(pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`, modelID, workingRevID).Scan(&gridDefCount), "grid def count")
	check("All three grid definitions exist", gridDefCount == 3, fmt.Sprintf("%d grids", gridDefCount))

	// -- manager access rules and business-role memberships ------------------------
	rulesOK := true
	for _, m := range managers {
		var n int
		must(pool.QueryRow(ctx, `SELECT COUNT(*) FROM identity.user_access_rule WHERE user_id=$1::uuid AND access='hidden'`, m.userID).Scan(&n), "access rule count")
		if n != 2 { // 2 other cost centers — their 8 employees cascade automatically, no rule of their own
			rulesOK = false
		}
	}
	var ccMemberCount, gmMemberCount int
	must(pool.QueryRow(ctx, `SELECT COUNT(*) FROM identity.business_role_member WHERE role_id=$1::uuid`, ccRoleID).Scan(&ccMemberCount), "cc role members")
	must(pool.QueryRow(ctx, `SELECT COUNT(*) FROM identity.business_role_member WHERE role_id=$1::uuid`, gmRoleID).Scan(&gmMemberCount), "gm role members")
	check("Each manager has exactly 2 hidden-access rules (cost centers only); business roles have 3+1 members",
		rulesOK && ccMemberCount == 3 && gmMemberCount == 1,
		fmt.Sprintf("rules_ok=%v cc_members=%d gm_members=%d", rulesOK, ccMemberCount, gmMemberCount))

	// -- Annual is empty after a clean reseed --------------------------------------
	var annualFactCount, annualCalcCount int
	must(pool.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id=$2::uuid`, modelID, annualRevID).Scan(&annualFactCount), "annual fact count")
	must(pool.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.calc_result WHERE model_id=$1::uuid AND revision_id=$2::uuid`, modelID, annualRevID).Scan(&annualCalcCount), "annual calc count")
	check("Annual revision has no facts after a clean reseed", annualFactCount == 0 && annualCalcCount == 0, fmt.Sprintf("facts=%d calc=%d", annualFactCount, annualCalcCount))

	// -- no active workflow instances after a clean reseed --------------------------
	var instanceCount int
	must(pool.QueryRow(ctx, `SELECT COUNT(*) FROM workflow.workflow_instance WHERE workflow_def_id=$1::uuid`, wfDef.Id).Scan(&instanceCount), "instance count")
	check("No workflow instances after a clean reseed", instanceCount == 0, fmt.Sprintf("%d instances", instanceCount))

	var g2Source, g3Source *string
	must(pool.QueryRow(ctx, `SELECT rollup_source_grid_id::text FROM model.grid_def WHERE id=$1::uuid`, grid2ID).Scan(&g2Source), "grid2 rollup source")
	must(pool.QueryRow(ctx, `SELECT rollup_source_grid_id::text FROM model.grid_def WHERE id=$1::uuid`, grid3ID).Scan(&g3Source), "grid3 rollup source")
	check("Grid 2 rollup_source_grid_id -> Grid 1", g2Source != nil && *g2Source == grid1ID, "grid2->grid1")
	check("Grid 3 rollup_source_grid_id -> Grid 1", g3Source != nil && *g3Source == grid1ID, "grid3->grid1")

	var g3MonthsLevel *int
	must(pool.QueryRow(ctx, `SELECT display_level FROM model.grid_dimension WHERE grid_id=$1::uuid AND dimension_id=$2::uuid`, grid3ID, monthsDimID).Scan(&g3MonthsLevel), "grid3 months display_level")
	check("Grid 3 months dimension at year level (display_level=0)", g3MonthsLevel != nil && *g3MonthsLevel == 0, "display_level="+intPtrStr(g3MonthsLevel))

	var regionsSourceDim, regionsSourceProp *string
	must(pool.QueryRow(ctx, `SELECT source_dimension_id::text, source_property FROM model.dimension_def WHERE id=$1::uuid`, regionsDimID).Scan(&regionsSourceDim, &regionsSourceProp), "regions source_*")
	check("regions.source_dimension_id -> employees", regionsSourceDim != nil && *regionsSourceDim == employeesDimID, "ok")
	check("regions.source_property = 'region'", regionsSourceProp != nil && *regionsSourceProp == "region", "ok")

	// fact_input is an append-only log (grid()/latestSalaryByCell always
	// take the latest row per dim_members via DISTINCT ON ... ORDER BY
	// entered_at DESC), so re-running this seed against an already-seeded
	// database adds new rows rather than replacing old ones — count
	// distinct cells, not raw rows, to stay correct across re-seeds.
	var factCellCount int
	must(pool.QueryRow(ctx, `SELECT COUNT(DISTINCT dim_members) FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid`, modelID, workingRevID, salaryMetricID).Scan(&factCellCount), "fact cell count")
	check("12 employees x 24 months salary cells written", factCellCount == 12*24, fmt.Sprintf("%d distinct cells", factCellCount))

	var wfStatus string
	var schemaLen int
	must(pool.QueryRow(ctx, `SELECT status, jsonb_array_length(context_schema) FROM workflow.workflow_def WHERE id=$1::uuid`, wfDef.Id).Scan(&wfStatus, &schemaLen), "workflow def status")
	check("Workflow def published with 3 context_schema vars", wfStatus == "published" && schemaLen == 3, fmt.Sprintf("status=%s vars=%d", wfStatus, schemaLen))

	var hasOnApprove, hasRequiredComment bool
	must(pool.QueryRow(ctx, `SELECT steps->0 ? 'on_approve' FROM workflow.workflow_def WHERE id=$1::uuid`, wfDef.Id).Scan(&hasOnApprove), "on_approve present")
	check("Approval step has on_approve config", hasOnApprove, "ok")
	must(pool.QueryRow(ctx, `SELECT COALESCE((steps->0->>'required_comment')::boolean, false) FROM workflow.workflow_def WHERE id=$1::uuid`, wfDef.Id).Scan(&hasRequiredComment), "required_comment present")
	check("Approval step requires a comment", hasRequiredComment, "ok")

	var socialTaxRows int
	must(pool.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.calc_result WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid`, modelID, workingRevID, socialTaxMetricID).Scan(&socialTaxRows), "calc_result count")
	check("social_tax recalculated (calc_result row exists)", socialTaxRows > 0, fmt.Sprintf("%d rows", socialTaxRows))

	// ── Summary ───────────────────────────────────────────────────────────────

	section("Demo complete — summary")
	printf("  Application : Budgeting Demo (%s)\n", short(appID))
	printf("  Model ID    : %s\n", short(modelID))
	printf("  Users (7)   : platform-admin@acme.com, dev@acme.com, tenant-admin+payroll@acme.com,\n")
	printf("                general.manager@acme.com, cc.manager.sales@acme.com,\n")
	printf("                cc.manager.ops@acme.com, cc.manager.ga@acme.com\n")
	printf("  Dimensions  : cost_centers(3), regions(4, property-derived), employees(12), months(26)\n")
	printf("  Metrics     : salary [input], social_tax [calc] = salary * %.2f\n", socialTaxRate)
	printf("\n")

	failed := 0
	for _, c := range checks {
		mark := "\033[32m✓\033[0m"
		if !c.pass {
			mark = "\033[31m✗\033[0m"
			failed++
		}
		printf("  %s %-55s %s\n", mark, c.label, c.note)
	}
	printf("\n")
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\033[31m%d check(s) failed\033[0m\n", failed)
		os.Exit(1)
	}
	printf("  All checks passed.\n")
	fmt.Println()
}

// ── seed helpers ─────────────────────────────────────────────────────────────

type widgetSpec struct {
	widgetType, refID, title, content string
	sizeH                             int
	widgetProps                       []byte // pre-marshaled JSON, or nil
}

func replaceWidgets(ctx context.Context, pool *pgxpool.Pool, dashID string, specs []widgetSpec) {
	var count int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.dashboard_widget WHERE dashboard_id=$1::uuid`, dashID).Scan(&count)
	if count > 0 {
		_, _ = pool.Exec(ctx, `DELETE FROM model.dashboard_widget WHERE dashboard_id=$1::uuid`, dashID)
	}
	y := 0
	for i, w := range specs {
		var refID, content, props any
		if w.refID != "" {
			refID = w.refID
		}
		if w.content != "" {
			content = w.content
		}
		if w.widgetProps != nil {
			props = w.widgetProps
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, content, title, show_title, pos_x, pos_y, size_w, size_h, sort_order, widget_props)
			VALUES ($1::uuid, $2, $3, $4, $5, true, 0, $6, 1200, $7, $8, $9::jsonb)
		`, dashID, w.widgetType, refID, content, w.title, y, w.sizeH, i, props)
		must(err, "widget "+w.widgetType)
		y += w.sizeH + 16
	}
}

func managerFor(managers []*ccManager, costCenter string) string {
	for _, m := range managers {
		if m.costCenter == costCenter {
			return m.userID
		}
	}
	return ""
}

func ccSuffix(costCenter string) string {
	switch costCenter {
	case "CC_SALES":
		return "sales"
	case "CC_OPS":
		return "ops"
	case "CC_GA":
		return "ga"
	}
	return costCenter
}
func ccSuffixUpper(costCenter string) string {
	switch costCenter {
	case "CC_SALES":
		return "SALES"
	case "CC_OPS":
		return "OPS"
	case "CC_GA":
		return "GA"
	}
	return costCenter
}

// ── output helpers (matches cmd/seed-budget's conventions) ────────────────────

func section(title string)              { fmt.Printf("\n\033[1;34m▶ %s\033[0m\n", title) }
func step(msg string)                   { fmt.Printf("  %s... ", msg) }
func ok()                               { fmt.Println("\033[32mok\033[0m") }
func printf(format string, args ...any) { fmt.Printf(format, args...) }
func intPtrStr(v *int) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d", *v)
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
