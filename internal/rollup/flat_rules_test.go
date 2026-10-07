package rollup

import (
	"context"
	"errors"
	"testing"
)

// globalGeo: Global > {World > {EMEA > {UK, DE, FR}, US}, APAC > {JP}} —
// three levels with uneven branches, reproducing the difference between
// grid parent rows and chart-data for member-metadata metrics. FR has no
// recorded value.
func globalGeo(id string) *Dimension {
	return &Dimension{ID: id, Members: []Member{
		{Code: "Global"},
		{Code: "World", ParentCode: "Global"}, {Code: "APAC", ParentCode: "Global"},
		{Code: "EMEA", ParentCode: "World"}, {Code: "US", ParentCode: "World"},
		{Code: "UK", ParentCode: "EMEA"}, {Code: "DE", ParentCode: "EMEA"}, {Code: "FR", ParentCode: "EMEA"},
		{Code: "JP", ParentCode: "APAC"},
	}}
}

// Resolve and ResolveTime combine average and count FLAT over the leaves
// with a recorded value, at every level: EMEA, World (two levels) and Global
// (three levels). A leaf with no value (ok=false, the input read's miss) is
// left out of the mean and the count, never a 0. Sum is unchanged: every
// leaf, a missing one 0.
func TestResolveAverageAndCountAreFlatOverLeaves(t *testing.T) {
	dims := map[string]*Dimension{"geo": globalGeo("geo")}
	vals := map[string]float64{"UK": 2, "DE": 3, "US": 4, "JP": 10}
	fetch := func(_ context.Context, _ string, c map[string]string) (float64, bool, error) {
		v, ok := vals[c["geo"]]
		return v, ok, nil
	}
	ctx := context.Background()
	ids := []string{"geo"}
	for _, tc := range []struct {
		member string
		rule   AggRule
		want   float64
	}{
		{"EMEA", AggAverage, 2.5},
		{"World", AggAverage, 3},     // (2+3+4)/3, not mean(EMEA 2.5, US 4) = 3.25
		{"Global", AggAverage, 4.75}, // (2+3+4+10)/4, not mean(World, APAC)
		{"EMEA", AggCount, 2},
		{"World", AggCount, 3}, // leaves with a value, not the 2 children
		{"Global", AggCount, 4},
		{"EMEA", AggSum, 5},
		{"World", AggSum, 9},
		{"Global", AggSum, 19},
		{"UK", AggAverage, 2},
	} {
		for name, resolve := range map[string]func() (float64, bool, error){
			"Resolve": func() (float64, bool, error) {
				return Resolve(ctx, dims, "m", ids, tc.rule, map[string]string{"geo": tc.member}, fetch)
			},
			"ResolveTime": func() (float64, bool, error) {
				return ResolveTime(ctx, dims, "m", ids, tc.rule, "sum", map[string]string{"geo": tc.member}, fetch)
			},
		} {
			v, ok, err := resolve()
			if err != nil || !ok || v != tc.want {
				t.Errorf("%s %s %s: got %v ok=%v err=%v, want %v", name, tc.rule, tc.member, v, ok, err, tc.want)
			}
		}
	}
	// Unpinned: the grand total, equal to the top member's value.
	if v, _, _ := Resolve(ctx, dims, "m", ids, AggAverage, map[string]string{}, fetch); v != 4.75 {
		t.Errorf("unpinned average: got %v, want 4.75", v)
	}
	// A parent with no recorded leaf: count 0 and average 0, ok=true (an
	// aggregate over nothing recorded), as Resolve's contract says.
	delete(vals, "JP")
	if v, ok, err := Resolve(ctx, dims, "m", ids, AggCount, map[string]string{"geo": "APAC"}, fetch); err != nil || !ok || v != 0 {
		t.Errorf("APAC count with nothing recorded: got %v ok=%v err=%v, want 0", v, ok, err)
	}
	// The trivial read still answers fetch's own miss.
	if _, ok, err := Resolve(ctx, dims, "m", ids, AggAverage, map[string]string{"geo": "FR"}, fetch); err != nil || ok {
		t.Errorf("FR leaf: ok=%v err=%v, want the miss", ok, err)
	}
	// Count counts leaves whose recorded value is non-zero — CombineAgg's
	// rule, the scheduler's for its totals and slice rows.
	vals["FR"] = 0
	if v, _, _ := Resolve(ctx, dims, "m", ids, AggCount, map[string]string{"geo": "EMEA"}, fetch); v != 2 {
		t.Errorf("EMEA count with FR recorded as 0: got %v, want 2", v)
	}
	// A fetch error fails the read, flat or not.
	boom := errors.New("boom")
	failing := func(context.Context, string, map[string]string) (float64, bool, error) { return 0, false, boom }
	for _, rule := range []AggRule{AggAverage, AggCount, AggSum} {
		if _, _, err := ResolveTime(ctx, dims, "m", ids, rule, "sum", map[string]string{"geo": "World"}, failing); !errors.Is(err, boom) {
			t.Errorf("%s failing leaf: err=%v, want boom", rule, err)
		}
	}
}

// Sum keeps its level-by-level traversal exactly: a metric on geo read at a
// pinned parent of a dimension it does not carry still sums once per child
// of that parent (documented Resolve behaviour), while average and count,
// which combine distinct leaves, read each leaf once.
func TestResolveUnrelatedParentPinSumUnchanged(t *testing.T) {
	dims := map[string]*Dimension{
		"geo":    globalGeo("geo"),
		"region": {ID: "region", Members: []Member{{Code: "R"}, {Code: "r1", ParentCode: "R"}, {Code: "r2", ParentCode: "R"}}},
	}
	vals := map[string]float64{"UK": 2, "DE": 3, "US": 4, "JP": 10}
	fetch := func(_ context.Context, _ string, c map[string]string) (float64, bool, error) {
		v, ok := vals[c["geo"]]
		return v, ok, nil
	}
	ctx := context.Background()
	combo := map[string]string{"geo": "World", "region": "R"}
	if v, _, _ := Resolve(ctx, dims, "m", []string{"geo"}, AggSum, combo, fetch); v != 18 {
		t.Errorf("sum: got %v, want 18 (2 x 9, unchanged)", v)
	}
	if v, _, _ := Resolve(ctx, dims, "m", []string{"geo"}, AggAverage, combo, fetch); v != 3 {
		t.Errorf("average: got %v, want 3", v)
	}
	if v, _, _ := Resolve(ctx, dims, "m", []string{"geo"}, AggCount, combo, fetch); v != 3 {
		t.Errorf("count: got %v, want 3 (each leaf once)", v)
	}
	if v, _, _ := ResolveTimeFlat(ctx, dims, "m", []string{"geo"}, AggSum, "", combo, fetch); v != 9 {
		t.Errorf("ResolveTimeFlat sum: got %v, want 9 (each leaf once)", v)
	}
}

// A parent member together with an aggregate period reduces the non-time
// leaves per leaf period first, then the periods by time_summary — the
// scheduler's summarizeOverTime order — whichever dimension ID sorts first.
func TestResolveTimeParentAndAggregatePeriodOrder(t *testing.T) {
	month := &Dimension{ID: "month", IsTime: true, Members: []Member{
		{Code: "FY", TimeIndex: -1},
		{Code: "Q1", ParentCode: "FY", TimeIndex: -1},
		{Code: "m1", ParentCode: "Q1", TimeIndex: 0},
		{Code: "m2", ParentCode: "Q1", TimeIndex: 1},
		{Code: "m3", ParentCode: "FY", TimeIndex: 2},
	}}
	// UK: m1 1, m2 2; DE: m1 10; US: m3 7.
	vals := map[string]float64{"UK/m1": 1, "UK/m2": 2, "DE/m1": 10, "US/m3": 7}
	ctx := context.Background()
	for _, geoID := range []string{"a_geo", "z_geo"} { // sorts before / after "month"
		dims := map[string]*Dimension{geoID: globalGeo(geoID), "month": month}
		ids := []string{geoID, "month"}
		fetch := func(_ context.Context, _ string, c map[string]string) (float64, bool, error) {
			v, ok := vals[c[geoID]+"/"+c["month"]]
			return v, ok, nil
		}
		for _, tc := range []struct {
			geo, period string
			rule        AggRule
			ts          TimeSummaryRule
			want        float64
		}{
			{"EMEA", "Q1", AggSum, "last", 2},           // m2's EMEA sum, not UK last 2 + DE last 10
			{"EMEA", "Q1", AggSum, "sum", 13},           // order-independent for sum/sum
			{"EMEA", "Q1", AggAverage, "average", 3.75}, // mean(m1 mean(1,10)=5.5, m2 2)
			{"EMEA", "Q1", AggCount, "sum", 3},          // m1: 2 leaves, m2: 1
			{"World", "FY", AggAverage, "last", 7},      // m3: US only
			{"World", "FY", AggAverage, "average", (5.5 + 2 + 7) / 3},
			{"World", "", AggCount, "max", 2},
			{"World", "m1", AggAverage, "last", 5.5}, // a leaf period: no time reduction
			// A leaf member over time: each period's one leaf is counted,
			// as the scheduler's slice rows count it; a leaf cell is its value.
			{"UK", "Q1", AggCount, "sum", 2},
			{"UK", "", AggCount, "sum", 2},
			{"DE", "", AggCount, "sum", 1},
			{"DE", "m1", AggCount, "sum", 10},
			{"DE", "", AggAverage, "sum", 10},
		} {
			combo := map[string]string{geoID: tc.geo}
			if tc.period != "" {
				combo["month"] = tc.period
			}
			v, ok, err := ResolveTime(ctx, dims, "m", ids, tc.rule, tc.ts, combo, fetch)
			if err != nil || !ok || v != tc.want {
				t.Errorf("%s %v %s/%s: got %v ok=%v err=%v, want %v", geoID, combo, tc.rule, tc.ts, v, ok, err, tc.want)
			}
		}
		// Nothing recorded under APAC in any period: no value.
		if _, ok, err := ResolveTime(ctx, dims, "m", ids, AggAverage, "sum", map[string]string{geoID: "APAC", "month": "FY"}, fetch); err != nil || ok {
			t.Errorf("%s APAC/FY: ok=%v err=%v, want no value", geoID, ok, err)
		}
	}
}
