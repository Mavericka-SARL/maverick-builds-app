package rollup

import (
	"context"
	"errors"
	"slices"
)

// Recorded lists every coordinate at which a RawValue has a value for
// metricID: each combo names exactly the metric's dimensions. known=false
// means the list is not known, and a read walks every leaf coordinate.
//
// The contract binds it to the RawValue it is passed with: at a listed
// coordinate the RawValue answers as it likes, at any other it answers
// (0, false, nil) — a RawValue that is a lookup in a map of stored facts.
// A RawValue that works a value out (a formula at a point) has no Recorded.
type Recorded func(ctx context.Context, metricID string) (coords []map[string]string, known bool, err error)

type recordedKey struct{}

// elsewhere is what the RawValue answers at a coordinate a Recorded does
// not list.
type elsewhere int

const (
	elsewhereZero         elsewhere = iota // (0, false, nil): a 0 that is no recorded value
	elsewhereNoValue                       // ErrNoValue: left out
	elsewhereRecordedZero                  // (0, true, nil): a recorded 0
)

// recordedCtx is what WithRecorded and its variants put on a context.
type recordedCtx struct {
	rec       Recorded
	elsewhere elsewhere
}

// virtualZeros listens for coordinates a Recorded with
// elsewhereRecordedZero did not list: each is a recorded 0, which makes a
// read recorded although no fetch answered ok. Every reader in this package
// that asks whether a leaf was recorded (a tracked fetch, a flat read's
// collect) listens, and a recorded 0 reaches every listener up the chain,
// as an ok fetch reaches every wrapper around it.
type virtualZeros struct {
	seen   bool
	parent *virtualZeros
}

type virtualZerosKey struct{}

// listenZeros is ctx with a new listener under the current one.
func listenZeros(ctx context.Context) (context.Context, *virtualZeros) {
	parent, _ := ctx.Value(virtualZerosKey{}).(*virtualZeros)
	obs := &virtualZeros{parent: parent}
	return context.WithValue(ctx, virtualZerosKey{}, obs), obs
}

func (v *virtualZeros) note() {
	for o := v; o != nil; o = o.parent {
		o.seen = true
	}
}

// WithRecorded makes the resolvers called with the returned context visit,
// below a coordinate, only the leaf coordinates rec lists, instead of every
// combination of the leaves the coordinate covers — the same value, read in
// the same order, from the facts there are rather than from all the places
// there could be one. A CRM pipeline grid over accounts, months, currencies,
// stages, managers and service categories has ten million leaf coordinates
// and a few hundred deals: one chart bar walked tens of thousands of them,
// and a read grouped by manager with nothing else fixed ran for minutes
// (2026-11-17).
//
// Pass it only on the context of a call whose RawValue rec describes.
func WithRecorded(ctx context.Context, rec Recorded) context.Context {
	return context.WithValue(WithFactIndexes(ctx), recordedKey{}, recordedCtx{rec: rec})
}

// WithRecordedZeros is WithRecorded for a RawValue that answers a recorded
// 0 — (0, true, nil) — at every coordinate rec does not list: a formula
// worked out at each leaf that reads its blank inputs as 0 there. A recorded
// 0 adds nothing to a sum or a count, and a level-by-level mean counts it as
// any 0 (the empty coordinates); it makes a period or a coordinate one with
// a value, which every listening reader hears. Only a flat average, which
// counts each distinct leaf with a value once, walks every coordinate.
func WithRecordedZeros(ctx context.Context, rec Recorded) context.Context {
	return context.WithValue(WithFactIndexes(ctx), recordedKey{}, recordedCtx{rec: rec, elsewhere: elsewhereRecordedZero})
}

// WithoutRecorded is ctx with no Recorded: the reads a RawValue makes of
// other metrics while it works its value out never see its caller's list.
func WithoutRecorded(ctx context.Context) context.Context {
	if ctx.Value(recordedKey{}) == nil && ctx.Value(virtualZerosKey{}) == nil {
		return ctx
	}
	return context.WithValue(context.WithValue(ctx, recordedKey{}, recordedCtx{}), virtualZerosKey{}, (*virtualZeros)(nil))
}

// WithRecordedAbsent is WithRecorded for a RawValue that answers ErrNoValue
// at every coordinate rec does not list, where WithRecorded's answers
// (0, false, nil): a coordinate left out, not a 0 — a formula worked out at
// each leaf that has no value where none of its inputs has one.
func WithRecordedAbsent(ctx context.Context, rec Recorded) context.Context {
	return context.WithValue(WithFactIndexes(ctx), recordedKey{}, recordedCtx{rec: rec, elsewhere: elsewhereNoValue})
}

func recordedFor(ctx context.Context, metricID string) (coords []map[string]string, known bool, at elsewhere, err error) {
	rc, _ := ctx.Value(recordedKey{}).(recordedCtx)
	if rc.rec == nil {
		return nil, false, elsewhereZero, nil
	}
	coords, known, err = rc.rec(ctx, metricID)
	return coords, known, rc.elsewhere, err
}

// flatCtx is ctx for a flat read of aggRule (resolveFlat), listening for
// recorded zeros: a Recorded answering recorded zeros elsewhere serves sum,
// count and none (a 0 adds nothing to them) and is dropped for an average,
// whose mean counts each distinct leaf's 0 once.
func flatCtx(ctx context.Context, aggRule AggRule) (context.Context, *virtualZeros) {
	if rc, _ := ctx.Value(recordedKey{}).(recordedCtx); rc.rec != nil && rc.elsewhere == elsewhereRecordedZero {
		switch aggRule {
		case AggSum, AggCount, AggNone:
		case AggAverage, AggFormula, AggRate:
			ctx = WithoutRecorded(ctx)
		}
	}
	return listenZeros(ctx)
}

// resolveListed is resolve's tier 2 over the coordinates ctx's Recorded
// lists, when it lists metricID's (handled=false: walk every coordinate).
func resolveListed(
	ctx context.Context,
	metricID string,
	dimIDs []string,
	aggRule AggRule,
	codeSets [][]string,
	fetch RawValue,
) (v float64, ok, handled bool, err error) {
	coords, known, at, err := recordedFor(ctx, metricID)
	if err != nil {
		return 0, false, true, err
	}
	if !known {
		return 0, false, false, nil
	}
	v, ok, empty, err := combineRecorded(ctx, metricID, dimIDs, aggRule, codeSets, coords, at == elsewhereNoValue, fetch)
	if at == elsewhereRecordedZero && empty > 0 {
		obs, _ := ctx.Value(virtualZerosKey{}).(*virtualZeros)
		obs.note()
	}
	return v, ok, true, err
}

// combineRecorded is resolve's combination of the leaf coordinates codeSets
// spans (their cartesian product, in dimIDs order), fetching only the ones
// coords lists. Every other coordinate is one fetch answers (0, false, nil)
// at, which resolve adds as a 0: it is counted, never fetched, so a mean
// keeps its denominator and a sum its value. The listed ones are fetched in
// the product's own order — a code listed twice in a set is fetched twice,
// as the product walks it — so wrapping RawValues see the same calls, less
// the empty ones, and floating-point sums come out the same to the bit.
// absent: the unlisted coordinates answer ErrNoValue, so they are left out
// instead of counted.
func combineRecorded(
	ctx context.Context,
	metricID string,
	dimIDs []string,
	aggRule AggRule,
	codeSets [][]string,
	coords []map[string]string,
	absent bool,
	fetch RawValue,
) (float64, bool, int64, error) {
	total := int64(1)
	positions := make([]map[string][]int, len(dimIDs))
	for i, codes := range codeSets {
		if len(codes) == 0 {
			return 0, true, 0, nil // cartesianProduct's empty product
		}
		total *= int64(len(codes))
		pos := make(map[string][]int, len(codes))
		for p, code := range codes {
			pos[code] = append(pos[code], p)
		}
		positions[i] = pos
	}

	type hit struct {
		at    []int
		combo map[string]string
	}
	var hits []hit
	lists := make([][]int, len(dimIDs))
	for _, c := range coords {
		if len(c) != len(dimIDs) {
			continue // not a coordinate of the metric's dimensions: never walked
		}
		match := true
		for i, id := range dimIDs {
			code, ok := c[id]
			if !ok {
				match = false
				break
			}
			if lists[i], ok = positions[i][code]; !ok {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		for _, at := range positionTuples(lists) {
			hits = append(hits, hit{at: at, combo: c})
		}
	}
	slices.SortFunc(hits, func(a, b hit) int { return slices.Compare(a.at, b.at) })

	vals := make([]float64, 0, len(hits))
	for i, h := range hits {
		if err := stopped(ctx, i); err != nil {
			return 0, false, 0, err
		}
		exact := make(map[string]string, len(dimIDs))
		for _, id := range dimIDs {
			exact[id] = h.combo[id]
		}
		v, _, err := fetch(ctx, metricID, exact)
		if errors.Is(err, ErrNoValue) {
			continue // no value: left out, not a 0
		}
		if err != nil {
			return 0, false, 0, err
		}
		vals = append(vals, v)
	}
	empty := total - int64(len(hits))
	if absent {
		empty = 0
	}
	if len(vals) == 0 && empty == 0 {
		return 0, false, 0, ErrNoValue
	}
	return combineAggWithEmpty(vals, empty, aggRule), true, empty, nil
}

// positionTuples is every choice of one position from each list, in
// lexicographic order.
func positionTuples(lists [][]int) [][]int {
	out := [][]int{{}}
	for _, l := range lists {
		next := make([][]int, 0, len(out)*len(l))
		for _, prefix := range out {
			for _, p := range l {
				t := make([]int, len(prefix), len(prefix)+1)
				copy(t, prefix)
				next = append(next, append(t, p))
			}
		}
		out = next
	}
	return out
}

// combineAggWithEmpty is combineAgg over vals followed by `empty` zeros,
// without making them: a zero adds nothing to a sum and is not counted by
// count, but it is one more value in a mean.
func combineAggWithEmpty(vals []float64, empty int64, rule AggRule) float64 {
	switch rule {
	case AggFormula, AggRate, AggAverage:
		n := float64(len(vals)) + float64(empty)
		if n == 0 {
			return 0
		}
		var sum float64
		for _, v := range vals {
			sum += v
		}
		return sum / n
	case AggSum, AggCount, AggNone:
	}
	return combineAgg(vals, rule)
}

// factlessChildren is, when ctx lists metricID's facts with a plain 0
// elsewhere (WithRecorded), the children of combo[dimID] beneath which no
// listed fact lies within combo's other pins on the metric's dimensions.
// Every read below such a child answers an unlisted coordinate's 0, and
// whatever the structure below — a sum or mean of 0s, a period reduction
// with nothing recorded, an empty product — the child adds exactly one 0 to
// its parent: the caller counts it without walking it (the accounts ×
// categories × periods under "All accounts" that no deal reaches). Not for
// no value or recorded zeros elsewhere: there the child is a 0 or left out
// depending on what lies below it. nil when it does not apply.
func factlessChildren(
	ctx context.Context,
	dims map[string]*Dimension,
	metricID string,
	metricDimIDs []string,
	dimID string,
	combo map[string]string,
	children []Member,
) map[string]bool {
	coords, known, at, err := recordedFor(ctx, metricID)
	if err != nil || !known || at != elsewhereZero || !slices.Contains(metricDimIDs, dimID) {
		return nil
	}
	if dims[dimID] != nil {
		if idx := factIndexFor(ctx, dims, metricID, metricDimIDs, coords); idx != nil {
			return idx.factless(dims, metricDimIDs, dimID, combo, children)
		}
	}
	return factlessChildrenScan(coords, dims, metricDimIDs, dimID, combo, children)
}

// factlessChildrenScan is factlessChildren's answer worked out by visiting
// every listed fact: what a read without an index of the facts (a member
// chain too deep to index) still does, and what the index must agree with.
func factlessChildrenScan(
	coords []map[string]string,
	dims map[string]*Dimension,
	metricDimIDs []string,
	dimID string,
	combo map[string]string,
	children []Member,
) map[string]bool {
	dim, parent := dims[dimID], combo[dimID]
	covered := map[string]bool{}
	for _, f := range coords {
		if len(f) != len(metricDimIDs) {
			continue
		}
		within := true
		for _, p := range metricDimIDs {
			if code, pinned := combo[p]; pinned && p != dimID && !atOrUnder(dims[p], f[p], code) {
				within = false
				break
			}
		}
		if within {
			if child := childAbove(dim, f[dimID], parent); child != "" {
				covered[child] = true
			}
		}
	}
	skip := map[string]bool{}
	for _, c := range children {
		if !covered[c.Code] {
			skip[c.Code] = true
		}
	}
	return skip
}

// atOrUnder reports whether code is ancestor or one of its descendants in
// dim. An unknown dimension constrains nothing (true): a child is skipped
// only when no fact can be beneath it.
func atOrUnder(dim *Dimension, code, ancestor string) bool {
	if dim == nil {
		return true
	}
	for depth := 0; depth <= 64; depth++ {
		if code == ancestor {
			return true
		}
		m := FindMember(dim, code)
		if m == nil || m.ParentCode == "" {
			return false
		}
		code = m.ParentCode
	}
	return true
}

// childAbove is the child of parent that code is or lies beneath ("" when
// code is not under parent).
func childAbove(dim *Dimension, code, parent string) string {
	if dim == nil {
		return ""
	}
	for depth := 0; depth <= 64; depth++ {
		m := FindMember(dim, code)
		if m == nil || m.ParentCode == "" {
			return ""
		}
		if m.ParentCode == parent {
			return code
		}
		code = m.ParentCode
	}
	return ""
}
