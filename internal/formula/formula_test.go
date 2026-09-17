package formula

import (
	"math"
	"testing"
)

func TestArithmetic(t *testing.T) {
	cases := []struct {
		formula string
		vars    map[string]float64
		want    float64
	}{
		{"=1+2", nil, 3},
		{"=10-3", nil, 7},
		{"=2*3", nil, 6},
		{"=10/4", nil, 2.5},
		{"=2^10", nil, 1024},
		{"=-5", nil, -5},
		{"=a+b", map[string]float64{"a": 10, "b": 20}, 30},
		{"=revenue - cogs", map[string]float64{"revenue": 100, "cogs": 60}, 40},
		{"={revenue} - {cogs}", map[string]float64{"revenue": 100, "cogs": 60}, 40}, // legacy syntax
		{"=2*(3+4)", nil, 14},
		{"=(10-2)/4", nil, 2},
	}
	for _, c := range cases {
		t.Run(c.formula, func(t *testing.T) {
			got, err := EvalNumber(c.formula, c.vars)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if math.Abs(got-c.want) > 1e-9 {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestDivisionByZero(t *testing.T) {
	result, err := Eval("=1/0", nil)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !result.IsError() || result.Err().Code != "#DIV/0!" {
		t.Errorf("expected #DIV/0!, got %v", result)
	}
}

func TestComparisons(t *testing.T) {
	cases := []struct {
		formula string
		want    bool
	}{
		{"=1=1", true},
		{"=1=2", false},
		{"=1<>2", true},
		{"=1<2", true},
		{"=2>1", true},
		{"=1<=1", true},
		{"=2>=3", false},
		{`="a"="A"`, true}, // case-insensitive
	}
	for _, c := range cases {
		t.Run(c.formula, func(t *testing.T) {
			v, err := Eval(c.formula, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.Bool() != c.want {
				t.Errorf("got %v, want %v", v.Bool(), c.want)
			}
		})
	}
}

func TestIF(t *testing.T) {
	cases := []struct {
		formula string
		vars    map[string]Value
		want    string
	}{
		{`=IF(1=1,"yes","no")`, nil, "yes"},
		{`=IF(1=2,"yes","no")`, nil, "no"},
		{`=IF(revenue=0,0,profit/revenue)`,
			map[string]Value{"revenue": NumberVal(0), "profit": NumberVal(50)},
			"0"},
		{`=IF(revenue=0,0,profit/revenue)`,
			map[string]Value{"revenue": NumberVal(200), "profit": NumberVal(50)},
			"0.25"},
	}
	for _, c := range cases {
		t.Run(c.formula, func(t *testing.T) {
			v, err := EvalWithContext(c.formula, &EvalContext{Vars: c.vars})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.String() != c.want {
				t.Errorf("got %q, want %q", v.String(), c.want)
			}
		})
	}
}

func TestIFERROR(t *testing.T) {
	v, err := Eval(`=IFERROR(1/0, 0)`, nil)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	n, _ := v.Number()
	if n != 0 {
		t.Errorf("expected 0, got %v", n)
	}
}

func TestANDOR(t *testing.T) {
	cases := []struct {
		formula string
		want    bool
	}{
		{"=AND(1=1, 2=2)", true},
		{"=AND(1=1, 1=2)", false},
		{"=OR(1=2, 2=2)", true},
		{"=OR(1=2, 1=3)", false},
		{"=NOT(1=2)", true},
	}
	for _, c := range cases {
		t.Run(c.formula, func(t *testing.T) {
			v, err := Eval(c.formula, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.Bool() != c.want {
				t.Errorf("got %v, want %v", v.Bool(), c.want)
			}
		})
	}
}

func TestMath(t *testing.T) {
	cases := []struct {
		formula string
		want    float64
	}{
		{"=ABS(-5)", 5},
		{"=INT(3.9)", 3},
		{"=INT(-3.1)", -4},
		{"=ROUND(3.567, 2)", 3.57},
		{"=ROUNDUP(3.1, 0)", 4},
		{"=ROUNDDOWN(3.9, 0)", 3},
		{"=CEILING(2.1, 1)", 3},
		{"=FLOOR(2.9, 1)", 2},
		{"=MOD(10, 3)", 1},
		{"=POWER(2, 8)", 256},
		{"=SQRT(9)", 3},
	}
	for _, c := range cases {
		t.Run(c.formula, func(t *testing.T) {
			got, err := EvalNumber(c.formula, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if math.Abs(got-c.want) > 1e-9 {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestAggregation(t *testing.T) {
	cases := []struct {
		formula string
		want    float64
	}{
		{"=SUM(1,2,3)", 6},
		{"=AVERAGE(1,2,3)", 2},
		{"=MIN(5,3,8)", 3},
		{"=MAX(5,3,8)", 8},
		{"=COUNT(1,2,3)", 3},
	}
	for _, c := range cases {
		t.Run(c.formula, func(t *testing.T) {
			got, err := EvalNumber(c.formula, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if math.Abs(got-c.want) > 1e-9 {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestText(t *testing.T) {
	cases := []struct {
		formula string
		want    string
	}{
		{`=CONCAT("hello"," ","world")`, "hello world"},
		{`=LEN("hello")`, "5"},
		{`=LEFT("hello",3)`, "hel"},
		{`=RIGHT("hello",3)`, "llo"},
		{`=MID("hello",2,3)`, "ell"},
		{`=UPPER("hello")`, "HELLO"},
		{`=LOWER("HELLO")`, "hello"},
		{`=TRIM("  hello  world  ")`, "hello world"},
		{`=SUBSTITUTE("hello world","world","there")`, "hello there"},
		{`=TEXTJOIN("-",TRUE,"a","","b")`, "a-b"},
		{`=TEXTJOIN("-",FALSE,"a","","b")`, "a--b"},
		{`="hello"&" "&"world"`, "hello world"},
	}
	for _, c := range cases {
		t.Run(c.formula, func(t *testing.T) {
			v, err := Eval(c.formula, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.String() != c.want {
				t.Errorf("got %q, want %q", v.String(), c.want)
			}
		})
	}
}

func TestSWITCH(t *testing.T) {
	vars := map[string]Value{"dept": StringVal("sales")}
	v, err := EvalWithContext(`=SWITCH(dept,"sales",0.08,"finance",0.05,0.03)`, &EvalContext{Vars: vars})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n, _ := v.Number()
	if math.Abs(n-0.08) > 1e-9 {
		t.Errorf("expected 0.08, got %v", n)
	}
}

func TestExtractRefs(t *testing.T) {
	refs, err := ExtractRefs("=revenue - cogs + IF(department=\"sales\", bonus, 0)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	refSet := map[string]bool{}
	for _, r := range refs {
		refSet[r] = true
	}
	for _, expected := range []string{"revenue", "cogs", "department", "bonus"} {
		if !refSet[expected] {
			t.Errorf("missing ref %q in %v", expected, refs)
		}
	}
}

func TestFormFieldFormula(t *testing.T) {
	// net_amount = quantity * unit_price * (1 - discount_pct)
	vars := map[string]Value{
		"quantity":     NumberVal(10),
		"unit_price":   NumberVal(25),
		"discount_pct": NumberVal(0.1),
	}
	v, err := EvalWithContext("=quantity * unit_price * (1 - discount_pct)", &EvalContext{Vars: vars})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n, _ := v.Number()
	if math.Abs(n-225) > 1e-9 {
		t.Errorf("expected 225, got %v", n)
	}
}

func TestApprovalStatus(t *testing.T) {
	formula := `=IF(net_amount > 10000, "Needs Approval", "Auto Approved")`
	cases := []struct {
		netAmount float64
		want      string
	}{
		{15000, "Needs Approval"},
		{5000, "Auto Approved"},
	}
	for _, c := range cases {
		vars := map[string]Value{"net_amount": NumberVal(c.netAmount)}
		v, err := EvalWithContext(formula, &EvalContext{Vars: vars})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v.String() != c.want {
			t.Errorf("net_amount=%v: got %q, want %q", c.netAmount, v.String(), c.want)
		}
	}
}

func TestEvalNumber_BackwardCompat(t *testing.T) {
	// Ensure old-style {name} formulas still work
	got, err := EvalNumber("={revenue} - {cogs}", map[string]float64{
		"revenue": 500,
		"cogs":    200,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(got-300) > 1e-9 {
		t.Errorf("expected 300, got %v", got)
	}
}

func TestUnknownIdent(t *testing.T) {
	v, err := Eval("=unknown_var", nil)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !v.IsError() {
		t.Errorf("expected error value, got %v", v)
	}
	if v.Err().Code != "#NAME?" {
		t.Errorf("expected #NAME?, got %v", v.Err().Code)
	}
}

func TestIFS(t *testing.T) {
	formula := `=IFS(x>10,"big",x>5,"medium",x>0,"small")`
	cases := []struct {
		x    float64
		want string
	}{
		{15, "big"},
		{7, "medium"},
		{2, "small"},
	}
	for _, c := range cases {
		vars := map[string]Value{"x": NumberVal(c.x)}
		v, err := EvalWithContext(formula, &EvalContext{Vars: vars})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v.String() != c.want {
			t.Errorf("x=%v: got %q, want %q", c.x, v.String(), c.want)
		}
	}
}
