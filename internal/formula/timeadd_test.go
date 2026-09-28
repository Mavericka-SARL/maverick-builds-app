package formula

import (
	"math"
	"strings"
	"testing"
	"time"
)

// tsFixture is a time axis with metric series (NaN = no recorded value),
// each metric's time_summary, and period spans — what the calculation
// package supplies through TimeEvalContext.Summarize and Span.
type tsFixture struct {
	gran        string
	fiscalStart int
	periods     []TimePeriod
	series      map[string][]float64 // UPPER metric -> per position
	summary     map[string]string    // UPPER metric -> time_summary
	spans       map[string][2]int
	noCallbacks bool
}

func monthly(year, from, n int) []TimePeriod {
	out := make([]TimePeriod, n)
	for i := 0; i < n; i++ {
		start := time.Date(year, time.Month(from+i), 1, 0, 0, 0, 0, time.UTC)
		out[i] = TimePeriod{Code: start.Format("2006-01"), Index: i, Start: start, End: start.AddDate(0, 1, -1)}
	}
	return out
}

func seq(from, to float64) []float64 {
	var out []float64
	for v := from; v <= to; v++ {
		out = append(out, v)
	}
	return out
}

func (f *tsFixture) at(pos int, parent *TimeEvalContext) *EvalContext {
	var tc *TimeEvalContext
	if parent == nil {
		tc = &TimeEvalContext{DimensionID: "month", Granularity: f.gran, FiscalYearStartMonth: f.fiscalStart, Position: pos, Periods: f.periods}
		if !f.noCallbacks {
			tc.Summarize = f.summarize
			tc.Span = func(code string) (int, int, bool) {
				s, ok := f.spans[code]
				return s[0], s[1], ok
			}
		}
	} else {
		tc = parent.Child(pos)
	}
	vars := map[string]Value{}
	for name, vals := range f.series {
		v := vals[pos]
		if math.IsNaN(v) {
			v = 0
		}
		vars[name] = NumberVal(v)
	}
	ctx := &EvalContext{Vars: vars, Time: tc}
	tc.EvalAt = func(node Node, position int) Value { return EvalNode(f.at(position, tc), node) }
	return ctx
}

// summarize mirrors rollup.CombineTime over the recorded positions.
func (f *tsFixture) summarize(metric string, positions []int) (float64, bool, *FormulaError) {
	name := strings.ToUpper(metric)
	vals, ok := f.series[name]
	if !ok {
		return 0, false, errName(metric)
	}
	rule := f.summary[name]
	if rule == "none" {
		return 0, false, nil
	}
	var got []float64
	for _, p := range positions {
		if !math.IsNaN(vals[p]) {
			got = append(got, vals[p])
		}
	}
	if len(got) == 0 {
		return 0, false, nil
	}
	switch rule {
	case "last":
		return got[len(got)-1], true, nil
	case "first":
		return got[0], true, nil
	case "average":
		var s float64
		for _, v := range got {
			s += v
		}
		return s / float64(len(got)), true, nil
	case "min", "max":
		m := got[0]
		for _, v := range got[1:] {
			if (rule == "min" && v < m) || (rule == "max" && v > m) {
				m = v
			}
		}
		return m, true, nil
	}
	var s float64
	for _, v := range got {
		s += v
	}
	return s, true, nil
}

func (f *tsFixture) eval(t *testing.T, text string, pos int) Value {
	t.Helper()
	node, err := Parse(text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return EvalNode(f.at(pos, nil), node)
}

type tsCase struct {
	formula string
	pos     int
	num     float64
	blank   bool
	code    string
}

func runTSCases(t *testing.T, f *tsFixture, cases []tsCase) {
	t.Helper()
	for _, c := range cases {
		v := f.eval(t, c.formula, c.pos)
		switch {
		case c.code != "":
			if !v.IsError() || v.Err().Code != c.code {
				t.Errorf("%s @%d: want error %s, got %v", c.formula, c.pos, c.code, v)
			}
		case v.IsError():
			t.Errorf("%s @%d: unexpected error %v", c.formula, c.pos, v.Err())
		case c.blank:
			if !v.IsBlank() {
				t.Errorf("%s @%d: want blank, got %v", c.formula, c.pos, v)
			}
		default:
			if n, _ := v.Number(); v.Kind() != KindNumber || math.Abs(n-c.num) > 1e-9 {
				t.Errorf("%s @%d: want %v, got %v", c.formula, c.pos, c.num, v)
			}
		}
	}
}

func year2026() *tsFixture {
	nan := math.NaN()
	return &tsFixture{
		gran: "month", fiscalStart: 1, periods: monthly(2026, 1, 12),
		series: map[string][]float64{
			"SALES":   seq(1, 12),
			"BAL":     {5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, nan},
			"RATE":    {1, nan, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
			"FLAT":    seq(1, 12),
			"PEAK":    {3, 9, 1, 4, 4, 4, 4, 4, 4, 4, 4, 4},
			"OFFSETS": {1, 2, 3, 1, 1.5, 0, -1, 0, 0, 0, 0, 0},
		},
		summary: map[string]string{"SALES": "sum", "BAL": "last", "RATE": "average", "FLAT": "none", "PEAK": "max"},
		spans: map[string][2]int{
			"Q1": {0, 2}, "Q2": {3, 5}, "H2": {6, 11}, "FY26": {0, 11}, "2026-02": {1, 1}, "2026-12": {11, 11},
		},
	}
}

func TestIntervalValues(t *testing.T) {
	runTSCases(t, year2026(), []tsCase{
		{formula: "YEARVALUE(sales)", pos: 0, num: 78},
		{formula: "YEARVALUE(sales)", pos: 11, num: 78},
		{formula: "QUARTERVALUE(sales)", pos: 1, num: 6},
		{formula: "QUARTERVALUE(sales)", pos: 11, num: 33},
		{formula: "HALFYEARVALUE(sales)", pos: 7, num: 57},
		{formula: "sales / YEARVALUE(sales)", pos: 2, num: 3.0 / 78},
		{formula: "YEARVALUE(bal)", pos: 0, num: 15},    // last recorded: December is missing
		{formula: "QUARTERVALUE(bal)", pos: 0, num: 7},  // last of Q1
		{formula: "QUARTERVALUE(rate)", pos: 2, num: 2}, // average skips the missing February
		{formula: "QUARTERVALUE(peak)", pos: 0, num: 9},
		{formula: "YEARVALUE(flat)", pos: 3, blank: true}, // time_summary none
		{formula: "YEARVALUE(flat) + 1", pos: 3, num: 1},
		{formula: "MONTHVALUE(sales)", pos: 0, code: CodeInvalidTimeMember}, // needs day granularity
		{formula: "YEARVALUE(sales * 2)", pos: 0, code: CodeSourceMustBeMetric},
		{formula: "YEARVALUE(nope)", pos: 0, code: "#NAME?"},
	})

	// Fiscal year starting in April: FY2025 runs Apr 2025 - Mar 2026.
	f := year2026()
	f.fiscalStart = 4
	runTSCases(t, f, []tsCase{
		{formula: "YEARVALUE(sales)", pos: 0, num: 6},       // Jan-Mar 2026 of FY2025 are on the axis
		{formula: "YEARVALUE(sales)", pos: 3, num: 72},      // Apr-Dec 2026 of FY2026
		{formula: "HALFYEARVALUE(sales)", pos: 6, num: 39},  // Apr-Sep
		{formula: "HALFYEARVALUE(sales)", pos: 9, num: 33},  // Oct-Dec on the axis
		{formula: "QUARTERVALUE(sales)", pos: 3, num: 15},   // Apr-Jun
		{formula: "HALFYEARTODATE(sales)", pos: 6, num: 22}, // Apr..Jul
	})

	// Day granularity: MONTHVALUE buckets days by calendar month.
	days := &tsFixture{gran: "day", fiscalStart: 1, series: map[string][]float64{"SALES": {1, 2, 4}}, summary: map[string]string{}}
	for i, d := range []string{"2024-01-30", "2024-01-31", "2024-02-01"} {
		start, _ := time.Parse(isoDateLayout, d)
		days.periods = append(days.periods, TimePeriod{Code: d, Index: i, Start: start, End: start})
	}
	runTSCases(t, days, []tsCase{
		{formula: "MONTHVALUE(sales)", pos: 1, num: 3},
		{formula: "MONTHVALUE(sales)", pos: 2, num: 4},
		{formula: "YEARVALUE(sales)", pos: 0, num: 7},
	})

	// Week granularity: W13 (Mar 30 - Apr 5) straddles the Q1/Q2 boundary.
	// It belongs to neither quarter and is invalid itself, but it only
	// ENDS Q1's bucket for the weeks before it — they still resolve, and
	// agree with QUARTERTODATE at the last whole week.
	weeks := &tsFixture{gran: "week", fiscalStart: 1, series: map[string][]float64{"SALES": seq(1, 14)}, summary: map[string]string{}}
	for i := 0; i < 14; i++ {
		start := time.Date(2026, time.January, 5+7*i, 0, 0, 0, 0, time.UTC)
		weeks.periods = append(weeks.periods, TimePeriod{Code: start.Format(isoDateLayout), Index: i, Start: start, End: start.AddDate(0, 0, 6)})
	}
	runTSCases(t, weeks, []tsCase{
		{formula: "QUARTERVALUE(sales)", pos: 0, num: 78},  // W01..W12
		{formula: "QUARTERVALUE(sales)", pos: 5, num: 78},  // W06
		{formula: "QUARTERVALUE(sales)", pos: 11, num: 78}, // W12, the last whole week
		{formula: "QUARTERTODATE(sales)", pos: 11, num: 78},
		{formula: "QUARTERVALUE(sales)", pos: 12, code: CodeInvalidTimeMember}, // W13 itself
		{formula: "QUARTERVALUE(sales)", pos: 13, num: 14},                     // W14, Q2
		{formula: "HALFYEARVALUE(sales)", pos: 12, num: 105},                   // W13 is inside H1: only the quarter is crossed
		{formula: "YEARVALUE(sales)", pos: 3, num: 105},                        // W13 is wholly inside 2026
	})

	// Without the calculation engine's time summary.
	none := year2026()
	none.noCallbacks = true
	runTSCases(t, none, []tsCase{{formula: "YEARVALUE(sales)", pos: 0, code: CodeTimeContextRequired}})
	if v, _ := Eval("YEARVALUE(sales)", map[string]Value{"sales": NumberVal(1)}); v.Err().Code != CodeTimeContextRequired {
		t.Errorf("YEARVALUE without a time context: %v", v)
	}
}

func TestHalfYearToDate(t *testing.T) {
	runTSCases(t, year2026(), []tsCase{
		{formula: "HALFYEARTODATE(sales)", pos: 0, num: 1},
		{formula: "HALFYEARTODATE(sales)", pos: 5, num: 21},
		{formula: "HALFYEARTODATE(sales)", pos: 6, num: 7},
		{formula: "HALFYEARTODATE(sales)", pos: 7, num: 15},
	})
	// Quarterly periods fit a half year; half-year periods do not.
	q := &tsFixture{gran: "quarter", fiscalStart: 1, series: map[string][]float64{"SALES": {1, 2, 3, 4}}}
	for i := 0; i < 4; i++ {
		start := time.Date(2026, time.Month(1+3*i), 1, 0, 0, 0, 0, time.UTC)
		q.periods = append(q.periods, TimePeriod{Code: start.Format("2006-01"), Index: i, Start: start, End: start.AddDate(0, 3, -1)})
	}
	runTSCases(t, q, []tsCase{
		{formula: "HALFYEARTODATE(sales)", pos: 1, num: 3},
		{formula: "HALFYEARTODATE(sales)", pos: 3, num: 7},
		{formula: "HALFYEARVALUE(sales)", pos: 2, num: 7},
	})
	q.gran = "half_year"
	runTSCases(t, q, []tsCase{{formula: "HALFYEARTODATE(sales)", pos: 0, code: CodeInvalidTimeMember}})
}

func TestTimeSum(t *testing.T) {
	runTSCases(t, year2026(), []tsCase{
		{formula: "TIMESUM(sales)", pos: 4, num: 78},
		{formula: "TIMESUM(sales, \"Q1\", \"Q2\")", pos: 0, num: 21},
		{formula: "TIMESUM(sales, \"2026-02\", \"Q1\")", pos: 0, num: 5},
		{formula: "TIMESUM(sales, \"Q1\", \"Q1\", AVERAGE)", pos: 0, num: 2},
		{formula: "TIMESUM(sales, \"Q1\", \"Q2\", MAX)", pos: 0, num: 6},
		{formula: "TIMESUM(sales, \"H2\", \"2026-12\", MIN)", pos: 0, num: 7},
		{formula: "TIMESUM(sales * 2, \"Q1\", \"Q1\")", pos: 0, num: 12},      // any expression
		{formula: "TIMESUM(PREVIOUS(sales), \"Q1\", \"Q1\")", pos: 0, num: 3}, // 0 + 1 + 2
		{formula: "TIMESUM(sales, \"Q2\", \"Q1\")", pos: 0, num: 0},           // start after end: empty
		{formula: "TIMESUM(sales, \"Q2\", \"Q1\", AVERAGE)", pos: 0, num: 0},
		{formula: "TIMESUM(sales, \"nope\", \"Q1\")", pos: 0, code: "#N/A"},
		{formula: "TIMESUM(sales, \"Q1\", \"\")", pos: 0, code: "#N/A"},
		{formula: "IFNA(TIMESUM(sales, \"Q1\", \"nope\"), -1)", pos: 0, num: -1},
		{formula: "TIMESUM(sales, \"Q1\")", pos: 0, code: "#VALUE!"},
		{formula: "TIMESUM(sales, \"Q1\", \"Q2\", MEDIAN)", pos: 0, code: "#VALUE!"},
		{formula: "TIMESUM(sales, \"Q\" & \"1\", \"Q2\")", pos: 0, num: 21}, // codes are expressions
	})
	f := year2026()
	f.noCallbacks = true
	runTSCases(t, f, []tsCase{
		{formula: "TIMESUM(sales)", pos: 0, num: 78}, // no range: no Span needed
		{formula: "TIMESUM(sales, \"Q1\", \"Q2\")", pos: 0, code: CodeTimeContextRequired},
	})
}

func TestPeriodDates(t *testing.T) {
	f := year2026()
	runTSCases(t, f, []tsCase{
		{formula: "START()", pos: 1, num: serial(2026, 2, 1)},
		{formula: "END()", pos: 1, num: serial(2026, 2, 28)},
		{formula: "END() - START() + 1", pos: 11, num: 31},
		{formula: "DAY(END())", pos: 3, num: 30},
		{formula: "START(1)", pos: 0, code: "#VALUE!"},
	})
	leap := &tsFixture{gran: "month", fiscalStart: 1, periods: monthly(2024, 2, 1)}
	runTSCases(t, leap, []tsCase{{formula: "END() - START() + 1", num: 29}})
	undated := &tsFixture{gran: "custom", periods: []TimePeriod{{Code: "P1"}}}
	runTSCases(t, undated, []tsCase{{formula: "START()", code: CodeInvalidTimeMember}})
	if v, _ := Eval("START()", nil); v.Err().Code != CodeTimeContextRequired {
		t.Errorf("START without a time context: %v", v)
	}
}

func TestDaysIn(t *testing.T) {
	for f, want := range map[string]float64{
		"DAYSINMONTH(2024, 2)":  29,
		"DAYSINMONTH(2023, 2)":  28,
		"DAYSINMONTH(1900, 2)":  28,
		"DAYSINMONTH(2000, 2)":  29,
		"DAYSINMONTH(2026, 12)": 31,
		"DAYSINMONTH(2026, 4)":  30,
		"DAYSINYEAR(2024)":      366,
		"DAYSINYEAR(2023)":      365,
		"DAYSINYEAR(1900)":      365,
		"DAYSINYEAR(2000)":      366,
	} {
		if got, err := EvalNumber(f, nil); err != nil || got != want {
			t.Errorf("%s = %v (%v), want %v", f, got, err, want)
		}
	}
	for f, code := range map[string]string{
		"DAYSINMONTH(2026, 13)":  "#NUM!",
		"DAYSINMONTH(2026, 0)":   "#NUM!",
		"DAYSINMONTH(2026)":      "#VALUE!",
		"DAYSINYEAR(\"x\")":      "#VALUE!",
		"DAYSINMONTH(2026, 1/0)": "#DIV/0!",
	} {
		v, _ := Eval(f, nil)
		if !v.IsError() || v.Err().Code != code {
			t.Errorf("%s: want %s, got %v", f, code, v)
		}
	}
	if IsTimeFunction("DAYSINMONTH") || IsTimeFunction("DAYSINYEAR") {
		t.Error("DAYSINMONTH/DAYSINYEAR are plain date functions")
	}
}

func TestDynamicOffsets(t *testing.T) {
	f := year2026()
	// Literal and dynamic forms agree.
	for pos := 0; pos < 12; pos++ {
		lit := f.eval(t, "LAG(sales, 2, -1)", pos)
		dyn := f.eval(t, "LAG(sales, 1 + 1, -1)", pos)
		if lit != dyn {
			t.Errorf("@%d: LAG literal %v vs dynamic %v", pos, lit, dyn)
		}
	}
	runTSCases(t, f, []tsCase{
		{formula: "LAG(sales, offsets, -1)", pos: 0, num: -1},           // 1 back from the first period: substitute
		{formula: "LAG(sales, offsets, -1)", pos: 2, num: -1},           // 3 back from March: out of range
		{formula: "LAG(sales, offsets, -1)", pos: 3, num: 3},            // 1 back from April
		{formula: "LAG(sales, offsets, -1)", pos: 5, num: 6},            // 0: the current period
		{formula: "LEAD(sales, offsets, -1)", pos: 6, num: 6},           // LEAD by -1 looks back
		{formula: "OFFSET(sales, 0 - offsets, -1)", pos: 1, num: 0 - 1}, // back 2 from Feb: out of range
		{formula: "OFFSET(sales, 0 - offsets, -1)", pos: 3, num: 3},
		{formula: "LEAD(sales, sales, -1)", pos: 4, num: 10},                         // 5 ahead of May
		{formula: "LEAD(sales, sales, -1)", pos: 8, num: -1},                         // 9 ahead of September
		{formula: "LAG(sales, offsets, -1)", pos: 4, code: CodeTimeOffsetNotInteger}, // 1.5
		{formula: "LAG(sales, \"two\", 0)", pos: 4, code: CodeTimeOffsetNotInteger},
		{formula: "LAG(sales, 1 / 0, 0)", pos: 4, code: "#DIV/0!"},
		{formula: "LAG(sales, 0 - 1, -1, SEMISTRICT)", pos: 4, num: -1}, // strictness applies to the evaluated offset
		{formula: "LAG(sales, 1 - 1, -1, STRICT)", pos: 4, num: -1},
		{formula: "LAG(sales, 1 + 0, -1, STRICT)", pos: 4, num: 4},
		{formula: "LAG(sales, 1e12 * offsets, -1)", pos: 3, num: -1},            // huge offsets are simply out of range
		{formula: "LAG(sales, 1.5, 0)", pos: 4, code: CodeTimeOffsetNotInteger}, // a literal is also refused at save
	})
	var sub error
	if _, sub = Analyze("LAG(sales, offsets, 0)"); sub != nil {
		t.Errorf("a dynamic offset must pass Analyze: %v", sub)
	}
}

func TestAnalyzeTimeAdditions(t *testing.T) {
	type want struct {
		min, max     int
		past, future bool
		dimensional  bool
	}
	cases := map[string]map[string]want{
		"LAG(x, y, 0)":                        {"x": {0, 0, true, true, false}, "y": {0, 0, false, false, false}},
		"LEAD(x, y + 1, z, STRICT)":           {"x": {0, 0, true, true, false}, "y": {0, 0, false, false, false}, "z": {0, 0, false, false, false}},
		"PREVIOUS(LAG(x, y, 0))":              {"x": {-1, -1, true, true, false}, "y": {-1, -1, false, false, false}},
		"YEARVALUE(x)":                        {"x": {0, 0, true, true, true}},
		"MONTHVALUE(x) + QUARTERVALUE(y)":     {"x": {0, 0, true, true, true}, "y": {0, 0, true, true, true}},
		"HALFYEARTODATE(x)":                   {"x": {0, 0, true, false, false}},
		"TIMESUM(x)":                          {"x": {0, 0, true, true, false}},
		"TIMESUM(x, start_code, \"Q2\", MAX)": {"x": {0, 0, true, true, false}, "start_code": {0, 0, false, false, false}},
		"START() + END() + DAYSINMONTH(y, 2)": {"y": {0, 0, false, false, false}},
		"PREVIOUS(LOOKUP(x, region, \"US\"))": {"x": {-1, -1, false, false, true}},
		"x + LOOKUP(x, region, \"US\")":       {"x": {0, 0, false, false, true}},
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
				t.Errorf("%s: unexpected reference %q", formulaText, r.Name)
				continue
			}
			if r.MinTimeOffset != w.min || r.MaxTimeOffset != w.max || r.UnboundedPast != w.past || r.UnboundedFuture != w.future || r.Dimensional != w.dimensional {
				t.Errorf("%s: %s = %+v, want %+v", formulaText, r.Name, r, w)
			}
		}
	}
	served := map[string]bool{
		"LAG(x, y, 0)": true, "LAG(x, 2, 0)": false, "LAG(x, -2, 0)": false, "YEARVALUE(x)": true, "TIMESUM(x)": true,
		"HALFYEARTODATE(x)": false, "START()": false, "MOVINGSUM(x, -2, 0)": false, "LOOKUP(x, r, \"a\")": true,
		"COUNTIF(r, \"a\")": true, "r.p": false, "PARENT(r)": false,
	}
	for f, want := range served {
		an, err := Analyze(f)
		if err != nil {
			t.Errorf("Analyze(%s): %v", f, err)
			continue
		}
		if an.ServedFromRows != want {
			t.Errorf("%s: ServedFromRows = %v, want %v", f, an.ServedFromRows, want)
		}
	}
	for _, f := range []string{"START()", "TIMESUM(x)", "YEARVALUE(x)", "HALFYEARTODATE(x)"} {
		if an, _ := Analyze(f); !an.UsesTimeSeries {
			t.Errorf("%s must route through the time-series evaluator", f)
		}
	}
	an, _ := Analyze("TIMESUM(x, \"Q1\", 2026) + TIMESUM(y)")
	if len(an.TimeSums) != 2 || !an.TimeSums[0].Ranged || an.TimeSums[1].Ranged {
		t.Fatalf("time sums: %+v", an.TimeSums)
	}
	if code, ok := an.TimeSums[0].Start.LiteralCode(); !ok || code != "Q1" {
		t.Errorf("literal start: %q", code)
	}
	if code, ok := an.TimeSums[0].End.LiteralCode(); !ok || code != "2026" {
		t.Errorf("literal end: %q", code)
	}
}
