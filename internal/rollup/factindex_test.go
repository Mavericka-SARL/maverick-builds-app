package rollup

import (
	"context"
	"fmt"
	"maps"
	"math/rand"
	"testing"
)

// TestFactIndexAnswersAsTheScan: the index's factless children are the
// scan's, at random coordinates over random facts — leaves, a fact at a
// parent account, a fact naming too few dimensions — for every dimension a
// parent of which a read expands, under pins on parents, leaves or nothing.
func TestFactIndexAnswersAsTheScan(t *testing.T) {
	dims := sparseModel()
	for seed := int64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		_, rec, _ := sparseFacts(rng, dims, 1+rng.Intn(300))
		coords, _, _ := rec(context.Background(), "m")
		if seed%5 == 0 {
			coords = append(coords, map[string]string{"acct": "A1", "stage": "s1"}) // too few dimensions
		}
		idx := buildFactIndex(dims, sparseMetricDims, coords)
		if idx == nil {
			t.Fatalf("seed %d: no index built", seed)
		}
		for i := 0; i < 300; i++ {
			combo := randomCombo(rng, dims)
			for _, dimID := range sparseMetricDims {
				parent := dims[dimID].Members[rng.Intn(len(dims[dimID].Members))].Code
				c := maps.Clone(combo)
				c[dimID] = parent
				children := childrenOf(dims[dimID], parent)
				if len(children) == 0 {
					continue
				}
				want := factlessChildrenScan(coords, dims, sparseMetricDims, dimID, c, children)
				got := idx.factless(dims, sparseMetricDims, dimID, c, children)
				if !maps.Equal(got, want) {
					t.Fatalf("seed %d, %s under %v: index %v, scan %v", seed, dimID, c, got, want)
				}
			}
		}
	}
}

// TestFactIndexIsBuiltOncePerRead: the reads made with one context
// (WithFactIndexes) share an index of a metric's facts, and another set of
// dimensions (a scope trimming a dimension's members) gets its own.
func TestFactIndexIsBuiltOncePerRead(t *testing.T) {
	dims := sparseModel()
	_, rec, _ := sparseFacts(rand.New(rand.NewSource(7)), dims, 50)
	coords, _, _ := rec(context.Background(), "m")
	ctx := WithFactIndexes(context.Background())
	for i := 0; i < indexAfterScans; i++ {
		if got := factIndexFor(ctx, dims, "m", sparseMetricDims, coords); got != nil {
			t.Fatalf("expansion %d was served an index before %d scans", i+1, indexAfterScans)
		}
	}
	a := factIndexFor(ctx, dims, "m", sparseMetricDims, coords)
	b := factIndexFor(WithRecorded(ctx, rec), dims, "m", sparseMetricDims, coords)
	if a == nil || a != b {
		t.Fatalf("second read built its own index: %p, %p", a, b)
	}
	trimmed := maps.Clone(dims)
	trimmed["acct"] = &Dimension{ID: "acct", Members: dims["acct"].Members[:4]}
	if c := factIndexFor(ctx, trimmed, "m", sparseMetricDims, coords); c == nil || c == a {
		t.Fatalf("other dimensions served the first index")
	}
	if got := factIndexFor(context.Background(), dims, "m", sparseMetricDims, coords); got != nil {
		t.Fatalf("a context keeping no indexes built one")
	}
}

// TestFactIndexLeavesACycleToTheScan: a member chain past maxChainDepth (a
// cycle) is not indexed; the scan answers, as it always has.
func TestFactIndexLeavesACycleToTheScan(t *testing.T) {
	dims := map[string]*Dimension{"d": {ID: "d", Members: []Member{{Code: "a", ParentCode: "b"}, {Code: "b", ParentCode: "a"}}}}
	if idx := buildFactIndex(dims, []string{"d"}, []map[string]string{{"d": "a"}}); idx != nil {
		t.Fatalf("a cyclic chain was indexed")
	}
}

// opexModel is a dense grid shaped like the OPEX budget read on the
// Playground (2026-11-22): 8 accounts under one root, 12 cost centres under
// departments under divisions, 24 months under quarters under two years,
// two versions — a fact at nearly every leaf.
func opexModel() (map[string]*Dimension, []string) {
	acct := &Dimension{ID: "acct", Members: []Member{{Code: "ALL"}}}
	for i := 1; i <= 8; i++ {
		acct.Members = append(acct.Members, Member{Code: fmt.Sprintf("6%d00", i), ParentCode: "ALL"})
	}
	cc := &Dimension{ID: "cc", Members: []Member{{Code: "ALLCC"}}}
	for d := 1; d <= 3; d++ {
		div := fmt.Sprintf("DIV%d", d)
		cc.Members = append(cc.Members, Member{Code: div, ParentCode: "ALLCC"})
		for p := 1; p <= 2; p++ {
			dep := fmt.Sprintf("%s-D%d", div, p)
			cc.Members = append(cc.Members, Member{Code: dep, ParentCode: div})
			for c := 1; c <= 2; c++ {
				cc.Members = append(cc.Members, Member{Code: fmt.Sprintf("%s-C%d", dep, c), ParentCode: dep})
			}
		}
	}
	period := &Dimension{ID: "period", IsTime: true}
	idx := 0
	for _, fy := range []string{"FY2026", "FY2027"} {
		period.Members = append(period.Members, Member{Code: fy, TimeIndex: -1})
		for q := 1; q <= 4; q++ {
			qc := fmt.Sprintf("%sQ%d", fy, q)
			period.Members = append(period.Members, Member{Code: qc, ParentCode: fy, TimeIndex: -1})
			for m := 1; m <= 3; m++ {
				period.Members = append(period.Members, Member{Code: fmt.Sprintf("%sM%02d", fy, (q-1)*3+m), ParentCode: qc, TimeIndex: idx})
				idx++
			}
		}
	}
	version := &Dimension{ID: "version", Members: []Member{{Code: "ACTUAL"}, {Code: "BUDGET"}}}
	return map[string]*Dimension{"acct": acct, "cc": cc, "period": period, "version": version},
		[]string{"acct", "cc", "period", "version"}
}

// BenchmarkDenseScopedRead reads the OPEX-shaped metric, its facts listed,
// at every account × cost centre (parents included) with a version and a
// year pinned — the cells a scoped grid read works out.
func BenchmarkDenseScopedRead(b *testing.B) {
	dims, metricDims := opexModel()
	facts := map[string]float64{}
	var coords []map[string]string
	rng := rand.New(rand.NewSource(1))
	for _, a := range leafCodes(dims["acct"]) {
		for _, c := range leafCodes(dims["cc"]) {
			for _, p := range leafCodes(dims["period"]) {
				for _, v := range []string{"ACTUAL", "BUDGET"} {
					if rng.Intn(10) == 0 {
						continue // a few gaps
					}
					combo := map[string]string{"acct": a, "cc": c, "period": p, "version": v}
					facts[comboKey(combo)] = float64(rng.Intn(10000))
					coords = append(coords, combo)
				}
			}
		}
	}
	fetch := func(_ context.Context, _ string, combo map[string]string) (float64, bool, error) {
		v, ok := facts[comboKey(combo)]
		return v, ok, nil
	}
	rec := func(context.Context, string) ([]map[string]string, bool, error) { return coords, true, nil }
	b.ReportMetric(float64(len(coords)), "facts")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx := WithFactIndexes(context.Background())
		for _, a := range dims["acct"].Members {
			for _, c := range dims["cc"].Members {
				combo := map[string]string{"acct": a.Code, "cc": c.Code, "period": "FY2027", "version": "BUDGET"}
				if _, _, err := ResolveTimeRecorded(WithRecorded(ctx, rec), dims, "m", metricDims, AggSum, "", combo, fetch); err != nil {
					b.Fatal(err)
				}
			}
		}
	}
}
