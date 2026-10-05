package formula

import (
	"strings"
	"testing"
)

// activityModel is a strategic-activities layout: activities carry their
// Region and Status as pick-lists, and an amount.
func activityModel() *fakeModel {
	key := PicklistKey
	return &fakeModel{
		members: map[string][]fakeMember{
			"ACTIVITY": {{code: "A1"}, {code: "A2"}, {code: "A3"}, {code: "A4"}},
			"REGION":   {{code: "NA"}, {code: "EU"}},
			"STATUS":   {{code: "Draft"}, {code: "Committed"}, {code: "Cancelled"}},
		},
		metricDims: map[string][]string{
			"ACT_REGION": {"ACTIVITY"},
			"ACT_STATUS": {"ACTIVITY"},
			"SPEND":      {"ACTIVITY"},
		},
		data: map[string]float64{
			"ACT_REGION|A1": key("NA"), "ACT_REGION|A2": key("EU"), "ACT_REGION|A3": key("NA"),
			"ACT_STATUS|A1": key("Committed"), "ACT_STATUS|A2": key("Draft"), "ACT_STATUS|A3": key("Cancelled"),
			"SPEND|A1": 10, "SPEND|A2": 20, "SPEND|A3": 5, "SPEND|A4": 7,
		},
	}
}

// picklistCtx is fakeModel.ctx with the pick-lists and metric dimensions
// wired in, the way calculation.DimMetadata wires them.
func picklistCtx(m *fakeModel, cell map[string]string, picklists map[string]string) *EvalContext {
	ctx := m.ctx(cell)
	codec := func(dim string) PicklistCodec {
		return PicklistCodec{
			Dim: dim,
			Code: func(k float64) (string, bool) {
				for _, mem := range m.members[strings.ToUpper(dim)] {
					if PicklistKey(mem.code) == k {
						return mem.code, true
					}
				}
				return "", false
			},
			Key: func(code string) (float64, bool) {
				for _, mem := range m.members[strings.ToUpper(dim)] {
					if strings.EqualFold(mem.code, code) {
						return PicklistKey(mem.code), true
					}
				}
				return 0, false
			},
		}
	}
	ctx.Dim.Picklist = func(metric string) (PicklistCodec, bool) {
		dim, ok := picklists[strings.ToUpper(metric)]
		if !ok {
			return PicklistCodec{}, false
		}
		return codec(dim), true
	}
	ctx.Dim.MetricDims = func(metric string) ([]string, bool) {
		dims, ok := m.metricDims[strings.ToUpper(metric)]
		return dims, ok
	}
	// The cell's own metric values, as evaluators bind them: numbers.
	if a, ok := cell["activity"]; ok {
		for _, metric := range []string{"ACT_REGION", "ACT_STATUS", "SPEND"} {
			if v, ok := m.data[metric+"|"+a]; ok {
				ctx.Vars[metric] = NumberVal(v)
			}
		}
	}
	return ctx
}

func TestPicklistKeyIsStableAndLarge(t *testing.T) {
	if PicklistKey("North America") != PicklistKey("North America") {
		t.Fatal("PicklistKey is not deterministic")
	}
	if PicklistKey("NA") == PicklistKey("na") {
		t.Fatal("PicklistKey ignores case; codes are stored exactly")
	}
	for _, code := range []string{"", "Yes", "No", "SPEND-001", "North America"} {
		k := PicklistKey(code)
		if k < 1<<50 || k >= 1<<51 || k != float64(int64(k)) {
			t.Fatalf("PicklistKey(%q) = %v, want an integer in [2^50, 2^51)", code, k)
		}
	}
}

func TestPicklistReadsAsMemberCode(t *testing.T) {
	m := activityModel()
	pl := map[string]string{"ACT_REGION": "region", "ACT_STATUS": "status"}
	ctx := picklistCtx(m, map[string]string{"activity": "A1"}, pl)
	for _, c := range []struct {
		formula string
		want    Value
	}{
		{`act_region`, StringVal("NA")},
		{`act_status = "committed"`, BoolVal(true)},
		{`IF(AND(act_status <> "Cancelled", act_region = "NA"), spend, 0)`, NumberVal(10)},
		{`act_region & "-" & act_status`, StringVal("NA-Committed")},
		{`LOOKUP(act_status, activity, "A3")`, StringVal("Cancelled")},
		{`LOOKUP(act_status, activity, "A4")`, BlankVal()}, // nothing chosen
		{`LOOKUP(spend, activity, "A2")`, NumberVal(20)},
	} {
		if got := evalIn(t, ctx, c.formula); got != c.want {
			t.Errorf("%s = %v (%v), want %v", c.formula, got, got.Kind(), c.want)
		}
	}
	// A key no member has any more is #N/A, never a number.
	ctx.Vars["ACT_REGION"] = NumberVal(PicklistKey("APAC"))
	if got := evalIn(t, ctx, `act_region`); !got.IsError() || got.Err().Code != "#N/A" {
		t.Errorf("a stale key = %v, want #N/A", got)
	}
}

func TestMetricCriteriaRanges(t *testing.T) {
	m := activityModel()
	pl := map[string]string{"ACT_REGION": "region", "ACT_STATUS": "status"}
	// A region cell of another grid: activities are not on it.
	ctx := picklistCtx(m, map[string]string{"region": "NA"}, pl)
	for _, c := range []struct {
		formula string
		want    float64
	}{
		{`SUMIFS(spend, act_region, region)`, 15},                            // A1 + A3
		{`SUMIFS(spend, act_region, region, act_status, "<>Cancelled")`, 10}, // A1
		{`SUMIFS(spend, act_region, "EU")`, 20},
		{`SUMIFS(spend, act_region, "")`, 7},       // A4 has no region
		{`COUNTIFS(act_region, "NA")`, 2},          // counts activities
		{`COUNTIFS(act_status, "<>Cancelled")`, 3}, // blank status included
		{`SUMIFS(spend, spend, ">=10")`, 30},       // a numeric metric range
		{`SUMIF(act_region, "NA", spend)`, 15},
		{`AVERAGEIFS(spend, act_region, "NA")`, 7.5},
		{`MAXIFS(spend, act_status, "<>Cancelled")`, 20},
	} {
		got := evalIn(t, ctx, c.formula)
		if n, ok := got.Number(); !ok || got.IsError() || n != c.want {
			t.Errorf("%s = %v, want %v", c.formula, got, c.want)
		}
	}
	// A dimension wins a name it shares with a metric, as everywhere.
	if got := evalIn(t, ctx, `SUMIFS(spend, activity, "A4")`); got != NumberVal(7) {
		t.Errorf("dimension range = %v, want 7", got)
	}
	// A pick-list is not a number to add up.
	if got := evalIn(t, ctx, `SUMIFS(act_region, act_status, "Draft")`); !got.IsError() || !strings.Contains(got.Err().Message, "pick-list") {
		t.Errorf("SUMIFS over a pick-list = %v, want a #VALUE! naming the pick-list", got)
	}
}

func TestPicklistResult(t *testing.T) {
	m := activityModel()
	codec := picklistCtx(m, nil, map[string]string{"S": "status"}).Dim.Picklist
	c, _ := codec("s")
	for _, tc := range []struct {
		in   Value
		want Value
	}{
		{StringVal("Committed"), NumberVal(PicklistKey("Committed"))},
		{StringVal("committed"), NumberVal(PicklistKey("Committed"))}, // the stored code's key
		{BlankVal(), BlankVal()},
	} {
		if got := PicklistResult(c, "s", tc.in); got != tc.want {
			t.Errorf("PicklistResult(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if got := PicklistResult(c, "s", StringVal("Review")); !got.IsError() || got.Err().Code != "#N/A" {
		t.Errorf("an unknown member = %v, want #N/A", got)
	}
}

func TestAnalyzeRangeNames(t *testing.T) {
	an, err := Analyze(`SUMIFS(spend, act_region, region, region.segment, "x") + COUNTIF(act_status, "Draft")`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(an.RangeNames, ",") != "act_region,act_status" {
		t.Errorf("RangeNames = %v, want [act_region act_status]", an.RangeNames)
	}
}
