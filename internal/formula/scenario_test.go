package formula_test

// Scenario tests that mirror real Mavericks business cases:
//
//  1. Grid with a chain of calculated metrics (gross_profit → gross_margin_pct)
//  2. Metrics evaluated for different dimensional contexts (department, region)
//  3. Metrics with different dimensional grain (driver metric broadcast)
//  4. Form-record formula → value posted to metric → dependent metric recalculates
//  5. Edge cases: zero revenue guard, IFERROR on bad data, multi-hop chain

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// ── in-memory planning engine ─────────────────────────────────────────────────
//
// Simulates what the calculation scheduler does without a DB:
//   - stores input metric values keyed by (metricName, dimCtx)
//   - evaluates calculated metrics in topological order
//   - returns a cell map that represents a Grid

type dimCtx map[string]string // e.g. {"department": "sales", "month": "jan"}

func (d dimCtx) key() string {
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(d))
	for _, k := range keys {
		pairs = append(pairs, k+"="+d[k])
	}
	return strings.Join(pairs, ",")
}

type planningEngine struct {
	// inputValues: metricName → dimCtx.key() → value
	inputValues map[string]map[string]float64
	// calcResults: same shape, filled by evaluate()
	calcResults map[string]map[string]float64
	// metrics: ordered list of calculated metrics (name, formula, deps)
	metrics []metricDef
}

type metricDef struct {
	name    string
	formula string // empty = input metric
	deps    []string
}

func newEngine() *planningEngine {
	return &planningEngine{
		inputValues: make(map[string]map[string]float64),
		calcResults: make(map[string]map[string]float64),
	}
}

func (e *planningEngine) addMetric(name, formulaStr string, deps ...string) {
	e.metrics = append(e.metrics, metricDef{name: name, formula: formulaStr, deps: deps})
}

func (e *planningEngine) setInput(metric string, ctx dimCtx, value float64) {
	if e.inputValues[metric] == nil {
		e.inputValues[metric] = make(map[string]float64)
	}
	e.inputValues[metric][ctx.key()] = value
}

func (e *planningEngine) getValue(metric string, ctx dimCtx) (float64, bool) {
	ctxKey := ctx.key()
	if m := e.inputValues[metric]; m != nil {
		if v, ok := m[ctxKey]; ok {
			return v, true
		}
	}
	if m := e.calcResults[metric]; m != nil {
		if v, ok := m[ctxKey]; ok {
			return v, true
		}
	}
	return 0, false
}

func (e *planningEngine) setCalc(metric string, ctx dimCtx, value float64) {
	if e.calcResults[metric] == nil {
		e.calcResults[metric] = make(map[string]float64)
	}
	e.calcResults[metric][ctx.key()] = value
}

// evaluate runs all calculated metrics against the given dimensional contexts.
// This mirrors what Scheduler.RecalcAffected does: topological order, resolve deps, call Evaluate.
func (e *planningEngine) evaluate(contexts []dimCtx) error {
	// topological sort by deps (simple: metrics appended in dependency order)
	for _, m := range e.metrics {
		if m.formula == "" {
			continue // input metric — skip
		}
		for _, ctx := range contexts {
			vars := make(map[string]float64, len(m.deps))
			for _, dep := range m.deps {
				v, _ := e.getValue(dep, ctx)
				vars[dep] = v
			}
			// Dimension values are also available as variables (e.g. department = "sales")
			dimVars := make(map[string]formula.Value, len(vars)+len(ctx))
			for k, v := range vars {
				dimVars[k] = formula.NumberVal(v)
			}
			for k, v := range ctx {
				dimVars[k] = formula.StringVal(v)
			}

			result, err := formula.EvalWithContext(m.formula, &formula.EvalContext{Vars: dimVars})
			if err != nil {
				return fmt.Errorf("metric %s at %s: %w", m.name, ctx.key(), err)
			}
			if result.IsError() {
				return fmt.Errorf("metric %s at %s: formula error %s", m.name, ctx.key(), result.Err())
			}
			n, ok := result.Number()
			if !ok {
				return fmt.Errorf("metric %s at %s: result is not a number (%s)", m.name, ctx.key(), result.String())
			}
			e.setCalc(m.name, ctx, n)
		}
	}
	return nil
}

// cell returns the value for a given metric+context (input or calculated).
func (e *planningEngine) cell(metric string, ctx dimCtx) float64 {
	v, _ := e.getValue(metric, ctx)
	return v
}

// ── Scenario 1: Grid with calculated metrics ──────────────────────────────────

func TestScenario_GridCalculatedMetrics(t *testing.T) {
	// Model:
	//   revenue    (input)
	//   cogs       (input)
	//   gross_profit     = revenue - cogs
	//   gross_margin_pct = IF(revenue = 0, 0, gross_profit / revenue)
	//
	// Grid dimensions: department × month
	// Scenario: Budget, Version: FY2026

	eng := newEngine()
	eng.addMetric("revenue", "") // input
	eng.addMetric("cogs", "")    // input
	eng.addMetric("gross_profit", "=revenue - cogs", "revenue", "cogs")
	eng.addMetric("gross_margin_pct", "=IF(revenue=0, 0, gross_profit / revenue)", "revenue", "gross_profit")

	cells := []struct {
		dept  string
		month string
		rev   float64
		cogs  float64
	}{
		{"sales", "jan", 1_000_000, 600_000},
		{"sales", "feb", 1_200_000, 700_000},
		{"finance", "jan", 80_000, 50_000},
		{"finance", "feb", 90_000, 55_000},
	}

	var contexts []dimCtx
	for _, c := range cells {
		ctx := dimCtx{"department": c.dept, "month": c.month}
		eng.setInput("revenue", ctx, c.rev)
		eng.setInput("cogs", ctx, c.cogs)
		contexts = append(contexts, ctx)
	}

	if err := eng.evaluate(contexts); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	for _, c := range cells {
		ctx := dimCtx{"department": c.dept, "month": c.month}
		wantGP := c.rev - c.cogs
		wantGM := wantGP / c.rev

		gotGP := eng.cell("gross_profit", ctx)
		gotGM := eng.cell("gross_margin_pct", ctx)

		if math.Abs(gotGP-wantGP) > 1e-6 {
			t.Errorf("[%s/%s] gross_profit: got %v, want %v", c.dept, c.month, gotGP, wantGP)
		}
		if math.Abs(gotGM-wantGM) > 1e-6 {
			t.Errorf("[%s/%s] gross_margin_pct: got %v, want %v", c.dept, c.month, gotGM, wantGM)
		}
	}

	// Zero-revenue guard: add a zero-revenue cell and verify no div/0 panic
	zeroCtx := dimCtx{"department": "new_dept", "month": "jan"}
	eng.setInput("revenue", zeroCtx, 0)
	eng.setInput("cogs", zeroCtx, 0)
	if err := eng.evaluate([]dimCtx{zeroCtx}); err != nil {
		t.Fatalf("zero revenue evaluate: %v", err)
	}
	if eng.cell("gross_margin_pct", zeroCtx) != 0 {
		t.Errorf("expected 0 for zero revenue, got %v", eng.cell("gross_margin_pct", zeroCtx))
	}
}

// ── Scenario 2: Dimensional context in formulas ───────────────────────────────

func TestScenario_DimensionConditionedMetric(t *testing.T) {
	// commission_rate depends on which department we're calculating for.
	// Formula: =IF(department = "sales", revenue * 0.08, IF(department = "marketing", revenue * 0.05, revenue * 0.02))
	//
	// This tests that dimension values are available as string variables during formula evaluation.

	eng := newEngine()
	eng.addMetric("revenue", "")
	eng.addMetric("commission", `=IF(department="sales", revenue*0.08, IF(department="marketing", revenue*0.05, revenue*0.02))`, "revenue")

	cases := []struct {
		dept string
		rev  float64
		want float64
	}{
		{"sales", 1_000_000, 80_000},
		{"marketing", 500_000, 25_000},
		{"finance", 200_000, 4_000},
	}

	for _, c := range cases {
		ctx := dimCtx{"department": c.dept, "month": "jan"}
		eng.setInput("revenue", ctx, c.rev)
	}
	var ctxs []dimCtx
	for _, c := range cases {
		ctxs = append(ctxs, dimCtx{"department": c.dept, "month": "jan"})
	}
	if err := eng.evaluate(ctxs); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	for _, c := range cases {
		ctx := dimCtx{"department": c.dept, "month": "jan"}
		got := eng.cell("commission", ctx)
		if math.Abs(got-c.want) > 1e-6 {
			t.Errorf("[dept=%s] commission: got %v, want %v", c.dept, got, c.want)
		}
	}
}

// ── Scenario 3: Metrics with different dimensional grain (driver broadcast) ───

func TestScenario_DifferentDimensionalGrain(t *testing.T) {
	// tax_rate  — dimension: region only (driver metric, broadcasted)
	// revenue   — dimensions: department, month, region
	// tax_amount = revenue * tax_rate
	//
	// The scheduler resolves both metric values for each full context before
	// calling Evaluate. We simulate that here:
	//   - For each (dept, month, region) cell, look up tax_rate using only the region key.

	eng := newEngine()
	eng.addMetric("revenue", "")
	// tax_rate is keyed only by region — we store it with a region-only context.
	// The scheduler would look it up with a broader context but fall back to region only.
	// Here we manually resolve the broadcast when setting up vars.

	// tax_amount formula is straightforward — the scheduler already resolved the right value.
	eng.addMetric("tax_amount", "=revenue * tax_rate", "revenue", "tax_rate")

	taxRates := map[string]float64{"us": 0.07, "emea": 0.05, "apac": 0.03}

	type cell struct {
		dept, month, region string
		rev                 float64
	}
	data := []cell{
		{"sales", "jan", "us", 1_000_000},
		{"sales", "jan", "emea", 800_000},
		{"sales", "jan", "apac", 300_000},
		{"finance", "feb", "us", 120_000},
	}

	// Simulate the scheduler's dimensional resolution:
	// For each full context, inject tax_rate from the region-scoped driver metric.
	for _, d := range data {
		ctx := dimCtx{"department": d.dept, "month": d.month, "region": d.region}
		eng.setInput("revenue", ctx, d.rev)
		// Broadcast: tax_rate value for this cell comes from region-scoped store
		eng.setInput("tax_rate", ctx, taxRates[d.region])
	}

	var ctxs []dimCtx
	for _, d := range data {
		ctxs = append(ctxs, dimCtx{"department": d.dept, "month": d.month, "region": d.region})
	}
	if err := eng.evaluate(ctxs); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	for _, d := range data {
		ctx := dimCtx{"department": d.dept, "month": d.month, "region": d.region}
		want := d.rev * taxRates[d.region]
		got := eng.cell("tax_amount", ctx)
		if math.Abs(got-want) > 1e-6 {
			t.Errorf("[%s/%s/%s] tax_amount: got %v, want %v", d.dept, d.month, d.region, got, want)
		}
	}
}

// ── Scenario 4: Form record → metric posting → metric recalculation ──────────

func TestScenario_FormRecordFeedsMetric(t *testing.T) {
	// Form: Expense Request
	//   expense_amount (number, input)
	//   tax_amount     (number, input)
	//   net_expense    (calculated) = expense_amount + tax_amount
	//   approval_status (calculated) = IF(net_expense > 10000, "Needs Approval", "Auto Approved")
	//
	// After form submission, net_expense is posted to the opex input metric.
	//
	// Metric model:
	//   revenue    (input)
	//   opex       (input, receives values from form posting)
	//   gross_profit = revenue - opex
	//
	// Flow:
	//   1. Evaluate form field formulas → get net_expense
	//   2. Post net_expense to opex metric input store
	//   3. RecalcAffected → gross_profit recalculates

	// --- Step 1: Form field formula evaluation ---
	type formRecord struct {
		data map[string]formula.Value
	}

	evaluateFormFields := func(record formRecord, fieldFormulas map[string]string) (map[string]formula.Value, error) {
		result := make(map[string]formula.Value, len(record.data)+len(fieldFormulas))
		for k, v := range record.data {
			result[k] = v
		}
		// Build a stable evaluation order and run multiple passes so that fields
		// which depend on other calculated fields (e.g. approval_status → net_expense)
		// always resolve correctly regardless of map iteration order.
		ordered := make([]string, 0, len(fieldFormulas))
		for f := range fieldFormulas {
			ordered = append(ordered, f)
		}
		sort.Strings(ordered)
		for pass := 0; pass <= len(ordered); pass++ {
			pending := false
			for _, field := range ordered {
				if _, done := result[field]; done {
					continue
				}
				ctx := &formula.EvalContext{Vars: result}
				v, err := formula.EvalWithContext(fieldFormulas[field], ctx)
				if err != nil {
					return nil, fmt.Errorf("field %s: %w", field, err)
				}
				if !v.IsError() {
					result[field] = v
				} else {
					pending = true // dependency not yet resolved; retry next pass
				}
			}
			if !pending {
				break
			}
		}
		// Store any still-unresolved fields (captures genuine formula errors)
		for _, field := range ordered {
			if _, done := result[field]; done {
				continue
			}
			ctx := &formula.EvalContext{Vars: result}
			v, _ := formula.EvalWithContext(fieldFormulas[field], ctx)
			result[field] = v
		}
		return result, nil
	}

	record := formRecord{
		data: map[string]formula.Value{
			"expense_amount": formula.NumberVal(1000),
			"tax_amount":     formula.NumberVal(100),
			"department":     formula.StringVal("sales"),
			"month":          formula.StringVal("jan"),
		},
	}

	fieldFormulas := map[string]string{
		"net_expense":     "=expense_amount + tax_amount",
		"approval_status": `=IF(net_expense > 10000, "Needs Approval", "Auto Approved")`,
	}

	evaluated, err := evaluateFormFields(record, fieldFormulas)
	if err != nil {
		t.Fatalf("form evaluation: %v", err)
	}

	netExpense, _ := evaluated["net_expense"].Number()
	if math.Abs(netExpense-1100) > 1e-6 {
		t.Errorf("net_expense: got %v, want 1100", netExpense)
	}
	if evaluated["approval_status"].String() != "Auto Approved" {
		t.Errorf("approval_status: got %q, want %q", evaluated["approval_status"].String(), "Auto Approved")
	}

	// --- Step 2 & 3: Post net_expense to opex, recalculate gross_profit ---
	eng := newEngine()
	eng.addMetric("revenue", "")
	eng.addMetric("opex", "") // input metric — receives posted values
	eng.addMetric("gross_profit", "=revenue - opex", "revenue", "opex")

	postingCtx := dimCtx{"department": "sales", "month": "jan"}
	eng.setInput("revenue", postingCtx, 5_000)
	// Simulate posting: net_expense value becomes the opex input value
	eng.setInput("opex", postingCtx, netExpense)

	if err := eng.evaluate([]dimCtx{postingCtx}); err != nil {
		t.Fatalf("metric evaluate: %v", err)
	}

	wantGP := 5000 - 1100.0
	gotGP := eng.cell("gross_profit", postingCtx)
	if math.Abs(gotGP-wantGP) > 1e-6 {
		t.Errorf("gross_profit: got %v, want %v", gotGP, wantGP)
	}

	// Verify with a second record in same department/month (aggregation: sum)
	record2 := formRecord{
		data: map[string]formula.Value{
			"expense_amount": formula.NumberVal(500),
			"tax_amount":     formula.NumberVal(50),
		},
	}
	evaluated2, _ := evaluateFormFields(record2, fieldFormulas)
	net2, _ := evaluated2["net_expense"].Number()

	// opex accumulates (sum aggregation): 1100 + 550 = 1650
	eng.setInput("opex", postingCtx, netExpense+net2)
	if err := eng.evaluate([]dimCtx{postingCtx}); err != nil {
		t.Fatalf("post-second-record evaluate: %v", err)
	}
	gotGP2 := eng.cell("gross_profit", postingCtx)
	wantGP2 := 5000 - (netExpense + net2)
	if math.Abs(gotGP2-wantGP2) > 1e-6 {
		t.Errorf("gross_profit after 2 records: got %v, want %v", gotGP2, wantGP2)
	}
}

// ── Scenario 5: Multi-hop dependency chain ────────────────────────────────────

func TestScenario_MultiHopDependencyChain(t *testing.T) {
	// revenue   (input)
	// cogs      (input)
	// opex      (input)
	// gross_profit      = revenue - cogs
	// ebit              = gross_profit - opex
	// ebit_margin       = IF(revenue=0, 0, ebit / revenue)
	//
	// Verifies that formulas referencing other calculated metrics work (scheduler executes in topo order).

	eng := newEngine()
	eng.addMetric("revenue", "")
	eng.addMetric("cogs", "")
	eng.addMetric("opex", "")
	eng.addMetric("gross_profit", "=revenue - cogs", "revenue", "cogs")
	eng.addMetric("ebit", "=gross_profit - opex", "gross_profit", "opex")
	eng.addMetric("ebit_margin", "=IF(revenue=0, 0, ebit/revenue)", "revenue", "ebit")

	ctx := dimCtx{"department": "total", "month": "q1"}
	eng.setInput("revenue", ctx, 10_000_000)
	eng.setInput("cogs", ctx, 6_000_000)
	eng.setInput("opex", ctx, 1_500_000)

	if err := eng.evaluate([]dimCtx{ctx}); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	tests := []struct {
		metric string
		want   float64
	}{
		{"gross_profit", 4_000_000},
		{"ebit", 2_500_000},
		{"ebit_margin", 0.25},
	}
	for _, tt := range tests {
		got := eng.cell(tt.metric, ctx)
		if math.Abs(got-tt.want) > 1e-6 {
			t.Errorf("%s: got %v, want %v", tt.metric, got, tt.want)
		}
	}
}

// ── Scenario 6: IFERROR guard on bad input data ──────────────────────────────

func TestScenario_IFERRORGuard(t *testing.T) {
	// share_of_total = IFERROR(revenue / total_revenue, 0)
	// If total_revenue is 0 (e.g. no data imported yet), result is 0, not an error value.

	cases := []struct {
		rev   float64
		total float64
		want  float64
	}{
		{500_000, 2_000_000, 0.25},
		{0, 0, 0},       // total_revenue = 0 → IFERROR catches div/0
		{800_000, 0, 0}, // same
	}

	for _, c := range cases {
		vars := map[string]formula.Value{
			"revenue":       formula.NumberVal(c.rev),
			"total_revenue": formula.NumberVal(c.total),
		}
		v, err := formula.EvalWithContext("=IFERROR(revenue / total_revenue, 0)", &formula.EvalContext{Vars: vars})
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		n, _ := v.Number()
		if math.Abs(n-c.want) > 1e-9 {
			t.Errorf("rev=%v total=%v: got %v, want %v", c.rev, c.total, n, c.want)
		}
	}
}

// ── Scenario 7: ExtractRefs for dependency graph ──────────────────────────────

func TestScenario_DependencyGraph(t *testing.T) {
	// Validates that ExtractRefs correctly identifies all metric dependencies
	// that the model server would use to build calc_dependency rows.

	cases := []struct {
		formulaStr string
		wantRefs   []string // subset that must be present
	}{
		{
			"=revenue - cogs",
			[]string{"revenue", "cogs"},
		},
		{
			"=IF(revenue=0, 0, gross_profit/revenue)",
			[]string{"revenue", "gross_profit"},
		},
		{
			"=IF(department=\"sales\", revenue*0.08, revenue*0.03)",
			[]string{"department", "revenue"},
		},
		{
			"=IFERROR(revenue / total_revenue, 0)",
			[]string{"revenue", "total_revenue"},
		},
		{
			"=ebit - interest_expense - tax_expense",
			[]string{"ebit", "interest_expense", "tax_expense"},
		},
	}

	for _, c := range cases {
		refs, err := formula.ExtractRefs(c.formulaStr)
		if err != nil {
			t.Fatalf("%q: extract error: %v", c.formulaStr, err)
		}
		refSet := make(map[string]bool, len(refs))
		for _, r := range refs {
			refSet[strings.ToLower(r)] = true
		}
		for _, want := range c.wantRefs {
			if !refSet[strings.ToLower(want)] {
				t.Errorf("%q: missing ref %q in %v", c.formulaStr, want, refs)
			}
		}
	}
}

// ── Scenario 8: Approval status derived label in form record ─────────────────

func TestScenario_ApprovalStatusInFormRecord(t *testing.T) {
	// Replicate the end-to-end approval_status calculation from FORMULA_CALCULATION_INSTRUCTIONS.md
	//
	// Fields:
	//   quantity, unit_price, discount_pct  (input)
	//   net_amount      = quantity * unit_price * (1 - discount_pct)
	//   approval_status = IF(net_amount > 10000, "Needs Approval", "Auto Approved")

	type testCase struct {
		qty, price, disc float64
		wantAmount       float64
		wantStatus       string
	}
	cases := []testCase{
		{10, 25, 0.1, 225, "Auto Approved"},
		{100, 150, 0.05, 14250, "Needs Approval"},
		{50, 200, 0.0, 10000, "Auto Approved"}, // boundary: exactly 10000, not > 10000
	}

	for _, c := range cases {
		vars := map[string]formula.Value{
			"quantity":     formula.NumberVal(c.qty),
			"unit_price":   formula.NumberVal(c.price),
			"discount_pct": formula.NumberVal(c.disc),
		}

		// Step 1: calculate net_amount
		netAmountVal, err := formula.EvalWithContext("=quantity * unit_price * (1 - discount_pct)", &formula.EvalContext{Vars: vars})
		if err != nil {
			t.Fatalf("net_amount parse: %v", err)
		}
		n, _ := netAmountVal.Number()
		if math.Abs(n-c.wantAmount) > 1e-6 {
			t.Errorf("qty=%v price=%v disc=%v: net_amount=%v want=%v", c.qty, c.price, c.disc, n, c.wantAmount)
		}

		// Step 2: add net_amount to vars and evaluate approval_status
		vars["net_amount"] = netAmountVal
		statusVal, err := formula.EvalWithContext(`=IF(net_amount > 10000, "Needs Approval", "Auto Approved")`, &formula.EvalContext{Vars: vars})
		if err != nil {
			t.Fatalf("approval_status parse: %v", err)
		}
		if statusVal.String() != c.wantStatus {
			t.Errorf("qty=%v price=%v disc=%v: status=%q want=%q", c.qty, c.price, c.disc, statusVal.String(), c.wantStatus)
		}
	}
}
