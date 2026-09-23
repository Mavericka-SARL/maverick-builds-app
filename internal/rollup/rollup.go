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
	"sort"
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
)

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

const maxDepth = 10

// Resolve resolves metricID's value at combo, which may pin any set of
// dimensions the caller currently has fixed (a chart's plotted axis plus its
// context selectors, or eventually a grid's full row/col/context combo) —
// combo's dimensions need not match metricDimIDs at all.
//
// Two mechanisms compose to do this, checked in order at every level of
// recursion:
//
//  1. Same-dimension rollup: if any dimension currently pinned in combo (not
//     only metricDimIDs — a plotted axis showing a rollup member rolls up
//     every metric shown against it, related to that axis or not) is pinned
//     to a member with children in its own hierarchy, recurse into each
//     child and combine via aggRule. Mirrors BusinessConsole.tsx's
//     resolveCell.
//  2. Cross-dimension resolution: once no pinned dimension has children left
//     to expand, relate each of metricDimIDs to combo — directly if pinned
//     (already guaranteed leaf by step 1), else via a structural parent/
//     child dimension chain or a source_property grouping against whichever
//     pinned dimension relates to it, else by aggregating over its entire
//     leaf set (an axis this combo doesn't slice by at all). Mirrors
//     resolveCrossDimensionValue/resolveAxisCodes.
//
// When every one of metricDimIDs is already pinned directly in combo (the
// common case — a metric plotted straight against its own dimensions, no
// rollup or cross-dimension relation needed), Resolve fetches once and
// returns fetch's own (value, ok, err) unchanged, so "no value recorded"
// stays ok=false rather than being defaulted to a misleading 0. Every other
// path (rollup or cross-dimension resolution actually ran) returns ok=true:
// an aggregate over a possibly-empty or partially-missing set is itself a
// well-defined value, never "missing" — a genuinely absent dependency just
// contributes 0 to the aggregate, the same way a spreadsheet SUM does.
func Resolve(
	ctx context.Context,
	dims map[string]*Dimension,
	metricID string,
	metricDimIDs []string,
	aggRule AggRule,
	combo map[string]string,
	fetch RawValue,
) (float64, bool, error) {
	return resolve(ctx, dims, metricID, metricDimIDs, aggRule, "", combo, fetch, 0)
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
		// is Q2's balance, never Q1 + Q2. Children are taken in
		// chronological order so first/last mean what they say.
		timeParent := dim.IsTime && timeSummary != ""
		if timeParent {
			if timeSummary == "none" {
				return 0, false, nil
			}
			children = chronological(dim, children)
		}
		vals := make([]float64, 0, len(children))
		for _, child := range children {
			childCombo := cloneCombo(combo)
			childCombo[dimID] = child.Code
			v, ok, err := resolve(ctx, dims, metricID, metricDimIDs, aggRule, timeSummary, childCombo, fetch, depth+1)
			if err != nil {
				return 0, false, err
			}
			if timeParent && !ok {
				continue // an empty aggregate below: nothing to reduce
			}
			vals = append(vals, v)
		}
		if timeParent {
			if len(vals) == 0 {
				return 0, false, nil
			}
			return CombineTime(vals, timeSummary), true, nil
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
		if err != nil {
			return 0, false, err
		}
		vals = append(vals, v)
	}
	return combineAgg(vals, aggRule), true, nil
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

// chronological orders a time dimension's members by the first leaf period
// beneath each (an aggregate sorts where its earliest descendant does).
func chronological(dim *Dimension, members []Member) []Member {
	first := map[string]int{}
	var firstLeaf func(code string) int
	firstLeaf = func(code string) int {
		if v, ok := first[code]; ok {
			return v
		}
		best := 1 << 30
		if m := findMember(dim, code); m != nil && m.IsLeafPeriod() {
			best = m.TimeIndex
		}
		for _, c := range childrenOf(dim, code) {
			if v := firstLeaf(c.Code); v < best {
				best = v
			}
		}
		first[code] = best
		return best
	}
	out := append([]Member(nil), members...)
	sort.SliceStable(out, func(i, j int) bool { return firstLeaf(out[i].Code) < firstLeaf(out[j].Code) })
	return out
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

		// Structural child chain: ownDim is a descendant of pinnedDim
		// (e.g. employees rolling up under a plotted cost_centers member).
		// A found chain commits to "this relation exists" unconditionally —
		// descendantsInChain legitimately returning zero codes (e.g. every
		// child is currently hidden) must still mean "zero", not fall
		// through to relate's own nil-means-"no relation" sentinel below,
		// which would wrongly trigger the full-leaf-aggregate fallback and
		// silently leak every OTHER branch's values into this one.
		if chain := dimensionChainTo(dims, ownDimID, pinnedDimID, 0); len(chain) > 1 {
			codes := descendantsInChain(dims, chain, *pinnedMember)
			if codes == nil {
				codes = []string{}
			}
			return codes
		}
		// Structural parent chain: pinnedDim is a descendant of ownDim
		// (e.g. a metric on cost_centers, broadcast down to a plotted
		// employees member's ancestor cost center).
		if chain := dimensionChainTo(dims, pinnedDimID, ownDimID, 0); len(chain) > 1 {
			code := ancestorCodeInChain(dims, chain, *pinnedMember)
			if code == "" {
				return []string{}
			}
			return []string{code}
		}
		// Property-derived grouping: pinnedDim's members group ownDim's
		// members by a property value (e.g. regions <- employees.region).
		// Starts non-nil for the same reason as the child-chain branch
		// above — a real match against zero currently-visible members must
		// stay "zero", not be reinterpreted as "no relation found".
		if pinnedDim.SourceDimensionID == ownDimID && pinnedDim.SourceProperty != "" {
			codes := []string{}
			for _, m := range ownDim.Members {
				if m.Properties[pinnedDim.SourceProperty] == pinnedCode {
					codes = append(codes, m.Code)
				}
			}
			return codes
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
// its time dimension is left unpinned by combo, non-time dimensions are
// reduced first (aggRule, via Resolve at each period) and the periods are
// then reduced by timeSummary — the order the calculation scheduler uses
// for its own aggregate and slice rows, so a chart or an export that
// aggregates over time reads the same number the grid shows. A time summary
// of "none" answers ok=false: a time total is meaningless for the metric.
// With no time dimension among metricDimIDs, or with it pinned, this is
// exactly Resolve.
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
	var axis *Dimension
	for _, id := range metricDimIDs {
		if d := dims[id]; d != nil && d.IsTime {
			axis = d
			break
		}
	}
	if axis == nil {
		return Resolve(ctx, dims, metricID, metricDimIDs, aggRule, combo, fetch)
	}
	if timeSummary == "" {
		timeSummary = "sum"
	}
	if _, pinned := combo[axis.ID]; pinned {
		// Pinned to a leaf period: a plain read. Pinned to an aggregate
		// period: resolve's tier-1 expansion reduces its children by the
		// time summary.
		return resolve(ctx, dims, metricID, metricDimIDs, aggRule, timeSummary, combo, fetch, 0)
	}
	if timeSummary == "none" {
		return 0, false, nil
	}
	var members []Member
	for _, m := range axis.Members {
		if m.IsLeafPeriod() {
			members = append(members, m)
		}
	}
	sort.SliceStable(members, func(i, j int) bool { return members[i].TimeIndex < members[j].TimeIndex })
	vals := make([]float64, 0, len(members))
	for _, m := range members {
		c := cloneCombo(combo)
		c[axis.ID] = m.Code
		v, ok, err := resolve(ctx, dims, metricID, metricDimIDs, aggRule, timeSummary, c, fetch, 0)
		if err != nil {
			return 0, false, err
		}
		if !ok {
			continue // no value at this period — an absent leaf, not a zero to average in
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return 0, false, nil
	}
	return CombineTime(vals, timeSummary), true, nil
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
