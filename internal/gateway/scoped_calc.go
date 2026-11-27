package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/readset"
	"github.com/mavericks-engine/mavericks/internal/rollup"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// Scoped grid reads of calculated metrics (a caller with hidden members, or
// a scope pin); package readset decides which persisted values they may show.
//
//   - A SERVED metric (time functions, LOOKUP, the *IFS/*IF family, TIMESUM,
//     the *VALUE family — readset.Served) reads members and periods outside
//     any scope, so it is never re-evaluated here: its cells are the
//     scheduler's persisted rows. A persisted cell whose read set touches a
//     member hidden from the caller is WITHHELD: absent from cells and listed
//     in the response's withheld array, never recomputed over visible data.
//   - A metric that reads ACROSS the scope (a dependency has a dimension the
//     scope restricted, the metric neither has it nor relates to it) is
//     served too. The scope restricts the cells shown, never what a cell is:
//     total orders on [month] filtered to one channel is still total orders,
//     and every metric reading it stays what it is. As the chart reads it
//     (ownPoint). A cell of it that reads a member hidden from the caller is
//     withheld like any served cell: the slice would pass for the total
//     (owner decision, 2026-11-07).
//   - Every other calculated metric is recomputed per combo from the
//     already-scoped input cells, exactly as the scheduler computes it
//     (rollup.ResolveTime with time_summary, the member-metadata context for
//     dim.property and PARENT), so a coarser metric never bakes in a hidden
//     finer-grained input. A combo that reads a withheld cell is withheld in
//     turn.
//   - A total or aggregate period whose lattice holds a withheld cell is
//     withheld: there are no partial totals.

// errWithheld is returned by a read that reached a withheld cell; it
// propagates through rollup's resolution to everything built on the read.
var errWithheld = errors.New("the value reads a member hidden from the caller")

// servedMetric is one served metric's persisted rows: every calc_result row
// of the metric (leaves, rollup combos, slices, aggregate periods and the
// '{}' total), keyed like calculation's dimKey (json of the combo).
type servedMetric struct {
	Rows map[string]float64
	// plain: served only because the caller has no hidden member
	// (loadServedReads); its cells are the leaves a recompute gives, no
	// aggregate periods worked out here.
	plain bool
}

// scopedReads is what a scoped read needs beyond the scoped input cells.
// The zero value (or nil) serves nothing, withholds nothing and evaluates
// without member metadata.
type scopedReads struct {
	// Served maps each served metric ID to its persisted rows.
	Served map[string]*servedMetric
	// Reads decides withheld cells; nil or unrestricted withholds nothing.
	Reads *readset.Set
	// Meta is the UNFILTERED member metadata ordinary metrics evaluate
	// dim.property and PARENT against.
	Meta *calculation.DimMetadata
	// Pinned are the scope's pins (dimension ID -> member code); Trimmed
	// every dimension the scope restricted (pinned, or reached down a
	// parent-dimension chain from a pin). Dims (unfiltered) relates them to
	// a metric's own dimensions.
	Pinned  map[string]string
	Trimmed map[string]bool
	Dims    map[string]*rollup.Dimension
	// Only is the metrics the read answers for and every metric they read
	// (readsClosure): the others are neither loaded nor worked out. nil
	// works out every metric.
	Only map[string]bool
}

// wanted reports whether the read works metricID out (Only).
func (sr *scopedReads) wanted(metricID string) bool {
	return sr == nil || sr.Only == nil || sr.Only[metricID]
}

// totalCombo is where a formula/rate total is evaluated: at the scope's
// pins, as the scheduler's slice rows are. At {} a dependency whose rule is
// none read nothing even where the pin left it one leaf: drv_global on
// [cost_type], read by a formula total in a cost_type=BASE_SALARY scope,
// gave 0 while the slice row gave 2.5.
func (sr *scopedReads) totalCombo() map[string]string {
	out := map[string]string{}
	if sr != nil {
		for dimID, code := range sr.Pinned {
			out[dimID] = code
		}
	}
	return out
}

func (sr *scopedReads) served(metricID string) *servedMetric {
	if sr == nil {
		return nil
	}
	return sr.Served[metricID]
}

func (sr *scopedReads) withheld(metricID string, combo map[string]string) bool {
	return sr != nil && sr.Reads.Withheld(metricID, combo)
}

func (sr *scopedReads) cellContext(combo map[string]string) *formula.DimEvalContext {
	if sr == nil || sr.Meta == nil {
		return nil
	}
	return sr.Meta.CellContext(combo)
}

// metadata is the member metadata of the scoped reads, nil without one.
func (sr *scopedReads) metadata() *calculation.DimMetadata {
	if sr == nil {
		return nil
	}
	return sr.Meta
}

func comboKey(combo map[string]string) string {
	b, _ := json.Marshal(combo)
	return string(b)
}

// cellKeyOf is the grid's cell key of metricID at combo: metricID:code1:…
// in ownDims order, or the bare metric ID (the key totals use) for a
// dimensionless metric.
func cellKeyOf(metricID string, ownDims []string, combo map[string]string) string {
	if len(ownDims) == 0 {
		return metricID
	}
	codes := make([]string, 0, len(ownDims))
	for _, d := range ownDims {
		codes = append(codes, combo[d])
	}
	return metricID + ":" + strings.Join(codes, ":")
}

// timeAxisOf returns the metric's time dimension in dims, or nil.
func timeAxisOf(dims map[string]*rollup.Dimension, ownDims []string) *rollup.Dimension {
	for _, d := range ownDims {
		if dim := dims[d]; dim != nil && dim.IsTime {
			return dim
		}
	}
	return nil
}

// timeAxisID is axis's dimension ID, "" without one.
func timeAxisID(axis *rollup.Dimension) string {
	if axis == nil {
		return ""
	}
	return axis.ID
}

// leafPeriods returns axis's dated leaf periods in chronological order.
func leafPeriods(axis *rollup.Dimension) []string {
	codes := rollup.LeafCodes(axis)
	idx := make(map[string]int, len(axis.Members))
	for _, m := range axis.Members {
		idx[m.Code] = m.TimeIndex
	}
	sort.SliceStable(codes, func(i, j int) bool { return idx[codes[i]] < idx[codes[j]] })
	return codes
}

// leafValue is one leaf combo's value.
type leafValue struct {
	combo map[string]string
	value float64
}

// combineLeaves reduces leaf values the way the scheduler totals a
// combining rule: with a time axis, non-time dimensions by aggRule per
// period and the periods by timeSummary (an average counts every leaf
// period of axis, one with no value as 0, so axis must already be trimmed
// to the scope); without one, by aggRule. ok is false when there is nothing
// to reduce or the summary is 'none'. oneLeaf says the scope narrows every
// non-time dimension to one leaf (singleLeafScope): agg_rule none has a
// total only there, its periods reduced by the time summary.
func combineLeaves(leaves []leafValue, axis *rollup.Dimension, aggRule, timeSummary string, oneLeaf bool) (float64, bool) {
	if len(leaves) == 0 {
		return 0, false
	}
	if !rollup.Aggregates(rollup.AggRule(aggRule)) && !oneLeaf {
		return 0, false // agg_rule none: no total across members
	}
	if axis == nil {
		vals := make([]float64, len(leaves))
		for i, l := range leaves {
			vals[i] = l.value
		}
		return rollup.CombineAgg(vals, rollup.AggRule(aggRule)), true
	}
	perPeriod := map[string][]float64{}
	for _, l := range leaves {
		p := l.combo[axis.ID]
		perPeriod[p] = append(perPeriod[p], l.value)
	}
	var ordered []float64
	periods := leafPeriods(axis)
	for _, p := range periods {
		if vals, ok := perPeriod[p]; ok {
			ordered = append(ordered, rollup.CombineAgg(vals, rollup.AggRule(aggRule)))
		}
	}
	if len(ordered) == 0 {
		return 0, false
	}
	return calculation.TimeSummaryOver(timeSummary, ordered, len(periods))
}

// singleLeafScope reports whether the scope (rollupDims, scope-trimmed to
// the pinned subtrees) leaves one member, a leaf, of each of the metric's
// non-time dimensions — the combo rollup.LeafCombo asks for.
func singleLeafScope(rollupDims map[string]*rollup.Dimension, ownDims []string) bool {
	for _, d := range ownDims {
		dim := rollupDims[d]
		if dim == nil {
			return false
		}
		if !dim.IsTime && len(dim.Members) != 1 {
			return false
		}
	}
	return true
}

// ruledLeaves keeps the leaves a calculated metric's total reads under its
// total rules (migration 174): over a dimension that totals as its first or
// last member, only that member's leaves — the reading rollup.RuleCombo gives
// the unscoped rollup and the scoped input totals (inputTotalsByRule). ok is
// false when a rule "none" leaves no total. oneLeaf is singleLeafScope with
// each ruled dimension counted as the one member it reads. Before
// 2026-11-21 a scoped read added the members up: a location pin over a model
// with a scenario dimension totalling as Base showed every calculated KPI
// as the sum of the scenarios.
func ruledLeaves(rollupDims map[string]*rollup.Dimension, metricID string, ownDims []string, pins map[string]string, leaves []leafValue) (kept []leafValue, oneLeaf, ok bool) {
	oneLeaf = singleLeafScope(rollupDims, ownDims)
	if !rollup.HasMemberRules(rollupDims, metricID, ownDims) {
		return leaves, oneLeaf, true
	}
	rc, ok := rollup.RuleCombo(rollupDims, metricID, ownDims, pins)
	if !ok {
		return nil, false, false
	}
	ruled := map[string]string{}
	for _, d := range ownDims {
		r := rollupDims[d].RuleFor(metricID)
		if code, pinned := rc[d]; pinned && r != "" && r != rollup.TotalSum {
			ruled[d] = code
		}
	}
	for _, l := range leaves {
		if atRuled(l.combo, ruled) {
			kept = append(kept, l)
		}
	}
	oneLeaf = true
	for _, d := range ownDims {
		dim := rollupDims[d]
		if dim == nil {
			return kept, false, true
		}
		if _, one := ruled[d]; !one && !dim.IsTime && len(dim.Members) != 1 {
			oneLeaf = false
		}
	}
	return kept, oneLeaf, true
}

// isFormulaRule: the total and rollups are the formula evaluated at the
// aggregate, never a combination of leaves.
func isFormulaRule(aggRule string) bool {
	return aggRule == string(rollup.AggFormula) || aggRule == string(rollup.AggRate)
}

// servedResult is one served metric's contribution to the response.
type servedResult struct {
	cells    map[string]float64
	withheld []string
	total    float64
	hasTotal bool
}

// serve returns a served metric's cells within the visible lattice
// (rollupDims: hidden-filtered, scope-trimmed) from its persisted rows.
func (sr *scopedReads) serve(m metricRow, sm *servedMetric, ownDims []string, rollupDims map[string]*rollup.Dimension, dimIDToName map[string]string) servedResult {
	res := servedResult{cells: map[string]float64{}}
	if len(ownDims) == 0 {
		// Dimensionless: its one value is its total.
		if sr.withheld(m.ID, map[string]string{}) {
			res.withheld = append(res.withheld, m.ID)
			return res
		}
		res.total, res.hasTotal = sm.Rows[comboKey(map[string]string{})]
		return res
	}
	formulaText := ""
	if m.Formula != nil {
		formulaText = *m.Formula
	}
	timeSummary := m.TimeSummary
	if timeSummary == "" {
		timeSummary = "sum"
	}
	// The scheduler evaluates the formula at the aggregate for these rules
	// (calculation.EffectiveAggRule), so their totals and rollup cells come
	// from its rows.
	rule := calculation.EffectiveAggRule(m.AggRule, formulaText, dimIDToName)
	evalTotal := isFormulaRule(rule)

	withheldLeaf := map[string]bool{}
	var leaves []leafValue
	for _, combo := range rollup.LeafCombos(rollupDims, ownDims) {
		key := cellKeyOf(m.ID, ownDims, combo)
		if sr.withheld(m.ID, combo) {
			withheldLeaf[key] = true
			res.withheld = append(res.withheld, key)
			continue
		}
		if v, ok := sm.Rows[comboKey(combo)]; ok {
			res.cells[key] = v
			leaves = append(leaves, leafValue{combo: combo, value: v})
		}
	}

	axis := timeAxisOf(rollupDims, ownDims)
	if evalTotal {
		// Rollup cells are the scheduler's own rows, formula re-evaluated at
		// the aggregate; one whose read set touches a hidden member (its
		// subtree holds one) is withheld.
		const rollupComboCap = 20000
		for _, combo := range rollup.RollupCombos(rollupDims, ownDims, rollupComboCap) {
			key := cellKeyOf(m.ID, ownDims, combo)
			if sr.withheld(m.ID, combo) {
				res.withheld = append(res.withheld, key)
				continue
			}
			if v, ok := sm.Rows[comboKey(combo)]; ok {
				res.cells[key] = v
			}
		}
	} else if axis != nil && timeSummary != "none" && !sm.plain {
		// Aggregate periods: each non-time leaf group's visible LEAF periods
		// under the aggregate, reduced by the time summary — withheld when
		// any of them is. Only dated leaves count: the persisted rows of the
		// aggregates themselves (Q1, H1) are never summed back in.
		nonTime := make([]string, 0, len(ownDims))
		for _, d := range ownDims {
			if d != axis.ID {
				nonTime = append(nonTime, d)
			}
		}
		groups := rollup.LeafCombos(rollupDims, nonTime)
		for _, am := range axis.Members {
			if am.IsLeafPeriod() {
				continue
			}
			periods := rollup.LeafDescendants(axis, am.Code)
			sort.SliceStable(periods, func(i, j int) bool {
				return rollup.FindMember(axis, periods[i]).TimeIndex < rollup.FindMember(axis, periods[j]).TimeIndex
			})
			if len(periods) == 0 {
				continue
			}
			for _, g := range groups {
				agg := make(map[string]string, len(g)+1)
				for k, v := range g {
					agg[k] = v
				}
				agg[axis.ID] = am.Code
				aggKey := cellKeyOf(m.ID, ownDims, agg)
				vals := make([]float64, 0, len(periods))
				blocked := false
				for _, p := range periods {
					leaf := make(map[string]string, len(g)+1)
					for k, v := range g {
						leaf[k] = v
					}
					leaf[axis.ID] = p
					if withheldLeaf[cellKeyOf(m.ID, ownDims, leaf)] {
						blocked = true
						break
					}
					if v, ok := sm.Rows[comboKey(leaf)]; ok {
						vals = append(vals, v)
					}
				}
				if blocked {
					res.withheld = append(res.withheld, aggKey)
					continue
				}
				if len(vals) == 0 {
					continue
				}
				if v, ok := calculation.TimeSummaryOver(timeSummary, vals, len(periods)); ok {
					res.cells[aggKey] = v
				}
			}
		}
	}

	switch {
	case evalTotal:
		res.total, res.hasTotal, res.withheld = sr.persistedTotal(m, sm, ownDims, res.withheld)
	case len(withheldLeaf) > 0:
		res.withheld = append(res.withheld, m.ID) // no partial totals
	case axis != nil && timeSummary == "none" && len(leafPeriods(axis)) != 1:
		// No total across periods; a scope of one period is that period
		// (calculation.TimeSummaryOver).
	default:
		if kept, oneLeaf, ok := ruledLeaves(rollupDims, m.ID, ownDims, sr.totalCombo(), leaves); ok {
			res.total, res.hasTotal = combineLeaves(kept, axis, rule, timeSummary, oneLeaf)
		}
	}
	return res
}

// persistedTotal serves the total of a metric whose total only the
// scheduler can compute: its persisted row at the scope's pins on the
// metric's own dimensions ('{}' unscoped, a one-dimension slice row under
// one pin). None when the scope restricts the metric in a way no row
// expresses (a dimension trimmed only through a parent chain, or a pin on a
// related dimension); withheld when that row's read set touches a hidden
// member.
func (sr *scopedReads) persistedTotal(m metricRow, sm *servedMetric, ownDims []string, withheld []string) (float64, bool, []string) {
	combo := map[string]string{}
	for _, d := range ownDims {
		if code, ok := sr.Pinned[d]; ok {
			combo[d] = code
		} else if sr.Trimmed[d] {
			return 0, false, withheld
		}
	}
	for d := range sr.Trimmed {
		if !containsString(ownDims, d) && sr.relatesToAny(ownDims, d) {
			return 0, false, withheld
		}
	}
	if sr.withheld(m.ID, combo) {
		return 0, false, append(withheld, m.ID)
	}
	if v, ok := sm.Rows[comboKey(combo)]; ok {
		return v, ok, withheld
	}
	// Under two pins or more no slice row exists, but the scheduler's rollup
	// row does: every unpinned dimension at its one root ("Consolidated"
	// beside the pinned month and scenario). A variance % that LOOKUPs
	// another dimension served no total at all — its FY column and KPI tile
	// were empty though the cells were there.
	rooted := make(map[string]string, len(ownDims))
	for _, d := range ownDims {
		if code, ok := combo[d]; ok {
			rooted[d] = code
			continue
		}
		root, ok := soleRoot(sr.Dims[d])
		if !ok {
			return 0, false, withheld
		}
		rooted[d] = root
	}
	if sr.withheld(m.ID, rooted) {
		return 0, false, append(withheld, m.ID)
	}
	v, ok := sm.Rows[comboKey(rooted)]
	return v, ok, withheld
}

// soleRoot is the code of dim's one top-level member, when it has exactly
// one: the member a total over the dimension is filed under.
func soleRoot(dim *rollup.Dimension) (string, bool) {
	if dim == nil {
		return "", false
	}
	root := ""
	for _, m := range dim.Members {
		if m.ParentCode != "" {
			continue
		}
		if root != "" {
			return "", false
		}
		root = m.Code
	}
	return root, root != ""
}

// relatesToAny reports whether dimID is one of ownDims or related to one (a
// parent dimension, a property grouping).
func (sr *scopedReads) relatesToAny(ownDims []string, dimID string) bool {
	for _, own := range ownDims {
		if rollup.Relates(sr.Dims, own, dimID) {
			return true
		}
	}
	return false
}

// readsAcrossScope reports whether a plain metric reads a dependency across
// a dimension the scope restricted: the dependency has the dimension, the
// metric neither has it nor relates to it, so its cells are totals over that
// dimension. Recomputed from the scoped inputs they would be the slice in
// view instead, under the same coordinates, and so would everything reading
// them (2026-11-06: a share of total orders, a capacity waterfall).
func (sr *scopedReads) readsAcrossScope(m metricRow, byName map[string]metricRow, metricDims map[string][]string) bool {
	if len(sr.Trimmed) == 0 {
		return false
	}
	refs, err := formula.ExtractRefs(*m.Formula)
	if err != nil {
		return false
	}
	own := metricDims[m.ID]
	for _, ref := range refs {
		dep, ok := byName[strings.ToLower(ref)]
		if !ok {
			continue // a function or dimension name, not a metric
		}
		for _, d := range metricDims[dep.ID] {
			if sr.Trimmed[d] && !sr.relatesToAny(own, d) {
				return true
			}
		}
	}
	return false
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// scopeCalcCells computes every calculated metric's cells and total for a
// scoped read, from the already-scoped per-combo INPUT cells (hidden-member
// filtered, scope-restricted) and, for served metrics, their persisted rows.
// It returns the calculated cells and totals to merge into the response and
// the keys withheld from the caller (cell keys, and bare metric IDs for
// totals), sorted.
//
// runtime.calc_result rows are written by the scheduler with no per-user
// access concept, so an ORDINARY metric declared at a coarser grain than one
// of its dependencies would bake a hidden finer-grained dependency into a
// row keyed only by the coarse member (total_comp at [department] over bonus
// at [staff], one staff member hidden). Recomputing it per combo against the
// scoped inputs — exactly as the scheduler computes it, just over a
// visibility-scoped input set — closes that leak, and deriving its total by
// combining its own per-combo results keeps the "evaluate per combo, then
// combine" model of the unscoped total.
//
// A SERVED metric cannot be recomputed that way — a scoped input set cannot
// reproduce reads outside the scope (a LOOKUP of another region, last
// year's periods) — so it is served from its rows, cell by cell, and
// withheld where its read set touches a hidden member (see the file
// comment).
//
// A combo that fails to evaluate for any other reason (a #DIV/0! at an
// intersection without data) is omitted, not guessed — the display
// contract's safe state.
func scopeCalcCells(
	ctx context.Context,
	rollupDims map[string]*rollup.Dimension,
	metricDimIDs map[string][]string,
	dimIDToName map[string]string,
	universe []metricRow, // allMetrics
	scopedInputCells map[string]float64, // this request's already hidden-member-scoped `cells`, input-only at this point
	sr *scopedReads, // served metrics, withheld decisions, member metadata (nil = none)
) (cells map[string]float64, totals map[string]float64, withheldKeys []string) {
	// Every metric is read at every combination below: one index of each
	// metric's facts serves them all (rollup.WithFactIndexes).
	ctx = rollup.WithFactIndexes(ctx)
	byName := make(map[string]metricRow, len(universe))
	for _, m := range universe {
		byName[m.Name] = m
	}

	// working mirrors `cells`' own composite-key convention
	// (metricID:code1:code2...) so a calc metric resolved earlier in this
	// pass is immediately visible to a calc metric that depends on it,
	// through the exact same fetch path used for inputs.
	working := make(map[string]float64, len(scopedInputCells))
	for k, v := range scopedInputCells {
		working[k] = v
	}
	withheld := map[string]bool{}
	fetchFor := func(metricID string) rollup.RawValue {
		ownDims := metricDimIDs[metricID]
		return func(_ context.Context, _ string, combo map[string]string) (float64, bool, error) {
			for _, dimID := range ownDims {
				if _, ok := combo[dimID]; !ok {
					return 0, false, nil
				}
			}
			key := cellKeyOf(metricID, ownDims, combo)
			if withheld[key] {
				return 0, false, errWithheld
			}
			v, ok := working[key]
			return v, ok, nil
		}
	}

	cells = make(map[string]float64)
	totals = make(map[string]float64)
	done := map[string]bool{}
	// recorded lists a metric's cells, working and withheld, once they are
	// all there — an input from the start, a calculated metric when done —
	// so a read over many leaf coordinates visits those only
	// (rollup.WithRecorded): fetchFor answers nothing anywhere else. A
	// metric still being worked out, or one whose codes hold the key's ":"
	// separator, is not known, and its reads walk every coordinate.
	inputIDs := map[string]bool{}
	for _, m := range universe {
		if m.IsInput {
			inputIDs[m.ID] = true
		}
	}
	cellCoords := map[string][]map[string]string{}
	recorded := func(_ context.Context, metricID string) ([]map[string]string, bool, error) {
		if !inputIDs[metricID] && !done[metricID] {
			return nil, false, nil
		}
		if cs, ok := cellCoords[metricID]; ok {
			return cs, true, nil
		}
		ownDims := metricDimIDs[metricID]
		if len(ownDims) == 0 {
			return nil, false, nil
		}
		prefix := metricID + ":"
		seen := map[string]bool{}
		var cs []map[string]string
		addKey := func(k string) bool {
			if !strings.HasPrefix(k, prefix) || seen[k] {
				return true
			}
			seen[k] = true
			codes := strings.Split(k[len(prefix):], ":")
			if len(codes) != len(ownDims) {
				return false
			}
			c := make(map[string]string, len(ownDims))
			for i, d := range ownDims {
				c[d] = codes[i]
			}
			cs = append(cs, c)
			return true
		}
		for k := range working {
			if !addKey(k) {
				return nil, false, nil
			}
		}
		for k := range withheld {
			if !addKey(k) {
				return nil, false, nil
			}
		}
		cellCoords[metricID] = cs
		return cs, true, nil
	}
	// formulaAt reads a formula/rate metric resolved earlier in this pass at
	// any combination above its leaves: its formula there, never a
	// combination of its leaves (see aggregateReader).
	formulaAt := map[string]aggregateReader{}

	// ratioRefs are a rate metric's numerator and denominator by name, as
	// RatioFormula reads them; nil for any other rule or a missing operand.
	byID := make(map[string]metricRow, len(universe))
	for _, m := range universe {
		byID[m.ID] = m
	}
	ratioRefs := func(m metricRow) []string {
		ops := rateOperands(m)
		if ops == nil {
			return nil
		}
		num, okN := byID[ops[0]]
		den, okD := byID[ops[1]]
		if !okN || !okD {
			return nil
		}
		return []string{num.Name, den.Name}
	}
	var remaining []metricRow
	for _, m := range universe {
		// A text calculation's cells are its stored texts (the grid's
		// texts); a number evaluation of it would only fail.
		if !m.IsInput && m.Formula != nil && *m.Formula != "" && m.Format != metricformula.FormatText && sr.wanted(m.ID) {
			remaining = append(remaining, m)
		}
		// An input totalled as a rate: its cells are as entered, its
		// subtotals and total the ratio of its operands (RatioFormula).
		if m.IsInput && m.Format != metricformula.FormatText && sr.wanted(m.ID) && ratioRefs(m) != nil {
			remaining = append(remaining, m)
		}
	}
	// Bounded by the count at the start: the bound used to be the count
	// still remaining, which shrinks as metrics resolve, so a chain deeper
	// than what was left (operating_margin ← operating_profit ← ebitda …,
	// evaluated in name order) stopped one pass short and its total vanished.
	// A pass that resolves nothing ends it too: what is left cannot resolve.
	for pass, maxPasses := 0, len(remaining)+1; pass < maxPasses && len(remaining) > 0; pass++ {
		var unresolved []metricRow
		for _, m := range remaining {
			ownDims := metricDimIDs[m.ID]
			// More visible cells than a pass may calculate
			// (calculation.MaxMetricCells): none are read — enumerating
			// them is what took a gateway down — and a metric reading it
			// stays unresolved instead of reading zeros.
			if rollup.LeafComboCount(rollupDims, ownDims) > calculation.MaxMetricCells() {
				continue
			}
			if sm := sr.served(m.ID); sm != nil {
				res := sr.serve(m, sm, ownDims, rollupDims, dimIDToName)
				for k, v := range res.cells {
					cells[k] = v
					working[k] = v
				}
				for _, k := range res.withheld {
					withheld[k] = true
				}
				if res.hasTotal {
					totals[m.ID] = res.total
					working[m.ID] = res.total
				}
				if m.Formula != nil && isFormulaRule(calculation.EffectiveAggRule(m.AggRule, *m.Formula, dimIDToName)) {
					formulaAt[m.ID] = servedAggregate(m.ID, sm, sr)
				}
				done[m.ID] = true
				continue
			}
			var refs []string
			ready := true
			if !m.IsInput {
				var err error
				refs, err = formula.ExtractRefs(*m.Formula)
				ready = err == nil
			}
			ratio := ratioRefs(m)
			if ready {
				for _, ref := range append(append([]string{}, refs...), ratio...) {
					refM, isMetric := byName[ref]
					if !isMetric || refM.IsInput {
						continue // not a metric reference, or an input (already fully available)
					}
					if !done[refM.ID] {
						ready = false
						break
					}
				}
			}
			if !ready {
				unresolved = append(unresolved, m)
				continue
			}
			scopeOrdinaryMetric(ctx, m, ownDims, refs, ratio, byName, rollupDims, metricDimIDs, dimIDToName, sr, fetchFor, recorded,
				formulaAt, cells, totals, working, withheld)
			done[m.ID] = true
		}
		if len(unresolved) == len(remaining) {
			break
		}
		remaining = unresolved
	}
	for k := range withheld {
		withheldKeys = append(withheldKeys, k)
	}
	sort.Strings(withheldKeys)
	return cells, totals, withheldKeys
}

// aggregateReader reads a formula/rate metric at a combination: its value
// there, or the evaluation's error.
type aggregateReader func(combo map[string]string) (float64, error)

// servedAggregate reads a served formula/rate metric above its leaves from
// the scheduler's persisted rows; a combination with no row has no value,
// and one whose read set touches a hidden member is withheld.
func servedAggregate(metricID string, sm *servedMetric, sr *scopedReads) aggregateReader {
	return func(combo map[string]string) (float64, error) {
		if sr.withheld(metricID, combo) {
			return 0, errWithheld
		}
		if v, ok := sm.Rows[comboKey(combo)]; ok {
			return v, nil
		}
		return 0, errNoAggregateValue
	}
}

type onceResult struct {
	v   float64
	err error
}

// errNoAggregateValue: a formula/rate dependency has no value at the
// combination read; the reading cell has none either, as in the scheduler.
var errNoAggregateValue = errors.New("no value at this combination")

// pinsLeaves reports whether combo pins every one of ownDims at a leaf.
func pinsLeaves(dims map[string]*rollup.Dimension, ownDims []string, combo map[string]string) bool {
	for _, d := range ownDims {
		code, ok := combo[d]
		if !ok || dims[d] == nil {
			return false
		}
		if leaves := rollup.LeafDescendants(dims[d], code); len(leaves) != 1 || leaves[0] != code {
			return false
		}
	}
	return true
}

// scopeOrdinaryMetric recomputes one ordinary calculated metric per combo.
//
// Once a metric is structurally ready (every calc dependency it references
// already resolved), each of ITS OWN combos is evaluated independently — a
// runtime error on one combo (e.g. a #DIV/0! from a dependency that's
// genuinely unentered for that combo, not hidden) skips only that combo,
// exactly like executePartition's own per-combo evaluation loop. An
// all-or-nothing design was tried and rejected: it silently produced ZERO
// cells for budget_variance_pct against real seed data. A combo that reads a
// withheld cell is withheld, and so is the total.
func scopeOrdinaryMetric(
	ctx context.Context, m metricRow, ownDims, refs, ratioRefs []string, byName map[string]metricRow,
	rollupDims map[string]*rollup.Dimension, metricDimIDs map[string][]string, dimIDToName map[string]string,
	sr *scopedReads, fetchFor func(string) rollup.RawValue, recorded rollup.Recorded, formulaAt map[string]aggregateReader,
	cells, totals, working map[string]float64, withheld map[string]bool,
) {
	combos := rollup.LeafCombos(rollupDims, ownDims)
	if len(combos) == 0 {
		combos = []map[string]string{{}}
	}
	// A pure ratio (agg_rule "average" whose formula reads no member) totals
	// as "formula", as executePartition does (calculation.EffectiveAggRule):
	// a ratio-of-sums at the total, its own value at every cell.
	// An input here is a rate (scopeCalcCells): it has no formula, and only
	// its rows above the leaves are worked out.
	formulaText := ""
	if m.Formula != nil {
		formulaText = *m.Formula
	}
	rule := m.AggRule
	if !m.IsInput {
		rule = calculation.EffectiveAggRule(m.AggRule, formulaText, dimIDToName)
	}
	evalWith := func(formulaText string, refs []string) aggregateReader {
		return func(combo map[string]string) (float64, error) {
			values := make(map[string]float64, len(refs))
			blank, noValue := map[string]bool{}, map[string]bool{}
			for _, ref := range refs {
				refM, isMetric := byName[ref]
				if !isMetric {
					continue // function or dimension name, not a metric reference
				}
				// ResolveTime with the dependency's time summary, as the
				// scheduler reads a plain reference (resolveDependency): an
				// unpinned time dimension reduces by time_summary, not agg_rule;
				// a pin on a dimension the dependency neither has nor relates to
				// is dropped (drv_global [cost_type] at All Departments × FY 2027
				// read 84 × 2.5 = 210).
				at := combo
				if norm, nErr := rollup.NormalizeCombo(rollupDims, metricDimIDs[refM.ID], combo, nil); nErr == nil {
					at = norm
				}
				// A formula/rate dependency above its leaves — a parent, an
				// aggregate period, the total — is its formula evaluated there,
				// as the scheduler reads it (resolveDependency's exact source):
				// a margin read at FY by `margin - ly_margin` summed its months'
				// margins by its time summary (2026-10-18, the CPG dashboard).
				if read := formulaAt[refM.ID]; read != nil && !pinsLeaves(rollupDims, metricDimIDs[refM.ID], at) {
					v, err := read(at)
					if err != nil {
						return 0, err
					}
					values[refM.Name] = v
					continue
				}
				// Recorded, as resolveDependency: nothing entered beneath the
				// cell (a month with no postings on any account) is blank to
				// ISBLANK and "", and 0 in arithmetic.
				v, found, err := rollup.ResolveTimeRecorded(rollup.WithRecorded(ctx, recorded), rollupDims, refM.ID, metricDimIDs[refM.ID], rollup.AggRule(refM.AggRule),
					rollup.TimeSummaryRule(refM.TimeSummary), at, fetchFor(refM.ID))
				if err != nil {
					return 0, err
				}
				values[refM.Name] = v
				if !found {
					blank[refM.Name] = true
					if calculation.NoValueAt(rollupDims, refM.ID, metricDimIDs[refM.ID], refM.AggRule, refM.TimeSummary, at) {
						noValue[refM.Name] = true
					}
				}
			}
			namedDims := make(map[string]string, len(combo))
			for dimID, code := range combo {
				if name, found := dimIDToName[dimID]; found {
					namedDims[name] = code
				}
			}
			// The member-metadata context (UNFILTERED): dim.property and PARENT
			// compute exactly as in the scheduler.
			return calculation.EvaluateMetricWithDimContext(sr.metadata(), m.Name, formulaText, values, blank, noValue, namedDims, sr.cellContext(combo))
		}
	}
	evalCombo := evalWith(formulaText, refs)
	// Above its leaves a rate metric is its numerator over its denominator,
	// each read there by its own rules — as the scheduler's rows are — not
	// its formula (calculation.RatioFormula).
	aggEval := evalCombo
	if len(ratioRefs) == 2 {
		aggEval = evalWith(calculation.RatioFormula(ratioRefs[0], ratioRefs[1]), ratioRefs)
	}
	if isFormulaRule(rule) {
		// Its readers evaluate it above its leaves (see evalCombo); each
		// combination once.
		memo := map[string]onceResult{}
		formulaAt[m.ID] = func(combo map[string]string) (float64, error) {
			key := comboKey(combo)
			if r, ok := memo[key]; ok {
				return r.v, r.err
			}
			v, err := aggEval(combo)
			memo[key] = onceResult{v, err}
			return v, err
		}
	}

	var leaves []leafValue
	anyWithheld := false
	if m.IsInput {
		combos = nil // its cells are as entered
	}
	for _, combo := range combos {
		key := cellKeyOf(m.ID, ownDims, combo)
		if len(combo) == 0 {
			key = m.ID
		}
		v, err := evalCombo(combo)
		if errors.Is(err, errWithheld) {
			withheld[key] = true
			anyWithheld = true
			continue
		}
		if err != nil {
			continue // genuine per-combo error (e.g. #DIV/0!) — skip just this combo
		}
		leaves = append(leaves, leafValue{combo: combo, value: v})
		working[key] = v
		if len(combo) > 0 {
			cells[key] = v
		}
	}
	if anyWithheld {
		withheld[m.ID] = true // no partial totals
		return
	}
	axis := timeAxisOf(rollupDims, ownDims)
	if isFormulaRule(rule) {
		axis = nil
	}
	leaves, oneLeaf, ruledOK := ruledLeaves(rollupDims, m.ID, ownDims, sr.totalCombo(), leaves)
	if !ruledOK {
		// A rule "none" leaves no total here; a formula total (below) is
		// evaluated at the aggregate and reads its own rules.
		leaves = nil
	}
	if axis != nil {
		if v, ok := combineLeaves(leaves, axis, rule, m.TimeSummary, oneLeaf); ok {
			totals[m.ID] = v
			working[m.ID] = v
		}
	} else if v, ok := combineLeaves(leaves, nil, rule, m.TimeSummary, oneLeaf); ok {
		totals[m.ID] = v
		working[m.ID] = v
	}

	if !isFormulaRule(rule) {
		return
	}
	// Rollup cells for rules the client cannot combine — the scoped twin of
	// the scheduler's own rollup-row persistence: evaluated from the SAME
	// scoped inputs, so a restricted user's World row aggregates exactly the
	// members they may see.
	const rollupComboCap = 20000
	for _, combo := range rollup.RollupCombos(rollupDims, ownDims, rollupComboCap) {
		key := cellKeyOf(m.ID, ownDims, combo)
		v, err := aggEval(combo)
		if errors.Is(err, errWithheld) {
			withheld[key] = true
			continue
		}
		if err != nil {
			continue // renders "—", the display contract's safe state
		}
		cells[key] = v
	}
	// agg_rule 'formula' / 'rate': the total is the formula evaluated once
	// against fully-aggregated (scoped) inputs — the scheduler's own '{}'
	// row and slice rows — never a combination of the per-combo results.
	// When that evaluation fails, the combined value computed above is NOT
	// left standing: it is the very number the rule exists to avoid.
	v, err := aggEval(sr.totalCombo())
	switch {
	case errors.Is(err, errWithheld):
		delete(totals, m.ID)
		delete(working, m.ID)
		withheld[m.ID] = true
	case err != nil:
		delete(totals, m.ID)
		delete(working, m.ID)
	default:
		totals[m.ID] = v
		working[m.ID] = v
	}
}

// loadUserAccessRules reads a user's access rules as they apply to
// revisionID: member ID -> access for dimension_member rules, metric ID ->
// access for metric rules, each resolved by lineage (migration 099) against
// that revision by writeguard.RulesForRevision — so an older revision keeps hidden and
// read-only what the active one does. Any error is returned, never
// swallowed: a caller must fail closed rather than treat an unreadable rule
// set as none.
func loadUserAccessRules(ctx context.Context, q writeguard.Querier, userID, revisionID string) (dimRules, metricRules map[string]string, err error) {
	return writeguard.RuleMaps(ctx, q, userID, revisionID)
}

// loadScopedReads prepares a scoped read: the persisted rows of every
// served metric the caller may see, the UNFILTERED member metadata, and —
// when the caller has hidden members — the read sets that decide what is
// withheld from them. revisionMetrics is every metric of the revision,
// hidden metrics included.
// comparesText reports whether a number formula reads a text metric (in a
// comparison or ISBLANK, the one way validation allows): its value is
// served from its persisted rows, which the scheduler computed with the
// text bound, never recomputed from number cells (the text would read 0).
func comparesText(formulaText, format string, textNames map[string]bool) bool {
	if format == metricformula.FormatText || len(textNames) == 0 {
		return false
	}
	an, err := formula.Analyze(formulaText)
	if err != nil {
		return false
	}
	for _, ref := range an.References {
		if textNames[strings.ToLower(ref.Name)] {
			return true
		}
	}
	return false
}

func (h *handler) loadScopedReads(
	ctx context.Context, modelID, revisionID string,
	unfiltered map[string]*rollup.Dimension, dimIDToName map[string]string,
	allMetrics, revisionMetrics []metricRow, metricDims map[string][]string,
	hiddenByDim map[string]map[string]bool, scopePinned map[string]string, scopeSubtrees map[string]map[string]bool,
	only map[string]bool,
) (*scopedReads, error) {
	return h.loadScopedReadsServing(ctx, modelID, revisionID, unfiltered, dimIDToName, allMetrics, revisionMetrics, metricDims,
		hiddenByDim, scopePinned, scopeSubtrees, only, false)
}

// loadServedReads is loadScopedReads for a caller with no hidden member: the
// read's own calculated metrics (direct) are served from the scheduler's
// rows, as the cells and totals a recompute over the scoped inputs would
// give — the scope restricts the cells shown, never what a cell is, and
// with nothing hidden no finer input can leak into a coarser cell. A metric
// whose total under this scope no stored row holds (a formula rule under
// two pins without a sole root) is worked out as before, with what it reads
// served where it can be. Every scoped read used to work out every metric
// it reaches again: a five-row region grid under a scenario selector took
// 27 s for HQ (0.5 s unscoped), its rows read through four groupings
// (2026-11-27).
func (h *handler) loadServedReads(
	ctx context.Context, modelID, revisionID string,
	unfiltered map[string]*rollup.Dimension, dimIDToName map[string]string,
	allMetrics, revisionMetrics []metricRow, metricDims map[string][]string,
	scopePinned map[string]string, scopeSubtrees map[string]map[string]bool, direct []string,
) (*scopedReads, error) {
	want := make(map[string]bool, len(direct))
	for _, id := range direct {
		want[id] = true
	}
	sr, err := h.loadScopedReadsServing(ctx, modelID, revisionID, unfiltered, dimIDToName, allMetrics, revisionMetrics, metricDims,
		nil, scopePinned, scopeSubtrees, want, true)
	if err != nil {
		return nil, err
	}
	var rest []string
	for _, m := range allMetrics {
		if want[m.ID] && !m.IsInput && m.Formula != nil && m.Format != metricformula.FormatText && sr.Served[m.ID] == nil {
			rest = append(rest, m.ID)
		}
	}
	if len(rest) == 0 {
		return sr, nil
	}
	for id := range readsClosure(rest, allMetrics) {
		want[id] = true
	}
	return h.loadScopedReadsServing(ctx, modelID, revisionID, unfiltered, dimIDToName, allMetrics, revisionMetrics, metricDims,
		nil, scopePinned, scopeSubtrees, want, true)
}

// loadScopedReadsServing is loadScopedReads; serveAll serves every wanted
// calculated metric whose total under the scope a stored row holds (a
// caller with no hidden member, loadServedReads).
func (h *handler) loadScopedReadsServing(
	ctx context.Context, modelID, revisionID string,
	unfiltered map[string]*rollup.Dimension, dimIDToName map[string]string,
	allMetrics, revisionMetrics []metricRow, metricDims map[string][]string,
	hiddenByDim map[string]map[string]bool, scopePinned map[string]string, scopeSubtrees map[string]map[string]bool,
	only map[string]bool, serveAll bool,
) (*scopedReads, error) {
	store := calculation.NewStore(h.db.For(ctx))
	schema, err := store.LoadDimensionSchema(ctx, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	meta := calculation.NewDimMetadata(unfiltered, dimIDToName, schema)
	for _, m := range revisionMetrics {
		meta.SetMetric(m.Name, metricDims[m.ID], m.PicklistDimensionID)
	}
	sr := &scopedReads{
		Served:  map[string]*servedMetric{},
		Meta:    meta,
		Pinned:  scopePinned,
		Trimmed: make(map[string]bool, len(scopeSubtrees)),
		Dims:    unfiltered,
		Only:    only,
	}
	for d := range scopeSubtrees {
		sr.Trimmed[d] = true
	}
	if len(hiddenByDim) > 0 {
		names := make(map[string]string, len(revisionMetrics))
		for _, m := range revisionMetrics {
			names[m.ID] = m.Name
		}
		metrics := make([]readset.Metric, 0, len(revisionMetrics))
		for _, m := range revisionMetrics {
			rm := readset.Metric{ID: m.ID, Name: m.Name, IsInput: m.IsInput, Dims: metricDims[m.ID]}
			if m.Formula != nil {
				rm.Formula = *m.Formula
			}
			// A served rate metric's rows above its leaves are its operands'
			// ratio: a reader is withheld them where the operands read a
			// member hidden from it, whatever its formula reads.
			for _, id := range rateOperands(m) {
				if name, ok := names[id]; ok {
					rm.Reads = append(rm.Reads, name)
				}
			}
			metrics = append(metrics, rm)
		}
		sr.Reads = readset.New(unfiltered, dimIDToName, meta, metrics, hiddenByDim)
	}
	textNames := map[string]bool{}
	for _, m := range allMetrics {
		if m.Format == metricformula.FormatText {
			textNames[strings.ToLower(m.Name)] = true
		}
	}
	byName := make(map[string]metricRow, len(revisionMetrics))
	for _, m := range revisionMetrics {
		byName[strings.ToLower(m.Name)] = m
	}
	for _, m := range allMetrics {
		if m.IsInput || m.Formula == nil || !sr.wanted(m.ID) {
			continue
		}
		natural := readset.Served(*m.Formula) || comparesText(*m.Formula, m.Format, textNames) || sr.readsAcrossScope(m, byName, metricDims)
		if !natural && (!serveAll || m.Format == metricformula.FormatText) {
			continue
		}
		rows, err := calcValueMapOnce(ctx, store, modelID, revisionID, m.ID)
		if err != nil {
			return nil, err
		}
		sm := &servedMetric{Rows: rows, plain: !natural}
		if !natural && isFormulaRule(calculation.EffectiveAggRule(m.AggRule, *m.Formula, dimIDToName)) {
			if _, ok, _ := sr.persistedTotal(m, sm, metricDims[m.ID], nil); !ok {
				continue // its total here is worked out (loadServedReads)
			}
		}
		sr.Served[m.ID] = sm
	}
	return sr, nil
}

// calcValueMemo is the stored values of served metrics that a batch of
// scoped reads in one request shares (withCalcValueMemo): an export with
// totals reads the engine's total at each parent row and column it shows
// (gridTotalsAt), and every one of those reads loaded each served metric's
// whole runtime.calc_result again — the same rows each time, about 60% of
// each read (2026-11-14). The rows are only looked up (sm.Rows[...]), never
// changed, so the reads running at once share one map.
type calcValueMemo struct {
	mu   sync.Mutex
	maps map[[3]string]*calcValueEntry
}

type calcValueEntry struct {
	once sync.Once
	rows map[string]float64
	err  error
}

type calcValueMemoKey struct{}

// withCalcValueMemo gives the reads made with ctx one calcValueMemo.
func withCalcValueMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, calcValueMemoKey{}, &calcValueMemo{maps: map[[3]string]*calcValueEntry{}})
}

// calcValueMapOnce is store.LoadCalcValueMap, read once per ctx's
// calcValueMemo when it has one.
func calcValueMapOnce(ctx context.Context, store *calculation.Store, modelID, revisionID, metricID string) (map[string]float64, error) {
	memo, _ := ctx.Value(calcValueMemoKey{}).(*calcValueMemo)
	if memo == nil {
		return store.LoadCalcValueMap(ctx, modelID, revisionID, metricID)
	}
	key := [3]string{modelID, revisionID, metricID}
	memo.mu.Lock()
	e := memo.maps[key]
	if e == nil {
		e = &calcValueEntry{}
		memo.maps[key] = e
	}
	memo.mu.Unlock()
	e.once.Do(func() { e.rows, e.err = store.LoadCalcValueMap(ctx, modelID, revisionID, metricID) })
	return e.rows, e.err
}

// readMetricIDs are the metrics a read answers for: a grid's own, or the
// ones a KPI tile asked for; nil for a whole-model read.
func readMetricIDs(gridDefID string, metrics []metricRow, asked []string) []string {
	if gridDefID != "" {
		return gridMetricIDs(metrics)
	}
	if len(asked) > 0 {
		return asked
	}
	return nil
}
