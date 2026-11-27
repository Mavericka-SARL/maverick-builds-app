package formula

import (
	"fmt"
	"math"
	"testing"
)

// The criteria index (criteria_index.go) gives every conditional function
// the answer testing each member gives: texts in any case, numbers to 15
// digits, blanks, two criteria, and the criteria it leaves alone.
func TestCriteriaIndexAnswersAsTestingEveryMember(t *testing.T) {
	key := PicklistKey
	m := &fakeModel{
		members: map[string][]fakeMember{
			"ACTIVITY": {},
			"REGION":   {{code: "NA"}, {code: "EU"}, {code: "APAC"}},
		},
		metricDims: map[string][]string{
			"ACT_REGION": {"ACTIVITY"},
			"ACT_SIZE":   {"ACTIVITY"},
			"SPEND":      {"ACTIVITY"},
		},
		data: map[string]float64{},
	}
	regions := []string{"NA", "EU", "", "NA", "EU", "NA"}
	sizes := []float64{5, 5.0000000000000001, 7, 0, 3, 12.5}
	for i := 1; i <= 48; i++ {
		a := fmt.Sprintf("A%02d", i)
		m.members["ACTIVITY"] = append(m.members["ACTIVITY"], fakeMember{code: a})
		if r := regions[i%len(regions)]; r != "" {
			m.data["ACT_REGION|"+a] = key(r)
		}
		if i%7 != 0 {
			m.data["ACT_SIZE|"+a] = sizes[i%len(sizes)]
		}
		m.data["SPEND|"+a] = float64(i)*1.1 + 0.07
	}
	pl := map[string]string{"ACT_REGION": "region"}
	formulas := []string{
		`SUMIFS(spend, act_region, region)`,
		`SUMIFS(spend, act_region, "na")`,
		`COUNTIFS(act_region, "EU")`,
		`AVERAGEIFS(spend, act_region, region)`,
		`MAXIFS(spend, act_region, region)`,
		`MINIFS(spend, act_region, region)`,
		`SUMIFS(spend, act_size, 5)`,
		`SUMIFS(spend, act_size, "=5")`,
		`SUMIFS(spend, act_size, 0)`,
		`COUNTIFS(act_size, 12.5)`,
		`SUMIFS(spend, act_region, region, act_size, ">4")`,
		`SUMIFS(spend, act_region, "<>NA")`,
		`SUMIFS(spend, act_region, "N*")`,
		`SUMIFS(spend, act_region, "")`,
		`SUMIFS(spend, act_region, "APAC")`,
	}
	used := false
	for _, region := range []string{"NA", "EU", "APAC"} {
		for _, f := range formulas {
			plain := picklistCtx(m, map[string]string{"region": region}, pl)
			want := evalIn(t, plain, f)
			indexed := picklistCtx(m, map[string]string{"region": region}, pl)
			idx := NewCriteriaIndexes()
			indexed.Dim.CriteriaIndexes = idx
			got := evalIn(t, indexed, f)
			gn, _ := got.Number()
			wn, _ := want.Number()
			if fmt.Sprint(got) != fmt.Sprint(want) || got.Kind() != want.Kind() || math.Float64bits(gn) != math.Float64bits(wn) {
				t.Errorf("%s at %s: with the index %v, member by member %v", f, region, got, want)
			}
			used = used || len(idx.m) > 0
		}
	}
	if !used {
		t.Error("no formula used the index")
	}
}
