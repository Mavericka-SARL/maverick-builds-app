// cmd/seed-capex — CapEx Portfolio reference application + engine stress demo.
//
// Purpose: exercise every generic engine primitive together — hierarchical
// dimensions, formatted metrics with calc dependencies, grids (incl. rollup),
// free-canvas dashboards with all widget types (chart × 5 types, metric_kpi,
// grid, form, automation_button, text), designer-shaped workflows (task /
// approval / condition / notification / join, routes, SLAs), all 5 automation
// trigger types, platform + business roles, RACI, metric policies, per-user
// access rules, form→metric mappings, notifications and audit — and then run
// a VERIFICATION phase that drives real workflow instances through the same
// code paths the gateway uses and reports PASS / FINDING for each probe.
//
// Everything is built from generic dev/tenant-admin primitives (the same
// tables and store methods the consoles write). Nothing here is bespoke.
//
// Usage:
//
//	DATABASE_URL=postgres://... go run ./cmd/seed-capex
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/migrate"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultDSN   = "postgres://mavericks:mavericks@localhost:5432/mavericks?sslmode=disable"
	revisionName = "FY2026 Plan"
)

// findings collects verification results for the final recap.
var findings []string

func finding(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	findings = append(findings, msg)
	fmt.Printf("  \033[33mFINDING\033[0m %s\n", msg)
}

func pass(format string, args ...any) {
	fmt.Printf("  \033[32mPASS\033[0m    %s\n", fmt.Sprintf(format, args...))
}

func main() {
	log := logger.New("seed-capex")
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultDSN
	}

	section("CapEx Portfolio FY2026 — Demo Seed + Engine Verification")

	// Demo users are reused across runs; notification probes must only count
	// rows produced by THIS run.
	seedStart := time.Now()

	step("Connecting to PostgreSQL")
	pool, err := db.Connect(ctx, dsn)
	must(err, "connect")
	defer pool.Close()
	ok()

	step("Running migrations")
	must(migrate.Run(ctx, pool, migrationfs.FS, "."), "migrate")
	ok()

	// ── Org hierarchy ──────────────────────────────────────────────────────────

	section("Building org hierarchy")

	var customerID string
	err = pool.QueryRow(ctx, `SELECT id::text FROM core.customer WHERE name <> 'OtherCorp' ORDER BY created_at LIMIT 1`).Scan(&customerID)
	if err != nil {
		customerID = mustScan(pool.QueryRow(ctx, `
			INSERT INTO core.customer (name, plan) VALUES ('GlobalCorp', 'enterprise') RETURNING id::text
		`), "customer")
	}
	printf("  Customer   : %s\n", short(customerID))

	workspaceID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Capital Planning') RETURNING id::text
	`, customerID), "workspace")
	printf("  Workspace  : Capital Planning (%s)\n", short(workspaceID))

	appID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO core.application (workspace_id, name, mode) VALUES ($1::uuid, 'CapEx Portfolio FY2026', 'crud') RETURNING id::text
	`, workspaceID), "application")
	printf("  Application: CapEx Portfolio FY2026 (%s)\n", short(appID))

	modelID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'CapEx Model') RETURNING id::text
	`, appID), "model")
	printf("  Model      : CapEx Model (%s)\n", short(modelID))

	// Second tenant used only by the isolation probes (cross-tenant leak tests).
	var otherCustomerID string
	err = pool.QueryRow(ctx, `SELECT id::text FROM core.customer WHERE name = 'OtherCorp' LIMIT 1`).Scan(&otherCustomerID)
	if err != nil {
		otherCustomerID = mustScan(pool.QueryRow(ctx, `
			INSERT INTO core.customer (name, plan) VALUES ('OtherCorp', 'standard') RETURNING id::text
		`), "other customer")
	}
	var otherWorkspaceID string
	err = pool.QueryRow(ctx, `SELECT id::text FROM core.workspace WHERE customer_id=$1::uuid LIMIT 1`, otherCustomerID).Scan(&otherWorkspaceID)
	if err != nil {
		otherWorkspaceID = mustScan(pool.QueryRow(ctx, `
			INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'OtherCorp HQ') RETURNING id::text
		`, otherCustomerID), "other workspace")
	}
	printf("  Probe tenant: OtherCorp (%s) / workspace %s\n", short(otherCustomerID), short(otherWorkspaceID))

	// ── Users & platform roles ─────────────────────────────────────────────────

	section("Creating / reusing demo users")

	// display_name is updated on conflict (not just keycloak_sub) so renaming
	// a persona in this file actually takes effect on rerun instead of
	// silently keeping whatever name a prior run first inserted.
	user := func(sub, email, name, custID string) string {
		id := mustScan(pool.QueryRow(ctx, `
			INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
			VALUES ($1, $2, $3, $4::uuid)
			ON CONFLICT (keycloak_sub) DO UPDATE SET display_name = EXCLUDED.display_name, email = EXCLUDED.email
			RETURNING id::text
		`, sub, email, name, custID), "user "+email)
		printf("  %-28s %s\n", email, short(id))
		return id
	}
	// "Dev" personas are named distinctly per demo (not a bare role label like
	// "Sam (Developer)") so the persona switcher doesn't show two
	// indistinguishable entries when multiple demos share a customer/tenant.
	mariaID := user("demo-capex-maria", "maria.requester@acme.com", "Maria (Requester)", customerID)
	davidID := user("demo-capex-david", "david.depthead@acme.com", "David (Dept Head)", customerID)
	fatimaID := user("demo-capex-fatima", "fatima.finance@acme.com", "Fatima (Finance Controller)", customerID)
	carlosID := user("demo-capex-carlos", "carlos.cfo@acme.com", "Carlos (CFO)", customerID)
	lenaID := user("demo-capex-lena", "lena.legal@acme.com", "Lena (Legal)", customerID)
	devID := user("demo-capex-dev", "dev.capex@acme.com", "Priya (CapEx Developer)", customerID)
	eveID := user("demo-capex-eve", "eve.other@othercorp.com", "Eve (OtherCorp Admin)", otherCustomerID)

	assignRole := func(userID, role, wsID string) {
		_, err := pool.Exec(ctx, `
			INSERT INTO identity.role_assignment (user_id, role, workspace_id)
			VALUES ($1::uuid, $2::identity.user_role, $3::uuid)
			ON CONFLICT (user_id, role, workspace_id) DO NOTHING
		`, userID, role, wsID)
		must(err, "role "+role)
	}
	assignRole(mariaID, "business_user", workspaceID)
	assignRole(davidID, "business_user", workspaceID)
	assignRole(fatimaID, "business_admin", workspaceID)
	assignRole(carlosID, "business_admin", workspaceID)
	assignRole(lenaID, "business_user", workspaceID)
	assignRole(devID, "developer", workspaceID)
	// Eve holds the SAME platform roles but in the OTHER tenant's workspace.
	assignRole(eveID, "business_admin", otherWorkspaceID)
	assignRole(eveID, "business_user", otherWorkspaceID)
	printf("  Platform roles assigned (incl. probe user in OtherCorp)\n")

	// ── Revision ───────────────────────────────────────────────────────────────

	revisionID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.revision (model_id, name, description)
		VALUES ($1::uuid, $2, 'Board-approved FY2026 capital plan')
		RETURNING id::text
	`, modelID, revisionName), "revision")
	_, err = pool.Exec(ctx, `
		UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name=$2 WHERE id=$3::uuid
	`, revisionID, revisionName, modelID)
	must(err, "set active revision")
	printf("  Revision   : %s (%s, active)\n", revisionName, short(revisionID))

	// ── Dimensions ─────────────────────────────────────────────────────────────

	section("Defining dimensions (with department hierarchy)")

	dim := func(name, props string) string {
		return mustScan(pool.QueryRow(ctx, `
			INSERT INTO model.dimension_def (model_id, revision_id, name, properties)
			VALUES ($1::uuid, $2::uuid, $3, $4::jsonb)
			RETURNING id::text
		`, modelID, revisionID, name, props), "dimension "+name)
	}
	deptDimID := dim("department", `[{"name":"code","type":"text","required":true}]`)
	catDimID := dim("category", `[{"name":"code","type":"text","required":true}]`)
	qtrDimID := dim("quarter", `[{"name":"code","type":"text","required":true}]`)
	projDimID := dim("project", `[{"name":"code","type":"text","required":true},{"name":"sponsor","type":"text","required":false}]`)

	member := func(dimID, code, label string, parentID *string, sort int) string {
		return mustScan(pool.QueryRow(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, sort_order)
			VALUES ($1::uuid, $2, $3, $4::uuid, $5)
			RETURNING id::text
		`, dimID, code, label, parentID, sort), "member "+code)
	}

	// Department: 3 divisions with 2 leaf departments each (same-dimension hierarchy).
	techID := member(deptDimID, "TECH", "Technology Division", nil, 0)
	commID := member(deptDimID, "COMM", "Commercial Division", nil, 1)
	corpID := member(deptDimID, "CORP", "Corporate Division", nil, 2)
	member(deptDimID, "ENG", "Engineering", &techID, 3)
	member(deptDimID, "IT", "IT Operations", &techID, 4)
	member(deptDimID, "SALES", "Sales", &commID, 5)
	member(deptDimID, "MKT", "Marketing", &commID, 6)
	member(deptDimID, "HR", "Human Resources", &corpID, 7)
	member(deptDimID, "FIN", "Finance", &corpID, 8)
	printf("  department : TECH(ENG,IT) COMM(SALES,MKT) CORP(HR,FIN)\n")

	for i, c := range []struct{ code, label string }{
		{"BLD", "Buildings & Facilities"}, {"EQP", "Equipment & Machinery"},
		{"SW", "Software & Licences"}, {"VEH", "Vehicles & Fleet"},
	} {
		member(catDimID, c.code, c.label, nil, i)
	}
	printf("  category   : BLD EQP SW VEH\n")

	for i, q := range []string{"Q1", "Q2", "Q3", "Q4"} {
		member(qtrDimID, q, q+" 2026", nil, i)
	}
	printf("  quarter    : Q1..Q4\n")

	// ── Projects: single source of truth for all fact data ─────────────────────

	type project struct {
		code, label, dept, cat, qtr           string
		requested, approved, spent, committed float64
		risk, roi                             float64
	}
	projects := []project{
		{"P001", "Data-Centre Migration", "IT", "EQP", "Q1", 450000, 420000, 310000, 60000, 7.5, 14},
		{"P002", "ERP Upgrade", "IT", "SW", "Q2", 380000, 350000, 190000, 90000, 8.2, 22},
		{"P003", "HQ Office Refit", "HR", "BLD", "Q1", 220000, 180000, 165000, 10000, 3.1, 6},
		{"P004", "Fleet Renewal", "SALES", "VEH", "Q3", 300000, 260000, 120000, 80000, 4.4, 9},
		{"P005", "CRM Platform", "SALES", "SW", "Q2", 270000, 250000, 150000, 70000, 6.8, 25},
		{"P006", "Lab Equipment", "ENG", "EQP", "Q1", 520000, 500000, 380000, 90000, 5.9, 18},
		{"P007", "Test Automation Rig", "ENG", "SW", "Q3", 240000, 220000, 90000, 60000, 4.2, 20},
		{"P008", "Brand Studio Build-out", "MKT", "BLD", "Q4", 180000, 140000, 30000, 40000, 5.5, 11},
		{"P009", "Ad-Tech Stack", "MKT", "SW", "Q4", 160000, 150000, 60000, 30000, 7.1, 16},
		{"P010", "Treasury System", "FIN", "SW", "Q2", 210000, 200000, 110000, 50000, 6.3, 13},
	}
	for i, p := range projects {
		member(projDimID, p.code, p.label, nil, i)
	}
	printf("  project    : %d members (P001..P010)\n", len(projects))

	// ── Metrics ────────────────────────────────────────────────────────────────

	section("Defining metrics (6 input + 3 calculated)")

	type metricSpec struct {
		name, formula, agg, format string
		decimals                   int
		isInput                    bool
	}
	metricSpecs := []metricSpec{
		{"requested_amount", "", "sum", "currency", 0, true},
		{"approved_amount", "", "sum", "currency", 0, true},
		{"spent_amount", "", "sum", "currency", 0, true},
		{"committed_amount", "", "sum", "currency", 0, true},
		{"risk_score", "", "average", "number", 1, true},
		{"roi_pct", "", "average", "percentage", 1, true},
		{"remaining_budget", "{approved_amount} - {spent_amount} - {committed_amount}", "sum", "currency", 0, false},
		{"utilization_pct", "{spent_amount} / {approved_amount} * 100", "average", "percentage", 1, false},
		{"approval_rate_pct", "{approved_amount} / {requested_amount} * 100", "average", "percentage", 1, false},
	}
	metrics := map[string]string{}
	for _, m := range metricSpecs {
		id := mustScan(pool.QueryRow(ctx, `
			INSERT INTO model.metric_def (model_id, revision_id, name, formula, is_input, agg_rule, format, format_decimals, format_currency)
			VALUES ($1::uuid, $2::uuid, $3, NULLIF($4,''), $5, $6, $7, $8, '$')
			RETURNING id::text
		`, modelID, revisionID, m.name, m.formula, m.isInput, m.agg, m.format, m.decimals), "metric "+m.name)
		metrics[m.name] = id
		kind := "INPUT"
		if !m.isInput {
			kind = "CALC "
		}
		printf("  [%s] %-20s %s\n", kind, m.name, short(id))
	}
	deps := map[string][]string{
		"remaining_budget":  {"approved_amount", "spent_amount", "committed_amount"},
		"utilization_pct":   {"spent_amount", "approved_amount"},
		"approval_rate_pct": {"approved_amount", "requested_amount"},
	}
	for calc, inputs := range deps {
		for _, in := range inputs {
			_, err = pool.Exec(ctx, `
				INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid)
			`, metrics[calc], metrics[in])
			must(err, "dep "+calc+"←"+in)
		}
	}

	// ── Facts (project → dept×quarter → category → totals, all consistent) ─────

	section("Writing fact data")

	writeFact := func(metric string, dims map[string]string, value float64, by string) {
		dimJSON, _ := json.Marshal(dims)
		_, err := pool.Exec(ctx, `
			INSERT INTO runtime.fact_input (model_id, revision_name, metric_id, dim_members, value, entered_by, revision_id)
			VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6::uuid, $7::uuid)
		`, modelID, revisionName, metrics[metric], dimJSON, value, by, revisionID)
		must(err, "fact "+metric)
	}

	moneyOf := func(p project) map[string]float64 {
		return map[string]float64{
			"requested_amount": p.requested, "approved_amount": p.approved,
			"spent_amount": p.spent, "committed_amount": p.committed,
		}
	}

	// Per-project facts (drives scatter, histogram, portfolio grid).
	for _, p := range projects {
		for m, v := range moneyOf(p) {
			writeFact(m, map[string]string{projDimID: p.code}, v, mariaID)
		}
		writeFact("risk_score", map[string]string{projDimID: p.code}, p.risk, fatimaID)
		writeFact("roi_pct", map[string]string{projDimID: p.code}, p.roi, fatimaID)
	}
	// Dept × quarter aggregates (drives dept grid + bar/line charts).
	deptQtr := map[[2]string]map[string]float64{}
	catAgg := map[string]map[string]float64{}
	tot := map[string]float64{}
	for _, p := range projects {
		key := [2]string{p.dept, p.qtr}
		if deptQtr[key] == nil {
			deptQtr[key] = map[string]float64{}
		}
		if catAgg[p.cat] == nil {
			catAgg[p.cat] = map[string]float64{}
		}
		for m, v := range moneyOf(p) {
			deptQtr[key][m] += v
			catAgg[p.cat][m] += v
			tot[m] += v
		}
	}
	for key, vals := range deptQtr {
		for m, v := range vals {
			writeFact(m, map[string]string{deptDimID: key[0], qtrDimID: key[1]}, v, davidID)
		}
	}
	// Per-quarter aggregates (line chart reads facts keyed by quarter alone).
	qtrAgg := map[string]map[string]float64{}
	for _, p := range projects {
		if qtrAgg[p.qtr] == nil {
			qtrAgg[p.qtr] = map[string]float64{}
		}
		for m, v := range moneyOf(p) {
			qtrAgg[p.qtr][m] += v
		}
	}
	for q, vals := range qtrAgg {
		for m, v := range vals {
			writeFact(m, map[string]string{qtrDimID: q}, v, davidID)
		}
	}
	// Per-department aggregates (bar chart by department).
	deptAgg := map[string]map[string]float64{}
	for _, p := range projects {
		if deptAgg[p.dept] == nil {
			deptAgg[p.dept] = map[string]float64{}
		}
		for m, v := range moneyOf(p) {
			deptAgg[p.dept][m] += v
		}
	}
	for d, vals := range deptAgg {
		for m, v := range vals {
			writeFact(m, map[string]string{deptDimID: d}, v, davidID)
		}
	}
	// Per-category aggregates (pie chart).
	for c, vals := range catAgg {
		for m, v := range vals {
			writeFact(m, map[string]string{catDimID: c}, v, fatimaID)
		}
	}
	// Division-level aggregates: charts do NOT roll up the member hierarchy —
	// every member of the plotted dimension is read by exact key, so parents
	// render empty unless facts exist at parent level too.
	divisionOf := map[string]string{"ENG": "TECH", "IT": "TECH", "SALES": "COMM", "MKT": "COMM", "HR": "CORP", "FIN": "CORP"}
	divAgg := map[string]map[string]float64{}
	for _, p := range projects {
		div := divisionOf[p.dept]
		if divAgg[div] == nil {
			divAgg[div] = map[string]float64{}
		}
		for m, v := range moneyOf(p) {
			divAgg[div][m] += v
		}
	}
	for d, vals := range divAgg {
		for m, v := range vals {
			writeFact(m, map[string]string{deptDimID: d}, v, davidID)
		}
	}
	// Grand totals for the calculation engine + KPI tiles.
	for m, v := range tot {
		writeFact(m, map[string]string{}, v, carlosID)
	}
	var avgRisk, avgROI float64
	for _, p := range projects {
		avgRisk += p.risk
		avgROI += p.roi
	}
	writeFact("risk_score", map[string]string{}, avgRisk/float64(len(projects)), fatimaID)
	writeFact("roi_pct", map[string]string{}, avgROI/float64(len(projects)), fatimaID)
	printf("  %d projects → dept×qtr, dept, quarter, category and total slices\n", len(projects))
	printf("  TOTAL requested=%0.f approved=%0.f spent=%0.f committed=%0.f\n",
		tot["requested_amount"], tot["approved_amount"], tot["spent_amount"], tot["committed_amount"])

	// ── Calculation engine ─────────────────────────────────────────────────────

	section("Running calculation engine")

	calcStore := calculation.NewStore(pool)
	scheduler := calculation.NewScheduler(log, calcStore, nil)
	changed := []string{
		metrics["requested_amount"], metrics["approved_amount"],
		metrics["spent_amount"], metrics["committed_amount"],
	}
	must(scheduler.RecalcAffected(ctx, modelID, revisionID, changed), "recalc")

	remaining, err := calcStore.GetCalcValue(ctx, modelID, revisionID, metrics["remaining_budget"], map[string]string{})
	must(err, "get remaining_budget")
	expectedRemaining := tot["approved_amount"] - tot["spent_amount"] - tot["committed_amount"]
	if fmt.Sprintf("%.2f", remaining) == fmt.Sprintf("%.2f", expectedRemaining) {
		pass("remaining_budget total = %.0f despite facts stored at 6 granularities (finest-family evaluation)", remaining)
	} else {
		finding("calc total double-counts multi-granularity facts: remaining_budget=%.0f, true value %.0f — '{}' totals must evaluate over ONE fact granularity family, not sum across all of them", remaining, expectedRemaining)
	}
	util, err := calcStore.GetCalcValue(ctx, modelID, revisionID, metrics["utilization_pct"], map[string]string{})
	must(err, "get utilization_pct")
	expectedUtil := tot["spent_amount"] / tot["approved_amount"] * 100
	if fmt.Sprintf("%.2f", util) == fmt.Sprintf("%.2f", expectedUtil) {
		pass("utilization_pct total = %.1f%% (ratio evaluated on total-level inputs, not summed per slice)", util)
	} else {
		finding("ratio total wrong: utilization_pct=%.1f%%, want %.1f%% — agg_rule 'average' metrics must divide total by total", util, expectedUtil)
	}

	// ── Security: RACI, metric policies, per-user access rules ─────────────────

	section("Configuring security (RACI, metric policies, access rules)")

	_, err = pool.Exec(ctx, `
		INSERT INTO security.raci_rule (application_id, user_id, resource_pattern, raci_type)
		VALUES
		    ($1::uuid, $2::uuid, '*', 'responsible'),
		    ($1::uuid, $3::uuid, '*', 'accountable'),
		    ($1::uuid, $4::uuid, '*', 'consulted'),
		    ($1::uuid, $5::uuid, '*', 'informed')
	`, appID, davidID, fatimaID, lenaID, carlosID)
	must(err, "raci rules")

	for _, m := range []string{"requested_amount", "spent_amount", "committed_amount"} {
		_, err = pool.Exec(ctx, `
			INSERT INTO security.metric_policy (application_id, user_id, metric_id, can_read, can_write)
			VALUES ($1::uuid, $2::uuid, $3::uuid, true, true)
		`, appID, mariaID, metrics[m])
		must(err, "metric policy "+m)
	}
	// approved_amount is finance-owned: maria can read, not write.
	_, err = pool.Exec(ctx, `
		INSERT INTO security.metric_policy (application_id, user_id, metric_id, can_read, can_write)
		VALUES ($1::uuid, $2::uuid, $3::uuid, true, false)
	`, appID, mariaID, metrics["approved_amount"])
	must(err, "metric policy approved_amount")

	// Per-user overrides: risk_score hidden from maria, approved read-only.
	for _, r := range []struct{ refID, access string }{
		{metrics["risk_score"], "hidden"},
		{metrics["approved_amount"], "read"},
	} {
		_, err = pool.Exec(ctx, `
			INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access)
			VALUES ($1::uuid, 'metric', $2, $3)
			ON CONFLICT (user_id, rule_type, ref_id) DO UPDATE SET access = EXCLUDED.access
		`, mariaID, r.refID, r.access)
		must(err, "access rule")
	}
	printf("  RACI: david=R fatima=A lena=C carlos=I | maria: approved read-only, risk hidden\n")

	// ── Grids ──────────────────────────────────────────────────────────────────

	section("Creating grids")

	grid := func(name string, metricNames []string, dims []string) string {
		id := mustScan(pool.QueryRow(ctx, `
			INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, $3) RETURNING id::text
		`, modelID, revisionID, name), "grid "+name)
		for i, mn := range metricNames {
			_, err := pool.Exec(ctx, `
				INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid, $2::uuid, $3)
			`, id, metrics[mn], i)
			must(err, "grid metric "+mn)
		}
		for _, d := range dims {
			_, err := pool.Exec(ctx, `
				INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)
			`, id, d)
			must(err, "grid dimension")
		}
		printf("  Grid: %-28s (%s)\n", name, short(id))
		return id
	}
	// A metric may belong to only ONE grid (grid_metric_metric_id_key), and a
	// chart may only plot dimensions/metrics of its source grid — so the one
	// grid owning the money metrics must carry every dimension any chart needs.
	overviewGridID := grid("CapEx Overview",
		[]string{"requested_amount", "approved_amount", "spent_amount", "committed_amount", "remaining_budget"},
		[]string{deptDimID, qtrDimID, catDimID})
	projGridID := grid("Project Portfolio",
		[]string{"risk_score", "roi_pct", "utilization_pct", "approval_rate_pct"},
		[]string{projDimID})

	// Probe: can the same metric appear in a second grid? (developer consoles
	// commonly want e.g. spent_amount in both a dept grid and a quarter grid)
	var dupErr error
	_, dupErr = pool.Exec(ctx, `
		INSERT INTO model.grid_metric (grid_id, metric_id, sort_order)
		SELECT $1::uuid, $2::uuid, 0
	`, projGridID, metrics["spent_amount"])
	if dupErr != nil {
		finding("a metric can be a direct member of only ONE grid per revision (grid_metric_metric_id_key): %v — combined with charts only plotting their own grid's metrics/dimensions, every chartable slice forces extra dimensions onto that single grid; separate purpose-built grids per metric are impossible without rollup grids", dupErr)
	} else {
		pass("metric reused across two grids")
		_, _ = pool.Exec(ctx, `DELETE FROM model.grid_metric WHERE grid_id=$1::uuid AND metric_id=$2::uuid`, projGridID, metrics["spent_amount"])
	}

	// Rollup grid: mirrors the overview grid's metrics, rolled up by quarter.
	rollupGridID := mustScan(pool.QueryRow(ctx, `
		INSERT INTO model.grid_def (model_id, revision_id, name, rollup_source_grid_id)
		VALUES ($1::uuid, $2::uuid, 'Spend by Quarter (rollup)', $3::uuid) RETURNING id::text
	`, modelID, revisionID, overviewGridID), "rollup grid")
	_, err = pool.Exec(ctx, `INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, rollupGridID, qtrDimID)
	must(err, "rollup grid dimension")
	printf("  Grid: %-28s (%s, rollup of overview grid)\n", "Spend by Quarter (rollup)", short(rollupGridID))

	// ── Form + form→metric mappings ────────────────────────────────────────────

	section("Creating CapEx Request form + metric mappings")

	crudStore := crudapp.NewStore(pool)
	formDef, err := crudStore.CreateForm(ctx, modelID, revisionID, "capex_request", "CapEx Request",
		[]crudapp.FormField{
			{Name: "title", Label: "Request Title", Type: "text", Required: true},
			{Name: "department", Label: "Department", Type: "select", Required: true,
				Options: []string{"ENG", "IT", "SALES", "MKT", "HR", "FIN"}},
			{Name: "category", Label: "Category", Type: "select", Required: true,
				Options: []string{"BLD", "EQP", "SW", "VEH"}},
			{Name: "quarter", Label: "Target Quarter", Type: "select", Required: true,
				Options: []string{"Q1", "Q2", "Q3", "Q4"}},
			{Name: "amount", Label: "Amount (USD)", Type: "number", Required: true},
			{Name: "justification", Label: "Business Justification", Type: "text", Required: true},
			{Name: "needed_by", Label: "Needed By", Type: "date", Required: false},
			{Name: "is_urgent", Label: "Urgent", Type: "boolean", Required: false},
		})
	must(err, "create form")
	printf("  Form: CapEx Request (%s), 8 fields\n", short(formDef.ID))

	mapping := func(name, sourceField, targetMetric string, statuses []string) {
		dimMap, _ := json.Marshal(map[string]string{deptDimID: "department", qtrDimID: "quarter"})
		_, err := pool.Exec(ctx, `
			INSERT INTO model.form_metric_mapping
			    (model_id, form_id, name, source_field, target_metric_id, aggregation, posting_statuses, dimension_mappings, revision_id, grid_id)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5::uuid, 'sum', $6, $7::jsonb, $8::uuid, $9::uuid)
		`, modelID, formDef.ID, name, sourceField, metrics[targetMetric], statuses, dimMap, revisionID, overviewGridID)
		must(err, "mapping "+name)
		printf("  Mapping: %s → %s on %v\n", sourceField, targetMetric, statuses)
	}
	mapping("Post requested amount", "amount", "requested_amount", []string{"submitted", "approved"})
	mapping("Post approved amount", "amount", "approved_amount", []string{"approved"})

	// ── Workflows (designer-shaped steps, published) ───────────────────────────

	section("Creating workflows")

	type step map[string]any
	createWF := func(name, description, trigger, status, subjectType string, subjectConfig map[string]any, ctxSchema []map[string]any, steps []step) string {
		stepsJSON, _ := json.Marshal(steps)
		schemaJSON, _ := json.Marshal(ctxSchema)
		subjJSON, _ := json.Marshal(subjectConfig)
		var publishedAt *time.Time
		if status == "published" {
			now := time.Now()
			publishedAt = &now
		}
		id := mustScan(pool.QueryRow(ctx, `
			INSERT INTO workflow.workflow_def
			    (application_id, revision_id, name, description, trigger_event, steps, status, published_at, context_schema, subject_type, subject_config, created_by)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6::jsonb, $7, $8, $9::jsonb, $10, $11::jsonb, $12::uuid)
			RETURNING id::text
		`, appID, revisionID, name, description, trigger, stepsJSON, status, publishedAt, schemaJSON, subjectType, subjJSON, devID), "workflow "+name)
		printf("  WF [%s]: %-26s (%s)\n", status, name, short(id))
		return id
	}

	amountCtx := []map[string]any{
		{"key": "amount", "data_type": "Number", "required": true},
		{"key": "department", "data_type": "Dimension member", "required": false, "dimension_id": deptDimID},
		{"key": "record_id", "data_type": "Text", "required": false},
		{"key": "form_id", "data_type": "Text", "required": false},
	}

	wf1ID := createWF("CapEx Request Approval",
		"Dept approval, then finance + CFO for amounts over 50k",
		"form_submit", "published", "form_record", map[string]any{"form_id": formDef.ID}, amountCtx,
		[]step{
			{"id": "step-dept", "name": "Department Approval", "type": "approval",
				"instructions":   "Confirm the request is in the department plan.",
				"assignee_roles": []string{"business_user"}, "sla_hours": 24, "required_comment": true,
				"routes": map[string]string{"approve": "step-threshold", "reject": "step-notify-reject"}},
			{"id": "step-threshold", "name": "Amount Threshold", "type": "condition",
				"condition": map[string]any{"left": "amount", "operator": "greater_than", "right": 50000},
				"routes":    map[string]string{"true": "step-finance", "false": "step-notify-approved"}},
			{"id": "step-finance", "name": "Finance Review", "type": "approval",
				"assignee_roles": []string{"Finance Review"}, "sla_hours": 48,
				"routes": map[string]string{"approve": "step-cfo", "reject": "step-notify-reject"}},
			{"id": "step-cfo", "name": "CFO Sign-off", "type": "approval",
				"assignee_roles": []string{"Executives"}, "sla_hours": 72,
				"routes": map[string]string{"approve": "step-notify-approved", "reject": "step-notify-reject"}},
			{"id": "step-notify-approved", "name": "Notify Requester (Approved)", "type": "notification",
				"notification": map[string]string{"recipient_type": "requester", "subject": "CapEx request approved", "message": "Your capital request has been approved."},
				"routes":       map[string]string{"next": "end-completed"}},
			{"id": "step-notify-reject", "name": "Notify Requester (Rejected)", "type": "notification",
				"notification": map[string]string{"recipient_type": "requester", "subject": "CapEx request rejected", "message": "Your capital request was rejected."},
				"routes":       map[string]string{"next": "end-completed"}},
		})

	wf2ID := createWF("Compliance Dual Review",
		"After approval: finance and legal review in parallel, then notify execs",
		"form_approval", "published", "form_record", map[string]any{"form_id": formDef.ID}, nil,
		[]step{
			{"id": "step-kickoff", "name": "Compliance Kickoff", "type": "task",
				"assignee_roles": []string{"Finance Review"}, "sla_hours": 12,
				"routes": map[string]string{"branch-1": "step-fin-check", "branch-2": "step-legal-check"}},
			{"id": "step-fin-check", "name": "Finance Compliance Check", "type": "task",
				"assignee_roles": []string{"Finance Review"}, "sla_hours": 24,
				"routes": map[string]string{"next": "step-join"}},
			{"id": "step-legal-check", "name": "Legal Compliance Check", "type": "task",
				"assignee_roles": []string{"Legal Review"}, "sla_hours": 24,
				"routes": map[string]string{"next": "step-join"}},
			{"id": "step-join", "name": "Reviews Complete", "type": "join",
				"routes": map[string]string{"next": "step-notify-exec"}},
			{"id": "step-notify-exec", "name": "Notify Executives", "type": "notification",
				"notification": map[string]string{"recipient_type": "role", "recipient_role": "business_admin", "subject": "Compliance review complete", "message": "Both compliance checks passed."},
				"routes":       map[string]string{"next": "end-completed"}},
		})

	wf3ID := createWF("Threshold Diamond",
		"Condition splits into two branches that re-join before a final notification",
		"manual", "published", "general", nil,
		[]map[string]any{{"key": "amount", "data_type": "Number", "required": true}},
		[]step{
			{"id": "step-gate", "name": "Size Gate", "type": "condition",
				"condition": map[string]any{"left": "amount", "operator": "greater_than", "right": 100000},
				"routes":    map[string]string{"true": "step-big", "false": "step-small"}},
			{"id": "step-big", "name": "Major Investment Review", "type": "task",
				"assignee_roles": []string{"Finance Review"},
				"routes":         map[string]string{"next": "step-merge"}},
			{"id": "step-small", "name": "Fast-Track Review", "type": "task",
				"assignee_roles": []string{"CapEx Requesters"},
				"routes":         map[string]string{"next": "step-merge"}},
			{"id": "step-merge", "name": "Review Complete", "type": "join",
				"routes": map[string]string{"next": "step-final"}},
			{"id": "step-final", "name": "Notify Requester", "type": "notification",
				"notification": map[string]string{"recipient_type": "requester", "subject": "Portfolio review finished", "message": "Your portfolio review is complete."},
				"routes":       map[string]string{"next": "end-completed"}},
		})

	wf4ID := createWF("Budget Replan Review",
		"Fires when quarterly phasing cells change",
		"grid_change", "published", "grid_row", map[string]any{"grid_id": rollupGridID}, nil,
		[]step{
			{"id": "step-replan", "name": "Review Replanned Cells", "type": "task",
				"assignee_roles": []string{"Finance Review"}, "sla_hours": 24,
				"routes": map[string]string{"next": "step-replan-note"}},
			{"id": "step-replan-note", "name": "Notify Requester", "type": "notification",
				"notification": map[string]string{"recipient_type": "requester", "subject": "Replan noted", "message": "Quarterly replan was reviewed."},
				"routes":       map[string]string{"next": "end-completed"}},
		})

	// Deliberately left in draft — mimics workflows created through the legacy
	// seed/gateway path (CreateWorkflowDef never publishes).
	wf5ID := createWF("Draft Trap",
		"Draft workflow wired to an automation rule — should this be triggerable?",
		"manual", "draft", "general", nil, nil,
		[]step{
			{"id": "step-only", "name": "Solo Task", "type": "task",
				"assignee_roles": []string{"Finance Review"},
				"routes":         map[string]string{"next": "end-completed"}},
		})

	// ── Automation rules (all 5 trigger types) ─────────────────────────────────

	section("Creating automation rules")

	wfStore := workflow.NewStore(pool)
	rule := func(name, desc, trigger, wfName, wfID, srcForm, srcGrid string) string {
		r, err := wfStore.CreateAutomationRule(ctx, appID, revisionID, name, desc, trigger, wfName, wfID, srcForm, srcGrid, nil)
		must(err, "rule "+name)
		printf("  Rule: %-28s %-13s → %s\n", name, trigger, wfName)
		return r.ID
	}
	rule("Route CapEx Requests", "Start approval when a CapEx request is submitted",
		"form_submit", "CapEx Request Approval", wf1ID, formDef.ID, "")
	rule("Compliance After Approval", "Dual compliance review once a request is approved",
		"form_approval", "Compliance Dual Review", wf2ID, formDef.ID, "")
	replanRuleID := rule("Replan on Quarterly Change", "Review quarterly rollup grid writebacks",
		"grid_change", "Budget Replan Review", wf4ID, "", rollupGridID)
	replanOverviewRuleID := rule("Replan on Overview Change", "Review overview grid writebacks",
		"grid_change", "Budget Replan Review", wf4ID, "", overviewGridID)
	diamondRuleID := rule("Start Portfolio Review", "Manual portfolio review (dashboard button)",
		"manual", "Threshold Diamond", wf3ID, "", "")
	rule("API Portfolio Kickoff", "External systems can start a portfolio review",
		"api", "Threshold Diamond", wf3ID, "", "")

	// ── Dashboards ─────────────────────────────────────────────────────────────

	section("Creating dashboards")

	dashboard := func(name string, tags []string) string {
		id := mustScan(pool.QueryRow(ctx, `
			INSERT INTO model.dashboard_def (model_id, revision_id, name, tags)
			VALUES ($1::uuid, $2::uuid, $3, $4) RETURNING id::text
		`, modelID, revisionID, name, tags), "dashboard "+name)
		printf("  Dashboard: %-22s (%s)\n", name, short(id))
		return id
	}
	widget := func(dashID, wtype, refID, content, title string, props map[string]any, x, y, w, h, sort int) {
		var propsJSON any
		if props != nil {
			b, _ := json.Marshal(props)
			propsJSON = string(b)
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO model.dashboard_widget
			    (dashboard_id, widget_type, ref_id, content, title, show_title, widget_props, pos_x, pos_y, size_w, size_h, sort_order)
			VALUES ($1::uuid, $2, NULLIF($3,''), NULLIF($4,''), NULLIF($5,''), $5 <> '', $6::jsonb, $7, $8, $9, $10, $11)
		`, dashID, wtype, refID, content, title, propsJSON, x, y, w, h, sort)
		must(err, "widget "+wtype+" on "+dashID)
	}

	chart := func(ctype, dimID string, metricIDs []string, extra map[string]any) map[string]any {
		cfg := map[string]any{
			"chart_type": ctype, "dimension_id": dimID, "metric_ids": metricIDs,
			"context_defaults": map[string]string{}, "show_legend": true,
		}
		for k, v := range extra {
			cfg[k] = v
		}
		return map[string]any{"chart": cfg}
	}

	// 1 — Executive Overview: text banner, 3 KPI tiles, pie, bar, line.
	execDashID := dashboard("Executive Overview", []string{"executive", "capex"})
	widget(execDashID, "text", "", "**CapEx Portfolio FY2026** — board-approved capital plan. Figures update live from grids, forms and workflows.", "", nil, 0, 0, 1200, 70, 0)
	widget(execDashID, "metric_kpi", metrics["approved_amount"], "", "Total Approved", map[string]any{"font_size": 28, "color": "#4f46e5"}, 0, 90, 390, 140, 1)
	widget(execDashID, "metric_kpi", metrics["spent_amount"], "", "Total Spent", map[string]any{"font_size": 28, "color": "#0891b2"}, 405, 90, 390, 140, 2)
	widget(execDashID, "metric_kpi", metrics["remaining_budget"], "", "Remaining Budget", map[string]any{"font_size": 28, "color": "#059669"}, 810, 90, 390, 140, 3)
	widget(execDashID, "chart", overviewGridID, "", "Approved by Category",
		chart("pie", catDimID, []string{metrics["approved_amount"]}, map[string]any{"value_format": "currency", "show_values": true}), 0, 250, 590, 380, 4)
	widget(execDashID, "chart", overviewGridID, "", "Requested vs Approved vs Spent by Department",
		chart("bar", deptDimID, []string{metrics["requested_amount"], metrics["approved_amount"], metrics["spent_amount"]}, map[string]any{"value_format": "compact"}), 610, 250, 590, 380, 5)
	widget(execDashID, "chart", overviewGridID, "", "Quarterly Spend & Commitments",
		chart("line", qtrDimID, []string{metrics["spent_amount"], metrics["committed_amount"]}, map[string]any{"value_format": "compact"}), 0, 650, 1200, 360, 6)

	// 2 — Portfolio Analysis: scatter, histogram, portfolio grid.
	analysisDashID := dashboard("Portfolio Analysis", []string{"analysis", "capex"})
	widget(analysisDashID, "chart", projGridID, "", "Risk vs ROI by Project",
		chart("scatter", projDimID, nil, map[string]any{"x_metric_id": metrics["risk_score"], "y_metric_id": metrics["roi_pct"]}), 0, 0, 590, 380, 0)
	widget(analysisDashID, "chart", projGridID, "", "ROI Distribution Across Projects",
		chart("histogram", projDimID, []string{metrics["roi_pct"]}, map[string]any{"bin_count": 6, "value_format": "percent"}), 610, 0, 590, 380, 1)
	widget(analysisDashID, "grid", projGridID, "", "Project Portfolio", nil, 0, 400, 1200, 420, 2)

	// 3 — CapEx Requests: instructions, form, manual-trigger button, dept grid.
	requestsDashID := dashboard("CapEx Requests", []string{"operations"})
	widget(requestsDashID, "text", "", "Submit capital requests below. Requests over $50k route to Finance and the CFO automatically.", "", nil, 0, 0, 1200, 60, 0)
	widget(requestsDashID, "form", formDef.ID, "", "New CapEx Request", nil, 0, 80, 560, 540, 1)
	widget(requestsDashID, "automation_button", diamondRuleID, "Start Portfolio Review", "Portfolio Review",
		map[string]any{"button_color": "#4f46e5", "context": map[string]string{"amount": "40000"}}, 580, 80, 300, 100, 2)
	widget(requestsDashID, "grid", overviewGridID, "", "CapEx Overview", nil, 0, 640, 1200, 420, 3)

	// ── Business roles + dashboard visibility ──────────────────────────────────

	section("Creating business roles and dashboard assignments")

	bizRole := func(name string, memberIDs []string, dashIDs []string) string {
		id := mustScan(pool.QueryRow(ctx, `
			INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, $2)
			ON CONFLICT (workspace_id, name) DO UPDATE SET name = EXCLUDED.name
			RETURNING id::text
		`, workspaceID, name), "business role "+name)
		for _, uid := range memberIDs {
			_, err := pool.Exec(ctx, `
				INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)
				ON CONFLICT DO NOTHING
			`, id, uid)
			must(err, "role member")
		}
		for _, did := range dashIDs {
			_, err := pool.Exec(ctx, `
				INSERT INTO identity.business_role_dashboard (role_id, dashboard_id) VALUES ($1::uuid, $2::uuid)
				ON CONFLICT DO NOTHING
			`, id, did)
			must(err, "role dashboard")
		}
		printf("  Business role: %-18s members=%d dashboards=%d\n", name, len(memberIDs), len(dashIDs))
		return id
	}
	bizRole("CapEx Requesters", []string{mariaID}, []string{requestsDashID})
	bizRole("Finance Review", []string{fatimaID}, []string{execDashID, analysisDashID, requestsDashID})
	bizRole("Legal Review", []string{lenaID}, []string{analysisDashID})
	bizRole("Executives", []string{carlosID}, []string{execDashID})
	// david deliberately holds NO business role — used by the visibility probe.

	// ═══════════════════════════════════════════════════════════════════════════
	//  VERIFICATION PHASE — drive real instances through gateway-equivalent
	//  code paths and probe for inconsistencies.
	// ═══════════════════════════════════════════════════════════════════════════

	section("VERIFY 1 — full approval path (submit → dept → condition → finance → CFO)")

	recA, err := crudStore.CreateRecord(ctx, formDef.ID, mariaID, map[string]any{
		"title": "GPU Cluster Expansion", "department": "ENG", "category": "EQP", "quarter": "Q2",
		"amount": 120000, "justification": "Model-training capacity for FY2026 roadmap", "is_urgent": true,
	})
	must(err, "record A")
	must(crudStore.UpdateRecord(ctx, recA.ID, "submitted", map[string]any{
		"title": "GPU Cluster Expansion", "department": "ENG", "category": "EQP", "quarter": "Q2",
		"amount": 120000, "justification": "Model-training capacity for FY2026 roadmap", "is_urgent": true,
	}), "submit record A")
	// Dispatch exactly what the gateway dispatches on POST /records.
	wfStore.DispatchEventRules(ctx, appID, revisionID, "form_submit", formDef.ID, mariaID,
		map[string]string{"form_id": formDef.ID, "record_id": recA.ID, "status": "submitted"})
	time.Sleep(1500 * time.Millisecond)

	instA := latestInstance(ctx, pool, wf1ID)
	if instA == "" {
		finding("form_submit rule did not start a workflow instance (async dispatch failed silently)")
	} else {
		pass("form_submit rule started instance %s", short(instA))

		// Probe: the trigger payload must carry the record's field values.
		var ctxJSON string
		_ = pool.QueryRow(ctx, `SELECT context::text FROM workflow.workflow_instance WHERE id=$1::uuid`, instA).Scan(&ctxJSON)
		if strings.Contains(ctxJSON, "amount") {
			pass("form_submit payload enriched with record fields (amount present in context)")
		} else {
			finding("form_submit payload carries no form field values — a condition step on 'amount' has nothing to read (context=%s)", ctxJSON)
		}

		// SLA probe: pending steps must not have a due date yet — the SLA
		// clock starts at activation, not at instance start.
		var cfoDuePending *time.Time
		_ = pool.QueryRow(ctx, `
			SELECT due_at FROM workflow.workflow_step
			WHERE instance_id=$1::uuid AND step_def_id='step-cfo'
		`, instA).Scan(&cfoDuePending)
		if cfoDuePending == nil {
			pass("SLA clock deferred: pending CFO step has no due_at until activated")
		} else {
			finding("SLA due_at anchored at instance start: pending CFO step already has due_at=%s", cfoDuePending)
		}

		completeInProgress(ctx, pool, wfStore, instA, "step-dept", davidID, "approve", "In departmental plan; budget line CAP-ENG-07.")

		// Probe: the condition step must auto-evaluate (120000 > 50000 → true).
		condStatus := stepStatus(ctx, pool, instA, "step-threshold")
		var condDecision string
		_ = pool.QueryRow(ctx, `
			SELECT COALESCE(decision,'') FROM workflow.workflow_step
			WHERE instance_id=$1::uuid AND step_def_id='step-threshold'
		`, instA).Scan(&condDecision)
		if condStatus == "completed" && condDecision == "true" {
			pass("condition step auto-evaluated (decision=true), finance activated")
		} else {
			finding("condition step not auto-evaluated: status=%s decision=%s", condStatus, condDecision)
			completeInProgress(ctx, pool, wfStore, instA, "step-threshold", davidID, "true", "manual fallback")
		}

		// SLA probe: the finance step activated just now — due_at ≈ now+48h.
		var finDue *time.Time
		_ = pool.QueryRow(ctx, `
			SELECT due_at FROM workflow.workflow_step
			WHERE instance_id=$1::uuid AND step_def_id='step-finance'
		`, instA).Scan(&finDue)
		if finDue != nil && time.Until(*finDue) > 47*time.Hour {
			pass("finance step due_at set at activation (+48h)")
		} else {
			finding("finance step due_at not set at activation (due=%v)", finDue)
		}

		completeInProgress(ctx, pool, wfStore, instA, "step-finance", fatimaID, "approve", "Budget available.")
		completeInProgress(ctx, pool, wfStore, instA, "step-cfo", carlosID, "approve", "Strategic priority.")

		if instanceStatus(ctx, pool, instA) == "completed" {
			pass("instance A completed via dept → condition(auto) → finance → CFO")
		} else {
			finding("instance A did not complete; status=%s", instanceStatus(ctx, pool, instA))
		}
		var notifCount int
		_ = pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM notification.notification
			WHERE recipient_user_id=$1::uuid AND template_vars->>'subject' = 'CapEx request approved'
			  AND created_at >= $2
		`, mariaID, seedStart).Scan(&notifCount)
		if notifCount > 0 {
			pass("requester received in-app approval notification")
		} else {
			finding("approval notification to requester was not delivered")
		}
	}

	section("VERIFY 2 — drafts do not trigger; rejection path")

	recB, err := crudStore.CreateRecord(ctx, formDef.ID, mariaID, map[string]any{
		"title": "Espresso Machine", "department": "MKT", "category": "EQP", "quarter": "Q1",
		"amount": 8000, "justification": "Team morale", "is_urgent": false,
	})
	must(err, "record B")
	// Emulate the gateway's POST /records dispatch: status is still 'draft'.
	wfStore.DispatchEventRules(ctx, appID, revisionID, "form_submit", formDef.ID, mariaID,
		map[string]string{"form_id": formDef.ID, "record_id": recB.ID, "status": "draft"})
	time.Sleep(1500 * time.Millisecond)

	instB := latestInstance(ctx, pool, wf1ID)
	if instB == instA {
		pass("draft record did NOT trigger form_submit rules")
	} else {
		finding("form_submit rules fire for DRAFT records — dispatch does not gate on submitted status")
	}

	// Now actually submit it and drive the rejection path.
	dataB := map[string]any{
		"title": "Espresso Machine", "department": "MKT", "category": "EQP", "quarter": "Q1",
		"amount": 8000, "justification": "Team morale", "is_urgent": false,
	}
	must(crudStore.UpdateRecord(ctx, recB.ID, "submitted", dataB), "submit record B")
	wfStore.DispatchEventRules(ctx, appID, revisionID, "form_submit", formDef.ID, mariaID,
		map[string]string{"form_id": formDef.ID, "record_id": recB.ID, "status": "submitted"})
	time.Sleep(1500 * time.Millisecond)

	instB = latestInstance(ctx, pool, wf1ID)
	if instB != "" && instB != instA {
		completeInProgress(ctx, pool, wfStore, instB, "step-dept", davidID, "reject", "Not capital expenditure; use opex.")
		st := instanceStatus(ctx, pool, instB)
		var rejNotif int
		_ = pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM notification.notification
			WHERE recipient_user_id=$1::uuid AND template_vars->>'subject' = 'CapEx request rejected'
			  AND created_at >= $2
		`, mariaID, seedStart).Scan(&rejNotif)
		if st == "cancelled" && rejNotif > 0 {
			pass("rejection: notify-reject step ran, instance closed as %q (note: 'cancelled' even though the workflow terminated normally through its reject route)", st)
		} else {
			finding("rejection path: instance=%s reject-notifications=%d", st, rejNotif)
		}
	} else {
		finding("submitted record did not start an approval instance")
	}

	section("VERIFY 3 — condition false path (small amount skips finance)")

	recC, err := crudStore.CreateRecord(ctx, formDef.ID, mariaID, map[string]any{
		"title": "Standing Desks", "department": "IT", "category": "EQP", "quarter": "Q3",
		"amount": 30000, "justification": "Ergonomics refresh", "is_urgent": false,
	})
	must(err, "record C")
	wfStore.DispatchEventRules(ctx, appID, revisionID, "form_submit", formDef.ID, mariaID,
		map[string]string{"form_id": formDef.ID, "record_id": recC.ID, "status": "submitted"})
	time.Sleep(1500 * time.Millisecond)

	instC := latestInstance(ctx, pool, wf1ID)
	if instC != "" && instC != instB {
		completeInProgress(ctx, pool, wfStore, instC, "step-dept", davidID, "approve", "OK.")
		// 30000 <= 50000 → the condition should auto-route to notify-approved,
		// the sweep should skip finance/CFO, and the instance should complete
		// with no further human action.
		if instanceStatus(ctx, pool, instC) == "completed" && stepStatus(ctx, pool, instC, "step-finance") == "skipped" {
			pass("condition auto-routed false branch; finance/CFO skipped, instance completed")
		} else {
			finding("condition false path: instance=%s threshold=%s finance=%s",
				instanceStatus(ctx, pool, instC), stepStatus(ctx, pool, instC, "step-threshold"), stepStatus(ctx, pool, instC, "step-finance"))
		}
	}

	section("VERIFY 4 — condition → branches → JOIN diamond")

	execRec, err := wfStore.TriggerRule(ctx, diamondRuleID, mariaID, map[string]string{"amount": "40000"})
	must(err, "trigger diamond")
	instD := execRec.InstanceID
	if execRec.Status == "running" {
		pass("execution mirrors instance lifecycle (running while instance runs)")
	} else if instanceStatus(ctx, pool, instD) == "running" {
		finding("execution log marks a triggered run %q while its instance is still running", execRec.Status)
	}

	// The gate condition (40000 > 100000 → false) should auto-evaluate at
	// start, activate the fast-track branch and sweep the big-review branch.
	gateSt := stepStatus(ctx, pool, instD, "step-gate")
	bigSt := stepStatus(ctx, pool, instD, "step-big")
	if gateSt == "completed" && bigSt == "skipped" {
		pass("gate auto-evaluated at start; untaken branch swept to 'skipped'")
	} else {
		finding("diamond gate: gate=%s big-branch=%s (expected completed/skipped)", gateSt, bigSt)
		completeInProgress(ctx, pool, wfStore, instD, "step-gate", fatimaID, "false", "manual fallback")
	}
	completeInProgress(ctx, pool, wfStore, instD, "step-small", mariaID, "complete", "Fast-track done.")

	dSt := instanceStatus(ctx, pool, instD)
	joinSt := stepStatus(ctx, pool, instD, "step-merge")
	finalSt := stepStatus(ctx, pool, instD, "step-final")
	if finalSt == "completed" && joinSt == "completed" && dSt == "completed" {
		pass("diamond join fired after the taken branch; final notification ran")
	} else {
		finding("JOIN after a condition failed: instance=%s join=%s final=%s", dSt, joinSt, finalSt)
	}
	var diamondNotif int
	_ = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM notification.notification
		WHERE recipient_user_id=$1::uuid AND template_vars->>'subject'='Portfolio review finished'
		  AND created_at >= $2
	`, mariaID, seedStart).Scan(&diamondNotif)
	if diamondNotif > 0 {
		pass("diamond final notification delivered to requester")
	} else {
		finding("diamond final notification was not delivered")
	}
	var execStAfter string
	_ = pool.QueryRow(ctx, `SELECT status::text FROM workflow.execution WHERE instance_id=$1::uuid`, instD).Scan(&execStAfter)
	if execStAfter == "completed" {
		pass("execution row updated to 'completed' when the instance finished")
	} else {
		finding("execution row still %q after instance completion", execStAfter)
	}

	section("VERIFY 5 — unpublished workflows and failing rules")

	// Wiring a rule to a draft workflow must be rejected at save time.
	_, trapErr := wfStore.CreateAutomationRule(ctx, appID, revisionID,
		"Draft Trap Rule", "Points at a workflow that was never published",
		"manual", "Draft Trap", wf5ID, "", "", nil)
	if trapErr != nil {
		pass("rule creation rejected for unpublished workflow: %v", trapErr)
	} else {
		finding("rules can be created against unpublished workflows and fail only at fire time")
	}

	// A rule that resolves its workflow by NAME can still dangle — firing it
	// must leave a 'failed' execution row instead of vanishing silently.
	ghostRule, err := wfStore.CreateAutomationRule(ctx, appID, revisionID,
		"Ghost Rule", "Name-based reference to a workflow that does not exist",
		"manual", "Ghost Workflow", "", "", "", nil)
	must(err, "ghost rule")
	if _, ghostErr := wfStore.TriggerRule(ctx, ghostRule.ID, fatimaID, nil); ghostErr != nil {
		var failedExecs int
		_ = pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM workflow.execution WHERE rule_id=$1::uuid AND status='failed'
		`, ghostRule.ID).Scan(&failedExecs)
		if failedExecs > 0 {
			pass("failed fire recorded in execution log (status='failed', error captured)")
		} else {
			finding("failed rule fire left no execution row — silent no-op (%v)", ghostErr)
		}
	} else {
		finding("ghost rule fired successfully despite missing workflow")
	}

	section("VERIFY 6 — grid_change source scoping")

	// Emulate the FIXED gateway cell-writeback dispatch: the changed metric's
	// owning grid (spent_amount → CapEx Overview) is passed as the source.
	writeFact("spent_amount", map[string]string{deptDimID: "ENG", qtrDimID: "Q2"}, 395000, davidID)
	var owningGridID string
	_ = pool.QueryRow(ctx, `SELECT grid_id::text FROM model.grid_metric WHERE metric_id=$1::uuid LIMIT 1`, metrics["spent_amount"]).Scan(&owningGridID)
	wfStore.DispatchEventRules(ctx, appID, revisionID, "grid_change", owningGridID, davidID,
		map[string]string{"model_id": modelID, "revision_id": revisionID, "metric_id": metrics["spent_amount"], "grid_id": owningGridID})
	time.Sleep(1500 * time.Millisecond)

	var rollupScopedExecs, overviewScopedExecs int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM workflow.execution WHERE rule_id=$1::uuid`, replanRuleID).Scan(&rollupScopedExecs)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM workflow.execution WHERE rule_id=$1::uuid`, replanOverviewRuleID).Scan(&overviewScopedExecs)
	if overviewScopedExecs > 0 && rollupScopedExecs == 0 {
		pass("source scoping honored: overview-scoped rule fired, rollup-scoped rule did not")
	} else {
		finding("grid_change scoping wrong: overview-scoped fired %d× (want ≥1), rollup-scoped fired %d× (want 0)", overviewScopedExecs, rollupScopedExecs)
	}

	// An event with NO source must match only unscoped rules.
	wfStore.DispatchEventRules(ctx, appID, revisionID, "grid_change", "", davidID,
		map[string]string{"model_id": modelID, "revision_id": revisionID, "metric_id": metrics["spent_amount"]})
	time.Sleep(1500 * time.Millisecond)
	var rollupAfter, overviewAfter int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM workflow.execution WHERE rule_id=$1::uuid`, replanRuleID).Scan(&rollupAfter)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM workflow.execution WHERE rule_id=$1::uuid`, replanOverviewRuleID).Scan(&overviewAfter)
	if rollupAfter == rollupScopedExecs && overviewAfter == overviewScopedExecs {
		pass("sourceless event matched no grid-scoped rules")
	} else {
		finding("sourceless grid_change event fired grid-scoped rules (rollup %d→%d, overview %d→%d)", rollupScopedExecs, rollupAfter, overviewScopedExecs, overviewAfter)
	}

	section("VERIFY 7 — form_approval → parallel branches + join")

	dataA := map[string]any{
		"title": "GPU Cluster Expansion", "department": "ENG", "category": "EQP", "quarter": "Q2",
		"amount": 120000, "justification": "Model-training capacity for FY2026 roadmap", "is_urgent": true,
	}
	must(crudStore.UpdateRecord(ctx, recA.ID, "approved", dataA), "approve record A")
	wfStore.DispatchEventRules(ctx, appID, revisionID, "form_approval", formDef.ID, fatimaID,
		map[string]string{"form_id": formDef.ID, "record_id": recA.ID, "status": "approved"})
	time.Sleep(1500 * time.Millisecond)

	instE := latestInstance(ctx, pool, wf2ID)
	if instE == "" {
		finding("form_approval rule did not start an instance")
	} else {
		completeInProgress(ctx, pool, wfStore, instE, "step-kickoff", fatimaID, "complete", "Kicking off both checks.")
		finSt := stepStatus(ctx, pool, instE, "step-fin-check")
		legSt := stepStatus(ctx, pool, instE, "step-legal-check")
		if finSt == "in_progress" && legSt == "in_progress" {
			pass("task fan-out activated both parallel branches")
		} else {
			finding("parallel fan-out: fin=%s legal=%s", finSt, legSt)
		}
		completeInProgress(ctx, pool, wfStore, instE, "step-fin-check", fatimaID, "complete", "Clean.")
		if stepStatus(ctx, pool, instE, "step-join") == "pending" {
			pass("join waited for the second branch")
		}
		completeInProgress(ctx, pool, wfStore, instE, "step-legal-check", lenaID, "complete", "No contract issues.")
		if instanceStatus(ctx, pool, instE) == "completed" && stepStatus(ctx, pool, instE, "step-join") == "completed" {
			pass("join fired after both branches completed (all-predecessors-active case works)")
		} else {
			finding("parallel join: instance=%s join=%s", instanceStatus(ctx, pool, instE), stepStatus(ctx, pool, instE, "step-join"))
		}

		// Same-tenant business_admins must receive the role notification…
		notifCountFor := func(uid string) int {
			var n int
			_ = pool.QueryRow(ctx, `
				SELECT COUNT(*) FROM notification.notification
				WHERE recipient_user_id=$1::uuid AND template_vars->>'subject'='Compliance review complete'
				  AND created_at >= $2
			`, uid, seedStart).Scan(&n)
			return n
		}
		if notifCountFor(fatimaID) > 0 && notifCountFor(carlosID) > 0 {
			pass("same-tenant business_admins (fatima, carlos) received the role notification")
		} else {
			finding("role notification over-restricted: fatima=%d carlos=%d (same tenant, should receive)", notifCountFor(fatimaID), notifCountFor(carlosID))
		}
		// …while another tenant's business_admin must not.
		if notifCountFor(eveID) > 0 {
			finding("CROSS-TENANT LEAK: eve@othercorp (business_admin of a different customer) received this tenant's 'Compliance review complete' notification")
		} else {
			pass("role notification did not leak to other tenants")
		}
	}

	section("VERIFY 8 — cross-workspace task visibility (/api/tasks role match)")

	// Leave a dept-approval step open, then evaluate the /api/tasks query as eve.
	recH, err := crudStore.CreateRecord(ctx, formDef.ID, mariaID, map[string]any{
		"title": "Showroom Fit-out", "department": "SALES", "category": "BLD", "quarter": "Q4",
		"amount": 95000, "justification": "Flagship showroom", "is_urgent": false,
	})
	must(err, "record H")
	wfStore.DispatchEventRules(ctx, appID, revisionID, "form_submit", formDef.ID, mariaID,
		map[string]string{"form_id": formDef.ID, "record_id": recH.ID, "status": "submitted"})
	time.Sleep(1500 * time.Millisecond)

	// Mirrors the /api/tasks platform-role clause (incl. its workspace scoping).
	tasksVisibleTo := func(uid string) int {
		var n int
		_ = pool.QueryRow(ctx, `
			SELECT COUNT(*)
			FROM workflow.workflow_step ws
			JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
			JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
			CROSS JOIN LATERAL (
				SELECT elem FROM jsonb_array_elements(wd.steps) AS elem
				WHERE elem->>'id' = ws.step_def_id LIMIT 1
			) step_def
			WHERE ws.status = 'in_progress'
			  AND wd.application_id = $2::uuid
			  AND EXISTS (
			      SELECT 1
			      FROM identity.role_assignment ra
			      LEFT JOIN core.workspace rws ON rws.id = ra.workspace_id
			      JOIN core.application app ON app.id = wd.application_id
			      WHERE ra.user_id = $1::uuid
			        AND ra.role::text IN (SELECT jsonb_array_elements_text(step_def.elem->'assignee_roles'))
			        AND (ra.workspace_id IS NULL
			             OR app.workspace_id = rws.id
			             OR app.customer_id = rws.customer_id)
			  )
		`, uid, appID).Scan(&n)
		return n
	}
	if tasksVisibleTo(davidID) > 0 {
		pass("same-workspace business_user (david) sees the open approval task")
	} else {
		finding("task visibility over-restricted: david (same workspace) sees no tasks")
	}
	if eveVisible := tasksVisibleTo(eveID); eveVisible > 0 {
		finding("CROSS-TENANT LEAK: eve@othercorp (business_user of another customer) sees %d open approval task(s) of this tenant", eveVisible)
	} else {
		pass("open tasks are not visible to other-tenant users")
	}

	section("VERIFY 9 — business-role dashboard gating fallback")

	// Mirrors /api/dashboards' role filter (workspace-level fallback).
	dashCount := func(uid string) int {
		var n int
		_ = pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM model.dashboard_def dd
			WHERE dd.model_id=$1::uuid AND dd.revision_id=$2::uuid
			  AND (
			      EXISTS (
			          SELECT 1 FROM identity.business_role_member brm
			          JOIN identity.business_role_dashboard brd ON brd.role_id = brm.role_id
			          WHERE brm.user_id = $3::uuid AND brd.dashboard_id = dd.id
			      )
			      OR NOT EXISTS (
			          SELECT 1 FROM identity.business_role br
			          JOIN core.application app ON app.workspace_id = br.workspace_id
			          JOIN core.model m ON m.application_id = app.id
			          WHERE m.id = dd.model_id
			      )
			  )
		`, modelID, revisionID, uid).Scan(&n)
		return n
	}
	mariaDash, davidDash := dashCount(mariaID), dashCount(davidID)
	if mariaDash == 1 && davidDash == 0 {
		pass("least-privilege gating: maria (role member) sees %d, david (no role, workspace HAS roles) sees %d", mariaDash, davidDash)
	} else if davidDash > mariaDash {
		finding("dashboard gating inverted: maria (role member) sees %d, david (no role) sees %d", mariaDash, davidDash)
	} else {
		finding("dashboard gating unexpected: maria=%d david=%d", mariaDash, davidDash)
	}

	// Give david a proper membership now that the probe is done, so his
	// console isn't empty when exploring the demo.
	var requestersRoleID string
	_ = pool.QueryRow(ctx, `SELECT id::text FROM identity.business_role WHERE workspace_id=$1::uuid AND name='CapEx Requesters'`, workspaceID).Scan(&requestersRoleID)
	if requestersRoleID != "" {
		_, _ = pool.Exec(ctx, `
			INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)
			ON CONFLICT DO NOTHING
		`, requestersRoleID, davidID)
	}

	section("VERIFY 10 — Developer Console app visibility (workspace-scoped app)")

	// This application was created with only workspace_id set — core.application
	// also carries an independent customer_id for customer-wide apps, and
	// /api/developer/applications must resolve tenant ownership through EITHER
	// column (mirrors the fixed query in internal/gateway/handler.go's
	// adminTenants). Regression guard for a real bug found via manual UI
	// testing: matching customer_id alone made every workspace-scoped app
	// (this one, and the pre-existing Procurement demo) invisible in the
	// Developer Console for every role, including platform_admin.
	var appCustomerID *string
	_ = pool.QueryRow(ctx, `SELECT customer_id::text FROM core.application WHERE id=$1::uuid`, appID).Scan(&appCustomerID)
	if appCustomerID != nil {
		finding("regression guard invalid: CapEx application unexpectedly has customer_id set — VERIFY 10 needs a workspace_id-only app to test the fixed code path")
	} else {
		developerSeesApp := func(uid string) bool {
			var n int
			_ = pool.QueryRow(ctx, `
				SELECT COUNT(DISTINCT a.id) FROM core.application a
				LEFT JOIN core.workspace aw ON aw.id = a.workspace_id
				WHERE a.id = $2::uuid
				  AND (a.customer_id=$1::uuid OR aw.customer_id=$1::uuid)
				  AND EXISTS (
				      SELECT 1 FROM identity.role_assignment ra
				      JOIN core.workspace rw ON rw.id = ra.workspace_id
				      WHERE ra.user_id=$3::uuid AND rw.customer_id = $1::uuid
				  )
			`, customerID, appID, uid).Scan(&n)
			return n > 0
		}
		if developerSeesApp(devID) {
			pass("workspace-scoped app resolves to its tenant for a developer holding a role in a sibling workspace")
		} else {
			finding("workspace-scoped app (customer_id NULL) is invisible to its own tenant's developer")
		}
	}

	// ── Audit trail ────────────────────────────────────────────────────────────

	section("Seeding audit events")

	audits := [][6]string{
		{"model_change", "workflow.published", devID, "developer", "workflow_def", wf1ID},
		{"model_change", "workflow.published", devID, "developer", "workflow_def", wf2ID},
		{"model_change", "form.created", devID, "developer", "form_def", formDef.ID},
		{"model_change", "dashboard.created", devID, "developer", "dashboard_def", execDashID},
		{"data_change", "capex_request.created", mariaID, "business_user", "form_record", recA.ID},
		{"data_change", "capex_request.approved", carlosID, "business_admin", "form_record", recA.ID},
		{"data_change", "capex_request.rejected", davidID, "business_user", "form_record", recB.ID},
		{"data_change", "cell.written", davidID, "business_user", "fact_input", metrics["spent_amount"]},
	}
	for _, a := range audits {
		_, err = pool.Exec(ctx, `
			INSERT INTO audit.audit_event (category, event_type, actor_user_id, actor_role, workspace_id, resource_type, resource_id, metadata)
			VALUES ($1::audit.event_category, $2, $3::uuid, $4, $5::uuid, $6, $7, '{}'::jsonb)
		`, a[0], a[1], a[2], a[3], workspaceID, a[4], a[5])
		must(err, "audit "+a[1])
	}
	printf("  %d audit events\n", len(audits))

	// ── Summary ────────────────────────────────────────────────────────────────

	section("Demo ready — summary")
	printf("  Application : CapEx Portfolio FY2026 (%s)\n", short(appID))
	printf("  Model/Rev   : %s / %s (%s)\n", short(modelID), revisionName, short(revisionID))
	printf("  Dimensions  : department(9, 2-level), category(4), quarter(4), project(10)\n")
	printf("  Metrics     : 6 input + 3 calculated (currency/percent formats, sum/avg agg)\n")
	printf("  Grids       : 3 (incl. 1 rollup) | Dashboards: 3 (14 widgets, 5 chart types + KPIs)\n")
	printf("  Workflows   : 5 (approval+condition, parallel+join, diamond, grid-replan, draft)\n")
	printf("  Rules       : 6 across all 5 trigger types | Business roles: 4\n")
	printf("  Users       : maria/david/fatima/carlos/lena/dev + eve (probe tenant)\n")
	fmt.Println()

	section(fmt.Sprintf("VERIFICATION RECAP — %d finding(s)", len(findings)))
	for i, f := range findings {
		printf("  %2d. %s\n", i+1, f)
	}
	fmt.Println()
}

// ── verification helpers ───────────────────────────────────────────────────────

func latestInstance(ctx context.Context, pool *pgxpool.Pool, defID string) string {
	var id string
	_ = pool.QueryRow(ctx, `
		SELECT id::text FROM workflow.workflow_instance
		WHERE workflow_def_id=$1::uuid ORDER BY started_at DESC LIMIT 1
	`, defID).Scan(&id)
	return id
}

func instanceStatus(ctx context.Context, pool *pgxpool.Pool, instID string) string {
	var s string
	_ = pool.QueryRow(ctx, `SELECT status::text FROM workflow.workflow_instance WHERE id=$1::uuid`, instID).Scan(&s)
	return s
}

func stepStatus(ctx context.Context, pool *pgxpool.Pool, instID, stepDefID string) string {
	var s string
	_ = pool.QueryRow(ctx, `
		SELECT status::text FROM workflow.workflow_step WHERE instance_id=$1::uuid AND step_def_id=$2
	`, instID, stepDefID).Scan(&s)
	return s
}

// completeInProgress completes the named step if it is currently actionable.
func completeInProgress(ctx context.Context, pool *pgxpool.Pool, wfStore *workflow.Store, instID, stepDefID, userID, decision, comment string) {
	var stepID string
	err := pool.QueryRow(ctx, `
		SELECT id::text FROM workflow.workflow_step
		WHERE instance_id=$1::uuid AND step_def_id=$2 AND status IN ('in_progress','pending')
	`, instID, stepDefID).Scan(&stepID)
	if err != nil {
		finding("step %s not actionable on instance %s", stepDefID, short(instID))
		return
	}
	if _, err := wfStore.CompleteStep(ctx, stepID, userID, decision, comment); err != nil {
		finding("complete %s: %v", stepDefID, err)
	}
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

func mustScan(row interface{ Scan(dest ...any) error }, context string) string {
	var id string
	if err := row.Scan(&id); err != nil {
		fmt.Fprintf(os.Stderr, "\n\033[31mERROR [%s]: %v\033[0m\n", context, err)
		os.Exit(1)
	}
	return id
}
