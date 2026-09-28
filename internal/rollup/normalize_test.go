package rollup

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// fxModel: region (EMEA -> DE, FR, UK; AMER -> US), currency (EUR, GBP,
// USD), employees under region structurally, a property-derived "segment"
// grouping employees by their "segment" property, and a monthly time
// dimension with an aggregate Q1.
func fxModel() map[string]*Dimension {
	day := func(m int) time.Time { return time.Date(2026, time.Month(m), 1, 0, 0, 0, 0, time.UTC) }
	return map[string]*Dimension{
		"region": {ID: "region", Members: []Member{
			{Code: "EMEA"}, {Code: "DE", ParentCode: "EMEA"}, {Code: "FR", ParentCode: "EMEA"}, {Code: "UK", ParentCode: "EMEA"},
			{Code: "AMER"}, {Code: "US", ParentCode: "AMER"},
		}},
		"currency": {ID: "currency", Members: []Member{{Code: "EUR"}, {Code: "GBP"}, {Code: "USD"}}},
		"employees": {ID: "employees", ParentDimensionID: "region", Members: []Member{
			{Code: "e1", ParentCode: "DE", Properties: map[string]string{"segment": "smb"}},
			{Code: "e2", ParentCode: "FR", Properties: map[string]string{"segment": "ent"}},
			{Code: "e3", ParentCode: "US", Properties: map[string]string{"segment": "smb"}},
		}},
		"segment": {ID: "segment", SourceDimensionID: "employees", SourceProperty: "segment", Members: []Member{{Code: "smb"}, {Code: "ent"}}},
		"month": {ID: "month", IsTime: true, Members: []Member{
			{Code: "Q1", TimeIndex: -1},
			{Code: "2026-01", ParentCode: "Q1", TimeIndex: 0, PeriodStart: day(1)},
			{Code: "2026-02", ParentCode: "Q1", TimeIndex: 1, PeriodStart: day(2)},
			{Code: "Q2", TimeIndex: -1},
		}},
	}
}

func TestRelates(t *testing.T) {
	dims := fxModel()
	cases := []struct {
		own, other string
		want       bool
	}{
		{"region", "region", true},
		{"employees", "region", true},   // own descends from other
		{"region", "employees", true},   // other descends from own
		{"employees", "segment", true},  // property grouping of own
		{"segment", "employees", false}, // the reverse property direction does not relate
		{"region", "segment", false},    // no multi-hop composition
		{"currency", "region", false},   // unrelated
		{"currency", "month", false},    // a time dimension the source does not carry
		{"region", "missing", false},    // unknown dimension
		{"missing", "missing", true},    // identity
	}
	for _, c := range cases {
		if got := Relates(dims, c.own, c.other); got != c.want {
			t.Errorf("Relates(%s, %s) = %v, want %v", c.own, c.other, got, c.want)
		}
	}
}

func TestLeafAndSubtreeHelpers(t *testing.T) {
	dims := fxModel()
	if got := LeafDescendants(dims["region"], "EMEA"); !reflect.DeepEqual(got, []string{"DE", "FR", "UK"}) {
		t.Errorf("LeafDescendants(EMEA) = %v", got)
	}
	if got := LeafDescendants(dims["region"], "US"); !reflect.DeepEqual(got, []string{"US"}) {
		t.Errorf("a leaf is its own leaf descendant: %v", got)
	}
	if got := LeafDescendants(dims["region"], "nope"); got != nil {
		t.Errorf("unknown member: %v", got)
	}
	// Time: only dated leaves; an empty aggregate has none (non-nil).
	if got := LeafDescendants(dims["month"], "Q1"); !reflect.DeepEqual(got, []string{"2026-01", "2026-02"}) {
		t.Errorf("LeafDescendants(Q1) = %v", got)
	}
	if got := LeafDescendants(dims["month"], "Q2"); got == nil || len(got) != 0 {
		t.Errorf("an empty aggregate period has no leaves: %#v", got)
	}
	if got := SubtreeCodes(dims["region"], "EMEA"); !reflect.DeepEqual(got, []string{"EMEA", "DE", "FR", "UK"}) {
		t.Errorf("SubtreeCodes(EMEA) = %v", got)
	}
	if got := LeafCodes(dims["month"]); !reflect.DeepEqual(got, []string{"2026-01", "2026-02"}) {
		t.Errorf("LeafCodes(month) = %v", got)
	}
	if FindMember(dims["region"], "FR") == nil || FindMember(nil, "FR") != nil {
		t.Error("FindMember")
	}
	// A cyclic hierarchy terminates.
	cyc := &Dimension{ID: "c", Members: []Member{{Code: "a", ParentCode: "b"}, {Code: "b", ParentCode: "a"}}}
	if got := SubtreeCodes(cyc, "a"); len(got) != 2 {
		t.Errorf("cyclic subtree: %v", got)
	}
}

func TestNormalizeCombo(t *testing.T) {
	dims := fxModel()
	cases := []struct {
		name      string
		source    []string
		combo     map[string]string
		overrides map[string]string
		want      map[string]string
	}{
		{"unrelated pins are dropped", []string{"currency"},
			map[string]string{"region": "EMEA", "currency": "EUR", "month": "2026-01"}, nil,
			map[string]string{"currency": "EUR"}},
		{"a leaf cell without overrides keeps its own pins", []string{"region", "month"},
			map[string]string{"region": "DE", "month": "2026-01"}, nil,
			map[string]string{"region": "DE", "month": "2026-01"}},
		{"related pins are kept", []string{"employees"},
			map[string]string{"region": "EMEA", "segment": "smb", "currency": "EUR"}, nil,
			map[string]string{"region": "EMEA", "segment": "smb"}},
		{"an override replaces its own pin", []string{"region", "month"},
			map[string]string{"region": "DE", "month": "2026-01"}, map[string]string{"region": "US"},
			map[string]string{"region": "US", "month": "2026-01"}},
		{"an override on the own dimension drops related pins", []string{"employees"},
			map[string]string{"employees": "e1", "region": "DE", "segment": "smb"}, map[string]string{"employees": "e3"},
			map[string]string{"employees": "e3"}},
		{"an override on a related dimension drops the own pin", []string{"employees"},
			map[string]string{"employees": "e1", "month": "2026-01"}, map[string]string{"region": "AMER"},
			map[string]string{"region": "AMER"}},
		{"an unrelated override is dropped", []string{"currency"},
			map[string]string{"currency": "EUR"}, map[string]string{"region": "DE"},
			map[string]string{"currency": "EUR"}},
		{"the time dimension is overridden like any own dimension", []string{"region", "month"},
			map[string]string{"region": "DE", "month": "2026-01"}, map[string]string{"month": "Q1"},
			map[string]string{"region": "DE", "month": "Q1"}},
	}
	for _, c := range cases {
		combo := cloneCombo(c.combo)
		got, err := NormalizeCombo(dims, c.source, c.combo, c.overrides)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
		if !reflect.DeepEqual(combo, c.combo) {
			t.Errorf("%s: the input combo was modified", c.name)
		}
	}
}

// TestNormalizeComboFXAtParent is the case that motivated normalisation:
// fx_rate[currency] read at {region: EMEA (a parent of three), currency:
// EUR} must be the one EUR rate, not three times it.
func TestNormalizeComboFXAtParent(t *testing.T) {
	dims := fxModel()
	fetch := memFetch(t, map[string]float64{
		fetchKey("fx_rate", map[string]string{"currency": "EUR"}): 1.1,
		fetchKey("fx_rate", map[string]string{"currency": "USD"}): 1,
	})
	cell := map[string]string{"region": "EMEA", "currency": "EUR"}
	raw, _, err := ResolveTime(context.Background(), dims, "fx_rate", []string{"currency"}, AggSum, "", cell, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if raw < 3.29 || raw > 3.31 {
		t.Fatalf("precondition: an unnormalised read rolls the rate up over EMEA's children (3.3), got %v", raw)
	}
	norm, err := NormalizeCombo(dims, []string{"currency"}, cell, nil)
	if err != nil {
		t.Fatal(err)
	}
	v, ok, err := ResolveTime(context.Background(), dims, "fx_rate", []string{"currency"}, AggSum, "", norm, fetch)
	if err != nil || !ok || v != 1.1 {
		t.Fatalf("normalised FX read at a parent: got %v ok=%v err=%v, want 1.1", v, ok, err)
	}
	// An override to another currency at the same parent cell.
	if norm, err = NormalizeCombo(dims, []string{"currency"}, cell, map[string]string{"currency": "USD"}); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := ResolveTime(context.Background(), dims, "fx_rate", []string{"currency"}, AggSum, "", norm, fetch); v != 1 {
		t.Fatalf("LOOKUP(fx_rate, currency, \"USD\") at EMEA: got %v, want 1", v)
	}
}

// TestNormalizeComboConflictingOverrides: two overrides whose dimensions
// both relate to the same own dimension of the source ask for an
// intersection one combo cannot express — ResolveTime would honour one pin
// and ignore the other (SUMIFS(cost, employees, "*", region, "DE") summed
// every employee instead of DE's). The shape is refused, never resolved.
func TestNormalizeComboConflictingOverrides(t *testing.T) {
	dims := fxModel()
	refused := []struct {
		name      string
		source    []string
		overrides map[string]string
	}{
		{"own and related", []string{"employees"}, map[string]string{"employees": "e1", "region": "DE"}},
		{"two related", []string{"employees"}, map[string]string{"region": "FR", "segment": "smb"}},
		{"own and property grouping", []string{"employees"}, map[string]string{"employees": "e2", "segment": "smb"}},
	}
	for _, c := range refused {
		got, err := NormalizeCombo(dims, c.source, map[string]string{"month": "2026-01"}, c.overrides)
		if !errors.Is(err, ErrConflictingOverrides) || got != nil {
			t.Errorf("%s: got %v err=%v, want ErrConflictingOverrides", c.name, got, err)
		}
	}
	allowed := []struct {
		name      string
		source    []string
		overrides map[string]string
		want      map[string]string
	}{
		// Overrides on different own dimensions are independent pins.
		{"two own dimensions", []string{"employees", "currency"},
			map[string]string{"employees": "e1", "currency": "USD"},
			map[string]string{"employees": "e1", "currency": "USD"}},
		// Both are own dimensions: each exact pin selects only itself, even
		// though region also relates to employees.
		{"own dimensions that also relate", []string{"employees", "region"},
			map[string]string{"employees": "e1", "region": "DE"},
			map[string]string{"employees": "e1", "region": "DE"}},
		{"a related override beside an own one", []string{"employees", "month"},
			map[string]string{"region": "DE", "month": "2026-02"},
			map[string]string{"region": "DE", "month": "2026-02"}},
	}
	for _, c := range allowed {
		got, err := NormalizeCombo(dims, c.source, map[string]string{"month": "2026-01"}, c.overrides)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v err=%v, want %v", c.name, got, err, c.want)
		}
	}
}

// TestResolveTimeAggregatePeriodIsFlat: an aggregate period in a nested
// time hierarchy (FY > Q > month) reduces its recorded LEAF periods once,
// never level by level — an average over unequally recorded quarters is
// the mean of the recorded months, not a mean of quarter means.
func TestResolveTimeAggregatePeriodIsFlat(t *testing.T) {
	members := []Member{{Code: "FY", TimeIndex: -1}}
	for q := 1; q <= 2; q++ {
		members = append(members, Member{Code: "Q" + string(rune('0'+q)), ParentCode: "FY", TimeIndex: -1})
	}
	// Q1: Jan, Feb, Mar; Q2: Apr, May, Jun — listed out of order on purpose.
	codes := []string{"m06", "m01", "m02", "m03", "m04", "m05"}
	idx := map[string]int{"m01": 0, "m02": 1, "m03": 2, "m04": 3, "m05": 4, "m06": 5}
	for _, c := range codes {
		parent := "Q1"
		if idx[c] >= 3 {
			parent = "Q2"
		}
		members = append(members, Member{Code: c, ParentCode: parent, TimeIndex: idx[c]})
	}
	dims := map[string]*Dimension{"month": {ID: "month", IsTime: true, Members: members}}
	// Recorded: Jan=1, Feb=3, Mar=5 (Q1 fully), May=11 (Q2 once).
	recorded := map[string]float64{"m01": 1, "m02": 3, "m03": 5, "m05": 11}
	fetch := func(_ context.Context, _ string, combo map[string]string) (float64, bool, error) {
		v, ok := recorded[combo["month"]]
		return v, ok, nil
	}
	cases := []struct {
		rule TimeSummaryRule
		want float64
	}{
		{"average", (1 + 3 + 5 + 11) / 4.0}, // not (3 + 11) / 2
		{"sum", 20},
		{"first", 1},
		{"last", 11},
		{"min", 1},
		{"max", 11},
	}
	for _, c := range cases {
		got, ok, err := ResolveTime(context.Background(), dims, "m", []string{"month"}, AggSum, c.rule, map[string]string{"month": "FY"}, fetch)
		if err != nil || !ok || got != c.want {
			t.Errorf("%s at FY: got %v ok=%v err=%v, want %v", c.rule, got, ok, err, c.want)
		}
	}
	if _, ok, _ := ResolveTime(context.Background(), dims, "m", []string{"month"}, AggSum, "none", map[string]string{"month": "FY"}, fetch); ok {
		t.Error("none at an aggregate period must be absent")
	}
}
