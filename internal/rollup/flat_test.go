package rollup

import (
	"context"
	"errors"
	"testing"
)

// worldGeo: World > EMEA > {DE, UK, FR}, World > AMER > {US}; a two-level
// hierarchy with uneven branches, where a mean of means differs from the
// mean of the leaves.
func worldGeo() *Dimension {
	return &Dimension{ID: "geo", Members: []Member{
		{Code: "World"},
		{Code: "EMEA", ParentCode: "World"}, {Code: "AMER", ParentCode: "World"},
		{Code: "DE", ParentCode: "EMEA"}, {Code: "UK", ParentCode: "EMEA"}, {Code: "FR", ParentCode: "EMEA"},
		{Code: "US", ParentCode: "AMER"},
	}}
}

// ResolveTimeFlat combines the leaves under the point once, by agg_rule —
// the scheduler's rule — never level by level. World averaged over
// {DE 1, UK 2, FR 3, US 6} is 3, not mean(EMEA 2, AMER 6) = 4. A leaf with
// no value (ErrNoValue, ok=false) is left out; a real error fails the read.
func TestResolveTimeFlatCombinesLeavesOnce(t *testing.T) {
	dims := map[string]*Dimension{"geo": worldGeo()}
	vals := map[string]float64{"DE": 1, "UK": 2, "FR": 3, "US": 6}
	fetch := func(_ context.Context, _ string, c map[string]string) (float64, bool, error) {
		v, ok := vals[c["geo"]]
		if !ok {
			return 0, false, ErrNoValue
		}
		return v, true, nil
	}
	ctx := context.Background()
	ids := []string{"geo"}
	for _, tc := range []struct {
		combo map[string]string
		rule  AggRule
		want  float64
	}{
		{map[string]string{"geo": "World"}, AggAverage, 3},
		{map[string]string{"geo": "EMEA"}, AggAverage, 2},
		{map[string]string{}, AggAverage, 3},
		{map[string]string{"geo": "World"}, AggSum, 12},
		{map[string]string{"geo": "World"}, AggCount, 4},
		{map[string]string{"geo": "US"}, AggAverage, 6},
	} {
		v, ok, err := ResolveTimeFlat(ctx, dims, "m", ids, tc.rule, "", tc.combo, fetch)
		if err != nil || !ok || v != tc.want {
			t.Errorf("%v %s: got %v ok=%v err=%v, want %v", tc.combo, tc.rule, v, ok, err, tc.want)
		}
	}
	// ResolveTime combines average flat too — never the mean of means 4.
	if v, _, _ := ResolveTime(ctx, dims, "m", ids, AggAverage, "", map[string]string{"geo": "World"}, fetch); v != 3 {
		t.Errorf("ResolveTime World average: got %v, want the leaf mean 3", v)
	}

	delete(vals, "US")
	if v, ok, err := ResolveTimeFlat(ctx, dims, "m", ids, AggAverage, "", map[string]string{"geo": "World"}, fetch); err != nil || !ok || v != 2 {
		t.Errorf("blank US left out: got %v ok=%v err=%v, want 2", v, ok, err)
	}
	if _, ok, err := ResolveTimeFlat(ctx, dims, "m", ids, AggAverage, "", map[string]string{"geo": "AMER"}, fetch); err != nil || ok {
		t.Errorf("AMER with no leaf value: ok=%v err=%v, want no value", ok, err)
	}
	boom := errors.New("boom")
	failing := func(context.Context, string, map[string]string) (float64, bool, error) { return 0, false, boom }
	if _, _, err := ResolveTimeFlat(ctx, dims, "m", ids, AggAverage, "", map[string]string{"geo": "World"}, failing); !errors.Is(err, boom) {
		t.Errorf("failing leaf: err=%v, want boom", err)
	}
}

// With a time dimension, the non-time leaves combine flat per period and
// the periods reduce by time_summary (the scheduler's summarizeOverTime);
// a period with no leaf value is skipped, except that an average counts it
// as 0. A leaf reached twice through an unrelated pinned parent counts once.
func TestResolveTimeFlatTimeAndUnrelatedPins(t *testing.T) {
	dims, _ := regionMonth()
	dims["geo"] = worldGeo()
	vals := map[string]float64{
		"DE/m1": 1, "UK/m1": 2, "FR/m1": 3, "US/m1": 6, // m1 flat mean 3
		"DE/m2": 10, // m2: 10
	}
	fetch := func(_ context.Context, _ string, c map[string]string) (float64, bool, error) {
		v, ok := vals[c["geo"]+"/"+c["month"]]
		return v, ok, nil
	}
	ctx := context.Background()
	ids := []string{"geo", "month"}
	for _, tc := range []struct {
		combo map[string]string
		ts    TimeSummaryRule
		want  float64
	}{
		{map[string]string{"geo": "World"}, "average", (3 + 10) / 4.0}, // m3, m4 empty: 0
		{map[string]string{"geo": "World", "month": "FY"}, "last", 10},
		{map[string]string{"geo": "World", "month": "m1"}, "none", 3},
		{map[string]string{"geo": "World"}, "sum", 13},
	} {
		v, ok, err := ResolveTimeFlat(ctx, dims, "m", ids, AggAverage, tc.ts, tc.combo, fetch)
		if err != nil || !ok || v != tc.want {
			t.Errorf("%v %s: got %v ok=%v err=%v, want %v", tc.combo, tc.ts, v, ok, err, tc.want)
		}
	}
	if _, ok, err := ResolveTimeFlat(ctx, dims, "m", ids, AggAverage, "none", map[string]string{"geo": "World"}, fetch); err != nil || ok {
		t.Errorf("time summary none over time: ok=%v err=%v, want no value", ok, err)
	}

	// region is unrelated to a metric on geo only: EMEA expands into DE and
	// UK, each reading every geo leaf; each leaf still counts once.
	sums := map[string]float64{"DE": 1, "UK": 2, "FR": 3, "US": 6}
	geoOnly := func(_ context.Context, _ string, c map[string]string) (float64, bool, error) {
		v, ok := sums[c["geo"]]
		return v, ok, nil
	}
	if v, ok, err := ResolveTimeFlat(ctx, dims, "m", []string{"geo"}, AggSum, "", map[string]string{"region": "EMEA"}, geoOnly); err != nil || !ok || v != 12 {
		t.Errorf("unrelated parent pin: got %v ok=%v err=%v, want 12", v, ok, err)
	}
}
