package formula

import (
	"math"
	"testing"
)

// The statistical, logarithmic and exponential functions against the
// spreadsheet's values (Python's statistics.NormalDist for the normal
// distribution, exact to the last digit or two).
func TestStatisticalFunctions(t *testing.T) {
	cases := []struct {
		formula string
		want    float64
		code    string
		tol     float64
	}{
		{formula: "NORM.S.INV(0.98)", want: 2.053748910631822},
		{formula: "NORM.S.INV(0.95)", want: 1.6448536269514715},
		{formula: "NORMSINV(0.95)", want: 1.6448536269514715},
		{formula: "NORM.S.INV(0.999999)", want: 4.753424308817089},
		{formula: "NORM.S.INV(0.000000001)", want: -5.9978070150076865},
		{formula: "NORM.S.INV(0.5)", want: 0},
		{formula: "NORM.S.INV(0.999999999)", want: 5.997807019601638}, // the double nearest 0.999999999 is not 1 − 1e-9
		{formula: "norm.s.inv (0.98)", want: 2.053748910631822},       // any case, a space before "("
		{formula: "NORM.S.INV(0)", code: "#NUM!"},
		{formula: "NORM.S.INV(1)", code: "#NUM!"},
		{formula: "NORM.INV(0.9, 100, 15)", want: 119.22327348316901},
		{formula: "NORM.INV(0.9, 100, 0)", code: "#NUM!"},
		{formula: "NORM.S.DIST(1.96, TRUE)", want: 0.9750021048517796},
		{formula: "NORM.S.DIST(1.96, FALSE)", want: 0.058440944333451476},
		{formula: "NORMSDIST(1.96)", want: 0.9750021048517796},
		{formula: "NORM.DIST(110, 100, 15, TRUE)", want: 0.7475074624530771},
		{formula: "NORM.DIST(110, 100, 15, FALSE)", want: 0.02129653370149015},
		{formula: "NORM.S.INV(NORM.S.DIST(1.2345, TRUE))", want: 1.2345, tol: 1e-12},
		{formula: "LN(EXP(1))", want: 1},
		{formula: "EXP(1)", want: math.E},
		{formula: "LN(0)", code: "#NUM!"},
		{formula: "EXP(1000)", code: "#NUM!"},
		{formula: "LOG(1000)", want: 3},
		{formula: "LOG(8, 2)", want: 3},
		{formula: "LOG(10, 1)", code: "#DIV/0!"},
		{formula: "LOG(-1)", code: "#NUM!"},
		{formula: "LOG10(0.001)", want: -3},
		{formula: "STDEV.S(2, 4, 4, 4, 5, 5, 7, 9)", want: 2.138089935299395},
		{formula: "STDEV(2, 4, 4, 4, 5, 5, 7, 9)", want: 2.138089935299395},
		{formula: "STDEV.P(2, 4, 4, 4, 5, 5, 7, 9)", want: 2},
		{formula: "VAR.S(2, 4, 4, 4, 5, 5, 7, 9)", want: 4.571428571428571},
		{formula: "VAR.P(2, 4, 4, 4, 5, 5, 7, 9)", want: 4},
		{formula: "STDEV.S(5)", code: "#DIV/0!"},
		{formula: "STDEV.P(5)", want: 0},
		{formula: "LN(\"x\")", code: "#VALUE!"},
	}
	for _, c := range cases {
		v, err := Eval(c.formula, nil)
		if err != nil {
			t.Errorf("%s: %v", c.formula, err)
			continue
		}
		if c.code != "" {
			if !v.IsError() || v.Err().Code != c.code {
				t.Errorf("%s = %v, want %s", c.formula, v, c.code)
			}
			continue
		}
		n, ok := v.Number()
		tol := c.tol
		if tol == 0 {
			tol = 1e-14 * math.Max(1, math.Abs(c.want))
		}
		if !ok || v.IsError() || math.Abs(n-c.want) > tol {
			t.Errorf("%s = %v, want %v", c.formula, v, c.want)
		}
	}
}

// A dotted name stays a dimension.property reference unless it names a
// function with dots and a "(" follows.
func TestDottedFunctionNamesLeavePropertiesAlone(t *testing.T) {
	refs, err := ExtractRefs("region.factor * STDEV.S(a, b) + norm.s")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0] != "a" || refs[1] != "b" {
		t.Errorf("idents: %v, want [a b] (the properties are not identifiers)", refs)
	}
	an, err := Analyze("region.factor * STDEV.S(a, b) + norm.s")
	if err != nil {
		t.Fatal(err)
	}
	if len(an.PropertyRefs) != 2 {
		t.Errorf("property references: %+v, want region.factor and norm.s", an.PropertyRefs)
	}
	if err := CheckArguments("NORM.S.INV(1, 2)"); err == nil {
		t.Errorf("NORM.S.INV with two arguments was accepted")
	}
}
