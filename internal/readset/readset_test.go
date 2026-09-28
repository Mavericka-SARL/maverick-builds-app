package readset

import (
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// fixture: region (EMEA > UK, DE; US), currency (GBP, EUR, USD), product
// (P1, P2) and a monthly time dimension Jan..Jun under Q1/Q2 under H1.
func fixture() (map[string]*rollup.Dimension, map[string]string, *calculation.DimMetadata) {
	dims := map[string]*rollup.Dimension{
		"reg": {ID: "reg", Members: []rollup.Member{
			{Code: "EMEA"},
			{Code: "UK", ParentCode: "EMEA", Properties: map[string]string{"segment": "SMB", "currency": "GBP"}},
			{Code: "DE", ParentCode: "EMEA", Properties: map[string]string{"segment": "SMB", "currency": "EUR"}},
			{Code: "US", Properties: map[string]string{"segment": "ENT", "currency": "USD"}},
		}},
		"cur":  {ID: "cur", Members: []rollup.Member{{Code: "GBP"}, {Code: "EUR"}, {Code: "USD"}}},
		"prod": {ID: "prod", Members: []rollup.Member{{Code: "P1"}, {Code: "P2"}}},
		"t": {ID: "t", IsTime: true, TimeGranularity: "month", FiscalYearStartMonth: 1, Members: []rollup.Member{
			{Code: "Jan", ParentCode: "Q1", TimeIndex: 0}, {Code: "Feb", ParentCode: "Q1", TimeIndex: 1}, {Code: "Mar", ParentCode: "Q1", TimeIndex: 2},
			{Code: "Apr", ParentCode: "Q2", TimeIndex: 3}, {Code: "May", ParentCode: "Q2", TimeIndex: 4}, {Code: "Jun", ParentCode: "Q2", TimeIndex: 5},
			{Code: "Q1", ParentCode: "H1", TimeIndex: -1}, {Code: "Q2", ParentCode: "H1", TimeIndex: -1}, {Code: "H1", TimeIndex: -1},
		}},
	}
	names := map[string]string{"reg": "region", "cur": "currency", "prod": "product", "t": "period"}
	schema := &calculation.DimensionSchema{Properties: map[string]map[string]calculation.PropertyDecl{
		"reg": {"SEGMENT": {Name: "segment", DataType: "text"}, "CURRENCY": {Name: "currency", DataType: "text"}},
	}}
	return dims, names, calculation.NewDimMetadata(dims, names, schema)
}

func newSet(t *testing.T, metrics []Metric, hidden map[string]map[string]bool) *Set {
	t.Helper()
	dims, names, meta := fixture()
	return New(dims, names, meta, metrics, hidden)
}

func hide(dim string, codes ...string) map[string]map[string]bool {
	out := map[string]map[string]bool{dim: {}}
	for _, c := range codes {
		out[dim][c] = true
	}
	return out
}

var baseMetrics = []Metric{
	{ID: "rev", Name: "revenue", IsInput: true, Dims: []string{"reg", "t"}},
	{ID: "sales", Name: "sales", IsInput: true, Dims: []string{"reg", "prod", "t"}},
	{ID: "fx", Name: "fx_rate", IsInput: true, Dims: []string{"cur"}},
	{ID: "k", Name: "k", IsInput: true, Dims: []string{"t"}},
}

func with(extra ...Metric) []Metric {
	return append(append([]Metric(nil), baseMetrics...), extra...)
}

func calc(id, text string, dims ...string) Metric {
	return Metric{ID: id, Name: id, Formula: text, Dims: dims}
}

func check(t *testing.T, s *Set, metric string, combo map[string]string, want bool) {
	t.Helper()
	if got := s.Withheld(metric, combo); got != want {
		t.Errorf("Withheld(%s, %v) = %v, want %v", metric, combo, got, want)
	}
}

func TestLookupLiteralMember(t *testing.T) {
	m := with(calc("us", `LOOKUP(revenue, region, "US")`, "reg", "t"),
		calc("emea", `LOOKUP(revenue, region, "EMEA")`, "reg", "t"))
	cell := map[string]string{"reg": "UK", "t": "Feb"}
	s := newSet(t, m, hide("reg", "DE"))
	check(t, s, "us", cell, false)  // reads US only
	check(t, s, "emea", cell, true) // EMEA's leaves include the hidden DE
	s = newSet(t, m, hide("reg", "US"))
	check(t, s, "us", cell, true)
	check(t, s, "emea", cell, false)
}

func TestCriteriaMatchHiddenMember(t *testing.T) {
	m := with(calc("smb", `SUMIFS(revenue, region.segment, "SMB")`, "reg", "t"),
		calc("n", `COUNTIFS(region, "*")`, "t"))
	cell := map[string]string{"reg": "US", "t": "Jan"}
	check(t, newSet(t, m, hide("reg", "DE")), "smb", cell, true) // DE is SMB
	check(t, newSet(t, m, hide("reg", "US")), "smb", map[string]string{"reg": "UK", "t": "Jan"}, false)
	// COUNTIFS reads member metadata only, but counts the hidden member.
	check(t, newSet(t, m, hide("reg", "US")), "n", map[string]string{"t": "Jan"}, true)
}

func TestMemberLocalLookup(t *testing.T) {
	m := with(calc("conv", `revenue * LOOKUP(fx_rate, currency, region.currency)`, "reg", "t"))
	// A hidden region: every other region reads only its own currency.
	s := newSet(t, m, hide("reg", "DE"))
	check(t, s, "conv", map[string]string{"reg": "UK", "t": "Jan"}, false)
	check(t, s, "conv", map[string]string{"reg": "US", "t": "Jan"}, false)
	// A hidden currency withholds exactly the regions whose currency it is.
	s = newSet(t, m, hide("cur", "EUR"))
	check(t, s, "conv", map[string]string{"reg": "DE", "t": "Jan"}, true)
	check(t, s, "conv", map[string]string{"reg": "UK", "t": "Jan"}, false)
	// At EMEA the metric combines UK and DE (and a formula rollup would
	// read EMEA's own property): DE's EUR is among them.
	check(t, s, "conv", map[string]string{"reg": "EMEA", "t": "Jan"}, true)
}

func TestCoarseGrainReadsEveryLeaf(t *testing.T) {
	m := with(calc("tot", `revenue * 2`, "t"))
	s := newSet(t, m, hide("reg", "DE"))
	check(t, s, "tot", map[string]string{"t": "Jan"}, true)
	s = newSet(t, m, hide("prod", "P1")) // a dimension it never reads
	check(t, s, "tot", map[string]string{"t": "Jan"}, false)
}

func TestTimeWindows(t *testing.T) {
	m := with(
		calc("prev", `PREVIOUS(revenue)`, "reg", "t"),
		calc("lag", `LAG(revenue, k, 0)`, "reg", "t"),
		calc("yv", `YEARVALUE(revenue)`, "reg", "t"),
		calc("q2", `TIMESUM(revenue, "Q2", "Q2")`, "reg", "t"),
		calc("q1", `TIMESUM(revenue, "Q1", "Q1")`, "reg", "t"),
		calc("shifted", `TIMESUM(PREVIOUS(revenue), "Q2", "Q2")`, "reg", "t"),
	)
	s := newSet(t, m, hide("t", "Jan"))
	uk := func(p string) map[string]string { return map[string]string{"reg": "UK", "t": p} }
	check(t, s, "prev", uk("Feb"), true)
	check(t, s, "prev", uk("Mar"), false)
	check(t, s, "prev", uk("Jan"), true) // its own period
	check(t, s, "prev", uk("Q2"), false) // Apr..Jun read Mar..May
	check(t, s, "prev", uk("Q1"), true)
	check(t, s, "lag", uk("Jun"), true) // a dynamic offset reaches any period
	check(t, s, "yv", uk("Jun"), true)
	check(t, s, "q2", uk("Mar"), false) // an absolute range: Apr..Jun only
	check(t, s, "q1", uk("Jun"), true)
	check(t, s, "shifted", uk("Jun"), false) // Mar..May
	s = newSet(t, m, hide("t", "Mar"))
	check(t, s, "shifted", uk("Jun"), true)
}

func TestTransitiveThroughCalculatedReferences(t *testing.T) {
	m := with(
		calc("a", `PREVIOUS(revenue)`, "reg", "t"),
		calc("b", `a * 2`, "reg", "t"),
		calc("m", `LOOKUP(b, region, "US")`, "reg", "t"),
	)
	s := newSet(t, m, hide("t", "Jan"))
	check(t, s, "m", map[string]string{"reg": "UK", "t": "Feb"}, true) // US@Feb -> a@Feb -> revenue@Jan
	check(t, s, "m", map[string]string{"reg": "UK", "t": "Mar"}, false)
	s = newSet(t, m, hide("reg", "US"))
	check(t, s, "m", map[string]string{"reg": "UK", "t": "Mar"}, true)
}

func TestRecurrenceIsUnbounded(t *testing.T) {
	m := with(
		calc("closing", `opening + revenue`, "reg", "t"),
		calc("opening", `PREVIOUS(closing)`, "reg", "t"),
	)
	s := newSet(t, m, hide("t", "Jan"))
	check(t, s, "opening", map[string]string{"reg": "UK", "t": "Jun"}, true)
}

func TestUnrestrictedWithholdsNothing(t *testing.T) {
	m := with(calc("us", `LOOKUP(revenue, region, "US")`, "reg", "t"))
	var nilSet *Set
	if nilSet.Restricted() || nilSet.Withheld("us", nil) {
		t.Error("a nil Set must withhold nothing")
	}
	s := newSet(t, m, nil)
	check(t, s, "us", map[string]string{}, false)
	// A metric the Set does not know is withheld once anything is hidden.
	check(t, newSet(t, m, hide("reg", "DE")), "nope", map[string]string{}, true)
}

func TestServed(t *testing.T) {
	for text, want := range map[string]bool{
		`revenue * 2`:                   false,
		`revenue * region.factor`:       false,
		`PARENT(region)`:                false,
		`PREVIOUS(revenue)`:             true,
		`LOOKUP(revenue, region, "US")`: true,
		`SUMIFS(revenue, region, "U*")`: true,
		`TIMESUM(revenue)`:              true,
		`not a formula (`:               false,
		``:                              false,
	} {
		if got := Served(text); got != want {
			t.Errorf("Served(%q) = %v, want %v", text, got, want)
		}
	}
}

// TestUsesMatchAnalyze holds the walk's window composition to
// formula.Analyze's: merged per name, the windows agree exactly.
func TestUsesMatchAnalyze(t *testing.T) {
	for _, text := range []string{
		`PREVIOUS(LAG(x, 2, 0)) + NEXT(y)`,
		`MOVINGSUM(x, -2, 0, AVERAGE) + MOVINGSUM(y, -1) + MOVINGSUM(z)`,
		`CUMULATE(x, y) + DECUMULATE(z)`,
		`YEARTODATE(PREVIOUS(x)) + QUARTERVALUE(y)`,
		`LAG(x, k, 0) + LEAD(y, 3, z)`,
		`LOOKUP(x, region, "US") + SUMIFS(y, region, "U*") + PREVIOUS(LOOKUP(z, region, "EU"))`,
		`TIMESUM(PREVIOUS(x))`,
	} {
		node, err := formula.Parse(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		us, err := uses(node)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		an, err := formula.Analyze(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		merged := map[string]formula.ReferenceUse{}
		for _, u := range us {
			if u.source == "" {
				continue
			}
			k := strings.ToUpper(u.source)
			r, ok := merged[k]
			if !ok {
				r = formula.ReferenceUse{Name: u.source, MinTimeOffset: u.win.min, MaxTimeOffset: u.win.max}
			}
			if u.win.min < r.MinTimeOffset {
				r.MinTimeOffset = u.win.min
			}
			if u.win.max > r.MaxTimeOffset {
				r.MaxTimeOffset = u.win.max
			}
			r.UnboundedPast = r.UnboundedPast || u.win.unbPast
			r.UnboundedFuture = r.UnboundedFuture || u.win.unbFuture
			merged[k] = r
		}
		for _, ref := range an.References {
			got, ok := merged[strings.ToUpper(ref.Name)]
			if !ok {
				t.Errorf("%s: walk misses %s", text, ref.Name)
				continue
			}
			if got.MinTimeOffset != ref.MinTimeOffset || got.MaxTimeOffset != ref.MaxTimeOffset ||
				got.UnboundedPast != ref.UnboundedPast || got.UnboundedFuture != ref.UnboundedFuture {
				t.Errorf("%s: %s window = [%d,%d] %v/%v, Analyze says [%d,%d] %v/%v", text, ref.Name,
					got.MinTimeOffset, got.MaxTimeOffset, got.UnboundedPast, got.UnboundedFuture,
					ref.MinTimeOffset, ref.MaxTimeOffset, ref.UnboundedPast, ref.UnboundedFuture)
			}
		}
	}
}

// TestWindowedMemberLocal: a member-local LOOKUP member or criterion inside
// a time window is evaluated by the scheduler at every period the window
// reaches (timeseries.go rebinds the dimension names and member context to
// the shifted cell), so its read set must be too — not only at the cell's
// own period.
func TestWindowedMemberLocal(t *testing.T) {
	m := with(
		calc("pl1", `PREVIOUS(LOOKUP(revenue, period, period))`, "reg", "t"),
		calc("pl2", `PREVIOUS(LOOKUP(revenue, period, PARENT(period)))`, "reg", "t"),
		calc("pl3", `PREVIOUS(LOOKUP(revenue, region, IF(period = "Jan", "US", "UK")))`, "reg", "t"),
		calc("yl", `YEARVALUE(LOOKUP(revenue, region, IF(period = "Jan", "US", "UK")))`, "reg", "t"),
		calc("sl", `PREVIOUS(SUMIFS(revenue, region, IF(period = "Jan", "US", "UK")))`, "reg", "t"),
		calc("pl0", `LOOKUP(revenue, region, IF(period = "Jan", "US", "UK"))`, "reg", "t"),
		calc("y0", `PREVIOUS(pl0)`, "reg", "t"),
	)
	at := func(r, p string) map[string]string { return map[string]string{"reg": r, "t": p} }

	s := newSet(t, m, hide("t", "Jan"))
	check(t, s, "pl1", at("UK", "Feb"), true) // reads UK/Jan
	check(t, s, "pl1", at("UK", "Mar"), false)

	s = newSet(t, m, hide("t", "Jun"))
	check(t, s, "pl2", at("UK", "Apr"), false) // Mar -> Q1
	check(t, s, "pl2", at("UK", "May"), true)  // Apr -> Q2 holds Jun

	s = newSet(t, m, hide("reg", "US"))
	check(t, s, "pl3", at("DE", "Feb"), true) // Jan reads US
	check(t, s, "pl3", at("DE", "Mar"), false)
	check(t, s, "yl", at("DE", "Jun"), true) // the year holds Jan
	check(t, s, "sl", at("DE", "Feb"), true)
	check(t, s, "sl", at("DE", "Mar"), false)
	check(t, s, "pl0", at("DE", "Jan"), true)
	check(t, s, "pl0", at("DE", "Feb"), false)
	check(t, s, "y0", at("DE", "Feb"), true) // pl0 at Jan, through the reference
	check(t, s, "y0", at("DE", "Mar"), false)
}

// TestRecurrenceDirection: a recurrence only extends its window the way
// its loop shifts — a backward loop reads the whole past, never the future.
func TestRecurrenceDirection(t *testing.T) {
	m := with(
		calc("closing", `opening + revenue`, "reg", "t"),
		calc("opening", `PREVIOUS(closing)`, "reg", "t"),
		calc("fwd", `NEXT(back) + 1`, "reg", "t"),
		calc("back", `fwd + revenue`, "reg", "t"),
		calc("uses", `closing * 2`, "reg", "t"),
	)
	at := func(p string) map[string]string { return map[string]string{"reg": "UK", "t": p} }
	s := newSet(t, m, hide("t", "Jun"))
	check(t, s, "closing", at("Feb"), false)
	check(t, s, "opening", at("Feb"), false)
	check(t, s, "uses", at("Mar"), false)
	check(t, s, "fwd", at("Feb"), true) // a forward loop reaches Jun
	s = newSet(t, m, hide("t", "Jan"))
	check(t, s, "closing", at("Jun"), true)
	check(t, s, "opening", at("Feb"), true)
	check(t, s, "uses", at("Jun"), true)
	check(t, s, "fwd", at("Feb"), false)
	check(t, s, "back", at("Feb"), false)
}
