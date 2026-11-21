package formula

import (
	"strings"
	"testing"
)

// rankModel: units by item, with a tie (B and C at 20), an item with no
// value (E) and a parent (ALL).
func rankModel() *fakeModel {
	return &fakeModel{
		members: map[string][]fakeMember{
			"ITEM": {
				{code: "ALL"},
				{code: "A", parent: "ALL", props: map[string]string{"grp": "x"}},
				{code: "B", parent: "ALL", props: map[string]string{"grp": "x"}},
				{code: "C", parent: "ALL", props: map[string]string{"grp": "y"}},
				{code: "D", parent: "ALL", props: map[string]string{"grp": "y"}},
				{code: "E", parent: "ALL", props: map[string]string{"grp": "y"}},
			},
		},
		propTypes:  map[string]map[string]string{"ITEM": {"GRP": "text"}},
		metricDims: map[string][]string{"UNITS": {"ITEM"}},
		data:       map[string]float64{"UNITS|A": 30, "UNITS|B": 20, "UNITS|C": 20, "UNITS|D": 5},
	}
}

func TestRankTiesTotalsAndBlanks(t *testing.T) {
	m := rankModel()
	// Every item's rank is RANK.EQ's: ties share a rank, the next is skipped.
	wantDesc := map[string]want{
		"A": num(1), "B": num(2), "C": num(2), "D": num(4),
		"E":   blankV(), // no value: not ranked
		"ALL": blankV(), // a total has no rank
	}
	wantAsc := map[string]want{"A": num(4), "B": num(2), "C": num(2), "D": num(1), "E": blankV(), "ALL": blankV()}
	wantGroup := map[string]want{"A": num(1), "B": num(2), "C": num(1), "D": num(2), "E": blankV()}
	ranks := map[string]*RankedSet{}
	for item, w := range wantDesc {
		ctx := m.ctx(map[string]string{"item": item})
		ctx.Dim.Ranks = ranks // one pass: every item reads the same ranked set
		w.check(t, "RANK(units, item) at "+item, evalIn(t, ctx, `RANK(units, item)`))
		wantAsc[item].check(t, "RANK(units, item, 1) at "+item, evalIn(t, ctx, `RANK(units, item, 1)`))
		if g, ok := wantGroup[item]; ok {
			g.check(t, "RANKIFS by group at "+item, evalIn(t, ctx, `RANKIFS(units, item.grp, item.grp)`))
		}
	}
	if len(ranks) == 0 {
		t.Fatal("the ranked sets were not kept for the pass")
	}
	// Not pinned on the ranked dimension: no rank.
	blankV().check(t, "RANK at no item", evalIn(t, m.ctx(map[string]string{}), `RANK(units, item)`))
}

func TestRankOfAMetricIsRefused(t *testing.T) {
	m := rankModel()
	ctx := m.ctx(map[string]string{"item": "A"})
	ctx.Dim.MetricDims = func(metric string) ([]string, bool) {
		dims, ok := m.metricDims[strings.ToUpper(metric)]
		return dims, ok
	}
	fails(CodeDimensionArgRequired).check(t, "RANK(units, units)", evalIn(t, ctx, `RANK(units, units)`))
}

func TestIsRankFormula(t *testing.T) {
	for f, w := range map[string]bool{
		`RANK(units, item)`: true, `rankifs(units, item, "*")`: true,
		`RANK(units, item) + 0`: false, `IF(RANK(units, item) <= 3, 1, 0)`: false, `units`: false, `(`: false,
	} {
		if got := IsRankFormula(f); got != w {
			t.Errorf("IsRankFormula(%s) = %v, want %v", f, got, w)
		}
	}
}
