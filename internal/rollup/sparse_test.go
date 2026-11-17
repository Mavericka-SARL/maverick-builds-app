package rollup

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// sparseModel is a small grid shaped like the CRM pipeline: accounts under
// a group (and a grouping of them by region), stages under Open and Closed
// with a leaf root beside them, a flat currency and manager, and a time
// dimension of months under quarters under years.
func sparseModel() map[string]*Dimension {
	acct := &Dimension{ID: "acct", Members: []Member{{Code: "ALL"}, {Code: "G1", ParentCode: "ALL"}}}
	for i := 1; i <= 8; i++ {
		parent := "ALL"
		if i > 6 {
			parent = "G1"
		}
		region := "R1"
		if i%2 == 0 {
			region = "R2"
		}
		acct.Members = append(acct.Members, Member{Code: fmt.Sprintf("A%d", i), ParentCode: parent, Properties: map[string]string{"region": region}})
	}
	month := &Dimension{ID: "month", IsTime: true}
	idx := 0
	for _, fy := range []string{"FY25", "FY26"} {
		month.Members = append(month.Members, Member{Code: fy, TimeIndex: -1})
		for q := 1; q <= 2; q++ {
			qc := fmt.Sprintf("%sQ%d", fy, q)
			month.Members = append(month.Members, Member{Code: qc, ParentCode: fy, TimeIndex: -1})
			for m := 1; m <= 3; m++ {
				month.Members = append(month.Members, Member{Code: fmt.Sprintf("%sM%d", qc, m), ParentCode: qc, TimeIndex: idx})
				idx++
			}
		}
	}
	return map[string]*Dimension{
		"acct":   acct,
		"region": {ID: "region", SourceDimensionID: "acct", SourceProperty: "region", Members: []Member{{Code: "R1"}, {Code: "R2"}}},
		"stage": {ID: "stage", Members: []Member{
			{Code: "OPEN"}, {Code: "s1", ParentCode: "OPEN"}, {Code: "s2", ParentCode: "OPEN"}, {Code: "s3", ParentCode: "OPEN"},
			{Code: "CLOSED"}, {Code: "s4", ParentCode: "CLOSED"}, {Code: "s5", ParentCode: "CLOSED"},
			{Code: "HOLD"},
		}},
		"cur":   {ID: "cur", Members: []Member{{Code: "USD"}, {Code: "EUR"}, {Code: "RUB"}}},
		"mgr":   {ID: "mgr", Members: []Member{{Code: "M1"}, {Code: "M2"}, {Code: "M3"}}},
		"month": month,
	}
}

var sparseMetricDims = []string{"acct", "stage", "cur", "month", "mgr"}

func leafCodes(d *Dimension) []string {
	var out []string
	for _, m := range d.Members {
		if len(childrenOf(d, m.Code)) == 0 {
			out = append(out, m.Code)
		}
	}
	return out
}

// sparseFacts records n random leaf coordinates (one also at a parent
// account, which no read walks) and the fetch over them, answering
// (0, false, nil) everywhere else; calls counts the fetches.
func sparseFacts(rng *rand.Rand, dims map[string]*Dimension, n int) (RawValue, Recorded, *int) {
	facts := map[string]float64{}
	var coords []map[string]string
	add := func(c map[string]string, v float64) {
		if _, dup := facts[comboKey(c)]; !dup {
			coords = append(coords, c)
		}
		facts[comboKey(c)] = v
	}
	for i := 0; i < n; i++ {
		c := map[string]string{}
		for _, id := range sparseMetricDims {
			leaves := leafCodes(dims[id])
			c[id] = leaves[rng.Intn(len(leaves))]
		}
		add(c, math.Round((rng.Float64()*2000-500)*100)/100)
	}
	add(map[string]string{"acct": "G1", "stage": "s1", "cur": "USD", "month": "FY25Q1M1", "mgr": "M1"}, 999)
	calls := 0
	fetch := func(_ context.Context, _ string, combo map[string]string) (float64, bool, error) {
		calls++
		v, ok := facts[comboKey(combo)]
		return v, ok, nil
	}
	rec := func(context.Context, string) ([]map[string]string, bool, error) { return coords, true, nil }
	return fetch, rec, &calls
}

// zeroEverywhereElse is fetch answering a recorded 0 wherever it has no
// fact, as a formula worked out at a leaf does that reads blank inputs as 0
// (WithRecordedZeros).
func zeroEverywhereElse(fetch RawValue) RawValue {
	return func(ctx context.Context, id string, combo map[string]string) (float64, bool, error) {
		v, ok, err := fetch(ctx, id, combo)
		if err == nil && !ok {
			return 0, true, nil
		}
		return v, ok, err
	}
}

// absentEverywhereElse is fetch answering ErrNoValue wherever it has no
// fact, as a formula worked out at a leaf does where none of its inputs has
// a value (WithRecordedAbsent).
func absentEverywhereElse(fetch RawValue) RawValue {
	return func(ctx context.Context, id string, combo map[string]string) (float64, bool, error) {
		v, ok, err := fetch(ctx, id, combo)
		if err == nil && !ok {
			return 0, false, ErrNoValue
		}
		return v, ok, err
	}
}

// randomCombo pins each dimension to a random member (a parent or a leaf),
// or leaves it unpinned; sometimes it pins the region grouping instead of
// an account.
func randomCombo(rng *rand.Rand, dims map[string]*Dimension) map[string]string {
	c := map[string]string{}
	for _, id := range sparseMetricDims {
		if rng.Intn(3) == 0 {
			continue
		}
		ms := dims[id].Members
		c[id] = ms[rng.Intn(len(ms))].Code
	}
	if _, pinned := c["acct"]; !pinned && rng.Intn(2) == 0 {
		c["region"] = []string{"R1", "R2"}[rng.Intn(2)]
	}
	return c
}

// TestRecordedFactsReadTheSameAsEveryLeaf: a read told which coordinates
// have facts (WithRecorded) answers exactly what the walk over every leaf
// coordinate answers — the same bits, the same ok and the same error — for
// every aggregation rule, every time summary and every resolver, at random
// coordinates pinning parents, leaves, a grouping or nothing.
func TestRecordedFactsReadTheSameAsEveryLeaf(t *testing.T) {
	rng := rand.New(rand.NewSource(17))
	dims := sparseModel()
	aggs := []AggRule{AggSum, AggAverage, AggCount, AggFormula, AggRate, AggNone}
	summaries := []TimeSummaryRule{"", "sum", "average", "min", "max", "first", "last", "none"}
	type resolver func(context.Context, map[string]*Dimension, string, []string, AggRule, TimeSummaryRule, map[string]string, RawValue) (float64, bool, error)
	resolvers := map[string]resolver{
		"Resolve": func(ctx context.Context, d map[string]*Dimension, id string, ids []string, a AggRule, _ TimeSummaryRule, c map[string]string, f RawValue) (float64, bool, error) {
			return Resolve(ctx, d, id, ids, a, c, f)
		},
		"ResolveTime":         ResolveTime,
		"ResolveTimeFlat":     ResolveTimeFlat,
		"ResolveTimeRecorded": ResolveTimeRecorded,
	}
	var walked, visited, zeroFlat, zeroFaster int
	// zeroWalk counts the fetches of the walk (calls is reset).
	zeroWalk := func(resolve resolver, dims map[string]*Dimension, agg AggRule, ts TimeSummaryRule, combo map[string]string, fetch RawValue, calls *int) int {
		*calls = 0
		_, _, _ = resolve(context.Background(), dims, "m", sparseMetricDims, agg, ts, combo, fetch)
		return *calls
	}
	cases := 0
	for round := 0; round < 15; round++ {
		fetch, rec, calls := sparseFacts(rng, dims, 5+rng.Intn(60))
		for k := 0; k < 12; k++ {
			combo := randomCombo(rng, dims)
			for _, agg := range aggs {
				for _, ts := range summaries {
					for name, resolve := range resolvers {
						for _, mode := range []struct {
							name  string
							fetch RawValue
							with  func(context.Context, Recorded) context.Context
						}{
							{"zero elsewhere", fetch, WithRecorded},
							{"no value elsewhere", absentEverywhereElse(fetch), WithRecordedAbsent},
							{"recorded zero elsewhere", zeroEverywhereElse(fetch), WithRecordedZeros},
						} {
							*calls = 0
							want, wantOK, wantErr := resolve(context.Background(), dims, "m", sparseMetricDims, agg, ts, combo, mode.fetch)
							if mode.name != "recorded zero elsewhere" {
								walked += *calls
							}
							*calls = 0
							got, gotOK, gotErr := resolve(mode.with(context.Background(), rec), dims, "m", sparseMetricDims, agg, ts, combo, mode.fetch)
							if mode.name != "recorded zero elsewhere" {
								visited += *calls
							} else if name == "ResolveTimeFlat" && (agg == AggSum || agg == AggCount) {
								zeroFlat++
								listed := *calls
								if listed*4 < zeroWalk(resolve, dims, agg, ts, combo, mode.fetch, calls) {
									zeroFaster++
								}
							}
							cases++
							if math.Float64bits(got) != math.Float64bits(want) || gotOK != wantOK || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
								t.Fatalf("%s (%s) agg=%s ts=%q at %v: recorded read (%v, %v, %v), every-leaf read (%v, %v, %v)",
									name, mode.name, agg, ts, combo, got, gotOK, gotErr, want, wantOK, wantErr)
							}
						}
					}
				}
			}
		}
	}
	if visited*20 > walked {
		t.Fatalf("the recorded reads fetched %d times against %d for every leaf: not the facts only", visited, walked)
	}
	if zeroFlat == 0 || zeroFaster*2 < zeroFlat {
		t.Fatalf("recorded zeros: %d of %d flat sum/count reads visited the facts only", zeroFaster, zeroFlat)
	}
	t.Logf("%d reads agree; fetches %d (facts only) against %d (every leaf); recorded zeros: %d of %d flat sum/count reads skipped the empty leaves",
		cases, visited, walked, zeroFaster, zeroFlat)
}

// TestRecordedFactsKeepAMeansDenominator: an average over three leaf
// coordinates with one fact is that fact over three, the two without a fact
// counted as the every-leaf walk counts them, never fetched.
func TestRecordedFactsKeepAMeansDenominator(t *testing.T) {
	dims := map[string]*Dimension{"cur": {ID: "cur", Members: []Member{{Code: "USD"}, {Code: "EUR"}, {Code: "RUB"}}}}
	calls := 0
	fetch := func(_ context.Context, _ string, c map[string]string) (float64, bool, error) {
		calls++
		if c["cur"] == "EUR" {
			return 9, true, nil
		}
		return 0, false, nil
	}
	rec := func(context.Context, string) ([]map[string]string, bool, error) {
		return []map[string]string{{"cur": "EUR"}}, true, nil
	}
	v, ok, err := Resolve(WithRecorded(context.Background(), rec), dims, "m", []string{"cur"}, AggFormula, map[string]string{}, fetch)
	if err != nil || !ok || v != 3 || calls != 1 {
		t.Fatalf("got (%v, %v, %v) after %d fetches; want (3, true, nil) after 1", v, ok, err, calls)
	}
}
