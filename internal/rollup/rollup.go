// Package rollup resolves a metric's value at an arbitrary dimension-member
// combination by composing two mechanisms uniformly for any caller: rolling
// up a same-dimension hierarchy (e.g. a "Q1" period member sums its Jan/Feb/
// Mar children) and relating one dimension to another structurally
// (parent_dimension_id) or by a shared property (source_dimension_id/
// source_property) when a metric's own dimensions differ from whatever the
// caller currently has pinned. This is the one general-purpose implementation
// of rollup arithmetic in the engine, mirroring the two independently
// hand-maintained copies already shipping in
// web/src/consoles/business/BusinessConsole.tsx (resolveCell and
// resolveCrossDimensionValue/resolveAxisCodes) — so a new caller (chart.go
// today; a future grid() calc-metric path) never has to duplicate this again.
package rollup

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Member is one dimension member, stripped to just what rollup arithmetic
// needs. ParentCode is "" for a root member with no same-dimension parent.
// Properties is nil when the member has none.
type Member struct {
	ID         string
	Code       string
	ParentCode string
	Properties map[string]string
	// Time-member fields (spec §3.2), meaningful only when the owning
	// Dimension.IsTime: the server-owned chronological ordinal and the
	// period's date range. TimeIndex is -1 for an AGGREGATE period (H1,
	// FY26 — a member without dates, whose value is its descendants reduced
	// by the metric's time summary); zero values on a standard member.
	TimeIndex   int
	PeriodStart time.Time
	PeriodEnd   time.Time
}

// IsLeafPeriod reports whether m is a dated leaf period of a time dimension
// (loaders set TimeIndex to -1 for an aggregate). Meaningful only when the
// owning dimension IsTime.
func (m Member) IsLeafPeriod() bool { return m.TimeIndex >= 0 }

// Dimension is one dimension's full member universe for a revision, already
// hidden-member-filtered by the caller (writeguard.ExpandHidden or
// equivalent) — a hidden member must simply not appear in Members, so it can
// never be summed into a visible rollup total or resolved as a
// cross-dimension match. ParentDimensionID/SourceDimensionID/SourceProperty
// are "" when the dimension has no such relation.
type Dimension struct {
	ID                string
	ParentDimensionID string
	SourceDimensionID string
	SourceProperty    string
	Members           []Member
	// IsTime marks an explicitly declared time dimension (dimension_type =
	// 'time'). Its Members are then in time_index order and time-series
	// formulas move along it; a standard dimension is never treated as time,
	// whatever its name or member codes look like.
	IsTime               bool
	TimeGranularity      string
	FiscalYearStartMonth int
	// Calculated are the dimension's calculated members, kept out of
	// Members (see CalculatedMember).
	Calculated []CalculatedMember
}

// AggRule is how multiple resolved values combine into one.
type AggRule string

const (
	AggSum     AggRule = "sum"
	AggAverage AggRule = "average"
	AggCount   AggRule = "count"

	// AggFormula marks a calculated metric whose total is not a combination
	// of its members at all: it is the metric's own formula evaluated once
	// against aggregated inputs. A variance percentage is the standard case —
	// summing each region's percentage is meaningless, and averaging them
	// weights a tiny region equally with a huge one; the answer wanted is
	// total_variance / total_target.
	//
	// The scheduler owns that evaluation, because only it can run a formula.
	// combineAgg below is reached for this rule only from Resolve, at an
	// intermediate rollup level, where no evaluator is available — see the
	// note there for what it does instead and why.
	AggFormula AggRule = "formula"

	// AggRate is Anaplan's Ratio summary: the total is one nominated metric's
	// total divided by another's, so it applies to input metrics as readily as
	// calculated ones. Like AggFormula, the operands live outside this package
	// and the scheduler owns the arithmetic; combineAgg below is only reached
	// at intermediate rollup levels.
	AggRate AggRule = "rate"

	// AggNone gives no total: the metric has a value at its own leaf
	// intersections only, and nothing above them along its non-time
	// dimensions (time follows its time summary). An index or a correction
	// percentage entered per region has no meaningful company total, and a
	// pick-list's members are never added up.
	AggNone AggRule = "none"
)

// Aggregates reports whether rule gives totals at all (every rule but
// AggNone).
func Aggregates(rule AggRule) bool { return rule != AggNone }

// leafCombo reports whether combo names one leaf intersection of the
// metric's own non-time dimensions: each pinned to a member with nothing
// under it, and no related dimension (a parent dimension, a grouping)
// pinned in its place. Time is left to the time summary.
func leafCombo(dims map[string]*Dimension, metricDimIDs []string, combo map[string]string) bool {
	own := make(map[string]bool, len(metricDimIDs))
	for _, id := range metricDimIDs {
		d := dims[id]
		if d != nil && d.IsTime {
			continue
		}
		own[id] = true
		code, pinned := combo[id]
		if !pinned {
			return false
		}
		if d != nil && len(childrenOf(d, code)) > 0 {
			return false
		}
	}
	for id := range combo {
		if own[id] {
			continue
		}
		if d := dims[id]; d == nil || d.IsTime {
			continue
		}
		for o := range own {
			if Relates(dims, o, id) {
				return false
			}
		}
	}
	return true
}

// RawValue fetches metricID's recorded value at an exact combo (dimension ID
// -> member code, covering exactly metricID's own dimensions, no more, no
// fewer). ok is false only when no value is recorded for that exact combo.
// Implementations must return value == 0 whenever ok is false — Resolve
// relies on that invariant to sum a rollup's children without checking ok on
// every one of them.
type RawValue func(ctx context.Context, metricID string, combo map[string]string) (value float64, ok bool, err error)

// ErrDepthExceeded is returned when a same-dimension member hierarchy
// recurses past a sane depth (10 levels) while rolling up — almost certainly
// a cyclic or malformed parent_member_id chain, not a legitimately deep
// hierarchy, so it is surfaced rather than silently resolved as 0.
var ErrDepthExceeded = errors.New("rollup: hierarchy depth exceeded")

// ErrNoValue, returned by a RawValue, marks a coordinate with no value that
// must stay out of an aggregate entirely — not enter it as 0 the way an
// ok=false read does. A caller that computes leaves on demand (a formula
// whose result is blank there, or fails there) returns it so a parent's
// average is the mean of the leaves that have a value, as the scheduler
// totals only the rows it wrote. Resolve and ResolveTime never return it:
// an aggregate over nothing but such coordinates answers ok=false.
var ErrNoValue = errors.New("rollup: no value at this coordinate")

// ErrConflictingOverrides is returned (wrapped, naming the dimensions) by
// NormalizeCombo when two overrides select members of dimensions related
// to the same own dimension of the source — a read no single combo can
// express. The calculation package reports it as the formula error
// CONFLICTING_DIMENSION_ARGUMENTS.
var ErrConflictingOverrides = errors.New("rollup: overrides on dimensions related to the same source dimension")

const maxDepth = 10

// Resolve resolves metricID's value at combo, which may pin any set of
// dimensions the caller currently has fixed (a chart's plotted axis plus its
// context selectors, or eventually a grid's full row/col/context combo) —
// combo's dimensions need not match metricDimIDs at all.
//
// Two mechanisms compose to find the LEAF coordinates under combo, checked
// in order at every level of recursion:
//
//  1. Same-dimension rollup: if any dimension currently pinned in combo (not
//     only metricDimIDs — a plotted axis showing a rollup member rolls up
//     every metric shown against it, related to that axis or not) is pinned
//     to a member with children in its own hierarchy, recurse into each
//     child. Mirrors BusinessConsole.tsx's resolveCell.
//  2. Cross-dimension resolution: once no pinned dimension has children left
//     to expand, relate each of metricDimIDs to combo — directly if pinned
//     (already guaranteed leaf by step 1), else via a structural parent/
//     child dimension chain or a source_property grouping against whichever
//     pinned dimension relates to it, else by aggregating over its entire
//     leaf set (an axis this combo doesn't slice by at all). Mirrors
//     resolveCrossDimensionValue/resolveAxisCodes.
//
// How the leaves combine depends on aggRule:
//
//   - average and count combine FLAT, once over the distinct leaf
//     coordinates under combo that have a recorded value (fetch ok, not
//     ErrNoValue) — never level by level. World averaged is the mean of
//     every country with a value, not mean(EMEA mean, AMER mean); World
//     counted is the number of countries with a (non-zero) value, not the
//     number of regions. That is the calculation scheduler's rule for its
//     totals and slice rows (CombineAgg over the leaf rows), so every reader
//     answers a parent with the same number. A leaf with no recorded value
//     is left out, never a 0 entering the mean.
//   - sum (and any unrecognised rule) sums level by level, a leaf with no
//     value contributing 0; flat and level-by-level sums agree.
//   - formula and rate combine each level's children by their mean — an
//     approximation for callers without an evaluator; the scheduler
//     re-evaluates these at the aggregate and persists that row (see
//     AggFormula).
//
// When every one of metricDimIDs is already pinned directly in combo (the
// common case — a metric plotted straight against its own dimensions, no
// rollup or cross-dimension relation needed), Resolve fetches once and
// returns fetch's own (value, ok, err) unchanged, so "no value recorded"
// stays ok=false rather than being defaulted to a misleading 0. Every other
// path (rollup or cross-dimension resolution actually ran) returns ok=true:
// an aggregate over a possibly-empty or partially-missing set is itself a
// well-defined value, never "missing" — the sum or count of nothing is 0,
// the same way a spreadsheet SUM does — unless every leaf read answered
// ErrNoValue, which answers ok=false.
func Resolve(
	ctx context.Context,
	dims map[string]*Dimension,
	metricID string,
	metricDimIDs []string,
	aggRule AggRule,
	combo map[string]string,
	fetch RawValue,
) (float64, bool, error) {
	if aggRule == AggNone && !leafCombo(dims, metricDimIDs, combo) {
		return 0, false, nil
	}
	if flatRule(aggRule, false) {
		v, ok, _, err := resolveFlat(ctx, dims, metricID, metricDimIDs, aggRule, combo, fetch, false)
		return v, ok, err
	}
	return noValue(resolve(ctx, dims, metricID, metricDimIDs, aggRule, "", combo, fetch, 0))
}

// flatRule reports whether aggRule combines its leaves flat (resolveFlat)
// rather than level by level (resolve): average and count always; sum too
// when flatSum (ResolveTimeFlat); formula and rate never. none reaches here
// only at a leaf intersection (Resolve answers nothing above), where it
// reads as sum does.
func flatRule(aggRule AggRule, flatSum bool) bool {
	switch aggRule {
	case AggAverage, AggCount:
		return true
	case AggFormula, AggRate:
		return false
	case AggSum, AggNone:
		return flatSum
	}
	return flatSum
}

// trivialCombo reports whether resolve would answer combo with ONE direct
// fetch: no pinned dimension has children to expand and every one of
// metricDimIDs is pinned.
func trivialCombo(dims map[string]*Dimension, metricDimIDs []string, combo map[string]string) bool {
	for dimID, code := range combo {
		if d, ok := dims[dimID]; ok && len(childrenOf(d, code)) > 0 {
			return false
		}
	}
	for _, id := range metricDimIDs {
		if _, ok := combo[id]; !ok {
			return false
		}
	}
	return true
}

// resolveFlat combines the distinct leaf coordinates resolve reads under
// combo ONCE by aggRule, those with a recorded value only. The traversal is
// resolve's own — the leaves it would fetch are collected, not combined
// level by level — so a flat and a level-by-level reader never disagree on
// WHICH leaves are under a coordinate. A leaf reached twice (through an
// unrelated pinned parent's children) counts once.
//
// ok follows Resolve's contract: a trivial combo answers fetch's own ok; any
// other is ok=true unless every leaf read answered ErrNoValue. recorded
// reports whether at least one leaf had a recorded value — a period with
// none is skipped by a time reduction. A fetch error other than ErrNoValue
// (a withheld read, a failing dependency) fails the read.
//
// inReduction marks one period of a time reduction: a trivial combo's leaf
// is then combined like any other set of leaves (a count of one recorded
// leaf is 1, not its value), as the scheduler reduces each period's leaf
// rows by CombineAgg before reducing the periods.
func resolveFlat(
	ctx context.Context,
	dims map[string]*Dimension,
	metricID string,
	metricDimIDs []string,
	aggRule AggRule,
	combo map[string]string,
	fetch RawValue,
	inReduction bool,
) (value float64, ok, recorded bool, err error) {
	if len(metricDimIDs) == 0 || trivialCombo(dims, metricDimIDs, combo) {
		exact := make(map[string]string, len(metricDimIDs))
		for _, id := range metricDimIDs {
			exact[id] = combo[id]
		}
		v, ok, err := noValue(fetch(ctx, metricID, exact))
		if ok && inReduction {
			v = combineAgg([]float64{v}, aggRule)
		}
		return v, ok, ok, err
	}
	seen := map[string]error{}
	var vals []float64
	collect := func(ctx context.Context, id string, leaf map[string]string) (float64, bool, error) {
		key := comboKey(leaf)
		if prev, dup := seen[key]; dup {
			return 0, false, prev // counted once; ErrNoValue stays ErrNoValue
		}
		v, ok, err := fetch(ctx, id, leaf)
		if errors.Is(err, ErrNoValue) {
			seen[key] = ErrNoValue
			return 0, false, ErrNoValue
		}
		if err != nil {
			return 0, false, err
		}
		seen[key] = nil
		if ok {
			vals = append(vals, v)
		}
		return 0, false, nil
	}
	_, _, err = resolve(ctx, dims, metricID, metricDimIDs, AggSum, "", combo, collect, 0)
	if errors.Is(err, ErrNoValue) {
		return 0, false, false, nil // every leaf read has no value
	}
	if err != nil {
		return 0, false, false, err
	}
	return combineAgg(vals, aggRule), true, len(vals) > 0, nil
}

// noValue turns an ErrNoValue from the aggregation into "no value".
func noValue(v float64, ok bool, err error) (float64, bool, error) {
	if errors.Is(err, ErrNoValue) {
		return 0, false, nil
	}
	return v, ok, err
}

// LeafCombos returns the full Cartesian product of every leaf member across
// dimIDs — e.g. every (department, period) pair a metric dimensioned by
// both could legitimately be recorded against. A caller enumerating every
// intersection to persist (rather than resolving one already-pinned combo,
// Resolve's job) uses this to get the combo set in the first place. nil if
// any one of dimIDs has zero currently-visible leaf members (mirrors
// cartesianProduct's existing empty-set-collapses-the-whole-product
// behavior); a single {} combo if dimIDs itself is empty.
func LeafCombos(dims map[string]*Dimension, dimIDs []string) []map[string]string {
	codeSets := make([][]string, len(dimIDs))
	for i, id := range dimIDs {
		codeSets[i] = allLeafCodes(dims[id])
	}
	return cartesianProduct(dimIDs, codeSets)
}

// RollupCombos returns every combo over dimIDs in which at least one member
// is a NON-leaf parent — the complement of LeafCombos within the full member
// lattice. Rules the client cannot combine (agg_rule "formula": the
// expression must be re-evaluated against aggregated inputs; "rate": a
// ratio of sums is not a sum of ratios) need a server-computed answer for
// every rollup cell a grid can render, and this enumerates exactly those
// combos. maxCombos guards pathological lattices: when the FULL lattice
// (leaves included) would exceed it, nil is returned and callers serve
// leaves plus the grand total only — a missing rollup renders as "—",
// which is the display contract's safe state, never a wrong number.
func RollupCombos(dims map[string]*Dimension, dimIDs []string, maxCombos int) []map[string]string {
	if len(dimIDs) == 0 {
		return nil
	}
	allCodes := make([][]string, len(dimIDs))
	leafSets := make([]map[string]bool, len(dimIDs))
	total := 1
	for i, id := range dimIDs {
		d := dims[id]
		if d == nil || len(d.Members) == 0 {
			return nil
		}
		codes := make([]string, 0, len(d.Members))
		for _, m := range d.Members {
			codes = append(codes, m.Code)
		}
		allCodes[i] = codes
		ls := make(map[string]bool)
		for _, c := range allLeafCodes(d) {
			ls[c] = true
		}
		leafSets[i] = ls
		total *= len(codes)
		if maxCombos > 0 && total > maxCombos {
			return nil
		}
	}
	combos := cartesianProduct(dimIDs, allCodes)
	out := make([]map[string]string, 0, len(combos))
	for _, c := range combos {
		allLeaf := true
		for i, id := range dimIDs {
			if !leafSets[i][c[id]] {
				allLeaf = false
				break
			}
		}
		if !allLeaf {
			out = append(out, c)
		}
	}
	return out
}

// CombineAgg combines vals per rule (sum default, average, or count of
// non-zero values) — exported so callers enumerating and evaluating every
// leaf combo themselves (rather than going through Resolve, which already
// uses this internally) can fold the results the same way, instead of
// re-deriving sum/average/count a second time.
func CombineAgg(vals []float64, rule AggRule) float64 {
	return combineAgg(vals, rule)
}

func resolve(
	ctx context.Context,
	dims map[string]*Dimension,
	metricID string,
	metricDimIDs []string,
	aggRule AggRule,
	timeSummary TimeSummaryRule, // "" = combine a time dimension's children like any other (legacy Resolve)
	combo map[string]string,
	fetch RawValue,
	depth int,
) (float64, bool, error) {
	if depth > maxDepth {
		return 0, false, ErrDepthExceeded
	}
	if len(metricDimIDs) == 0 {
		return fetch(ctx, metricID, map[string]string{})
	}

	// Tier 1 — same-dimension rollup: expand the first pinned dimension
	// found with children under its pinned code (stable order); any further
	// non-leaf dimension gets expanded by the recursive call re-scanning
	// combo from scratch, exactly like the TS reference's own loop.
	for _, dimID := range sortedKeys(combo) {
		dim, ok := dims[dimID]
		if !ok {
			continue
		}
		children := childrenOf(dim, combo[dimID])
		if len(children) == 0 {
			continue
		}
		// An aggregate PERIOD (H1, FY26) reduces its children by the
		// metric's time summary, not by agg_rule: a closing balance at H1
		// is Q2's balance, never Q1 + Q2. Its leaf periods are taken in
		// chronological order so first/last mean what they say.
		timeParent := dim.IsTime && timeSummary != ""
		if timeParent {
			if timeSummary == "none" {
				return 0, false, nil
			}
			// The period's LEAF periods reduce once, flat — never level by
			// level: FY26 averaged is its months' sum over its twelve
			// months, not a mean of quarter means. For sum, min, max, first
			// and last the two agree; for average only the flat reduction
			// is the period's own value, and it is the one the scheduler
			// persists for the period's row and the *VALUE family computes.
			children = leafPeriods(dim, combo[dimID])
		}
		vals := make([]float64, 0, len(children))
		for _, child := range children {
			childCombo := cloneCombo(combo)
			childCombo[dimID] = child.Code
			if timeParent {
				// A period with no recorded value under the other pins
				// (EMEA in a month nobody in EMEA recorded) is skipped —
				// never answering as the last balance; an average still
				// counts it, as 0 (CombineTimeOver).
				v, ok, err := noValue(resolveRecorded(ctx, dims, metricID, metricDimIDs, aggRule, timeSummary, childCombo, fetch, depth+1))
				if err != nil {
					return 0, false, err
				}
				if ok {
					vals = append(vals, v)
				}
				continue
			}
			v, _, err := resolve(ctx, dims, metricID, metricDimIDs, aggRule, timeSummary, childCombo, fetch, depth+1)
			if errors.Is(err, ErrNoValue) {
				continue // no value anywhere below: left out, not a 0
			}
			if err != nil {
				return 0, false, err
			}
			vals = append(vals, v)
		}
		if !timeParent && len(vals) == 0 {
			return 0, false, ErrNoValue
		}
		if timeParent {
			if len(vals) == 0 {
				return 0, false, nil
			}
			return CombineTimeOver(vals, len(children), timeSummary), true, nil
		}
		return combineAgg(vals, aggRule), true, nil
	}

	// Tier 2 — no pinned dimension has children left to expand: relate each
	// of the metric's own dimensions to combo.
	trivial := true
	codeSets := make([][]string, len(metricDimIDs))
	for i, ownDimID := range metricDimIDs {
		if code, pinned := combo[ownDimID]; pinned {
			codeSets[i] = []string{code}
			continue
		}
		trivial = false
		codes := relate(dims, ownDimID, combo)
		if codes == nil {
			codes = allLeafCodes(dims[ownDimID])
		}
		codeSets[i] = codes
	}

	if trivial {
		exact := make(map[string]string, len(metricDimIDs))
		for _, id := range metricDimIDs {
			exact[id] = combo[id]
		}
		return fetch(ctx, metricID, exact)
	}

	combos := cartesianProduct(metricDimIDs, codeSets)
	if len(combos) == 0 {
		return 0, true, nil
	}
	vals := make([]float64, 0, len(combos))
	for _, c := range combos {
		v, _, err := fetch(ctx, metricID, c)
		if errors.Is(err, ErrNoValue) {
			continue // no value: left out, not a 0
		}
		if err != nil {
			return 0, false, err
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return 0, false, ErrNoValue
	}
	return combineAgg(vals, aggRule), true, nil
}

// resolveRecorded is resolve for one period of a time reduction: ok=false
// when no leaf read beneath combo had a recorded value, so the caller skips
// the period instead of reducing a 0 that an aggregate over nothing
// produced. With something recorded it is resolve's own answer.
func resolveRecorded(
	ctx context.Context,
	dims map[string]*Dimension,
	metricID string,
	metricDimIDs []string,
	aggRule AggRule,
	timeSummary TimeSummaryRule,
	combo map[string]string,
	fetch RawValue,
	depth int,
) (float64, bool, error) {
	recorded := false
	tracked := func(ctx context.Context, id string, c map[string]string) (float64, bool, error) {
		v, ok, err := fetch(ctx, id, c)
		if ok {
			recorded = true
		}
		return v, ok, err
	}
	v, ok, err := resolve(ctx, dims, metricID, metricDimIDs, aggRule, timeSummary, combo, tracked, depth)
	if err != nil || !ok || !recorded {
		return 0, false, err
	}
	return v, true, nil
}

// ResolveTimeRecorded is ResolveTime answering ok=false when no leaf read
// beneath combo returned a recorded value — a parent member or aggregate
// whose every leaf is empty, where ResolveTime still answers ok=true with
// the aggregate of nothing. A reduction over periods uses it to skip the
// periods nothing was recorded in.
func ResolveTimeRecorded(
	ctx context.Context,
	dims map[string]*Dimension,
	metricID string,
	metricDimIDs []string,
	aggRule AggRule,
	timeSummary TimeSummaryRule,
	combo map[string]string,
	fetch RawValue,
) (float64, bool, error) {
	recorded := false
	tracked := func(ctx context.Context, id string, c map[string]string) (float64, bool, error) {
		v, ok, err := fetch(ctx, id, c)
		if ok {
			recorded = true
		}
		return v, ok, err
	}
	v, ok, err := ResolveTime(ctx, dims, metricID, metricDimIDs, aggRule, timeSummary, combo, tracked)
	if err != nil || !ok || !recorded {
		return 0, false, err
	}
	return v, true, nil
}

// childrenOf returns dim's members whose ParentCode is parentCode.
func childrenOf(dim *Dimension, parentCode string) []Member {
	var out []Member
	for _, m := range dim.Members {
		if m.ParentCode != "" && m.ParentCode == parentCode {
			out = append(out, m)
		}
	}
	return out
}

// allLeafCodes returns every leaf (childless) member's code in dim — used
// when a metric's own dimension has no relationship at all to any of
// combo's currently-pinned dimensions, so it is aggregated over in full
// rather than treated as unresolvable. Mirrors allLeafCodes in
// BusinessConsole.tsx.
func allLeafCodes(dim *Dimension) []string {
	if dim == nil {
		return nil
	}
	hasChildren := make(map[string]bool, len(dim.Members))
	for _, m := range dim.Members {
		if m.ParentCode != "" {
			hasChildren[m.ParentCode] = true
		}
	}
	var out []string
	for _, m := range dim.Members {
		if hasChildren[m.Code] {
			continue
		}
		// On a time dimension only a DATED period is a leaf; an aggregate
		// that has no children yet is an empty grouping, not a period.
		if dim.IsTime && !m.IsLeafPeriod() {
			continue
		}
		out = append(out, m.Code)
	}
	return out
}

// leafPeriods returns the dated leaf periods at or under code in a time
// dimension, in chronological order.
func leafPeriods(dim *Dimension, code string) []Member {
	codes := LeafDescendants(dim, code)
	in := make(map[string]bool, len(codes))
	for _, c := range codes {
		in[c] = true
	}
	out := make([]Member, 0, len(codes))
	for _, m := range dim.Members {
		if in[m.Code] {
			in[m.Code] = false // a duplicated code counts once
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TimeIndex < out[j].TimeIndex })
	return out
}

// relationKind is how one dimension relates to a metric's own dimension.
type relationKind int

const (
	relationNone relationKind = iota
	// relationChildChain: the own dimension descends structurally from the
	// other (employees under a cost_centers member).
	relationChildChain
	// relationParentChain: the other dimension descends structurally from
	// the own one (a metric on cost_centers broadcast to an employee).
	relationParentChain
	// relationProperty: the other dimension groups the own dimension's
	// members by a property value (regions <- employees.region).
	relationProperty
)

// relation reports how otherDimID relates to ownDimID — the ONE rule both
// Relates (save time) and relate (run time) use — with the structural
// chain when there is one. Checked in relate's historical order: child
// chain, parent chain, property grouping. A dimension never relates to
// itself here; Relates handles identity.
func relation(dims map[string]*Dimension, ownDimID, otherDimID string) (relationKind, []string) {
	if ownDimID == otherDimID {
		return relationNone, nil
	}
	if chain := dimensionChainTo(dims, ownDimID, otherDimID, 0); len(chain) > 1 {
		return relationChildChain, chain
	}
	if chain := dimensionChainTo(dims, otherDimID, ownDimID, 0); len(chain) > 1 {
		return relationParentChain, chain
	}
	if other, ok := dims[otherDimID]; ok && other.SourceDimensionID == ownDimID && other.SourceProperty != "" {
		return relationProperty, nil
	}
	return relationNone, nil
}

// Relates reports whether otherDimID is ownDimID itself or relates to it
// by the relation rollup resolves through: a structural parent/child
// dimension chain in either direction, or a source_property grouping of
// ownDimID's members. The reverse property direction and multi-hop
// compositions do not relate. A dimension missing from dims relates only
// to itself.
func Relates(dims map[string]*Dimension, ownDimID, otherDimID string) bool {
	if ownDimID == otherDimID {
		return true
	}
	if _, ok := dims[ownDimID]; !ok {
		return false
	}
	if _, ok := dims[otherDimID]; !ok {
		return false
	}
	kind, _ := relation(dims, ownDimID, otherDimID)
	return kind != relationNone
}

// relate finds the resolved code-set for ownDimID against whichever of
// combo's pinned dimensions structurally or property-relates to it — the
// first such relation found, in stable order. Returns nil when none relates
// at all, so the caller aggregates over ownDimID's full leaf set instead.
// Mirrors resolveAxisCodes's structural/property branches (the exact-match
// branch is handled by the caller before relate is ever reached).
func relate(dims map[string]*Dimension, ownDimID string, combo map[string]string) []string {
	ownDim, ok := dims[ownDimID]
	if !ok {
		return nil
	}
	for _, pinnedDimID := range sortedKeys(combo) {
		if pinnedDimID == ownDimID {
			continue
		}
		pinnedDim, ok := dims[pinnedDimID]
		if !ok {
			continue
		}
		pinnedCode := combo[pinnedDimID]
		pinnedMember := findMember(pinnedDim, pinnedCode)
		if pinnedMember == nil {
			continue
		}

		kind, chain := relation(dims, ownDimID, pinnedDimID)
		switch kind {
		case relationChildChain:
			// A found chain commits to "this relation exists"
			// unconditionally — descendantsInChain legitimately returning
			// zero codes (e.g. every child is currently hidden) must still
			// mean "zero", not fall through to relate's own
			// nil-means-"no relation" sentinel below, which would wrongly
			// trigger the full-leaf-aggregate fallback and silently leak
			// every OTHER branch's values into this one.
			codes := descendantsInChain(dims, chain, *pinnedMember)
			if codes == nil {
				codes = []string{}
			}
			return codes
		case relationParentChain:
			code := ancestorCodeInChain(dims, chain, *pinnedMember)
			if code == "" {
				return []string{}
			}
			return []string{code}
		case relationProperty:
			// Starts non-nil for the same reason as the child-chain branch
			// above — a real match against zero currently-visible members
			// must stay "zero", not be reinterpreted as "no relation found".
			codes := []string{}
			for _, m := range ownDim.Members {
				if PropertyValue(m.Properties, pinnedDim.SourceProperty) == pinnedCode {
					codes = append(codes, m.Code)
				}
			}
			return codes
		case relationNone:
		}
	}
	return nil
}

// dimensionChainTo returns the chain of dimension IDs from fromID up to (and
// including) ancestorID via ParentDimensionID — e.g. [cabinet, department,
// region] walking from cabinet to region — or nil if ancestorID isn't an
// ancestor of fromID within maxDepth levels. Mirrors dimensionChainTo in
// BusinessConsole.tsx; unlike the same-dimension member recursion in
// resolve, a too-deep result here silently means "unrelated" rather than
// ErrDepthExceeded — this walks the model's own small, fixed dimension
// graph, not a data-driven member hierarchy, so hitting the cap means the
// two dimensions simply don't relate, not a suspected data cycle.
func dimensionChainTo(dims map[string]*Dimension, fromID, ancestorID string, depth int) []string {
	if depth > maxDepth {
		return nil
	}
	if fromID == ancestorID {
		return []string{fromID}
	}
	dim, ok := dims[fromID]
	if !ok || dim.ParentDimensionID == "" {
		return nil
	}
	rest := dimensionChainTo(dims, dim.ParentDimensionID, ancestorID, depth+1)
	if rest == nil {
		return nil
	}
	return append([]string{fromID}, rest...)
}

// descendantsInChain returns every member of dims[chain[0]] descending
// (through every level of chain) from ancestorMember, which belongs to
// dims[chain[len(chain)-1]]. Mirrors descendantsInChain in
// BusinessConsole.tsx.
func descendantsInChain(dims map[string]*Dimension, chain []string, ancestorMember Member) []string {
	current := []string{ancestorMember.Code}
	for i := len(chain) - 2; i >= 0; i-- {
		dim, ok := dims[chain[i]]
		if !ok {
			return nil
		}
		currentSet := make(map[string]bool, len(current))
		for _, c := range current {
			currentSet[c] = true
		}
		var next []string
		for _, m := range dim.Members {
			if m.ParentCode != "" && currentSet[m.ParentCode] {
				next = append(next, m.Code)
			}
		}
		current = next
	}
	return current
}

// ancestorCodeInChain walks startMember (in dims[chain[0]]) up through
// ParentCode at each level to find its ancestor's code in
// dims[chain[len(chain)-1]]. Returns "" if the chain breaks anywhere.
// Mirrors ancestorCodeInChain in BusinessConsole.tsx.
func ancestorCodeInChain(dims map[string]*Dimension, chain []string, startMember Member) string {
	current := startMember
	for i := 0; i < len(chain)-1; i++ {
		if current.ParentCode == "" {
			return ""
		}
		dim, ok := dims[chain[i+1]]
		if !ok {
			return ""
		}
		next := findMember(dim, current.ParentCode)
		if next == nil {
			return ""
		}
		current = *next
	}
	return current.Code
}

func findMember(dim *Dimension, code string) *Member {
	for i := range dim.Members {
		if dim.Members[i].Code == code {
			return &dim.Members[i]
		}
	}
	return nil
}

// cartesianProduct expands codeSets (one set per dimIDs, in dimIDs order)
// into every full combo — e.g. [[a,b],[x,y]] with dimIDs [d1,d2] yields
// {d1:a,d2:x}, {d1:a,d2:y}, {d1:b,d2:x}, {d1:b,d2:y}. An empty codeSets
// entry collapses the whole product to zero combos.
func cartesianProduct(dimIDs []string, codeSets [][]string) []map[string]string {
	combos := []map[string]string{{}}
	for i, codes := range codeSets {
		if len(codes) == 0 {
			return nil
		}
		next := make([]map[string]string, 0, len(combos)*len(codes))
		for _, prefix := range combos {
			for _, code := range codes {
				c := cloneCombo(prefix)
				c[dimIDs[i]] = code
				next = append(next, c)
			}
		}
		combos = next
	}
	return combos
}

func combineAgg(vals []float64, rule AggRule) float64 {
	switch rule {
	// A formula metric's authoritative total is the scheduler's
	// formula-evaluated row. This path is only reached when Resolve is
	// rolling one up at an intermediate member and has no way to re-evaluate
	// the formula there, so it falls back to the mean. That is an
	// approximation, not the right answer — but summing ratios is wrong by
	// orders of magnitude, whereas a mean is wrong only by weighting.
	case AggFormula, AggRate, AggAverage:
		if len(vals) == 0 {
			return 0
		}
		var sum float64
		for _, v := range vals {
			sum += v
		}
		return sum / float64(len(vals))
	case AggCount:
		var n float64
		for _, v := range vals {
			if v != 0 {
				n++
			}
		}
		return n
	default: // AggSum, and any unrecognized value, both sum.
		var sum float64
		for _, v := range vals {
			sum += v
		}
		return sum
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func cloneCombo(combo map[string]string) map[string]string {
	out := make(map[string]string, len(combo))
	for k, v := range combo {
		out[k] = v
	}
	return out
}

// TimeSummaryRule is how a metric aggregates ACROSS its time dimension
// (metric_def.time_summary); AggRule stays the rule for every other
// dimension. See ResolveTime.
type TimeSummaryRule string

// ResolveTime is Resolve for a metric that may carry a time dimension: when
// its time dimension is left unpinned by combo, or pinned to an AGGREGATE
// period (H1, FY26), the non-time dimensions are reduced first, per leaf
// period (aggRule, as Resolve does — flat for average and count), and the
// leaf periods are then reduced by timeSummary, a period with nothing
// recorded under combo skipped — the order the calculation scheduler uses
// for its own aggregate and slice rows (summarizeOverTime), so a chart or an
// export that aggregates over time reads the same number the grid shows,
// whichever dimension ID happens to sort first. A time summary of "none"
// answers ok=false: a time total is meaningless for the metric. With no
// time dimension among metricDimIDs, or with it pinned to a leaf period,
// this is exactly Resolve.
//
// agg_rule formula and rate keep resolve's own expansion at an aggregate
// period (see Resolve): the scheduler re-evaluates them at the aggregate.
func ResolveTime(
	ctx context.Context,
	dims map[string]*Dimension,
	metricID string,
	metricDimIDs []string,
	aggRule AggRule,
	timeSummary TimeSummaryRule,
	combo map[string]string,
	fetch RawValue,
) (float64, bool, error) {
	return resolveTime(ctx, dims, metricID, metricDimIDs, aggRule, timeSummary, combo, fetch, false)
}

// resolveTime is ResolveTime and ResolveTimeFlat; flatSum makes sum combine
// flat too (distinct leaves, each once).
func resolveTime(
	ctx context.Context,
	dims map[string]*Dimension,
	metricID string,
	metricDimIDs []string,
	aggRule AggRule,
	timeSummary TimeSummaryRule,
	combo map[string]string,
	fetch RawValue,
	flatSum bool,
) (float64, bool, error) {
	if aggRule == AggNone && !leafCombo(dims, metricDimIDs, combo) {
		return 0, false, nil
	}
	flat := flatRule(aggRule, flatSum)
	// one resolves combo with its time dimension (if any) at a leaf period
	// or absent; recorded reports whether a leaf read had a recorded value.
	// inReduction: c is one period of a time reduction.
	one := func(c map[string]string, ts TimeSummaryRule, inReduction bool) (float64, bool, bool, error) {
		if flat {
			return resolveFlat(ctx, dims, metricID, metricDimIDs, aggRule, c, fetch, inReduction)
		}
		recorded := false
		tracked := func(ctx context.Context, id string, leaf map[string]string) (float64, bool, error) {
			v, ok, err := fetch(ctx, id, leaf)
			if ok {
				recorded = true
			}
			return v, ok, err
		}
		v, ok, err := noValue(resolve(ctx, dims, metricID, metricDimIDs, aggRule, ts, c, tracked, 0))
		return v, ok, recorded, err
	}

	var axis *Dimension
	for _, id := range metricDimIDs {
		if d := dims[id]; d != nil && d.IsTime {
			axis = d
			break
		}
	}
	if axis == nil {
		v, ok, _, err := one(combo, "", false)
		return v, ok, err
	}
	if timeSummary == "" {
		timeSummary = "sum"
	}
	var periods []Member
	if code, pinned := combo[axis.ID]; pinned {
		m := findMember(axis, code)
		if m == nil || len(childrenOf(axis, code)) == 0 {
			// A leaf period (or an unknown code): a plain read, no time
			// reduction. An undated aggregate with no children yet has no
			// period under it and reads as such.
			v, ok, _, err := one(combo, timeSummary, false)
			return v, ok, err
		}
		if aggRule == AggFormula || aggRule == AggRate {
			// resolve's tier-1 expansion reduces the aggregate period by the
			// time summary.
			return noValue(resolve(ctx, dims, metricID, metricDimIDs, aggRule, timeSummary, combo, fetch, 0))
		}
		periods = leafPeriods(axis, code)
	} else {
		for _, m := range axis.Members {
			if m.IsLeafPeriod() {
				periods = append(periods, m)
			}
		}
		sort.SliceStable(periods, func(i, j int) bool { return periods[i].TimeIndex < periods[j].TimeIndex })
	}
	if timeSummary == "none" {
		return 0, false, nil
	}
	// The period's LEAF periods reduce once, flat — never level by level:
	// FY26 averaged is its months' sum over its twelve months, not a mean of
	// quarter means.
	vals := make([]float64, 0, len(periods))
	for _, p := range periods {
		c := cloneCombo(combo)
		c[axis.ID] = p.Code
		v, ok, recorded, err := one(c, timeSummary, true)
		if err != nil {
			return 0, false, err
		}
		if ok && recorded {
			vals = append(vals, v) // a period nothing under combo recorded has no value (an average counts it as 0)
		}
	}
	if len(vals) == 0 {
		return 0, false, nil
	}
	return CombineTimeOver(vals, len(periods), timeSummary), true, nil
}

// CombineTimeOver is CombineTime over a reduction of `periods` periods, of
// which vals are those with a value. An average counts every period, one
// with no value as 0: FY averaged is the sum of its months over twelve, as a
// workbook's SUM(Jan:Dec)/12, however many months have been entered. The
// other rules read only the periods with a value.
func CombineTimeOver(vals []float64, periods int, rule TimeSummaryRule) float64 {
	if rule == "average" && periods > len(vals) {
		var s float64
		for _, v := range vals {
			s += v
		}
		return s / float64(periods)
	}
	return CombineTime(vals, rule)
}

// CombineTime reduces per-period values by a time summary rule. Mirrors
// the calculation scheduler's TimeSummary for every rule but "none" (which
// callers handle before combining).
func CombineTime(vals []float64, rule TimeSummaryRule) float64 {
	if len(vals) == 0 {
		return 0
	}
	switch rule {
	case "average":
		var s float64
		for _, v := range vals {
			s += v
		}
		return s / float64(len(vals))
	case "min":
		m := vals[0]
		for _, v := range vals[1:] {
			if v < m {
				m = v
			}
		}
		return m
	case "max":
		m := vals[0]
		for _, v := range vals[1:] {
			if v > m {
				m = v
			}
		}
		return m
	case "first":
		return vals[0]
	case "last":
		return vals[len(vals)-1]
	default:
		var s float64
		for _, v := range vals {
			s += v
		}
		return s
	}
}

// ResolveTimeFlat is ResolveTime with sum combined flat as well: each
// distinct leaf coordinate under combo counts once, so a leaf reached twice
// through a pinned parent of a dimension the metric does not carry (a
// region-only metric at {currency: ALL}) is not summed once per child the
// way Resolve's level-by-level sum does. For average and count it is exactly
// ResolveTime (both combine those flat). A caller computing leaves on demand
// (a formula reading member metadata, whose value exists only at leaves)
// uses it; ok=false when every leaf under combo answered ErrNoValue, and a
// fetch error other than ErrNoValue fails the whole read.
func ResolveTimeFlat(
	ctx context.Context,
	dims map[string]*Dimension,
	metricID string,
	metricDimIDs []string,
	aggRule AggRule,
	timeSummary TimeSummaryRule,
	combo map[string]string,
	fetch RawValue,
) (float64, bool, error) {
	return resolveTime(ctx, dims, metricID, metricDimIDs, aggRule, timeSummary, combo, fetch, true)
}

// comboKey is a canonical string for a combo.
func comboKey(combo map[string]string) string {
	var b []byte
	for _, k := range sortedKeys(combo) {
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, combo[k]...)
		b = append(b, 0)
	}
	return string(b)
}

// FindMember returns dim's member with exactly code, or nil.
func FindMember(dim *Dimension, code string) *Member {
	if dim == nil {
		return nil
	}
	return findMember(dim, code)
}

// LeafCodes returns every leaf member's code in dim, in dim.Members order.
// On a time dimension only dated leaf periods count: an aggregate with no
// children yet is an empty grouping, not a period.
func LeafCodes(dim *Dimension) []string {
	return allLeafCodes(dim)
}

// SubtreeCodes returns code and the code of every member below it in dim's
// hierarchy, in dim.Members order (code itself first); nil when code is not
// a member. A malformed cyclic hierarchy terminates.
func SubtreeCodes(dim *Dimension, code string) []string {
	if FindMember(dim, code) == nil {
		return nil
	}
	in := map[string]bool{code: true}
	for grew := true; grew; {
		grew = false
		for _, m := range dim.Members {
			if !in[m.Code] && m.ParentCode != "" && in[m.ParentCode] {
				in[m.Code] = true
				grew = true
			}
		}
	}
	out := []string{code}
	for _, m := range dim.Members {
		if m.Code != code && in[m.Code] {
			out = append(out, m.Code)
		}
	}
	return out
}

// LeafDescendants returns the leaf members at or under code in dim — code
// itself when it is a leaf — in dim.Members order. On a time dimension only
// dated leaf periods count. nil when code is not a member; empty (non-nil)
// for an aggregate with no leaf below it.
func LeafDescendants(dim *Dimension, code string) []string {
	subtree := SubtreeCodes(dim, code)
	if subtree == nil {
		return nil
	}
	in := make(map[string]bool, len(subtree))
	for _, c := range subtree {
		in[c] = true
	}
	out := []string{}
	for _, c := range allLeafCodes(dim) {
		if in[c] {
			out = append(out, c)
		}
	}
	return out
}

// NormalizeCombo prepares a cell's combo for reading a source metric at
// other members (LOOKUP and the conditional aggregations, contract C2),
// returning the combo to pass to ResolveTime with the source's dimensions:
//
//  1. keep pins on the source's own dimensions and on dimensions related
//     to one of them (Relates);
//  2. drop pins on dimensions unrelated to the source — so a source that
//     does not carry a dimension is never rolled up along it (fx_rate
//     [currency] at a region parent reads the one rate, not the rate times
//     the region's children);
//  3. for each override, drop the other pins related to the same own
//     dimension, then pin the override.
//
// Overrides map dimension IDs to member codes; an override on a dimension
// unrelated to the source is dropped like any other unrelated pin (save-time
// validation refuses it: DIMENSION_NOT_ON_SOURCE). Neither input is
// modified.
//
// Two overrides on different dimensions that relate to the SAME own
// dimension of the source (employees and region on a source keyed by
// employees, or region and segment) would ask for the intersection of two
// member sets, which one combo cannot express: ResolveTime would honour one
// pin and silently ignore the other. That shape is refused with an error
// wrapping ErrConflictingOverrides instead of returning a wrong number.
func NormalizeCombo(dims map[string]*Dimension, sourceDimIDs []string, combo, overrides map[string]string) (map[string]string, error) {
	relatedOwn := func(dimID string) []string {
		var owns []string
		for _, own := range sourceDimIDs {
			if Relates(dims, own, dimID) {
				owns = append(owns, own)
			}
		}
		return owns
	}
	claimedBy := make(map[string]string, len(overrides)) // own dimension -> first override on it
	for _, dimID := range sortedKeys(overrides) {
		claims := relatedOwn(dimID)
		for _, own := range sourceDimIDs {
			if own == dimID {
				claims = []string{dimID} // an exact own pin selects only itself
				break
			}
		}
		for _, own := range claims {
			if prev, ok := claimedBy[own]; ok {
				return nil, fmt.Errorf("%w: %s and %s both select members of %s",
					ErrConflictingOverrides, prev, dimID, own)
			}
			claimedBy[own] = dimID
		}
	}
	out := make(map[string]string, len(combo)+len(overrides))
	for dimID, code := range combo {
		if len(relatedOwn(dimID)) > 0 {
			out[dimID] = code
		}
	}
	for _, dimID := range sortedKeys(overrides) {
		for _, own := range relatedOwn(dimID) {
			for pinned := range out {
				if pinned != dimID && Relates(dims, own, pinned) {
					delete(out, pinned)
				}
			}
		}
	}
	for dimID, code := range overrides {
		if len(relatedOwn(dimID)) > 0 {
			out[dimID] = code
		}
	}
	return out, nil
}

// PropertyValue is a member's value of property name, the key matched
// case-insensitively as formulas read dim.property: a grouping by the
// declared "area" still sees a value stored under "Area".
func PropertyValue(props map[string]string, name string) string {
	if v, ok := props[name]; ok {
		return v
	}
	for k, v := range props {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}
