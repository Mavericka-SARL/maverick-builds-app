package calculation_test

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

func insertMetric(t *testing.T, store *calculation.Store, ctx context.Context, modelID, revisionID, name, f string, isInput bool) string {
	t.Helper()
	var id string
	var fp *string
	if f != "" {
		fp = &f
	}
	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, revision_id, name, formula, is_input)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5) RETURNING id::text
	`, modelID, revisionID, name, fp, isInput).Scan(&id); err != nil {
		t.Fatalf("insert metric %s: %v", name, err)
	}
	return id
}

// insertMember creates a real model.dimension_member row (needed so
// LoadAllDimensions/rollup.LeafCombos have real config to enumerate,
// instead of the old fact-driven combo discovery that only needed a code
// literal embedded in fact_input's own JSON).
func insertMember(t *testing.T, store *calculation.Store, ctx context.Context, dimensionID, code, label string) string {
	t.Helper()
	var id string
	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, $2, $3) RETURNING id::text
	`, dimensionID, code, label).Scan(&id); err != nil {
		t.Fatalf("insert member %s: %v", code, err)
	}
	return id
}

// insertGridSetup creates one grid_def, a grid_dimension row per dimID, and
// a grid_metric row per metricID (inputs and the calc metric alike) — so a
// metric's own declared dimensions (LoadMetricDimensionIDs) resolve to
// something real. Mirrors internal/query/chart_test.go's
// TestResolveCalcMetricRollsUpThroughRollupPackage.
func insertGridSetup(t *testing.T, store *calculation.Store, ctx context.Context, modelID string, dimIDs, metricIDs []string) string {
	t.Helper()
	var gridID string
	if err := store.Pool().QueryRow(ctx,
		`INSERT INTO model.grid_def (model_id, name) VALUES ($1::uuid, 'test grid') RETURNING id::text`, modelID,
	).Scan(&gridID); err != nil {
		t.Fatalf("insert grid: %v", err)
	}
	for _, dimID := range dimIDs {
		if _, err := store.Pool().Exec(ctx,
			`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, gridID, dimID,
		); err != nil {
			t.Fatalf("insert grid_dimension: %v", err)
		}
	}
	for _, metricID := range metricIDs {
		if _, err := store.Pool().Exec(ctx,
			`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, gridID, metricID,
		); err != nil {
			t.Fatalf("insert grid_metric: %v", err)
		}
	}
	return gridID
}

func insertDep(t *testing.T, store *calculation.Store, ctx context.Context, metricID, depID string) {
	t.Helper()
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id)
		VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING
	`, metricID, depID); err != nil {
		t.Fatalf("insert dep: %v", err)
	}
}

func insertFact(t *testing.T, store *calculation.Store, ctx context.Context, modelID, revID, metricID, dimMembers string, value float64) {
	t.Helper()
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5)
	`, modelID, revID, metricID, dimMembers, value); err != nil {
		t.Fatalf("insert fact: %v", err)
	}
}

func runRecalc(t *testing.T, store *calculation.Store, ctx context.Context, modelID, revID string, inputIDs []string) {
	t.Helper()
	sched := calculation.NewScheduler(logger.New("calc-test"), store, nil)
	if err := sched.RecalcAffected(ctx, modelID, revID, inputIDs); err != nil {
		t.Fatalf("RecalcAffected: %v", err)
	}
}

func assertCalc(t *testing.T, store *calculation.Store, ctx context.Context, modelID, revID, metricID string, dims map[string]string, want float64) {
	t.Helper()
	got, err := store.GetCalcValue(ctx, modelID, revID, metricID, dims)
	if err != nil {
		t.Fatalf("GetCalcValue: %v", err)
	}
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("got %v, want %v", got, want)
	}
}

func insertDim(t *testing.T, store *calculation.Store, ctx context.Context, modelID, name string) string {
	t.Helper()
	var id string
	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO model.dimension_def (model_id, name) VALUES ($1::uuid, $2) RETURNING id::text
	`, modelID, name).Scan(&id); err != nil {
		t.Fatalf("insert dim %s: %v", name, err)
	}
	return id
}

// TestDimensionConditionalFormula: =IF(department="SALES", a+b, 0) sums only
// the SALES partition across all dimension combos.
func TestDimensionConditionalFormula(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const (
		model = "00000000-0000-0000-0000-000000000010"
		rev   = "00000000-0000-0000-0010-000000000010"
	)

	dimID := insertDim(t, store, ctx, model, "department")
	insertMember(t, store, ctx, dimID, "SALES", "Sales")
	insertMember(t, store, ctx, dimID, "ENG", "Engineering")
	hcID := insertMetric(t, store, ctx, model, rev, "hc_cost", "", true)
	swID := insertMetric(t, store, ctx, model, rev, "sw_cost", "", true)
	salesOnlyID := insertMetric(t, store, ctx, model, rev, "sales_only",
		`=IF(department = "SALES", hc_cost + sw_cost, 0)`, false)
	insertDep(t, store, ctx, salesOnlyID, hcID)
	insertDep(t, store, ctx, salesOnlyID, swID)
	insertGridSetup(t, store, ctx, model, []string{dimID}, []string{hcID, swID, salesOnlyID})

	sales := `{"` + dimID + `": "SALES"}`
	eng := `{"` + dimID + `": "ENG"}`
	insertFact(t, store, ctx, model, rev, hcID, sales, 40000)
	insertFact(t, store, ctx, model, rev, swID, sales, 10000)
	insertFact(t, store, ctx, model, rev, hcID, eng, 80000)
	insertFact(t, store, ctx, model, rev, swID, eng, 5000)

	runRecalc(t, store, ctx, model, rev, []string{hcID, swID})

	// SALES: 40000+10000=50000; ENG: 0 → aggregate = 50000
	assertCalc(t, store, ctx, model, rev, salesOnlyID, map[string]string{}, 50000)
}

// TestNestedIF: =IF(dept="SALES", base*1.2, IF(dept="ENG", base*0.9, base))
func TestNestedIF(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const (
		model = "00000000-0000-0000-0000-000000000011"
		rev   = "00000000-0000-0000-0011-000000000011"
	)

	dimID := insertDim(t, store, ctx, model, "department")
	insertMember(t, store, ctx, dimID, "SALES", "Sales")
	insertMember(t, store, ctx, dimID, "ENG", "Engineering")
	insertMember(t, store, ctx, dimID, "GA", "General & Admin")
	baseID := insertMetric(t, store, ctx, model, rev, "base", "", true)
	rateID := insertMetric(t, store, ctx, model, rev, "rate",
		`=IF(department = "SALES", base * 1.2, IF(department = "ENG", base * 0.9, base))`, false)
	insertDep(t, store, ctx, rateID, baseID)
	insertGridSetup(t, store, ctx, model, []string{dimID}, []string{baseID, rateID})

	for _, row := range []struct {
		dept string
		val  float64
	}{
		{"SALES", 100}, // → 120
		{"ENG", 100},   // → 90
		{"GA", 100},    // → 100
	} {
		insertFact(t, store, ctx, model, rev, baseID, `{"`+dimID+`": "`+row.dept+`"}`, row.val)
	}

	runRecalc(t, store, ctx, model, rev, []string{baseID})
	// 120 + 90 + 100 = 310
	assertCalc(t, store, ctx, model, rev, rateID, map[string]string{}, 310)
}

// TestNumericThresholdPerDim: =IF(cost > 50000, cost * 0.1, 0)
func TestNumericThresholdPerDim(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const (
		model = "00000000-0000-0000-0000-000000000012"
		rev   = "00000000-0000-0000-0012-000000000012"
	)

	dimID := insertDim(t, store, ctx, model, "department")
	insertMember(t, store, ctx, dimID, "A", "A")
	insertMember(t, store, ctx, dimID, "B", "B")
	costID := insertMetric(t, store, ctx, model, rev, "cost", "", true)
	bonusID := insertMetric(t, store, ctx, model, rev, "bonus", `=IF(cost > 50000, cost * 0.1, 0)`, false)
	insertDep(t, store, ctx, bonusID, costID)
	insertGridSetup(t, store, ctx, model, []string{dimID}, []string{costID, bonusID})

	insertFact(t, store, ctx, model, rev, costID, `{"`+dimID+`": "A"}`, 80000) // → 8000
	insertFact(t, store, ctx, model, rev, costID, `{"`+dimID+`": "B"}`, 30000) // → 0

	runRecalc(t, store, ctx, model, rev, []string{costID})
	assertCalc(t, store, ctx, model, rev, bonusID, map[string]string{}, 8000)
}

// TestMathFunctionsInFormula: ABS, ROUND, IFERROR in calc metrics.
func TestMathFunctionsInFormula(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const (
		model = "00000000-0000-0000-0000-000000000013"
		rev   = "00000000-0000-0000-0013-000000000013"
	)

	aID := insertMetric(t, store, ctx, model, rev, "a", "", true)
	bID := insertMetric(t, store, ctx, model, rev, "b", "", true)
	absVarID := insertMetric(t, store, ctx, model, rev, "abs_var", `=ABS(a - b)`, false)
	safeRatioID := insertMetric(t, store, ctx, model, rev, "safe_ratio", `=IFERROR(ROUND(a / b * 100, 1), 0)`, false)
	insertDep(t, store, ctx, absVarID, aID)
	insertDep(t, store, ctx, absVarID, bID)
	insertDep(t, store, ctx, safeRatioID, aID)
	insertDep(t, store, ctx, safeRatioID, bID)

	insertFact(t, store, ctx, model, rev, aID, "{}", 80)
	insertFact(t, store, ctx, model, rev, bID, "{}", 100)

	runRecalc(t, store, ctx, model, rev, []string{aID, bID})
	assertCalc(t, store, ctx, model, rev, absVarID, map[string]string{}, 20)    // |80-100|
	assertCalc(t, store, ctx, model, rev, safeRatioID, map[string]string{}, 80) // ROUND(80/100*100,1)
}

// TestCalcMetricDependsOnCalcMetricPerIntersection is the core regression
// guard for this change: net depends on tax, itself a calculated metric —
// before per-intersection persistence, evalOne's GetCalcValue(tax, combo)
// lookup always missed (calc_result held only the '{}' aggregate row) and
// silently resolved to 0 at every combo, since a miss there was ok=false
// but treated as a plain 0, not an error. With real per-combo tax rows now
// persisted (tax is topologically evaluated before net, so by the time
// net's own executePartition runs, tax already has real per-department
// rows), net must resolve to the correct, non-zero, per-department value.
func TestCalcMetricDependsOnCalcMetricPerIntersection(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const (
		model = "00000000-0000-0000-0000-000000000014"
		rev   = "00000000-0000-0000-0014-000000000014"
	)

	dimID := insertDim(t, store, ctx, model, "department")
	insertMember(t, store, ctx, dimID, "SALES", "Sales")
	insertMember(t, store, ctx, dimID, "ENG", "Engineering")
	revenueID := insertMetric(t, store, ctx, model, rev, "revenue", "", true)
	taxID := insertMetric(t, store, ctx, model, rev, "tax", `=revenue * 0.1`, false)
	netID := insertMetric(t, store, ctx, model, rev, "net", `=revenue - tax`, false)
	insertDep(t, store, ctx, taxID, revenueID)
	insertDep(t, store, ctx, netID, revenueID)
	insertDep(t, store, ctx, netID, taxID)
	insertGridSetup(t, store, ctx, model, []string{dimID}, []string{revenueID, taxID, netID})

	insertFact(t, store, ctx, model, rev, revenueID, `{"`+dimID+`": "SALES"}`, 1000)
	insertFact(t, store, ctx, model, rev, revenueID, `{"`+dimID+`": "ENG"}`, 2000)

	runRecalc(t, store, ctx, model, rev, []string{revenueID})

	assertCalc(t, store, ctx, model, rev, netID, map[string]string{dimID: "SALES"}, 900) // 1000 - 100
	assertCalc(t, store, ctx, model, rev, netID, map[string]string{dimID: "ENG"}, 1800)  // 2000 - 200
	assertCalc(t, store, ctx, model, rev, netID, map[string]string{}, 2700)              // aggregate: 900+1800
}

// ── Form-field formula unit tests (no DB needed) ──────────────────────────────

func TestFormFieldFormulas(t *testing.T) {
	cases := []struct {
		name    string
		formula string
		vars    map[string]formula.Value
		want    float64
	}{
		{
			"net_amount",
			`=quantity * unit_price * (1 - discount_pct)`,
			map[string]formula.Value{
				"quantity":     formula.NumberVal(10),
				"unit_price":   formula.NumberVal(25),
				"discount_pct": formula.NumberVal(0.1),
			},
			225,
		},
		{
			"approval_flag",
			`=IF(net_amount > 10000, 1, 0)`,
			map[string]formula.Value{"net_amount": formula.NumberVal(15000)},
			1,
		},
		{
			"tiered_commission",
			`=IF(amount > 100000, amount * 0.05, IF(amount > 50000, amount * 0.03, amount * 0.01))`,
			map[string]formula.Value{"amount": formula.NumberVal(75000)},
			2250, // 75000 * 0.03
		},
		{
			"abs_variance",
			`=ABS(actual - budget)`,
			map[string]formula.Value{"actual": formula.NumberVal(80), "budget": formula.NumberVal(100)},
			20,
		},
		{
			"rounded_margin_pct",
			`=ROUND(profit / revenue * 100, 1)`,
			map[string]formula.Value{"profit": formula.NumberVal(123), "revenue": formula.NumberVal(1000)},
			12.3,
		},
		{
			"iferror_zero_div",
			`=IFERROR(profit / revenue, 0)`,
			map[string]formula.Value{"profit": formula.NumberVal(50), "revenue": formula.NumberVal(0)},
			0,
		},
		{
			"and_condition",
			`=IF(AND(qty > 0, price > 0), qty * price, 0)`,
			map[string]formula.Value{"qty": formula.NumberVal(5), "price": formula.NumberVal(20)},
			100,
		},
		{
			"switch_category",
			`=SWITCH(category, "A", 1.1, "B", 1.05, 1.0)`,
			map[string]formula.Value{"category": formula.StringVal("B")},
			1.05,
		},
		{
			"ifs_tiers",
			`=IFS(score > 90, 4, score > 75, 3, score > 60, 2, 1 = 1, 1)`,
			map[string]formula.Value{"score": formula.NumberVal(80)},
			3,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := formula.EvalWithContext(c.formula, &formula.EvalContext{Vars: c.vars})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.IsError() {
				t.Fatalf("formula error: %v", v.Err())
			}
			n, ok := v.Number()
			if !ok {
				t.Fatalf("expected number, got %v", v)
			}
			if math.Abs(n-c.want) > 1e-9 {
				t.Errorf("got %v, want %v", n, c.want)
			}
		})
	}
}

// insertMetricAgg is insertMetric with an explicit aggregation rule.
func insertMetricAgg(t *testing.T, store *calculation.Store, ctx context.Context, modelID, revisionID, name, f string, isInput bool, aggRule string) string {
	t.Helper()
	var id string
	var fp *string
	if f != "" {
		fp = &f
	}
	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, revision_id, name, formula, is_input, agg_rule)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6) RETURNING id::text
	`, modelID, revisionID, name, fp, isInput, aggRule).Scan(&id); err != nil {
		t.Fatalf("insert metric %s: %v", name, err)
	}
	return id
}

// A ratio's total is not a combination of its members' ratios. Summing them is
// meaningless and averaging them weights a tiny member equally with a huge one;
// what is wanted is the formula run against the aggregated inputs. agg_rule
// 'formula' is that, and this pins the difference numerically.
func TestFormulaAggRuleTotalsFromAggregatedInputs(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const (
		model = "00000000-0000-0000-0000-000000000031"
		rev   = "00000000-0000-0000-0031-000000000031"
	)

	dimID := insertDim(t, store, ctx, model, "department")
	insertMember(t, store, ctx, dimID, "SALES", "Sales")
	insertMember(t, store, ctx, dimID, "ENG", "Engineering")

	revenueID := insertMetric(t, store, ctx, model, rev, "revenue", "", true)
	profitID := insertMetric(t, store, ctx, model, rev, "profit", "", true)

	// Same formula, different rules — the only variable under test.
	const pct = `=profit / revenue * 100`
	summedID := insertMetricAgg(t, store, ctx, model, rev, "margin_pct_summed", pct, false, "sum")
	formulaID := insertMetricAgg(t, store, ctx, model, rev, "margin_pct", pct, false, "formula")
	for _, id := range []string{summedID, formulaID} {
		insertDep(t, store, ctx, id, revenueID)
		insertDep(t, store, ctx, id, profitID)
	}
	insertGridSetup(t, store, ctx, model, []string{dimID}, []string{revenueID, profitID, summedID, formulaID})

	// Deliberately lopsided, so sum, average and ratio-of-sums all differ.
	insertFact(t, store, ctx, model, rev, revenueID, `{"`+dimID+`": "SALES"}`, 1000)
	insertFact(t, store, ctx, model, rev, revenueID, `{"`+dimID+`": "ENG"}`, 3000)
	insertFact(t, store, ctx, model, rev, profitID, `{"`+dimID+`": "SALES"}`, 100)
	insertFact(t, store, ctx, model, rev, profitID, `{"`+dimID+`": "ENG"}`, 900)

	runRecalc(t, store, ctx, model, rev, []string{revenueID, profitID})

	// Per-member values are identical under both rules: the rule changes only
	// how they roll up, never what each member computes.
	for _, id := range []string{summedID, formulaID} {
		assertCalc(t, store, ctx, model, rev, id, map[string]string{dimID: "SALES"}, 10) // 100/1000
		assertCalc(t, store, ctx, model, rev, id, map[string]string{dimID: "ENG"}, 30)   // 900/3000
	}

	// 'sum' does what it says, and what it says is wrong for a percentage.
	assertCalc(t, store, ctx, model, rev, summedID, map[string]string{}, 40) // 10 + 30

	// 'formula' evaluates against the aggregated inputs: 1000/4000*100.
	// Note this is not the mean of the members either (that would be 20) —
	// ENG's larger revenue pulls the true margin up.
	assertCalc(t, store, ctx, model, rev, formulaID, map[string]string{}, 25)
}

// insertMetricRate creates a metric whose total is numerator ÷ denominator —
// agg_rule 'rate', Anaplan's Ratio summary.
func insertMetricRate(t *testing.T, store *calculation.Store, ctx context.Context, modelID, revisionID, name, f string, isInput bool, numID, denID string) string {
	t.Helper()
	id := insertMetricAgg(t, store, ctx, modelID, revisionID, name, f, isInput, "rate")
	if _, err := store.Pool().Exec(ctx, `
		UPDATE model.metric_def SET agg_numerator_metric_id=$2::uuid, agg_denominator_metric_id=$3::uuid WHERE id=$1::uuid
	`, id, numID, denID); err != nil {
		t.Fatalf("set ratio operands on %s: %v", name, err)
	}
	return id
}

// A rate metric's total is not built from its own member values at all — it is
// one metric's total over another's. The case that makes this unmistakable is
// an INPUT metric: a price typed per product has no formula to re-evaluate, so
// 'formula' cannot express it, and neither summing nor averaging the prices
// gives the blended price the business means.
func TestRateAggRuleDividesTwoMetricTotals(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const (
		model = "00000000-0000-0000-0000-000000000032"
		rev   = "00000000-0000-0000-0032-000000000032"
	)

	dimID := insertDim(t, store, ctx, model, "product")
	insertMember(t, store, ctx, dimID, "WIDGET", "Widget")
	insertMember(t, store, ctx, dimID, "GIZMO", "Gizmo")

	volumeID := insertMetric(t, store, ctx, model, rev, "volume", "", true)
	revenueID := insertMetric(t, store, ctx, model, rev, "revenue", "", true)
	// Blended price: revenue_total / volume_total.
	priceID := insertMetricRate(t, store, ctx, model, rev, "avg_price", `=revenue / volume`, false, revenueID, volumeID)
	insertDep(t, store, ctx, priceID, revenueID)
	insertDep(t, store, ctx, priceID, volumeID)
	insertGridSetup(t, store, ctx, model, []string{dimID}, []string{volumeID, revenueID, priceID})

	// Deliberately lopsided: one cheap high-volume line, one dear low-volume
	// one, so sum, mean and the true blended price are three different numbers.
	insertFact(t, store, ctx, model, rev, volumeID, `{"`+dimID+`": "WIDGET"}`, 900)
	insertFact(t, store, ctx, model, rev, revenueID, `{"`+dimID+`": "WIDGET"}`, 900) // £1 each
	insertFact(t, store, ctx, model, rev, volumeID, `{"`+dimID+`": "GIZMO"}`, 100)
	insertFact(t, store, ctx, model, rev, revenueID, `{"`+dimID+`": "GIZMO"}`, 1100) // £11 each

	runRecalc(t, store, ctx, model, rev, []string{volumeID, revenueID})

	// Per-member values are untouched by the rule.
	assertCalc(t, store, ctx, model, rev, priceID, map[string]string{dimID: "WIDGET"}, 1)
	assertCalc(t, store, ctx, model, rev, priceID, map[string]string{dimID: "GIZMO"}, 11)

	// The total: 2000 revenue over 1000 units = £2. Not 12 (the sum of the two
	// prices) and not 6 (their mean) — the cheap line dominates because it is
	// where the volume is, which is the entire point of a ratio summary.
	assertCalc(t, store, ctx, model, rev, priceID, map[string]string{}, 2)
}

// A formula that reads only a dimension depends on no metric, so the graph
// walk from changed inputs can never reach it: with no edges it is never
// anyone's dependent, is never "affected", and produces no rows at all. It
// saves cleanly and reports no error — a permanently blank column is the only
// symptom, which is how this went unnoticed.
//
// Its value is not constant either, so "nothing it depends on moved" was never
// a safe conclusion: the member set it reads can change underneath it.
func TestDimensionOnlyFormulaStillComputes(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const (
		model = "00000000-0000-0000-0000-000000000033"
		rev   = "00000000-0000-0000-0033-000000000033"
	)

	dimID := insertDim(t, store, ctx, model, "period")
	insertMember(t, store, ctx, dimID, "Q1", "Q1")
	insertMember(t, store, ctx, dimID, "Q2", "Q2")

	revenueID := insertMetric(t, store, ctx, model, rev, "revenue", "", true)
	// References the period and nothing else — no calc_dependency rows exist
	// for it, by construction.
	flagID := insertMetric(t, store, ctx, model, rev, "is_q1", `=IF(period = "Q1", 1, 0)`, false)
	insertGridSetup(t, store, ctx, model, []string{dimID}, []string{revenueID, flagID})

	insertFact(t, store, ctx, model, rev, revenueID, `{"`+dimID+`": "Q1"}`, 100)
	insertFact(t, store, ctx, model, rev, revenueID, `{"`+dimID+`": "Q2"}`, 200)

	// Recalculating from an input the flag has nothing to do with is the only
	// trigger that ever fires in practice — nothing "changes" a dimension-only
	// metric, so if this does not compute it, nothing will.
	runRecalc(t, store, ctx, model, rev, []string{revenueID})

	assertCalc(t, store, ctx, model, rev, flagID, map[string]string{dimID: "Q1"}, 1)
	assertCalc(t, store, ctx, model, rev, flagID, map[string]string{dimID: "Q2"}, 0)
}

// TestDivisionSkipsEmptyIntersections: a division formula over a sparse plan
// must not report "calculation failing" at intersections that simply hold no
// data. Found live: with 4 of 64 intersections populated, avg_price /
// margin_pct / attainment_pct each carried a permanent 58-60/64 #DIV/0!
// error badge while every number they produced was correct. A combo whose
// evaluation fails with every referenced value at zero is skipped (no row,
// no error); a failure with any nonzero operand still flags; a metric whose
// every combo is skipped writes nothing at all rather than fabricating an
// aggregate 0.
func TestDivisionSkipsEmptyIntersections(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const (
		model = "00000000-0000-0000-0000-000000000077"
		rev   = "00000000-0000-0000-0077-000000000077"
	)

	dimID := insertDim(t, store, ctx, model, "region")
	for _, code := range []string{"A", "B", "C", "D"} {
		insertMember(t, store, ctx, dimID, code, code)
	}
	revenueID := insertMetric(t, store, ctx, model, rev, "revenue", "", true)
	unitsID := insertMetric(t, store, ctx, model, rev, "units", "", true)
	// Sparse but healthy: data only at A.
	sparseID := insertMetric(t, store, ctx, model, rev, "sparse_ratio", "={revenue} / {units}", false)
	insertDep(t, store, ctx, sparseID, revenueID)
	insertDep(t, store, ctx, sparseID, unitsID)
	// Genuinely broken at B: revenue present, units genuinely absent/zero.
	badID := insertMetric(t, store, ctx, model, rev, "bad_ratio", "={revenue} / {units}", false)
	insertDep(t, store, ctx, badID, revenueID)
	insertDep(t, store, ctx, badID, unitsID)
	// No data anywhere.
	emptyRevID := insertMetric(t, store, ctx, model, rev, "lonely", "", true)
	emptyID := insertMetric(t, store, ctx, model, rev, "empty_ratio", "=100 / {lonely}", false)
	insertDep(t, store, ctx, emptyID, emptyRevID)
	insertGridSetup(t, store, ctx, model, []string{dimID},
		[]string{revenueID, unitsID, sparseID, badID, emptyRevID, emptyID})

	comboA := `{"` + dimID + `": "A"}`
	comboB := `{"` + dimID + `": "B"}`
	insertFact(t, store, ctx, model, rev, revenueID, comboA, 100)
	insertFact(t, store, ctx, model, rev, unitsID, comboA, 4)
	insertFact(t, store, ctx, model, rev, revenueID, comboB, 500) // units missing at B

	runRecalc(t, store, ctx, model, rev, []string{revenueID, unitsID, emptyRevID})

	errState := func(metricID string) string {
		var e string
		_ = store.Pool().QueryRow(ctx, `
			SELECT COALESCE(error,'') FROM runtime.metric_partition_state
			WHERE metric_id=$1::uuid AND status='error' LIMIT 1
		`, metricID).Scan(&e)
		return e
	}
	rowCount := func(metricID string) int {
		var n int
		_ = store.Pool().QueryRow(ctx, `
			SELECT count(*) FROM runtime.calc_result WHERE metric_id=$1::uuid
		`, metricID).Scan(&n)
		return n
	}

	// sparse_ratio: A computes (100/4=25), B fails but revenue is nonzero
	// there — wait, sparse and bad share inputs, so both see B. Assert the
	// aggregate and A instead, and that C/D contributed neither rows nor an
	// inflated failure count: the error names 1 failed combo (B), not 3.
	assertCalc(t, store, ctx, model, rev, sparseID, map[string]string{dimID: "A"}, 25)
	if e := errState(sparseID); !strings.Contains(e, "1/2 combos failed") {
		t.Errorf("sparse_ratio error state = %q, want a 1/2 failure (B fails for real; C and D are skipped as no-data, not counted)", e)
	}

	if e := errState(badID); !strings.Contains(e, "1/2 combos failed") {
		t.Errorf("bad_ratio error state = %q, want 1/2 (only B is a real failure)", e)
	}

	// empty_ratio: every combo is all-zero → all skipped → no error state
	// and no rows at all (not even a fabricated aggregate 0).
	if e := errState(emptyID); e != "" {
		t.Errorf("empty_ratio error state = %q, want none — a metric with no data anywhere is not failing", e)
	}
	if n := rowCount(emptyID); n != 0 {
		t.Errorf("empty_ratio calc_result rows = %d, want 0 — nothing genuine to persist", n)
	}
}

// TestRecalcClearsStaleIntersectionsAfterDataRemoval: per-combo calc_result
// rows must not outlive their inputs. Found live: a business user's junk
// test-writes (revenue 4, cost 3) were wiped by a full_reload import, but
// margin_pct kept showing 25 at that intersection forever — the recompute
// skipped the now-empty combo, the skip wrote nothing, and nothing ever
// deleted the stale row. Each recompute's per-combo set is now
// authoritative: rows for intersections that lost their data are cleared,
// while the '{}' aggregate row is never touched by the clear.
func TestRecalcClearsStaleIntersectionsAfterDataRemoval(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const (
		model = "00000000-0000-0000-0000-000000000078"
		rev   = "00000000-0000-0000-0078-000000000078"
	)

	dimID := insertDim(t, store, ctx, model, "region")
	insertMember(t, store, ctx, dimID, "A", "A")
	insertMember(t, store, ctx, dimID, "B", "B")
	revenueID := insertMetric(t, store, ctx, model, rev, "revenue", "", true)
	costID := insertMetric(t, store, ctx, model, rev, "cost", "", true)
	pctID := insertMetric(t, store, ctx, model, rev, "margin_pct", "=({revenue} - {cost}) / {revenue} * 100", false)
	insertDep(t, store, ctx, pctID, revenueID)
	insertDep(t, store, ctx, pctID, costID)
	insertGridSetup(t, store, ctx, model, []string{dimID}, []string{revenueID, costID, pctID})

	comboA := `{"` + dimID + `": "A"}`
	insertFact(t, store, ctx, model, rev, revenueID, comboA, 4)
	insertFact(t, store, ctx, model, rev, costID, comboA, 3)
	runRecalc(t, store, ctx, model, rev, []string{revenueID, costID})
	assertCalc(t, store, ctx, model, rev, pctID, map[string]string{dimID: "A"}, 25)

	// The data is wiped (what a full_reload or member cleanup does) and the
	// metric recomputes: the A intersection must stop reporting 25.
	if _, err := store.Pool().Exec(ctx, `
		DELETE FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id=$2::uuid
	`, model, rev); err != nil {
		t.Fatalf("wipe facts: %v", err)
	}
	runRecalc(t, store, ctx, model, rev, []string{revenueID, costID})

	var staleRows int
	if err := store.Pool().QueryRow(ctx, `
		SELECT count(*) FROM runtime.calc_result
		WHERE metric_id=$1::uuid AND dim_members::text <> '{}'
	`, pctID).Scan(&staleRows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if staleRows != 0 {
		t.Fatalf("stale per-combo calc_result rows survived data removal: %d (the fossil-25 bug)", staleRows)
	}
}

// TestRollupRowsPersistFormulaOverAggregates: agg_rule "formula" rollup rows
// are the formula re-evaluated against aggregated inputs — never a
// combination of the member ratios. A: 20% margin on 100, B: 60% on 300;
// the ALL rollup must be (400-200)/400 = 50, not 20+60=80 nor their mean.
// These rows are what lets the grid answer a rollup cell under a pinned
// context with a real number instead of "—".
func TestRollupRowsPersistFormulaOverAggregates(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const (
		model = "00000000-0000-0000-0000-000000000079"
		rev   = "00000000-0000-0000-0079-000000000079"
	)

	dimID := insertDim(t, store, ctx, model, "region")
	insertMember(t, store, ctx, dimID, "ALL", "All")
	allMemberID := func() string {
		var id string
		_ = store.Pool().QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='ALL'`, dimID).Scan(&id)
		return id
	}()
	_ = allMemberID
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id)
		SELECT $1::uuid, c.code, c.code, m.id FROM (VALUES ('A'), ('B')) AS c(code),
		       model.dimension_member m WHERE m.dimension_id=$1::uuid AND m.code='ALL'
	`, dimID); err != nil {
		t.Fatalf("insert children: %v", err)
	}
	revenueID := insertMetric(t, store, ctx, model, rev, "revenue", "", true)
	costID := insertMetric(t, store, ctx, model, rev, "cost", "", true)
	pctID := insertMetric(t, store, ctx, model, rev, "margin_pct", "=({revenue} - {cost}) / {revenue} * 100", false)
	if _, err := store.Pool().Exec(ctx, `UPDATE model.metric_def SET agg_rule='formula' WHERE id=$1::uuid`, pctID); err != nil {
		t.Fatalf("set agg_rule: %v", err)
	}
	insertDep(t, store, ctx, pctID, revenueID)
	insertDep(t, store, ctx, pctID, costID)
	insertGridSetup(t, store, ctx, model, []string{dimID}, []string{revenueID, costID, pctID})

	comboA, comboB := `{"`+dimID+`": "A"}`, `{"`+dimID+`": "B"}`
	insertFact(t, store, ctx, model, rev, revenueID, comboA, 100)
	insertFact(t, store, ctx, model, rev, costID, comboA, 80)
	insertFact(t, store, ctx, model, rev, revenueID, comboB, 300)
	insertFact(t, store, ctx, model, rev, costID, comboB, 120)
	runRecalc(t, store, ctx, model, rev, []string{revenueID, costID})

	assertCalc(t, store, ctx, model, rev, pctID, map[string]string{dimID: "A"}, 20)
	assertCalc(t, store, ctx, model, rev, pctID, map[string]string{dimID: "B"}, 60)
	// The rollup row: formula over aggregated inputs.
	assertCalc(t, store, ctx, model, rev, pctID, map[string]string{dimID: "ALL"}, 50)
}
