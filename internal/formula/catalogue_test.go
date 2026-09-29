package formula

import (
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// The function catalogue: every registered function with its normal,
// boundary and error behaviour, and the combinations a developer writes in
// practice. TestCatalogueCoversEveryFunction fails when a function is
// registered without a case here, so the catalogue — and the formulas
// manual built from it (docs/formulas-manual) — cannot silently fall behind
// the engine.

type want struct {
	kind byte // 'n' number, 's' string, 'b' bool, '_' blank, 'e' error
	num  float64
	str  string
	b    bool
	code string
}

func num(x float64) want     { return want{kind: 'n', num: x} }
func text(s string) want     { return want{kind: 's', str: s} }
func boolean(b bool) want    { return want{kind: 'b', b: b} }
func blankV() want           { return want{kind: '_'} }
func fails(code string) want { return want{kind: 'e', code: code} }

type catCase struct {
	formula string
	want    want
}

func (w want) check(t *testing.T, label string, v Value) {
	t.Helper()
	switch w.kind {
	case 'e':
		if !v.IsError() || v.Err().Code != w.code {
			t.Errorf("%s: want error %s, got %v (%v)", label, w.code, v, describe(v))
		}
		return
	}
	if v.IsError() {
		t.Errorf("%s: unexpected error %v", label, v.Err())
		return
	}
	switch w.kind {
	case 'n':
		n, _ := v.Number()
		if v.Kind() != KindNumber || math.Abs(n-w.num) > 1e-9*math.Max(1, math.Abs(w.num)) {
			t.Errorf("%s: want %v, got %v (%v)", label, w.num, v, describe(v))
		}
	case 's':
		if v.Kind() != KindString || v.String() != w.str {
			t.Errorf("%s: want %q, got %v (%v)", label, w.str, v, describe(v))
		}
	case 'b':
		if v.Kind() != KindBool || v.Bool() != w.b {
			t.Errorf("%s: want %v, got %v (%v)", label, w.b, v, describe(v))
		}
	case '_':
		if !v.IsBlank() {
			t.Errorf("%s: want blank, got %v (%v)", label, v, describe(v))
		}
	}
}

func describe(v Value) string {
	switch v.Kind() {
	case KindNumber:
		return "number"
	case KindString:
		return "text"
	case KindBool:
		return "boolean"
	case KindBlank:
		return "blank"
	case KindError:
	}
	return "error"
}

func d(y, m, day int) float64 { return serial(y, m, day) }

// scalarVars are the metric values the scalar catalogue reads: a blank
// (no recorded value), numbers and a text.
func scalarVars() map[string]Value {
	return map[string]Value{
		"blank_m": BlankVal(), "five": NumberVal(5), "neg": NumberVal(-3), "word": StringVal("abc"),
		"price": NumberVal(19.99), "qty": NumberVal(3), "revenue": NumberVal(1200), "cost": NumberVal(900),
		"zero": NumberVal(0),
	}
}

func scalarCatalogue() []catCase {
	return []catCase{
		// ── Syntax, operators and precedence ────────────────────────────
		{`1+2*3`, num(7)},
		{`=(1+2)*3`, num(9)},
		{`  revenue - cost  `, num(300)},
		{`{revenue} - {cost}`, num(300)},
		{`REVENUE - Cost`, num(300)},
		{`2^10`, num(1024)},
		{`2^3^2`, num(64)}, // left to right, as in Excel
		{`-2^2`, num(4)},   // unary minus binds tighter than ^, as in Excel
		{`2^-1`, num(0.5)},
		{`10/4`, num(2.5)},
		{`1/0`, fails("#DIV/0!")},
		{`(-8)^(1/3)`, fails("#NUM!")},
		{`1+"a"`, fails("#VALUE!")},
		{`TRUE+TRUE`, num(2)},
		{`blank_m + 1`, num(1)},
		{`1.5e3`, num(1500)},
		{`.5 * 4`, num(2)},
		{`"a" & "b" & 1`, text("ab1")},
		{`"Total " & 1234567`, text("Total 1234567")},
		{`"Share " & 0.25`, text("Share 0.25")},
		{`1+2 & 3`, text("33")}, // + before &
		{`"say ""hi"""`, text(`say "hi"`)},
		{`1+1 = 2`, boolean(true)}, // comparison last
		{`"a" & "b" = "AB"`, boolean(true)},
		{`3 > 2`, boolean(true)},
		{`2 >= 2`, boolean(true)},
		{`1 <= 0`, boolean(false)},
		{`1 <> 2`, boolean(true)},
		{`"abc" = "ABC"`, boolean(true)}, // text compares ignoring case
		{`"apple" < "Banana"`, boolean(true)},
		{`1 = "1"`, boolean(true)}, // a number against text compares as text
		{`blank_m = 0`, boolean(true)},
		{`blank_m = ""`, boolean(true)},
		{`ROUND(1.2345; 2)`, num(1.23)}, // ; separates arguments too

		// ── Logic ───────────────────────────────────────────────────────
		{`IF(TRUE, 1, 2)`, num(1)},
		{`IF(0, 1, 2)`, num(2)},
		{`IF(FALSE, 1)`, boolean(false)},
		{`IF(revenue = 0, 0, (revenue - cost) / revenue)`, num(0.25)},
		{`IF(zero = 0, 0, revenue / zero)`, num(0)}, // the untaken branch is never evaluated
		{`IF(1/0 > 0, 1, 2)`, fails("#DIV/0!")},
		{`IF(1)`, fails("#VALUE!")},
		{`IFS(revenue > 2000, "high", revenue > 1000, "mid", TRUE, "low")`, text("mid")},
		{`IFS(FALSE, 1)`, fails("#N/A")},
		{`IFS(TRUE)`, fails("#VALUE!")},
		{`AND(TRUE, 1, 2 > 1)`, boolean(true)},
		{`AND(TRUE, FALSE)`, boolean(false)},
		{`AND()`, fails("#VALUE!")},
		{`OR(FALSE, 0)`, boolean(false)},
		{`OR(FALSE, qty > 2)`, boolean(true)},
		{`OR(TRUE, 1/0)`, boolean(true)}, // stops at the first TRUE
		{`NOT(0)`, boolean(true)},
		{`NOT(five)`, boolean(false)},
		{`NOT(1, 2)`, fails("#VALUE!")},
		{`IFERROR(revenue / zero, -1)`, num(-1)},
		{`IFERROR(revenue / five, -1)`, num(240)},
		{`IFERROR(1/0)`, fails("#VALUE!")},
		{`IFNA(IFS(FALSE, 1), 7)`, num(7)},
		{`IFNA(1/0, 7)`, fails("#DIV/0!")}, // only #N/A is replaced
		{`SWITCH(2, 1, "one", 2, "two")`, text("two")},
		{`SWITCH(3, 1, "one", 2, "two", "other")`, text("other")},
		{`SWITCH(3, 1, "one", 2, "two")`, fails("#N/A")},
		{`SWITCH("b", "A", 1, "B", 2)`, num(2)},
		{`SWITCH(1, 2)`, fails("#VALUE!")},

		// ── Math ────────────────────────────────────────────────────────
		{`ABS(-3.5)`, num(3.5)},
		{`ABS(word)`, fails("#VALUE!")},
		{`INT(7.8)`, num(7)},
		{`INT(-7.2)`, num(-8)},
		{`ROUND(3.14159, 2)`, num(3.14)},
		{`ROUND(2.5, 0)`, num(3)},
		{`ROUND(-2.5, 0)`, num(-3)}, // half away from zero
		{`ROUND(1234.5, -2)`, num(1200)},
		{`ROUND(1.005, 2)`, num(1.01)},
		{`ROUND(2.675, 2)`, num(2.68)},
		{`ROUND(price * qty, 1)`, num(60)},
		{`ROUND(1.5)`, fails("#VALUE!")},
		{`ROUNDUP(3.111, 2)`, num(3.12)},
		{`ROUNDUP(-3.111, 2)`, num(-3.12)}, // away from zero
		{`ROUNDUP(0.1 + 0.2, 1)`, num(0.3)},
		{`ROUNDUP(1234, -2)`, num(1300)},
		{`ROUNDDOWN(3.199, 2)`, num(3.19)},
		{`ROUNDDOWN(-3.199, 2)`, num(-3.19)}, // toward zero
		{`ROUNDDOWN(1299, -2)`, num(1200)},
		{`ROUNDDOWN(0.7 * 3, 1)`, num(2.1)},
		{`CEILING(22, 5)`, num(25)},
		{`CEILING(-22, 5)`, num(-20)},
		{`CEILING(0.1 + 0.2, 0.1)`, num(0.3)},
		{`CEILING(5, 0)`, fails("#DIV/0!")},
		{`FLOOR(22, 5)`, num(20)},
		{`FLOOR(-22, 5)`, num(-25)},
		{`FLOOR(5, 0)`, fails("#DIV/0!")},
		{`MOD(23, 5)`, num(3)},
		{`MOD(-7, 3)`, num(2)}, // the sign of the divisor
		{`MOD(7, -3)`, num(-2)},
		{`MOD(5, 0)`, fails("#DIV/0!")},
		{`POWER(2, 10)`, num(1024)},
		{`POWER(1.05, 2)`, num(1.1025)},
		{`POWER(-8, 1/3)`, fails("#NUM!")},
		{`SQRT(144)`, num(12)},
		{`SQRT(-1)`, fails("#NUM!")},

		// ── Aggregate over arguments ────────────────────────────────────
		{`SUM(1, 2, 3)`, num(6)},
		{`SUM(revenue, cost, blank_m)`, num(2100)},
		{`SUM(1, TRUE)`, num(2)},
		{`SUM(1, word)`, fails("#VALUE!")},
		{`SUM()`, fails("#VALUE!")},
		{`AVERAGE(2, 4, blank_m)`, num(3)}, // a blank is left out
		{`AVERAGE(blank_m)`, fails("#DIV/0!")},
		{`MIN(3, 1, 2)`, num(1)},
		{`MIN(blank_m)`, num(0)},
		{`MAX(3, neg, 2)`, num(3)},
		{`MAX(0, revenue - cost - 500)`, num(0)},
		{`COUNT(1, word, TRUE, 1/0, blank_m)`, num(2)},
		{`COUNTA(1, word, "", 1/0, blank_m)`, num(3)},

		// ── Text ────────────────────────────────────────────────────────
		{`CONCAT("a", "b", 1)`, text("ab1")},
		{`CONCAT("Q", 1.5)`, text("Q1.5")},
		{`TEXTJOIN("-", TRUE, "a", "", "b")`, text("a-b")},
		{`TEXTJOIN("-", FALSE, "a", "", "b")`, text("a--b")},
		{`TEXTJOIN("-", TRUE)`, fails("#VALUE!")},
		{`LEN("héllo")`, num(5)},
		{`LEN(12345)`, num(5)},
		{`LEN(1234567)`, num(7)},
		{`LEFT("abc")`, text("a")},
		{`LEFT("abc", 2)`, text("ab")},
		{`LEFT("abc", 10)`, text("abc")},
		{`LEFT("abc", -1)`, fails("#VALUE!")},
		{`RIGHT("abc")`, text("c")},
		{`RIGHT("abc", 2)`, text("bc")},
		{`RIGHT("abc", -1)`, fails("#VALUE!")},
		{`MID("abcdef", 2, 3)`, text("bcd")},
		{`MID("abc", 5, 2)`, text("")},
		{`MID("abc", 2, -1)`, fails("#VALUE!")},
		{`MID("abc", 0, 2)`, fails("#VALUE!")},
		{`UPPER("aBc")`, text("ABC")},
		{`LOWER("aBc")`, text("abc")},
		{`TRIM("  a   b  ")`, text("a b")},
		{`TEXT(3.14159, "0.00")`, text("3.14")},
		{`TEXT(3.6, "0")`, text("4")},
		{`TEXT(1.23456, "0.000")`, text("1.235")},
		{`TEXT(DATE(2026, 3, 5), "yyyy-mm-dd")`, text("2026-03-05")},
		{`TEXT(DATE(2026, 3, 5), "DD/MM/YYYY")`, text("05/03/2026")},
		{`TEXT(word, "0.00")`, text("abc")},
		{`SUBSTITUTE("a-b-c", "-", "+")`, text("a+b+c")},
		{`SUBSTITUTE("a-b-c", "-", "+", 2)`, text("a-b+c")},
		{`SUBSTITUTE("abc", "", "x")`, text("abc")},
		{`SUBSTITUTE("abc", "", "x", 1)`, text("abc")},
		{`SUBSTITUTE("abc", "b", "x", 0)`, fails("#VALUE!")},

		// ── Dates (serial numbers, day 1 = 1900-01-01 as in Excel) ─────
		{`DATE(2026, 3, 5)`, num(d(2026, 3, 5))},
		{`DATE(1900, 3, 1)`, num(61)},
		{`DATE(2026, 13, 1)`, num(d(2027, 1, 1))},
		{`DATE(2026, 3, 0)`, num(d(2026, 2, 28))},
		{`YEAR(DATE(2026, 3, 5))`, num(2026)},
		{`MONTH(DATE(2026, 3, 5))`, num(3)},
		{`DAY(DATE(2026, 3, 5))`, num(5)},
		{`YEAR(word)`, fails("#VALUE!")},
		{`DAYS(DATE(2026, 3, 1), DATE(2026, 2, 1))`, num(28)},
		{`EDATE(DATE(2026, 3, 15), -2)`, num(d(2026, 1, 15))},
		{`EDATE(DATE(2026, 1, 31), 1)`, num(d(2026, 2, 28))},
		{`EDATE(DATE(2024, 1, 31), 1)`, num(d(2024, 2, 29))},
		{`EOMONTH(DATE(2026, 1, 15), 0)`, num(d(2026, 1, 31))},
		{`EOMONTH(DATE(2026, 1, 31), 0)`, num(d(2026, 1, 31))},
		{`EOMONTH(DATE(2026, 1, 30), 1)`, num(d(2026, 2, 28))},
		{`EOMONTH(DATE(2024, 1, 31), 1)`, num(d(2024, 2, 29))},
		{`EOMONTH(DATE(2026, 3, 31), -1)`, num(d(2026, 2, 28))},
		{`DAYSINMONTH(2024, 2)`, num(29)},
		{`DAYSINMONTH(2026, 13)`, fails("#NUM!")},
		{`DAYSINYEAR(2024)`, num(366)},
		{`DAYSINYEAR(2026)`, num(365)},
		{`TODAY(1)`, fails("#VALUE!")},
		{`DATE(2026, 3)`, fails("#VALUE!")},

		// ── Combinations ────────────────────────────────────────────────
		{`IFERROR(ROUND((revenue - cost) / revenue * 100, 1), 0)`, num(25)},
		{`IF(AND(revenue > 1000, cost < revenue), MAX(revenue - cost, 0), 0)`, num(300)},
		{`ROUNDUP(revenue / 7, 0) * 7`, num(1204)},
		{`ROUNDUP(qty / 12, 0) * 12`, num(12)},
		{`CEILING(qty, 12)`, num(12)},
		{`SWITCH(MONTH(DATE(2026, 5, 1)), 1, 0.1, 5, 0.25, 0)`, num(0.25)},
		{`IF(LEFT(word, 1) = "A", LEN(word), 0)`, num(3)},
		{`DAYS(EOMONTH(DATE(2026, 2, 10), 0), DATE(2026, 2, 1)) + 1`, num(28)},
		{`YEAR(EDATE(DATE(2026, 11, 30), 3))`, num(2027)},
		{`IF(ABS(0.1 + 0.2 - 0.3) < 0.000001, 1, 0)`, num(1)},
		{`ROUND(0.1 + 0.2, 2) = 0.3`, boolean(true)},
		{`0.1 + 0.2 = 0.3`, boolean(true)}, // numbers compare on 15 significant digits, as in Excel
		{`0.1 + 0.2 > 0.3`, boolean(false)},
		{`0.3 >= 0.1 + 0.2`, boolean(true)},
		{`1 / 3 * 3 = 1`, boolean(true)},
		{`0.1 + 0.2 - 0.3 = 0`, boolean(false)},  // the difference itself is 5.55e-17, not 0 (Excel agrees)
		{`1.00000000000001 = 1`, boolean(false)}, // a difference within 15 digits still counts
		{`1.000000000000001 = 1`, boolean(true)}, // the 16th digit does not, as in Excel
		{`SWITCH(0.1 + 0.2, 0.3, "hit", "miss")`, text("hit")},
		{`"" & (0.1 + 0.2)`, text("0.3")},
		{`IFS(MOD(qty, 2) = 0, "even", TRUE, "odd") & "-" & UPPER(word)`, text("odd-ABC")},
		{`SUM(IF(five > 3, 10, 0), IFERROR(1/zero, 5), POWER(2, 2))`, num(19)},
	}
}

func TestCatalogueScalar(t *testing.T) {
	for _, c := range scalarCatalogue() {
		c := c
		t.Run(c.formula, func(t *testing.T) {
			done := make(chan Value, 1)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s: panicked: %v", c.formula, r)
						done <- ErrorVal(&FormulaError{Code: "PANIC"})
					}
				}()
				v, err := Eval(c.formula, scalarVars())
				if err != nil {
					v = ErrorVal(&FormulaError{Code: "PARSE", Message: err.Error()})
				}
				done <- v
			}()
			select {
			case v := <-done:
				c.want.check(t, c.formula, v)
			case <-time.After(2 * time.Second):
				t.Fatalf("%s: did not finish in 2s", c.formula)
			}
		})
	}
}

func TestCatalogueToday(t *testing.T) {
	now := time.Now().UTC()
	v, err := Eval(`TODAY()`, nil)
	if err != nil {
		t.Fatal(err)
	}
	num(d(now.Year(), int(now.Month()), now.Day())).check(t, "TODAY()", v)
	v, _ = Eval(`YEAR(TODAY()) >= 2026`, nil)
	boolean(true).check(t, "YEAR(TODAY())", v)
}

// TestCatalogueParseErrors: what the parser refuses outright (the metric
// save shows the message).
func TestCatalogueParseErrors(t *testing.T) {
	for _, f := range []string{
		`1 +`, `(1 + 2`, `1 + 2)`, `SUM(1, 2`, `"unterminated`, `IF(x = "a, 1, 0)`, `{unterminated`,
		`revenue cost`, `50%`, `region.a.b`, `region.2026`, `x.y(1)`, ``, `=`,
	} {
		if _, err := Parse(f); err == nil {
			t.Errorf("%q: want a parse error", f)
		}
	}
}

// ── Time series ────────────────────────────────────────────────────────────

func timeCatalogue() []tsCase {
	// year2026: SALES = 1..12 (Jan..Dec 2026, fiscal year from January);
	// BAL last-summary, RATE average-summary with Feb missing, FLAT none,
	// OFFSETS per month {1,2,3,1,1.5,0,-1,0,...}.
	return []tsCase{
		{formula: `PREVIOUS(sales)`, pos: 0, num: 0},
		{formula: `PREVIOUS(sales)`, pos: 5, num: 5},
		{formula: `NEXT(sales)`, pos: 0, num: 2},
		{formula: `NEXT(sales)`, pos: 11, num: 0},
		{formula: `LAG(sales, 2, -1)`, pos: 1, num: -1},
		{formula: `LAG(sales, 2, -1)`, pos: 5, num: 4},
		{formula: `LAG(sales, -1, 0)`, pos: 0, num: 2},
		{formula: `LAG(sales, -1, 99, SEMISTRICT)`, pos: 0, num: 99},
		{formula: `LAG(sales, 0, 99, STRICT)`, pos: 4, num: 99},
		{formula: `LAG(sales, 0, 99, SEMISTRICT)`, pos: 4, num: 5},
		{formula: `LAG(sales, 1, 0, FOO)`, pos: 4, code: "#VALUE!"},
		{formula: `LEAD(sales, 1, 0)`, pos: 10, num: 12},
		{formula: `LEAD(sales, 1, sales)`, pos: 11, num: 12}, // substitute at the current period
		{formula: `OFFSET(sales, -2, 0)`, pos: 5, num: 4},
		{formula: `OFFSET(sales, 3, -1)`, pos: 10, num: -1},
		{formula: `LAG(sales, 1)`, pos: 3, code: "#VALUE!"},
		{formula: `MOVINGSUM(sales)`, pos: 3, num: 78},
		{formula: `MOVINGSUM(sales, 1)`, pos: 5, num: 57},
		{formula: `MOVINGSUM(sales, -2, 0)`, pos: 5, num: 15},
		{formula: `MOVINGSUM(sales, -2, 0)`, pos: 0, num: 1},
		{formula: `MOVINGSUM(sales, -2, 0, AVERAGE)`, pos: 5, num: 5},
		{formula: `MOVINGSUM(sales, -2, 0, MIN)`, pos: 5, num: 4},
		{formula: `MOVINGSUM(sales, 1, 3, MAX)`, pos: 5, num: 9},
		{formula: `MOVINGSUM(sales, 1, 0)`, pos: 3, num: 0},
		{formula: `MOVINGSUM(sales, offsets, 0)`, pos: 3, code: CodeMovingWindowNotLiteral},
		{formula: `MOVINGSUM(sales, -1.5, 0)`, pos: 3, code: CodeTimeOffsetNotInteger},
		{formula: `CUMULATE(sales)`, pos: 3, num: 10},
		{formula: `CUMULATE(sales, MONTH(START()) = 7)`, pos: 8, num: 24},
		{formula: `CUMULATE(sales, MONTH(START()) = 7)`, pos: 5, num: 21},
		{formula: `DECUMULATE(sales)`, pos: 0, num: 1},
		{formula: `DECUMULATE(CUMULATE(sales))`, pos: 6, num: 7},
		{formula: `QUARTERTODATE(sales)`, pos: 4, num: 9},
		{formula: `HALFYEARTODATE(sales)`, pos: 8, num: 24},
		{formula: `YEARTODATE(sales)`, pos: 11, num: 78},
		{formula: `MONTHTODATE(sales)`, pos: 2, code: CodeInvalidTimeMember},
		{formula: `YEARVALUE(sales)`, pos: 3, num: 78},
		{formula: `QUARTERVALUE(sales)`, pos: 4, num: 15},
		{formula: `HALFYEARVALUE(sales)`, pos: 0, num: 21},
		{formula: `MONTHVALUE(sales)`, pos: 0, code: CodeInvalidTimeMember},
		{formula: `YEARVALUE(bal)`, pos: 0, num: 15},
		{formula: `YEARVALUE(rate)`, pos: 0, num: (1 + 75.0) / 11 * 1},
		{formula: `YEARVALUE(flat)`, pos: 0, blank: true},
		{formula: `YEARVALUE(sales * 2)`, pos: 0, code: CodeSourceMustBeMetric},
		{formula: `TIMESUM(sales)`, pos: 0, num: 78},
		{formula: `TIMESUM(sales, "Q1", "Q2")`, pos: 7, num: 21},
		{formula: `TIMESUM(sales, "Q2", "Q1")`, pos: 7, num: 0},
		{formula: `TIMESUM(sales, "H2", "FY26", MAX)`, pos: 0, num: 12},
		{formula: `TIMESUM(sales, "Q1", "Q1", AVERAGE)`, pos: 0, num: 2},
		{formula: `TIMESUM(sales, "2026-02", "2026-02")`, pos: 9, num: 2},
		{formula: `TIMESUM(sales, "Q9", "Q1")`, pos: 0, code: "#N/A"},
		{formula: `TIMESUM(sales, "Q1")`, pos: 0, code: "#VALUE!"},
		{formula: `START()`, pos: 2, num: d(2026, 3, 1)},
		{formula: `END()`, pos: 1, num: d(2026, 2, 28)},
		{formula: `START(1)`, pos: 1, code: "#VALUE!"},
		{formula: `LAG(sales, offsets, 0)`, pos: 3, num: 3},
		{formula: `LAG(sales, offsets, 0)`, pos: 4, code: CodeTimeOffsetNotInteger},
		{formula: `LAG(sales, offsets, 0)`, pos: 6, num: 8},
		{formula: `LAG(sales, 1.5, 0)`, pos: 3, code: CodeTimeOffsetNotInteger},

		// Combinations.
		{formula: `PREVIOUS(LAG(sales, 2, 0))`, pos: 5, num: 3},
		{formula: `LAG(MOVINGSUM(sales, -1, 0), 1, 0)`, pos: 3, num: 5},
		{formula: `PREVIOUS(CUMULATE(sales))`, pos: 3, num: 6},
		{formula: `IF(PREVIOUS(sales) = 0, 0, sales / PREVIOUS(sales) - 1)`, pos: 0, num: 0},
		{formula: `IF(PREVIOUS(sales) = 0, 0, sales / PREVIOUS(sales) - 1)`, pos: 1, num: 1},
		{formula: `ROUND(sales / YEARVALUE(sales) * 100, 2)`, pos: 11, num: 15.38},
		{formula: `YEARTODATE(sales) - PREVIOUS(YEARTODATE(sales))`, pos: 5, num: 6},
		{formula: `YEARTODATE(sales) / TIMESUM(sales)`, pos: 5, num: 21.0 / 78},
		{formula: `IFERROR(MONTHTODATE(sales), -1)`, pos: 3, num: -1},
		{formula: `IFNA(TIMESUM(sales, "Q9", "Q1"), -1)`, pos: 3, num: -1},
		{formula: `DAYS(END(), START()) + 1`, pos: 1, num: 28},
		{formula: `DAYSINMONTH(YEAR(START()), MONTH(START()))`, pos: 1, num: 28},
		{formula: `sales / DAYSINMONTH(YEAR(END()), MONTH(END()))`, pos: 0, num: 1.0 / 31},
		{formula: `MOVINGSUM(IF(sales > 6, sales, 0), -2, 0)`, pos: 7, num: 7 + 8},
		{formula: `SUM(PREVIOUS(sales), sales, NEXT(sales)) / 3`, pos: 4, num: 5},
		{formula: `MAX(0, sales - LAG(sales, 3, sales))`, pos: 3, num: 3},
		{formula: `TIMESUM(sales * 2, "Q1", "Q1")`, pos: 11, num: 12},
		{formula: `LEAD(sales, 12 - MONTH(START()), 0)`, pos: 2, num: 12},
	}
}

func TestCatalogueTime(t *testing.T) {
	runTSCases(t, year2026(), timeCatalogue())
}

func TestCatalogueTimeNeedsContext(t *testing.T) {
	for _, f := range []string{
		`PREVIOUS(x)`, `LAG(x, 1, 0)`, `MOVINGSUM(x)`, `CUMULATE(x)`, `YEARTODATE(x)`, `YEARVALUE(x)`,
		`TIMESUM(x)`, `START()`, `END()`,
	} {
		v, err := Eval(f, map[string]Value{"x": NumberVal(1)})
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		fails(CodeTimeContextRequired).check(t, f, v)
	}
}

// ── Dimensional references, LOOKUP and conditional aggregation ────────────

func dimCatalogue() []catCase {
	// dimModel: region EMEA > DE (EUR, headcount 10, opened 2020-05-01),
	// FR (EUR, 4, 2019-01-15); AMER > US (USD, 7). Products A100, A200,
	// B100, star*code, 1000000. sales[region, product]: DE 10/20/5/-/3,
	// FR 1/-/2, US 100/-/50. fx_rate EUR 1.1, USD 1. Cell: DE × A100.
	return []catCase{
		{`region`, text("DE")},
		{`PARENT(region)`, text("EMEA")},
		{`region.currency`, text("EUR")},
		{`region.headcount`, num(10)},
		{`region.opened`, num(d(2020, 5, 1))},
		{`YEAR(region.opened)`, num(2020)},
		{`DAYS(DATE(2020, 6, 1), region.opened)`, num(31)},
		{`region.headcount * 2`, num(20)},
		{`IF(region = "de", 1, 0)`, num(1)},
		{`IF(PARENT(region) = "EMEA", 1, 0)`, num(1)},
		{`region.nope`, fails(CodeUnknownProperty)},
		{`PARENT(region.currency)`, fails(CodeDimensionArgRequired)},
		{`LOOKUP(sales, region, "FR")`, num(1)},
		{`LOOKUP(sales, region, PARENT(region))`, num(11)},
		{`LOOKUP(sales, region, "US", product, "B100")`, num(50)},
		{`LOOKUP(sales, product, 1000000)`, num(3)},
		{`LOOKUP(fx_rate, currency, region.currency)`, num(1.1)},
		{`LOOKUP(sales, region, "XX")`, fails("#N/A")},
		{`LOOKUP(sales, region, "de")`, fails("#N/A")},
		{`IFNA(LOOKUP(sales, region, "XX"), 0)`, num(0)},
		{`LOOKUP(sales, region, "FR", region, "US")`, fails("#VALUE!")},
		{`LOOKUP(sales, region)`, fails("#VALUE!")},
		{`LOOKUP(1 + 1, region, "DE")`, fails(CodeSourceMustBeMetric)},
		{`LOOKUP(sales, region.currency, "EUR")`, fails(CodeDimensionArgRequired)},
		{`SUMIFS(sales, region.currency, "EUR")`, num(11)},
		{`SUMIFS(sales, region.currency, "EUR", product, "A*")`, num(31)},
		{`SUMIFS(sales, region.headcount, ">=7")`, num(110)},
		{`SUMIFS(sales, region.opened, ">=2020-01-01")`, num(10)},
		{`SUMIFS(sales, region.currency, region.currency)`, num(11)},
		{`SUMIFS(sales, region, "<>" & region)`, num(101)},
		{`COUNTIFS(region.headcount, 4 * (0.1 + 0.2) / 0.3)`, num(1)},        // 4.000000000000001 matches 4
		{`COUNTIFS(region.headcount, ">=" & (0.1 + 0.2) / 0.3 * 7)`, num(2)}, // ">=7", not ">=7.000000000000001": DE and US
		{`LOOKUP(sales, product, 0.1 * 3 * 10 / 3 * 1000000)`, num(3)},       // 1000000.0000000002 names member 1000000
		{`COUNTIF(product, "star~*code")`, num(1)},
		{`COUNTIF(product, "star*")`, num(1)},
		{`COUNTIF(product, "???0")`, num(3)},
		{`SUMIFS(sales, region, "*")`, num(111)},
		{`SUMIFS(sales, region, "Z*")`, num(0)},
		{`SUMIFS(sales, region.currency)`, fails("#VALUE!")},
		{`AVERAGEIFS(sales, region.currency, "EUR")`, num(5.5)},
		{`AVERAGEIFS(sales, region.currency, "GBP")`, fails("#DIV/0!")},
		{`IFERROR(AVERAGEIFS(sales, region.currency, "GBP"), 0)`, num(0)},
		{`MINIFS(sales, region.currency, "EUR")`, num(1)},
		{`MAXIFS(sales, region.currency, "EUR")`, num(10)},
		{`MAXIFS(sales, region.currency, "GBP")`, num(0)},
		{`COUNTIFS(region.currency, "EUR")`, num(2)},
		{`COUNTIFS(region.currency, "<>")`, num(3)},
		{`COUNTIFS(region.currency, "EUR", product, "A*")`, num(4)},
		{`COUNTIF(product, "A*")`, num(2)},
		{`COUNTIF(product, "<>A100")`, num(4)},
		{`SUMIF(region.currency, "USD", sales)`, num(100)},
		{`AVERAGEIF(product, "B*", sales)`, num(5)},
		{`SUMIF(region.currency, "USD")`, fails("#VALUE!")},

		// Combinations.
		{`LOOKUP(sales, region, "DE") / SUMIFS(sales, region, "*")`, num(10.0 / 111)},
		{`ROUND(LOOKUP(sales, region, PARENT(region)) * LOOKUP(fx_rate, currency, region.currency), 2)`, num(12.1)},
		{`SUMIFS(sales, region.currency, region.currency) / COUNTIFS(region.currency, region.currency)`, num(5.5)},
		{`IF(region.headcount >= 10, LOOKUP(sales, product, "A200"), 0)`, num(20)},
		{`SUMIFS(sales, product, LEFT(product, 1) & "*")`, num(30)},
		{`IFNA(LOOKUP(sales, region, UPPER("fr")), -1)`, num(1)},
		{`SWITCH(region.currency, "EUR", 1.1, "USD", 1, 0) * LOOKUP(sales, product, "A200")`, num(22)},
		{`SUMIFS(sales, region, "<>" & region) - SUMIF(region.currency, "USD", sales)`, num(1)},
		{`MAX(0, MAXIFS(sales, region.currency, "EUR") - MINIFS(sales, region.currency, "EUR"))`, num(9)},
	}
}

func TestCatalogueDimensional(t *testing.T) {
	m := dimModel()
	for _, c := range dimCatalogue() {
		ctx := m.ctx(map[string]string{"region": "DE", "product": "A100"})
		c.want.check(t, c.formula, evalIn(t, ctx, c.formula))
	}
}

func TestCatalogueDimensionalAtTotal(t *testing.T) {
	// Region is not pinned: a total or a slice by product.
	m := dimModel()
	for _, c := range []catCase{
		{`region`, blankV()},
		{`region.currency`, blankV()},
		{`PARENT(region)`, blankV()},
		{`IF(region = "EMEA", 1, 0)`, num(0)},
		{`IF(region = "", 1, 0)`, num(1)},
		{`SUMIFS(sales, region.currency, "EUR")`, num(11)},
		{`LOOKUP(sales, region, "US")`, num(100)},
	} {
		ctx := m.ctx(map[string]string{"product": "A100"})
		c.want.check(t, c.formula, evalIn(t, ctx, c.formula))
	}
}

func TestCatalogueDimensionalNeedsContext(t *testing.T) {
	for _, f := range []string{
		`LOOKUP(sales, region, "DE")`, `SUMIFS(sales, region, "DE")`, `COUNTIF(region, "DE")`,
		`PARENT(region)`, `region.currency`,
	} {
		v, err := Eval(f, map[string]Value{"sales": NumberVal(1)})
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		fails(CodeDimContextRequired).check(t, f, v)
	}
}

// TestCatalogueArgumentCounts: every function refuses a wrong number of
// arguments with #VALUE!, never a silent result.
func TestCatalogueArgumentCounts(t *testing.T) {
	scalar := []string{
		`IF()`, `IF(1,2,3,4)`, `IFS(TRUE)`, `NOT()`, `IFERROR(1)`, `IFNA(1)`, `SWITCH(1,2)`,
		`ABS()`, `ABS(1,2)`, `INT()`, `ROUND(1)`, `ROUNDUP(1)`, `ROUNDDOWN(1)`, `CEILING(1)`, `FLOOR(1)`,
		`MOD(1)`, `POWER(1)`, `SQRT()`, `SUM()`, `AVERAGE()`, `MIN()`, `MAX()`, `COUNT()`, `COUNTA()`,
		`CONCAT()`, `TEXTJOIN(",", TRUE)`, `LEN()`, `LEFT()`, `LEFT("a",1,2)`, `RIGHT()`, `MID("a",1)`,
		`UPPER()`, `LOWER()`, `TRIM()`, `TEXT(1)`, `SUBSTITUTE("a","b")`, `TODAY(1)`, `DATE(1,2)`,
		`YEAR()`, `MONTH()`, `DAY()`, `DAYS(1)`, `EDATE(1)`, `EOMONTH(1)`, `DAYSINMONTH(2026)`, `DAYSINYEAR()`,
	}
	for _, f := range scalar {
		v, err := Eval(f, nil)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		fails("#VALUE!").check(t, f, v)
	}
	f := year2026()
	for _, text := range []string{
		`PREVIOUS()`, `NEXT(sales, 1)`, `LAG(sales)`, `LEAD(sales, 1)`, `OFFSET(sales, 1, 0, STRICT)`,
		`MOVINGSUM()`, `MOVINGSUM(sales, 1, 2, SUM, 5)`, `CUMULATE()`, `DECUMULATE(sales, 1)`,
		`MONTHTODATE()`, `QUARTERTODATE()`, `HALFYEARTODATE()`, `YEARTODATE(sales, 1)`,
		`MONTHVALUE()`, `QUARTERVALUE()`, `HALFYEARVALUE()`, `YEARVALUE(sales, 1)`, `TIMESUM()`, `TIMESUM(sales, "Q1")`,
		`START(1)`, `END(1)`,
	} {
		fails("#VALUE!").check(t, text, f.eval(t, text, 3))
	}
	m := dimModel()
	for _, text := range []string{
		`PARENT()`, `PARENT(region, product)`, `LOOKUP(sales)`, `LOOKUP(sales, region)`,
		`SUMIFS(sales)`, `SUMIFS(sales, region)`, `AVERAGEIFS(sales, region)`, `MINIFS(sales)`, `MAXIFS(sales, region)`,
		`COUNTIFS()`, `COUNTIFS(region)`, `SUMIF(region, "DE")`, `AVERAGEIF(region)`, `COUNTIF(region)`,
	} {
		fails("#VALUE!").check(t, text, evalIn(t, m.ctx(map[string]string{"region": "DE", "product": "A100"}), text))
	}
}

// TestArityMatchesEvaluator: the save-time arity table and the evaluator's
// own argument checks agree, and every scalar built-in is in the table.
func TestArityMatchesEvaluator(t *testing.T) {
	for _, name := range BuiltinNames() {
		_, ok := arity[name]
		if ok == (IsTimeFunction(name) || IsDimensionalFunction(name) || name == dimensionalFuncParent) {
			t.Errorf("%s: in the arity table %v, but time/dimensional %v", name, ok, !ok)
		}
	}
	call := func(name string, k int) string {
		args := make([]string, k)
		for i := range args {
			args[i] = "1"
		}
		return name + "(" + strings.Join(args, ", ") + ")"
	}
	countErr := func(v Value) bool {
		return v.IsError() && (strings.Contains(v.Err().Message, "wrong number of arguments") ||
			strings.Contains(v.Err().Message, "requires"))
	}
	for name, a := range arity {
		for k := 0; k <= 5; k++ {
			if name == "IFS" && k%2 != 0 {
				continue // pairs: covered by the IFS cases
			}
			v, err := Eval(call(name, k), nil)
			if err != nil {
				t.Fatalf("%s: %v", call(name, k), err)
			}
			wantRefused := k < a[0] || (a[1] >= 0 && k > a[1])
			if countErr(v) != wantRefused {
				t.Errorf("%s: evaluator refuses=%v, table refuses=%v (%v)", call(name, k), countErr(v), wantRefused, v)
			}
			if (CheckArguments(call(name, k)) != nil) != wantRefused {
				t.Errorf("%s: CheckArguments disagrees with the table", call(name, k))
			}
		}
	}
}

// TestCatalogueSaveTimeArity: a wrong number of arguments is refused when
// the formula is saved (CheckCalls), not discovered cell by cell later.
func TestCatalogueSaveTimeArity(t *testing.T) {
	for _, bad := range []string{
		`ROUND(x)`, `IF(x)`, `IF(1,2,3,4)`, `ABS(1, 2)`, `SUM()`, `LEFT("a", 1, 2)`, `MID("a", 1)`,
		`SWITCH(x, 1)`, `IFS(TRUE)`, `IFS(TRUE, 1, FALSE)`, `TEXTJOIN(",", TRUE)`, `TODAY(1)`, `DATE(2026, 1)`,
		`IF(x > 0, ROUND(x), 0)`, `DAYSINMONTH(2026)`, `SUBSTITUTE("a", "b")`,
		`SUMIFS(x, region)`, `PARENT()`, `COUNTIF(region)`, `LAG(x, 1)`, `START(1)`, `YEARVALUE()`, `LOOKUP(x, region)`,
	} {
		_, err := Analyze(bad)
		if err == nil {
			err = CheckArguments(bad)
		}
		if err == nil {
			t.Errorf("%s: want a save-time refusal", bad)
			continue
		}
		if !strings.Contains(err.Error(), "#VALUE!") {
			t.Errorf("%s: unexpected refusal %v", bad, err)
		}
	}
	for _, good := range []string{
		`ROUND(x, 2)`, `IF(x, 1)`, `IF(x, 1, 2)`, `SUM(1)`, `SUM(1, 2, 3, 4, 5)`, `LEFT("a")`, `SWITCH(x, 1, 2)`,
		`SWITCH(x, 1, 2, 3)`, `IFS(TRUE, 1)`, `TODAY()`, `TEXTJOIN(",", TRUE, "a")`, `LAG(x, 1, 0)`, `START()`,
	} {
		_, err := Analyze(good)
		if err == nil {
			err = CheckArguments(good)
		}
		if err != nil {
			t.Errorf("%s: unexpected refusal %v", good, err)
		}
	}
}

// TestCatalogueCoversEveryFunction: every registered function appears in
// the catalogue.
func TestCatalogueCoversEveryFunction(t *testing.T) {
	seen := map[string]bool{}
	var collect func(Node)
	collect = func(n Node) {
		switch x := n.(type) {
		case *CallExpr:
			seen[x.Name] = true
			for _, a := range x.Args {
				collect(a)
			}
		case *BinaryExpr:
			collect(x.Left)
			collect(x.Right)
		case *UnaryExpr:
			collect(x.Expr)
		}
	}
	add := func(f string) {
		if n, err := Parse(f); err == nil {
			collect(n)
		}
	}
	for _, c := range scalarCatalogue() {
		add(c.formula)
	}
	for _, c := range timeCatalogue() {
		add(c.formula)
	}
	for _, c := range dimCatalogue() {
		add(c.formula)
	}
	add(`TODAY()`)
	var missing []string
	for _, name := range BuiltinNames() {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("functions with no catalogue case: %s", strings.Join(missing, ", "))
	}
}

// TestEvalRecoversFromAPanic: a function that panics yields a #VALUE! cell,
// never a panic in the caller (recalculation runs in background goroutines).
func TestEvalRecoversFromAPanic(t *testing.T) {
	ctx := &EvalContext{Funcs: map[string]CustomFunc{
		"BOOM": func(*EvalContext, []Node) Value { panic("boom") },
	}}
	v, err := EvalWithContext(`1 + BOOM()`, ctx)
	if err != nil {
		t.Fatal(err)
	}
	fails("#VALUE!").check(t, "1 + BOOM()", v)
	node, _ := Parse(`BOOM()`)
	fails("#VALUE!").check(t, "EvalNode(BOOM())", EvalNode(ctx, node))
}

// TestExtractIdentsSkipsKeywords: the console's reference hint
// (/api/formula/refs) must not report a keyword argument as an unknown name.
func TestExtractIdentsSkipsKeywords(t *testing.T) {
	for text, want := range map[string]string{
		`MOVINGSUM(sales, -2, 0, AVERAGE)`:     "sales",
		`LAG(sales, 1, 0, STRICT)`:             "sales",
		`LEAD(sales, n, 0, SEMISTRICT)`:        "sales,n",
		`TIMESUM(sales, "Q1", "Q2", MAX)`:      "sales",
		`SUM(sales, average) + MOVINGSUM(max)`: "sales,average,max",
	} {
		got, err := ExtractIdents(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if strings.Join(got, ",") != want {
			t.Errorf("%s: refs %v, want %s", text, got, want)
		}
	}
}

// TestFormulasManualIndexMatchesEngine: the formulas manual's function index
// (docs/formulas-manual) lists exactly the registered functions.
func TestFormulasManualIndexMatchesEngine(t *testing.T) {
	raw, err := os.ReadFile("../../docs/formulas-manual/parts/80-index.html")
	if err != nil {
		t.Fatalf("read the manual's index: %v", err)
	}
	listed := map[string]bool{}
	for _, m := range regexp.MustCompile(`<tr><td>([A-Z]+)</td>`).FindAllStringSubmatch(string(raw), -1) {
		listed[m[1]] = true
	}
	for _, name := range BuiltinNames() {
		if !listed[name] {
			t.Errorf("the manual's index does not list %s", name)
		}
		delete(listed, name)
	}
	for name := range listed {
		t.Errorf("the manual's index lists %s, which the engine does not have", name)
	}
}
