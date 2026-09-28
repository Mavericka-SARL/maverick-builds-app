package rollup

import (
	"context"
	"testing"
)

// regionMonth: region EMEA > {DE, UK}; month FY > four dated leaf months.
// Only DE records a value, in the first three months.
func regionMonth() (map[string]*Dimension, RawValue) {
	dims := map[string]*Dimension{
		"region": {ID: "region", Members: []Member{
			{Code: "EMEA"}, {Code: "DE", ParentCode: "EMEA"}, {Code: "UK", ParentCode: "EMEA"},
		}},
		"month": {ID: "month", IsTime: true, Members: []Member{
			{Code: "FY", TimeIndex: -1},
			{Code: "m1", ParentCode: "FY", TimeIndex: 0},
			{Code: "m2", ParentCode: "FY", TimeIndex: 1},
			{Code: "m3", ParentCode: "FY", TimeIndex: 2},
			{Code: "m4", ParentCode: "FY", TimeIndex: 3},
		}},
	}
	table := map[string]float64{
		fetchKey("bal", map[string]string{"region": "DE", "month": "m1"}): 10,
		fetchKey("bal", map[string]string{"region": "DE", "month": "m2"}): 20,
		fetchKey("bal", map[string]string{"region": "DE", "month": "m3"}): 30,
	}
	fetch := func(_ context.Context, metricID string, combo map[string]string) (float64, bool, error) {
		v, ok := table[fetchKey(metricID, combo)]
		return v, ok, nil
	}
	return dims, fetch
}

// A time reduction at a PARENT member skips the periods nothing beneath it
// recorded, as it does at a leaf: EMEA's closing balance over the year is
// DE's March balance, not the 0 an empty April aggregates to — whichever
// dimension the reduction expands first.
func TestResolveTimeSkipsUnrecordedPeriodsAtParent(t *testing.T) {
	dims, fetch := regionMonth()
	ctx := context.Background()
	ids := []string{"region", "month"}
	for _, tc := range []struct {
		name  string
		combo map[string]string
		rule  TimeSummaryRule
		want  float64
	}{
		{"time unpinned, last", map[string]string{"region": "EMEA"}, "last", 30},
		{"aggregate period, last", map[string]string{"region": "EMEA", "month": "FY"}, "last", 30},
		{"time unpinned, average", map[string]string{"region": "EMEA"}, "average", 20},
		{"aggregate period, average", map[string]string{"region": "EMEA", "month": "FY"}, "average", 20},
		{"leaf member, last", map[string]string{"region": "DE"}, "last", 30},
	} {
		v, ok, err := ResolveTime(ctx, dims, "bal", ids, AggSum, tc.rule, tc.combo, fetch)
		if err != nil || !ok || v != tc.want {
			t.Errorf("%s: got %v ok=%v err=%v, want %v", tc.name, v, ok, err, tc.want)
		}
	}

	// ResolveTimeRecorded: a parent in a month nobody beneath recorded has
	// no value; one with a recorded leaf does.
	if v, ok, err := ResolveTimeRecorded(ctx, dims, "bal", ids, AggSum, "last", map[string]string{"region": "EMEA", "month": "m4"}, fetch); err != nil || ok {
		t.Errorf("EMEA/m4: got %v ok=%v err=%v, want no value", v, ok, err)
	}
	if v, ok, err := ResolveTimeRecorded(ctx, dims, "bal", ids, AggSum, "last", map[string]string{"region": "EMEA", "month": "m2"}, fetch); err != nil || !ok || v != 20 {
		t.Errorf("EMEA/m2: got %v ok=%v err=%v, want 20", v, ok, err)
	}
}

// A fetch answering ErrNoValue leaves that coordinate out of the aggregate
// (a blank formula result is not a 0 in an average), and an aggregate of
// nothing but such coordinates has no value.
func TestResolveErrNoValueLeavesCoordinateOut(t *testing.T) {
	dims := map[string]*Dimension{"dept": deptDim()}
	fetch := func(_ context.Context, _ string, combo map[string]string) (float64, bool, error) {
		if combo["dept"] == "sales" {
			return 8, true, nil
		}
		return 0, false, ErrNoValue
	}
	ctx := context.Background()
	if v, ok, err := Resolve(ctx, dims, "m", []string{"dept"}, AggAverage, map[string]string{"dept": "region-a"}, fetch); err != nil || !ok || v != 8 {
		t.Errorf("average over {sales 8, ga no value}: got %v ok=%v err=%v, want 8", v, ok, err)
	}
	if v, ok, err := Resolve(ctx, dims, "m", []string{"dept"}, AggAverage, map[string]string{}, fetch); err != nil || !ok || v != 8 {
		t.Errorf("unpinned: got %v ok=%v err=%v, want 8", v, ok, err)
	}
	if _, ok, err := Resolve(ctx, dims, "m", []string{"dept"}, AggSum, map[string]string{"dept": "ga"}, fetch); err != nil || ok {
		t.Errorf("leaf with no value: ok=%v err=%v, want no value and no error", ok, err)
	}
	none := func(context.Context, string, map[string]string) (float64, bool, error) { return 0, false, ErrNoValue }
	if _, ok, err := Resolve(ctx, dims, "m", []string{"dept"}, AggSum, map[string]string{"dept": "region-a"}, none); err != nil || ok {
		t.Errorf("parent of nothing: ok=%v err=%v, want no value and no error", ok, err)
	}
}
