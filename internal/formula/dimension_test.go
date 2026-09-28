package formula

import (
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeModel is an in-memory stand-in for what the calculation package
// supplies through DimEvalContext: member metadata, typed properties and a
// source read that applies overrides to the cell and rolls a parent member
// up by summing its leaves.
type fakeModel struct {
	members    map[string][]fakeMember      // DIM -> members, dimension order
	propTypes  map[string]map[string]string // DIM -> PROP -> data_type
	metricDims map[string][]string          // METRIC -> DIMs
	data       map[string]float64           // key(metric, combo)
	resolves   int                          // Resolve calls, for the memo test
}

type fakeMember struct {
	code, parent string
	props        map[string]string // raw, property name as declared
}

func dimModel() *fakeModel {
	return &fakeModel{
		members: map[string][]fakeMember{
			"REGION": {
				{code: "EMEA", props: map[string]string{"headcount": "14"}},
				{code: "DE", parent: "EMEA", props: map[string]string{"currency": "EUR", "headcount": "10", "opened": "2020-05-01"}},
				{code: "FR", parent: "EMEA", props: map[string]string{"currency": "EUR", "headcount": "4", "opened": "2019-01-15"}},
				{code: "AMER"},
				{code: "US", parent: "AMER", props: map[string]string{"currency": "USD", "headcount": "7"}},
			},
			"PRODUCT": {
				{code: "A100"}, {code: "A200"}, {code: "B100"}, {code: "star*code"}, {code: "1000000"},
			},
			"CURRENCY": {{code: "EUR"}, {code: "USD"}},
		},
		propTypes: map[string]map[string]string{
			"REGION": {"CURRENCY": "text", "HEADCOUNT": "number", "OPENED": "date"},
		},
		metricDims: map[string][]string{
			"SALES":   {"REGION", "PRODUCT"},
			"FX_RATE": {"CURRENCY"},
		},
		data: map[string]float64{
			"SALES|DE|A100": 10, "SALES|DE|A200": 20, "SALES|DE|B100": 5, "SALES|DE|1000000": 3,
			"SALES|FR|A100": 1, "SALES|FR|B100": 2,
			"SALES|US|A100": 100, "SALES|US|B100": 50,
			"FX_RATE|EUR": 1.1, "FX_RATE|USD": 1,
		},
	}
}

func (m *fakeModel) find(dim, code string) *fakeMember {
	for i, mem := range m.members[strings.ToUpper(dim)] {
		if mem.code == code {
			return &m.members[strings.ToUpper(dim)][i]
		}
	}
	return nil
}

func (m *fakeModel) isLeaf(dim, code string) bool {
	for _, mem := range m.members[dim] {
		if mem.parent == code {
			return false
		}
	}
	return true
}

func (m *fakeModel) leavesUnder(dim, code string) []string {
	if m.isLeaf(dim, code) {
		return []string{code}
	}
	var out []string
	for _, mem := range m.members[dim] {
		if mem.parent == code {
			out = append(out, m.leavesUnder(dim, mem.code)...)
		}
	}
	return out
}

func (m *fakeModel) typed(dim string, mem *fakeMember, prop string) Value {
	for k, raw := range mem.props {
		if strings.EqualFold(k, prop) {
			return TypedPropertyValue(raw, m.propTypes[dim][strings.ToUpper(prop)])
		}
	}
	return BlankVal()
}

// ctx builds the evaluation context of one cell (dimension name -> code).
func (m *fakeModel) ctx(cell map[string]string) *EvalContext {
	up := map[string]string{}
	vars := map[string]Value{}
	for k, v := range cell {
		up[strings.ToUpper(k)] = v
		vars[strings.ToUpper(k)] = StringVal(v)
	}
	unknownDim := func(dim string) *FormulaError {
		if _, ok := m.members[strings.ToUpper(dim)]; !ok {
			return errName(dim)
		}
		return nil
	}
	d := &DimEvalContext{
		Current: func(dim string) (string, bool, *FormulaError) {
			if e := unknownDim(dim); e != nil {
				return "", false, e
			}
			code, ok := up[strings.ToUpper(dim)]
			return code, ok, nil
		},
		Property: func(dim, prop string) Value {
			if e := unknownDim(dim); e != nil {
				return ErrorVal(e)
			}
			D := strings.ToUpper(dim)
			if _, ok := m.propTypes[D][strings.ToUpper(prop)]; !ok {
				return ErrorVal(&FormulaError{Code: CodeUnknownProperty, Message: dim + " has no property " + prop})
			}
			code, ok := up[D]
			if !ok {
				return BlankVal()
			}
			return m.typed(D, m.find(D, code), prop)
		},
		Parent: func(dim string) (string, *FormulaError) {
			if e := unknownDim(dim); e != nil {
				return "", e
			}
			code, ok := up[strings.ToUpper(dim)]
			if !ok {
				return "", nil
			}
			return m.find(dim, code).parent, nil
		},
		Member: func(dim, code string) (string, bool, *FormulaError) {
			if e := unknownDim(dim); e != nil {
				return "", false, e
			}
			if mem := m.find(dim, code); mem != nil {
				return mem.code, true, nil
			}
			return "", false, nil
		},
		Leaves: func(dim string) ([]DimMember, *FormulaError) {
			if e := unknownDim(dim); e != nil {
				return nil, e
			}
			D := strings.ToUpper(dim)
			var out []DimMember
			for i := range m.members[D] {
				mem := &m.members[D][i]
				if !m.isLeaf(D, mem.code) {
					continue
				}
				props := map[string]Value{}
				for p := range m.propTypes[D] {
					props[p] = m.typed(D, mem, p)
				}
				out = append(out, DimMember{Code: mem.code, Properties: props})
			}
			return out, nil
		},
		Resolve: func(metric string, overrides map[string]string) (float64, bool, *FormulaError) {
			m.resolves++
			combo := map[string]string{}
			for k, v := range up {
				combo[k] = v
			}
			for k, v := range overrides {
				combo[strings.ToUpper(k)] = v
			}
			dims := m.metricDims[strings.ToUpper(metric)]
			if dims == nil {
				return 0, false, errName(metric)
			}
			// Expand every source dimension's pin to its leaves and sum.
			keys := []string{strings.ToUpper(metric)}
			for _, d := range dims {
				var next []string
				for _, prefix := range keys {
					for _, leaf := range m.leavesUnder(d, combo[d]) {
						next = append(next, prefix+"|"+leaf)
					}
				}
				keys = next
			}
			var sum float64
			found := false
			for _, k := range keys {
				if v, ok := m.data[k]; ok {
					sum += v
					found = true
				}
			}
			return sum, found, nil
		},
		CoordKey: func(exclude []string) string {
			var parts []string
			for k, v := range up {
				if !containsFold(exclude, k) {
					parts = append(parts, k+"="+v)
				}
			}
			sort.Strings(parts)
			return strings.Join(parts, ",")
		},
		Memo: map[string]Value{},
	}
	return &EvalContext{Vars: vars, Dim: d}
}

func evalIn(t *testing.T, ctx *EvalContext, text string) Value {
	t.Helper()
	node, err := Parse(text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return EvalNode(ctx, node)
}

func serial(y, m, d int) float64 {
	return timeToSerial(time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC))
}

type dimCase struct {
	formula string
	num     float64
	str     string
	blank   bool
	code    string // expected error code
}

func runDimCases(t *testing.T, m *fakeModel, cell map[string]string, cases []dimCase) {
	t.Helper()
	for _, c := range cases {
		v := evalIn(t, m.ctx(cell), c.formula)
		switch {
		case c.code != "":
			if !v.IsError() || v.Err().Code != c.code {
				t.Errorf("%s: want error %s, got %v", c.formula, c.code, v)
			}
		case v.IsError():
			t.Errorf("%s: unexpected error %v", c.formula, v.Err())
		case c.blank:
			if !v.IsBlank() {
				t.Errorf("%s: want blank, got %v (%v)", c.formula, v, v.Kind())
			}
		case c.str != "":
			if v.Kind() != KindString || v.String() != c.str {
				t.Errorf("%s: want %q, got %v", c.formula, c.str, v)
			}
		default:
			if n, ok := v.Number(); !ok || v.Kind() != KindNumber || math.Abs(n-c.num) > 1e-9 {
				t.Errorf("%s: want %v, got %v", c.formula, c.num, v)
			}
		}
	}
}

func TestDimPropertyAndParent(t *testing.T) {
	m := dimModel()
	runDimCases(t, m, map[string]string{"region": "DE", "product": "A100"}, []dimCase{
		{formula: "region.currency", str: "EUR"},
		{formula: "REGION.Currency", str: "EUR"},
		{formula: "region.headcount * 2", num: 20},
		{formula: "region.opened", num: serial(2020, 5, 1)},
		{formula: "YEAR(region.opened)", num: 2020},
		{formula: "region.nope", code: CodeUnknownProperty},
		{formula: "planet.currency", code: "#NAME?"},
		{formula: "PARENT(region)", str: "EMEA"},
		{formula: "PARENT(region) & \"-\" & region.currency", str: "EMEA-EUR"},
		{formula: "PARENT(\"region\")", code: CodeDimensionArgRequired},
		{formula: "PARENT(region.currency)", code: CodeDimensionArgRequired},
		{formula: "PARENT(region, product)", code: "#VALUE!"},
	})
	// A parent member returns its own value, never its children's.
	runDimCases(t, m, map[string]string{"region": "EMEA"}, []dimCase{
		{formula: "region.headcount", num: 14},
		{formula: "region.currency", blank: true},
		{formula: "PARENT(region)", blank: true},
	})
	// A dimension not pinned in the cell (a total or another slice) is blank.
	runDimCases(t, m, map[string]string{"product": "A100"}, []dimCase{
		{formula: "region.currency", blank: true},
		{formula: "PARENT(region)", blank: true},
	})
}

// A bare dimension name reads like dim.property: its member where pinned,
// blank where it is not (a total, or a rollup/slice row that leaves it
// open) — never #NAME?. A name that is neither a bound variable nor a
// dimension of the context stays #NAME?.
func TestBareDimensionUnpinnedIsBlank(t *testing.T) {
	m := dimModel()
	runDimCases(t, m, map[string]string{"product": "A100"}, []dimCase{
		{formula: "region", blank: true},
		{formula: "REGION", blank: true},
		{formula: "product", str: "A100"},
		{formula: "IF(region = \"EMEA\", 1, 0)", num: 0},
		{formula: "IF(region = \"\", 1, 0)", num: 1},
		{formula: "IF(region <> \"EMEA\", 1, 0)", num: 1},
		{formula: "region & \"-\" & product", str: "-A100"},
		{formula: "IF(region = \"EMEA\", 5, 0)", num: 0},
		{formula: "planet", code: "#NAME?"},
		{formula: "IF(planet = \"EMEA\", 5, 0)", code: "#NAME?"},
	})

	// Pinned but not bound by the caller: the context's member code.
	ctx := m.ctx(map[string]string{"region": "DE"})
	ctx.Vars = map[string]Value{"REVENUE": NumberVal(7)}
	if v := evalIn(t, ctx, "IF(region = \"DE\", revenue, 0)"); v.Kind() != KindNumber || v.String() != "7" {
		t.Errorf("pinned, unbound region: want 7, got %v", v)
	}

	// A bound variable is read before the Dim context. (At a pinned cell the
	// evaluators bind the member code into the same key, so the dimension
	// wins there: see calculation.TestDimensionNameSharedWithMetric.)
	ctx = m.ctx(map[string]string{"product": "A100"})
	ctx.Vars = map[string]Value{"REGION": NumberVal(3)}
	if v := evalIn(t, ctx, "region * 2"); v.Kind() != KindNumber || v.String() != "6" {
		t.Errorf("metric named region: want 6, got %v", v)
	}

	// Without a Dim context an unbound name is #NAME?, as before.
	if v := evalIn(t, &EvalContext{}, "region"); !v.IsError() || v.Err().Code != "#NAME?" {
		t.Errorf("no Dim context: want #NAME?, got %v", v)
	}
}

func TestTypedPropertyValue(t *testing.T) {
	cases := []struct {
		raw, typ string
		want     Value
	}{
		{"12.5", "number", NumberVal(12.5)},
		{" 7 ", "NUMBER", NumberVal(7)},
		{"1e3", "number", NumberVal(1000)},
		{"2024-02-29", "date", NumberVal(serial(2024, 2, 29))},
		{"EUR", "text", StringVal("EUR")},
		{"EUR", "", StringVal("EUR")},
		{"42", "currency", StringVal("42")},
		{"", "number", BlankVal()},
		{"   ", "date", BlankVal()},
		{"", "text", BlankVal()},
	}
	for _, c := range cases {
		if got := TypedPropertyValue(c.raw, c.typ); !reflect.DeepEqual(got, c.want) {
			t.Errorf("TypedPropertyValue(%q, %q) = %v (%v), want %v", c.raw, c.typ, got, got.Kind(), c.want)
		}
	}
	for _, c := range [][2]string{{"ten", "number"}, {"NaN", "number"}, {"2024-02-30", "date"}, {"29/02/2024", "date"}} {
		if v := TypedPropertyValue(c[0], c[1]); !v.IsError() || v.Err().Code != "#VALUE!" {
			t.Errorf("TypedPropertyValue(%q, %q) = %v, want #VALUE!", c[0], c[1], v)
		}
	}
}

func TestLookup(t *testing.T) {
	m := dimModel()
	runDimCases(t, m, map[string]string{"region": "DE", "product": "A100", "currency": "EUR"}, []dimCase{
		{formula: "LOOKUP(sales, region, \"US\")", num: 100},
		{formula: "lookup(Sales, Region, \"US\", product, \"B100\")", num: 50},
		{formula: "LOOKUP(sales, region, \"EMEA\")", num: 11},             // a parent combines its leaves
		{formula: "LOOKUP(sales, product, 1000000)", num: 3},              // a number converts without an exponent
		{formula: "LOOKUP(fx_rate, currency, region.currency)", num: 1.1}, // member from metadata
		{formula: "LOOKUP(sales, region, IF(region.currency = \"EUR\", \"FR\", \"US\"))", num: 1},
		{formula: "LOOKUP(sales, region, \"FR\", product, \"A200\")", num: 0}, // nothing recorded
		{formula: "LOOKUP(sales, region, \"XX\")", code: "#N/A"},
		{formula: "LOOKUP(sales, region, \"\")", code: "#N/A"},
		{formula: "LOOKUP(sales, region, PARENT(region) & \"x\")", code: "#N/A"},
		{formula: "IFNA(LOOKUP(sales, region, \"XX\"), -1)", num: -1},
		{formula: "LOOKUP(sales, region, 1/0)", code: "#DIV/0!"},
		{formula: "LOOKUP(sales * 2, region, \"US\")", code: CodeSourceMustBeMetric},
		{formula: "LOOKUP(sales, region.currency, \"US\")", code: CodeDimensionArgRequired},
		{formula: "LOOKUP(sales, \"region\", \"US\")", code: CodeDimensionArgRequired},
		{formula: "LOOKUP(sales, region, \"US\", REGION, \"FR\")", code: "#VALUE!"},
		{formula: "LOOKUP(sales, region)", code: "#VALUE!"},
		{formula: "LOOKUP(sales, region, \"US\", product)", code: "#VALUE!"},
		{formula: "LOOKUP(sales, planet, \"US\")", code: "#NAME?"},
	})
	// The IFNA-caught #N/A message names the dimension and the code.
	v := evalIn(t, m.ctx(map[string]string{"region": "DE"}), "LOOKUP(sales, region, \"XX\")")
	if !strings.Contains(v.Err().Message, "region") || !strings.Contains(v.Err().Message, "XX") {
		t.Errorf("#N/A message must name the dimension and the code: %q", v.Err().Message)
	}
}

func TestConditionalAggregation(t *testing.T) {
	m := dimModel()
	cell := map[string]string{"region": "DE", "product": "A100"}
	runDimCases(t, m, cell, []dimCase{
		// Member-code ranges, text operators and wildcards.
		{formula: "SUMIFS(sales, region, \"<>US\")", num: 11},
		{formula: "SUMIFS(sales, region, \"us\")", num: 100},
		{formula: "SUMIFS(sales, product, \"A*\")", num: 30},
		{formula: "SUMIFS(sales, product, \"?100\")", num: 15},
		{formula: "SUMIFS(sales, product, \"<>a*\")", num: 8},
		{formula: "SUMIFS(sales, product, 1000000)", num: 3},
		{formula: "COUNTIFS(product, \"*\")", num: 5},
		{formula: "COUNTIFS(product, \"star~*code\")", num: 1},
		{formula: "COUNTIFS(product, \"star~*\")", num: 0},
		{formula: "COUNTIFS(product, \"*~**\")", num: 1},
		{formula: "COUNTIFS(product, \"<B\")", num: 3}, // text ordering: 1000000, A100, A200
		// Typed properties: numeric comparisons on a number property.
		{formula: "SUMIFS(sales, region.headcount, \">5\")", num: 110},
		{formula: "SUMIFS(sales, region.headcount, \">=10\")", num: 10},
		{formula: "SUMIFS(sales, region.headcount, \"<5\")", num: 1},
		{formula: "SUMIFS(sales, region.headcount, \"=4\")", num: 1},
		{formula: "SUMIFS(sales, region.headcount, 4)", num: 1},
		{formula: "SUMIFS(sales, region.headcount, \">=4\", region.headcount, \"<=7\")", num: 101},
		{formula: "SUMIFS(sales, region.currency, \"eur\")", num: 11},
		{formula: "SUMIFS(sales, region.currency, region.currency)", num: 11},
		{formula: "SUMIFS(sales, region.currency, \"E?R\")", num: 11},
		// Date property and blanks.
		{formula: "COUNTIFS(region.opened, \">=2020-01-01\")", num: 1},
		{formula: "COUNTIFS(region.opened, \">\" & DATE(2019, 1, 1))", num: 2},
		{formula: "COUNTIFS(region.opened, \"\")", num: 1},
		{formula: "COUNTIFS(region.opened, \"=\")", num: 1},
		{formula: "COUNTIFS(region.opened, \"<>\")", num: 2},
		{formula: "COUNTIFS(region.opened, \"<2030-01-01\")", num: 2}, // a blank never matches an ordering
		{formula: "COUNTIFS(region.currency, \"<>EUR\")", num: 1},
		// Several dimensions: a cartesian product of each one's matches.
		{formula: "SUMIFS(sales, region, \"<>US\", product, \"A*\")", num: 31},
		{formula: "COUNTIFS(region, \"<>US\", product, \"A*\")", num: 4},
		{formula: "AVERAGEIFS(sales, region, \"<>US\", product, \"A*\")", num: 31.0 / 3}, // FR/A200 has no value: skipped
		{formula: "MAXIFS(sales, region, \"<>US\", product, \"A*\")", num: 20},
		{formula: "MINIFS(sales, region, \"<>US\", product, \"A*\")", num: 1},
		// Nothing matches.
		{formula: "SUMIFS(sales, region, \"ZZ\")", num: 0},
		{formula: "COUNTIFS(region, \"ZZ\", product, \"*\")", num: 0},
		{formula: "AVERAGEIFS(sales, region, \"ZZ\")", code: "#DIV/0!"},
		{formula: "AVERAGEIFS(sales, region, \"FR\", product, \"A200\")", code: "#DIV/0!"}, // matches, but nothing recorded
		{formula: "MINIFS(sales, region, \"ZZ\")", num: 0},
		{formula: "MAXIFS(sales, region, \"ZZ\")", num: 0},
		// Excel's single-criterion forms, source last.
		{formula: "SUMIF(region, \"<>US\", sales)", num: 11},
		{formula: "AVERAGEIF(region, \"<>US\", sales)", num: 5.5},
		{formula: "COUNTIF(region, \"D*\")", num: 1},
		// Argument errors.
		{formula: "SUMIF(region, \"x\")", code: "#VALUE!"},
		{formula: "COUNTIF(region, \"x\", sales)", code: "#VALUE!"},
		{formula: "COUNTIFS(region)", code: "#VALUE!"},
		{formula: "SUMIFS(sales, region)", code: "#VALUE!"},
		{formula: "SUMIFS(sales * 2, region, \"x\")", code: CodeSourceMustBeMetric},
		{formula: "SUMIFS(sales, \"region\", \"x\")", code: CodeDimensionArgRequired},
		{formula: "SUMIFS(sales, region, 1/0)", code: "#DIV/0!"},
		{formula: "SUMIFS(sales, planet, \"x\")", code: "#NAME?"},
	})
}

func TestDimensionalFunctionsNeedContext(t *testing.T) {
	for _, f := range []string{
		"region.currency", "PARENT(region)", "LOOKUP(sales, region, \"US\")", "SUMIFS(sales, region, \"x\")",
		"COUNTIFS(region, \"x\")", "AVERAGEIF(region, \"x\", sales)",
	} {
		v, err := Eval(f, map[string]Value{"sales": NumberVal(5), "region": StringVal("DE")})
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if !v.IsError() || v.Err().Code != CodeDimContextRequired {
			t.Errorf("%s without a dimension context: want %s, got %v", f, CodeDimContextRequired, v)
		}
	}
}

func TestConditionalMemo(t *testing.T) {
	m := dimModel()
	ctx := m.ctx(map[string]string{"region": "DE", "product": "A100"})
	node, _ := Parse("SUMIFS(sales, region, \"<>US\") + SUMIFS(sales, region, \"<>US\")")
	if v := EvalNode(ctx, node); v.IsError() {
		t.Fatal(v.Err())
	}
	first := m.resolves
	if first != 2 {
		t.Errorf("the second identical call in one cell must be memoised: %d resolves, want 2", first)
	}
	// Another cell differing only in a range dimension reuses the result.
	ctx2 := m.ctx(map[string]string{"region": "FR", "product": "A100"})
	ctx2.Dim.Memo = ctx.Dim.Memo
	again, _ := Parse("SUMIFS(sales, region, \"<>US\")") // a separate parse: the key is the expression, not the pointer
	if v := EvalNode(ctx2, again); v.IsError() || m.resolves != first {
		t.Errorf("a cell differing only in the range dimension must hit the memo: %v, %d resolves", v, m.resolves)
	}
	// A different criterion value, or a different non-range coordinate, recomputes.
	ctx3 := m.ctx(map[string]string{"region": "DE", "product": "B100"})
	ctx3.Dim.Memo = ctx.Dim.Memo
	if v := EvalNode(ctx3, again); v.IsError() || m.resolves == first {
		t.Errorf("another product must not hit the memo: %v", v)
	}
	n, _ := EvalNode(ctx3, again).Number()
	if n != 7 {
		t.Errorf("SUMIFS at B100 = %v, want 7", n)
	}
}

func TestCriteriaMatching(t *testing.T) {
	cases := []struct {
		crit      Value
		candidate Value
		want      bool
	}{
		{StringVal("abc"), StringVal("ABC"), true},
		{StringVal("=abc"), StringVal("abc"), true},
		{StringVal("<>abc"), StringVal("abd"), true},
		{StringVal("<>abc"), BlankVal(), true},
		{StringVal("abc"), BlankVal(), false},
		{StringVal(""), BlankVal(), true},
		{StringVal(""), StringVal("x"), false},
		{StringVal("="), BlankVal(), true},
		{StringVal("="), StringVal("x"), false},
		{StringVal("<>"), BlankVal(), false},
		{StringVal("<>"), NumberVal(0), true},
		{BlankVal(), BlankVal(), true},
		{StringVal(">5"), NumberVal(6), true},
		{StringVal(">5"), NumberVal(5), false},
		{StringVal(">=5"), NumberVal(5), true},
		{StringVal("<5"), NumberVal(4.5), true},
		{StringVal("<=5"), NumberVal(5.0001), false},
		{StringVal("<5"), BlankVal(), false},
		{StringVal(">5"), StringVal("6"), true},   // a text candidate compares as text: "6" > "5"
		{StringVal(">5"), StringVal("10"), false}, // ... so "10" < "5"
		{StringVal("10"), NumberVal(10), true},
		{StringVal("1e1"), NumberVal(10), true},
		{NumberVal(10), NumberVal(10), true},
		{NumberVal(10), StringVal("10"), true},
		{NumberVal(1e6), StringVal("1000000"), true},
		{StringVal("10"), StringVal("10.0"), false}, // text equality for a text candidate
		{StringVal(">b"), StringVal("C"), true},
		{StringVal("<b"), StringVal("a"), true},
		{StringVal("a*"), StringVal("Apple"), true},
		{StringVal("a*"), StringVal("banana"), false},
		{StringVal("*an*"), StringVal("banana"), true},
		{StringVal("b?n*"), StringVal("banana"), true},
		{StringVal("???"), StringVal("abcd"), false},
		{StringVal("a~*"), StringVal("a*"), true},
		{StringVal("a~*"), StringVal("ab"), false},
		{StringVal("a~?"), StringVal("a?"), true},
		{StringVal("a~~"), StringVal("a~"), true},
		{StringVal("a~b"), StringVal("a~b"), true},
		{StringVal("<>a*"), StringVal("apple"), false},
		{StringVal("<>a*"), StringVal("pear"), true},
		{StringVal("TRUE"), BoolVal(true), true},
		{StringVal(">=2026-01-01"), NumberVal(serial(2026, 1, 1)), true},
		{StringVal("<2026-01-01"), NumberVal(serial(2026, 1, 1)), false},
		// A text operand never equals or orders a number; only <> matches.
		{StringVal("<abc"), NumberVal(5), false},
		{StringVal(">abc"), NumberVal(5), false},
		{StringVal("abc"), NumberVal(5), false},
		{StringVal("1*"), NumberVal(10), false},
		{StringVal("<>abc"), NumberVal(5), true},
		{StringVal("<>1*"), NumberVal(10), true},
		// An error candidate is skipped by every criterion but <>.
		{StringVal("x"), ErrorVal(ErrValue), false},
		{StringVal(">5"), ErrorVal(ErrValue), false},
		{StringVal(""), ErrorVal(ErrValue), false},
		{StringVal("<>x"), ErrorVal(ErrValue), true},
		{StringVal("<>"), ErrorVal(ErrValue), true},
		// Negative zero is 0.
		{NumberVal(math.Copysign(0, -1)), StringVal("0"), true},
		{NumberVal(math.Copysign(0, -1)), NumberVal(0), true},
		{StringVal("-0"), NumberVal(0), true},
	}
	for _, c := range cases {
		if got := parseCriterion(c.crit).matches(c.candidate); got != c.want {
			t.Errorf("criterion %q (%v) against %q (%v): got %v, want %v", c.crit.String(), c.crit.Kind(), c.candidate.String(), c.candidate.Kind(), got, c.want)
		}
	}
}

func TestDottedLexing(t *testing.T) {
	ok := map[string]Node{
		"region.currency":       &DimProperty{Dim: "region", Property: "currency"},
		"region._x1":            &DimProperty{Dim: "region", Property: "_x1"},
		"_r.p":                  &DimProperty{Dim: "_r", Property: "p"},
		"{rev.eu}":              &Ident{Name: "rev.eu"},
		"x":                     &Ident{Name: "x"},
		"1.5":                   &NumberLit{Val: 1.5},
		".5":                    &NumberLit{Val: 0.5},
		"region.currency&\"x\"": &BinaryExpr{Op: "&", Left: &DimProperty{Dim: "region", Property: "currency"}, Right: &StringLit{Val: "x"}},
	}
	for text, want := range ok {
		got, err := Parse(text)
		if err != nil {
			t.Errorf("Parse(%q): %v", text, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Parse(%q) = %#v, want %#v", text, got, want)
		}
	}
	for _, text := range []string{"a.b.c", "region.2026", "x.5", "STDEV.S(x)", "region .currency", "region. currency", "region.", "a.b.c + 1"} {
		if _, err := Parse(text); err == nil {
			t.Errorf("Parse(%q) must fail", text)
		}
	}
	if _, err := Parse("a.b.c"); err == nil || !strings.Contains(err.Error(), "exactly one dot") {
		t.Errorf("a second dot must say so: %v", err)
	}
	// ExtractIdents never reports a property reference as a name.
	ids, _ := ExtractIdents("region.currency & x")
	if !reflect.DeepEqual(ids, []string{"x"}) {
		t.Errorf("ExtractIdents = %v", ids)
	}
}

func TestEvalWithContextKeepsDim(t *testing.T) {
	m := dimModel()
	ctx := m.ctx(map[string]string{"region": "DE", "product": "A100"})
	v, err := EvalWithContext("lookup(sales, region, \"US\") + region.headcount", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := v.Number(); v.IsError() || n != 110 {
		t.Errorf("EvalWithContext lost the dimension context: %v", v)
	}
}

func TestAnalyzeDimensional(t *testing.T) {
	an, err := Analyze("LOOKUP(sales, region, \"US\", product, 1000000) + region.currency_rate + PARENT(org)")
	if err != nil {
		t.Fatal(err)
	}
	if !an.ServedFromRows || an.UsesTimeSeries {
		t.Errorf("flags: %+v", an)
	}
	if len(an.References) != 1 || an.References[0].Name != "sales" || !an.References[0].Dimensional ||
		!reflect.DeepEqual(an.References[0].OverriddenDims, []string{"region", "product"}) || !an.References[0].IsDirect() {
		t.Errorf("references: %+v", an.References)
	}
	if !reflect.DeepEqual(an.DimensionArgs, []string{"region", "product", "org"}) {
		t.Errorf("dimension args: %v", an.DimensionArgs)
	}
	if !reflect.DeepEqual(an.PropertyRefs, []PropertyRef{{Dim: "region", Property: "currency_rate"}}) {
		t.Errorf("property refs: %v", an.PropertyRefs)
	}
	if len(an.DimensionalCalls) != 1 {
		t.Fatalf("dimensional calls: %+v", an.DimensionalCalls)
	}
	call := an.DimensionalCalls[0]
	if call.Func != "LOOKUP" || call.Source != "sales" || !reflect.DeepEqual(call.Dims, []string{"region", "product"}) || len(call.Members) != 2 {
		t.Fatalf("LOOKUP call: %+v", call)
	}
	if code, ok := call.Members[0].LiteralCode(); !ok || code != "US" || call.Members[0].Dim != "region" {
		t.Errorf("literal member: %+v", call.Members[0])
	}
	if code, ok := call.Members[1].LiteralCode(); !ok || code != "1000000" {
		t.Errorf("a literal number member converts without an exponent: %q", code)
	}

	// Member-local, value-dependent and literal-expression arguments.
	an, err = Analyze("SUMIFS(sales, region.currency, region.currency, product, \"A\" & \"*\", month, LOOKUP(first_month, org, org)) + COUNTIFS(org, PARENT(org))")
	if err != nil {
		t.Fatal(err)
	}
	crits := an.DimensionalCalls[0].Criteria
	if len(crits) != 3 || crits[0].Kind != ArgMemberLocal || !reflect.DeepEqual(crits[0].Names, []string{"region"}) || crits[0].Property != "currency" {
		t.Errorf("member-local criterion: %+v", crits)
	}
	if crits[1].Kind != ArgLiteral || crits[1].Value.String() != "A*" {
		t.Errorf("literal criterion: %+v", crits[1])
	}
	if crits[2].Kind != ArgValueDependent {
		t.Errorf("value-dependent criterion: %+v", crits[2])
	}
	if len(an.DimensionalCalls) != 3 || an.DimensionalCalls[1].Func != "LOOKUP" {
		t.Fatalf("calls are listed outer first: %+v", an.DimensionalCalls)
	}
	if c := an.DimensionalCalls[2]; c.Func != "COUNTIFS" || c.Source != "" || c.Criteria[0].Kind != ArgMemberLocal {
		t.Errorf("COUNTIFS call: %+v", c)
	}
	names := map[string]ReferenceUse{}
	for _, r := range an.References {
		names[r.Name] = r
	}
	if !names["sales"].Dimensional || !reflect.DeepEqual(names["sales"].OverriddenDims, []string{"region", "product", "month"}) {
		t.Errorf("SUMIFS source: %+v", names["sales"])
	}
	if !names["first_month"].Dimensional {
		t.Errorf("nested LOOKUP source: %+v", names["first_month"])
	}
	if _, ok := names["org"]; !ok {
		t.Errorf("a bare dimension used as a member expression is a reference like any bare name: %+v", an.References)
	}
	if !reflect.DeepEqual(an.PropertyRefs, []PropertyRef{{Dim: "region", Property: "currency"}}) {
		t.Errorf("property refs de-duplicate: %v", an.PropertyRefs)
	}

	// dim.property and PARENT alone are cell-local: not served from rows.
	an, _ = Analyze("IF(PARENT(region) = \"EMEA\", region.rate, 0) * x")
	if an.ServedFromRows || len(an.DimensionalCalls) != 0 || len(an.References) != 1 {
		t.Errorf("cell-local analysis: %+v", an)
	}
	if !IsDimensionalFunction("sumifs") || IsDimensionalFunction("PARENT") || !IsDimensionalFunction("LOOKUP") {
		t.Error("IsDimensionalFunction")
	}
}

func TestAnalyzeDimensionalErrors(t *testing.T) {
	cases := map[string]string{
		"LOOKUP(sales * 2, region, \"US\")":            CodeSourceMustBeMetric,
		"LOOKUP(\"sales\", region, \"US\")":            CodeSourceMustBeMetric,
		"LOOKUP(sales, region.currency, \"US\")":       CodeDimensionArgRequired,
		"LOOKUP(sales, PARENT(region), \"US\")":        CodeDimensionArgRequired,
		"LOOKUP(sales, region, \"US\", Region, \"x\")": "#VALUE!",
		"LOOKUP(sales, region)":                        "#VALUE!",
		"PARENT(\"region\")":                           CodeDimensionArgRequired,
		"PARENT()":                                     "#VALUE!",
		"SUMIFS(sales, 1, \"x\")":                      CodeDimensionArgRequired,
		"SUMIFS(sales, region)":                        "#VALUE!",
		"SUMIFS(sales + 1, region, \"x\")":             CodeSourceMustBeMetric,
		"SUMIF(region, \"x\")":                         "#VALUE!",
		"SUMIF(region, \"x\", sales * 2)":              CodeSourceMustBeMetric,
		"COUNTIF(region)":                              "#VALUE!",
		"COUNTIFS(region, \"x\", product)":             "#VALUE!",
		"YEARVALUE(sales * 2)":                         CodeSourceMustBeMetric,
		"MONTHVALUE(sales, 1)":                         "#VALUE!",
		"TIMESUM(x, \"Q1\")":                           "#VALUE!",
		"TIMESUM(x, \"Q1\", \"Q2\", MEDIAN)":           "#VALUE!",
		"TIMESUM(x, \"Q1\", \"Q2\", SUM, 1)":           "#VALUE!",
		"START(1)":                                     "#VALUE!",
		"END(x)":                                       "#VALUE!",
		"LAG(x, y, 0, LOOSE)":                          "#VALUE!",
	}
	for f, code := range cases {
		_, err := Analyze(f)
		if err == nil {
			t.Errorf("Analyze(%s): want %s, got nil", f, code)
			continue
		}
		if !strings.HasPrefix(err.Error(), code+":") {
			t.Errorf("Analyze(%s): want %s, got %v", f, code, err)
		}
	}
}

// TestConditionalDefectRegressions pins four stage-1 verifier findings:
// an unknown range dimension errors whatever the argument order; an
// unparsable typed property is skipped rather than failing the call, so the
// result never depends on criteria order; and a computed -0 member is "0".
func TestConditionalDefectRegressions(t *testing.T) {
	m := dimModel()
	m.members["REGION"][1].props = map[string]string{"currency": "EUR", "headcount": "ten"}
	m.members["PRODUCT"] = append(m.members["PRODUCT"], fakeMember{code: "0"})
	m.data["SALES|DE|0"] = 42
	cell := map[string]string{"region": "DE", "product": "A100"}
	if code, ok := memberCode(NumberVal(math.Copysign(0, -1))); !ok || code != "0" {
		t.Errorf("memberCode(-0) = %q, %v; want \"0\"", code, ok)
	}
	runDimCases(t, m, cell, []dimCase{
		// Unknown dimension after a range that matches nothing, and before.
		{formula: "SUMIFS(sales, region, \"ZZ\", nosuchdim, \"x\")", code: "#NAME?"},
		{formula: "SUMIFS(sales, nosuchdim, \"x\", region, \"ZZ\")", code: "#NAME?"},
		{formula: "COUNTIFS(region, \"ZZ\", nosuchdim, \"x\")", code: "#NAME?"},
		// DE's headcount "ten" is unparsable: skipped in either order.
		{formula: "SUMIFS(sales, region, \"FR\", region.headcount, \">1\")", num: 1},
		{formula: "SUMIFS(sales, region.headcount, \">1\", region, \"FR\")", num: 1},
		{formula: "SUMIFS(sales, region.headcount, \">1\")", num: 101}, // FR + US at A100, not DE
		{formula: "COUNTIFS(region.headcount, \"<>5\")", num: 3},       // DE's error counts for <>
		// A computed negative zero member is the code "0".
		{formula: "LOOKUP(sales, product, 0*-1)", num: 42},
		{formula: "SUMIFS(sales, product, 0*-1)", num: 42},
		{formula: "SUMIFS(sales, product, -0)", num: 42},
	})
}
