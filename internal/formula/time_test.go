package formula

import (
	"math"
	"strings"
	"testing"
	"time"
)

// fixture is the spec §11 four-month sales series with a scalar EvalAt that
// rebinds "sales" per period — the shape the calculation scheduler provides.
func fixtureAt(t *testing.T, sales []float64, pos int) *EvalContext {
	t.Helper()
	periods := make([]TimePeriod, len(sales))
	for i := range sales {
		start := time.Date(2026, time.Month(i+1), 1, 0, 0, 0, 0, time.UTC)
		periods[i] = TimePeriod{Code: start.Format("2006-01"), Index: i, Start: start, End: start.AddDate(0, 1, -1)}
	}
	var build func(pos int, parent *TimeEvalContext) *EvalContext
	build = func(pos int, parent *TimeEvalContext) *EvalContext {
		var tc *TimeEvalContext
		if parent == nil {
			tc = &TimeEvalContext{DimensionID: "month", Granularity: "month", FiscalYearStartMonth: 1, Position: pos, Periods: periods}
		} else {
			tc = parent.Child(pos)
		}
		ctx := &EvalContext{Vars: map[string]Value{"SALES": NumberVal(sales[pos]), "MONTH": StringVal(periods[pos].Code)}, Time: tc}
		tc.EvalAt = func(node Node, position int) Value {
			return EvalNode(build(position, tc), node)
		}
		return ctx
	}
	return build(pos, nil)
}

func evalSeries(t *testing.T, formulaText string, sales []float64) []Value {
	t.Helper()
	node, err := Parse(formulaText)
	if err != nil {
		t.Fatalf("parse %q: %v", formulaText, err)
	}
	out := make([]Value, len(sales))
	for i := range sales {
		out[i] = EvalNode(fixtureAt(t, sales, i), node)
	}
	return out
}

func numbers(t *testing.T, vals []Value) []float64 {
	t.Helper()
	out := make([]float64, len(vals))
	for i, v := range vals {
		if v.IsError() {
			t.Fatalf("period %d: unexpected error %v", i, v.Err())
		}
		n, _ := v.Number()
		out[i] = n
	}
	return out
}

func approxEq(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-4 {
			return false
		}
	}
	return true
}

// TestSpecAcceptanceTable is the exact table in spec §11.
func TestSpecAcceptanceTable(t *testing.T) {
	sales := []float64{100, 120, 80, 150}
	for formulaText, want := range map[string][]float64{
		"PREVIOUS(sales)":                  {0, 100, 120, 80},
		"NEXT(sales)":                      {120, 80, 150, 0},
		"LAG(sales, 2, 0)":                 {0, 0, 100, 120},
		"LEAD(sales, 1, 0)":                {120, 80, 150, 0},
		"OFFSET(sales, -1, 0)":             {0, 100, 120, 80},
		"MOVINGSUM(sales, -2, 0)":          {100, 220, 300, 350},
		"MOVINGSUM(sales, -2, 0, AVERAGE)": {100, 110, 100, 116.6667},
		"CUMULATE(sales)":                  {100, 220, 300, 450},
		"DECUMULATE(sales)":                {100, 20, -40, 70},
		// case-insensitivity of names and keywords
		"movingsum(Sales, -2, 0, average)": {100, 110, 100, 116.6667},
		"previous(SALES)":                  {0, 100, 120, 80},
	} {
		got := numbers(t, evalSeries(t, formulaText, sales))
		if !approxEq(got, want) {
			t.Errorf("%s: got %v, want %v", formulaText, got, want)
		}
	}
}

func TestMovingSumForms(t *testing.T) {
	sales := []float64{100, 120, 80, 150}
	for formulaText, want := range map[string][]float64{
		"MOVINGSUM(sales)":                {450, 450, 450, 450},
		"MOVINGSUM(sales, 1)":             {350, 230, 150, 0},
		"MOVINGSUM(sales, 1, 3, MAX)":     {150, 150, 150, 0},
		"MOVINGSUM(sales, -1, 0, MIN)":    {100, 100, 80, 80},
		"MOVINGSUM(sales, 2, 1)":          {0, 0, 0, 0},
		"MOVINGSUM(sales, -10, 10)":       {450, 450, 450, 450},
		"MOVINGSUM(sales, 5, 9, AVERAGE)": {0, 0, 0, 0},
	} {
		got := numbers(t, evalSeries(t, formulaText, sales))
		if !approxEq(got, want) {
			t.Errorf("%s: got %v, want %v", formulaText, got, want)
		}
	}
}

func TestLagLeadStrictness(t *testing.T) {
	sales := []float64{100, 120, 80, 150}
	for formulaText, want := range map[string][]float64{
		"LAG(sales, -1, 7)":             {120, 80, 150, 7}, // non-strict: a negative lag looks ahead
		"LAG(sales, -1, 7, NONSTRICT)":  {120, 80, 150, 7},
		"LAG(sales, -1, 7, SEMISTRICT)": {7, 7, 7, 7},
		"LAG(sales, 0, 7, SEMISTRICT)":  {100, 120, 80, 150},
		"LAG(sales, 0, 7, STRICT)":      {7, 7, 7, 7},
		"LAG(sales, 1, 7, STRICT)":      {7, 100, 120, 80},
		"LEAD(sales, -1, 7, STRICT)":    {7, 7, 7, 7},
		"LEAD(sales, 1, sales * 10)":    {120, 80, 150, 1500}, // substitute evaluated at the CURRENT period
		"LAG(sales * 2, 1, 0)":          {0, 200, 240, 160},   // value is an expression
	} {
		got := numbers(t, evalSeries(t, formulaText, sales))
		if !approxEq(got, want) {
			t.Errorf("%s: got %v, want %v", formulaText, got, want)
		}
	}
}

func TestNestedTimeFunctions(t *testing.T) {
	sales := []float64{100, 120, 80, 150}
	got := numbers(t, evalSeries(t, "PREVIOUS(LAG(sales, 2, 0))", sales))
	if !approxEq(got, []float64{0, 0, 0, 100}) {
		t.Errorf("PREVIOUS(LAG(x,2)) reads x at -3: got %v", got)
	}
	got = numbers(t, evalSeries(t, "CUMULATE(PREVIOUS(sales))", sales))
	if !approxEq(got, []float64{0, 100, 220, 300}) {
		t.Errorf("CUMULATE(PREVIOUS(x)): got %v", got)
	}
}

func TestCumulateReset(t *testing.T) {
	sales := []float64{100, 120, 80, 150}
	// Reset in March: the run restarts and March's own value is its first term.
	got := numbers(t, evalSeries(t, `CUMULATE(sales, month = "2026-03")`, sales))
	if !approxEq(got, []float64{100, 220, 80, 230}) {
		t.Errorf("CUMULATE with reset: got %v", got)
	}
}

func TestPeriodToDate(t *testing.T) {
	// Monthly periods Jan..Apr 2026 with a January fiscal year: QTD resets in April.
	sales := []float64{100, 120, 80, 150}
	got := numbers(t, evalSeries(t, "QUARTERTODATE(sales)", sales))
	if !approxEq(got, []float64{100, 220, 300, 150}) {
		t.Errorf("QUARTERTODATE: got %v", got)
	}
	got = numbers(t, evalSeries(t, "YEARTODATE(sales)", sales))
	if !approxEq(got, []float64{100, 220, 300, 450}) {
		t.Errorf("YEARTODATE: got %v", got)
	}
	// MONTHTODATE needs day granularity: a month source is an error, not zero.
	vals := evalSeries(t, "MONTHTODATE(sales)", sales)
	if !vals[0].IsError() || vals[0].Err().Code != CodeInvalidTimeMember {
		t.Errorf("MONTHTODATE on months: want %s, got %v", CodeInvalidTimeMember, vals[0])
	}
}

func TestTimeFunctionsWithoutContext(t *testing.T) {
	for _, f := range []string{"PREVIOUS(x)", "LAG(x, 1, 0)", "MOVINGSUM(x)", "CUMULATE(x)", "YEARTODATE(x)"} {
		v, err := Eval(f, map[string]Value{"x": NumberVal(1)})
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if !v.IsError() || v.Err().Code != CodeTimeContextRequired {
			t.Errorf("%s without time context: want %s, got %v", f, CodeTimeContextRequired, v)
		}
	}
}

func TestEvalWithContextKeepsTime(t *testing.T) {
	ctx := fixtureAt(t, []float64{1, 2, 3}, 1)
	v, err := EvalWithContext("previous(sales)", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := v.Number(); v.IsError() || n != 1 {
		t.Errorf("EvalWithContext lost the time context: %v", v)
	}
}

func TestDynamicOffsetsRejected(t *testing.T) {
	for _, f := range []string{"LAG(sales, sales, 0)", "LAG(sales, 1.5, 0)", "MOVINGSUM(sales, -1 - 1, 0)", "LEAD(sales, 1+1, 0)"} {
		vals := evalSeries(t, f, []float64{1, 2})
		if !vals[0].IsError() || vals[0].Err().Code != CodeDynamicTimeOffset {
			t.Errorf("%s: want %s, got %v", f, CodeDynamicTimeOffset, vals[0])
		}
		if _, err := Analyze(f); err == nil || !strings.Contains(err.Error(), CodeDynamicTimeOffset) {
			t.Errorf("Analyze(%s): want %s, got %v", f, CodeDynamicTimeOffset, err)
		}
	}
	if _, err := Analyze("LAG(sales, 1, 0, LOOSE)"); err == nil {
		t.Error("unknown strictness keyword must be rejected")
	}
	if _, err := Analyze("MOVINGSUM(sales, 0, 1, MEDIAN)"); err == nil {
		t.Error("unknown moving method must be rejected")
	}
	if _, err := Analyze("OFFSET(sales, 1, 0, STRICT)"); err == nil {
		t.Error("OFFSET takes no strictness keyword")
	}
}

func TestErrorPropagation(t *testing.T) {
	// A #DIV/0! in an included period propagates through the window ...
	node, _ := Parse("MOVINGSUM(1 / (sales - 120), -1, 0)")
	ctx := fixtureAt(t, []float64{100, 120, 80}, 2)
	if v := EvalNode(ctx, node); !v.IsError() || v.Err() != ErrDiv0 {
		t.Errorf("error in window must propagate: %v", v)
	}
	// ... unless the formula handles it.
	node, _ = Parse("IFERROR(MOVINGSUM(1 / (sales - 120), -1, 0), -1)")
	if v := EvalNode(ctx, node); v.IsError() {
		t.Errorf("IFERROR must catch the propagated error: %v", v)
	}
	// Errors inside an included period can be handled per period too.
	node, _ = Parse("MOVINGSUM(IFERROR(1 / (sales - 120), 0), -1, 0)")
	if v := EvalNode(ctx, node); v.IsError() {
		t.Errorf("per-period IFERROR: %v", v)
	}
}

func TestAnalyzeWindows(t *testing.T) {
	type want struct {
		min, max     int
		past, future bool
	}
	cases := map[string]map[string]want{
		"revenue - cost":                {"revenue": {0, 0, false, false}, "cost": {0, 0, false, false}},
		"PREVIOUS(x)":                   {"x": {-1, -1, false, false}},
		"NEXT(x)":                       {"x": {1, 1, false, false}},
		"LAG(x, 2, 0)":                  {"x": {-2, -2, false, false}},
		"LEAD(x, 2, 0)":                 {"x": {2, 2, false, false}},
		"OFFSET(x, -3, 0)":              {"x": {-3, -3, false, false}},
		"MOVINGSUM(x, -2, 0)":           {"x": {-2, 0, false, false}},
		"MOVINGSUM(x, -2, 0, AVERAGE)":  {"x": {-2, 0, false, false}},
		"MOVINGSUM(x, 1)":               {"x": {1, 1, false, true}},
		"MOVINGSUM(x)":                  {"x": {0, 0, true, true}},
		"CUMULATE(x)":                   {"x": {0, 0, true, false}},
		"DECUMULATE(x)":                 {"x": {-1, 0, false, false}},
		"YEARTODATE(x)":                 {"x": {0, 0, true, false}},
		"PREVIOUS(LAG(x, 2, 0))":        {"x": {-3, -3, false, false}},
		"LAG(x, 1, y)":                  {"x": {-1, -1, false, false}, "y": {0, 0, false, false}},
		"LAG(x, 1, 0) + x":              {"x": {-1, 0, false, false}},
		"LAG(x, 1, 0, STRICT)":          {"x": {-1, -1, false, false}},
		"LAG(x, 1, 0) + LEAD(x, 2, 0)":  {"x": {-1, 2, false, false}},
		"MOVINGSUM(PREVIOUS(x), -1, 0)": {"x": {-2, -1, false, false}},
		"CUMULATE(x, flag)":             {"x": {0, 0, true, false}, "flag": {0, 0, true, false}},
		"IF(x > 0, PREVIOUS(y), 0)":     {"x": {0, 0, false, false}, "y": {-1, -1, false, false}},
	}
	for formulaText, refs := range cases {
		an, err := Analyze(formulaText)
		if err != nil {
			t.Errorf("Analyze(%s): %v", formulaText, err)
			continue
		}
		if len(an.References) != len(refs) {
			t.Errorf("%s: references %+v, want %v", formulaText, an.References, refs)
			continue
		}
		for _, r := range an.References {
			w, ok := refs[r.Name]
			if !ok {
				t.Errorf("%s: unexpected reference %q (keywords must not be references)", formulaText, r.Name)
				continue
			}
			if r.MinTimeOffset != w.min || r.MaxTimeOffset != w.max || r.UnboundedPast != w.past || r.UnboundedFuture != w.future {
				t.Errorf("%s: %s = [%d,%d] past=%v future=%v, want %+v", formulaText, r.Name, r.MinTimeOffset, r.MaxTimeOffset, r.UnboundedPast, r.UnboundedFuture, w)
			}
		}
	}
	an, _ := Analyze("revenue - cost")
	if an.UsesTimeSeries {
		t.Error("scalar formula flagged as time series")
	}
	an, _ = Analyze("MOVINGSUM(x, -2, 0, AVERAGE) + LAG(y, 1, 0, STRICT)")
	if !an.UsesTimeSeries || len(an.Calls) != 2 {
		t.Errorf("time series flag / calls: %+v", an)
	}
}

func TestLaterParityNamesStayUnknown(t *testing.T) {
	for _, name := range []string{"POST", "SPREAD", "PROFILE", "TIMESUM", "WEEKVALUE", "MONTHVALUE", "QUARTERVALUE", "HALFYEARVALUE", "YEARVALUE"} {
		if IsBuiltin(name) {
			t.Errorf("%s must stay unregistered until fully implemented", name)
		}
	}
	for _, name := range TimeFunctionNames {
		if !IsBuiltin(name) {
			t.Errorf("%s must be a registered builtin", name)
		}
	}
}
