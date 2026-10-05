package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/readset"
	"github.com/mavericks-engine/mavericks/internal/rollup"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// Scoped grid reads of calculated metrics (a caller with hidden members, or
// a scope pin): FORMULA_CALCULATION_INSTRUCTIONS.md contracts C6 and C7.
//
//   - A SERVED metric (time functions, LOOKUP, the *IFS/*IF family, TIMESUM,
//     the *VALUE family — readset.Served) reads members and periods outside
//     any scope, so it is never re-evaluated here: its cells are the
//     scheduler's persisted rows. A persisted cell whose read set touches a
//     member hidden from the caller is WITHHELD: absent from cells and listed
//     in the response's withheld array, never recomputed over visible data.
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
// period and the periods by timeSummary; without one, by aggRule. ok is
// false when there is nothing to reduce or the summary is 'none'.
func combineLeaves(leaves []leafValue, axis *rollup.Dimension, aggRule, timeSummary string) (float64, bool) {
	if len(leaves) == 0 {
		return 0, false
	}
	if !rollup.Aggregates(rollup.AggRule(aggRule)) {
		// agg_rule none: a scope of one leaf is that leaf, anything wider
		// has no total.
		if len(leaves) == 1 {
			return leaves[0].value, true
		}
		return 0, false
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
	for _, p := range leafPeriods(axis) {
		if vals, ok := perPeriod[p]; ok {
			ordered = append(ordered, rollup.CombineAgg(vals, rollup.AggRule(aggRule)))
		}
	}
	if len(ordered) == 0 {
		return 0, false
	}
	return calculation.TimeSummary(timeSummary, ordered)
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
	// (tsEvaluator.finish's useEval), so their totals come from its rows.
	evalTotal := isFormulaRule(m.AggRule) ||
		(m.AggRule == string(rollup.AggAverage) && !calculation.FormulaReferencesDims(formulaText, dimIDToName))

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
	if isFormulaRule(m.AggRule) {
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
	} else if axis != nil && timeSummary != "none" {
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
				if v, ok := calculation.TimeSummary(timeSummary, vals); ok {
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
	case axis != nil && timeSummary == "none":
	default:
		res.total, res.hasTotal = combineLeaves(leaves, axis, m.AggRule, timeSummary)
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
		if containsString(ownDims, d) {
			continue
		}
		for _, own := range ownDims {
			if rollup.Relates(sr.Dims, own, d) {
				return 0, false, withheld
			}
		}
	}
	if sr.withheld(m.ID, combo) {
		return 0, false, append(withheld, m.ID)
	}
	v, ok := sm.Rows[comboKey(combo)]
	return v, ok, withheld
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

	var remaining []metricRow
	for _, m := range universe {
		// A text calculation's cells are its stored texts (the grid's
		// texts); a number evaluation of it would only fail.
		if !m.IsInput && m.Formula != nil && *m.Formula != "" && m.Format != metricformula.FormatText {
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
				done[m.ID] = true
				continue
			}
			refs, err := formula.ExtractRefs(*m.Formula)
			ready := err == nil
			if ready {
				for _, ref := range refs {
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
			scopeOrdinaryMetric(ctx, m, ownDims, refs, byName, rollupDims, metricDimIDs, dimIDToName, sr, fetchFor,
				cells, totals, working, withheld)
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
	ctx context.Context, m metricRow, ownDims, refs []string, byName map[string]metricRow,
	rollupDims map[string]*rollup.Dimension, metricDimIDs map[string][]string, dimIDToName map[string]string,
	sr *scopedReads, fetchFor func(string) rollup.RawValue,
	cells, totals, working map[string]float64, withheld map[string]bool,
) {
	combos := rollup.LeafCombos(rollupDims, ownDims)
	if len(combos) == 0 {
		combos = []map[string]string{{}}
	}
	// Mirror executePartition's own collapse for a pure ratio metric
	// (agg_rule "average" whose formula isn't itself dimension-conditional):
	// a ratio-of-sums computed once, not an average of per-combo ratios —
	// otherwise a restricted caller sees a numerically different value than
	// the unrestricted calc_result everyone else reads.
	collapsed := false
	if m.AggRule == string(rollup.AggAverage) && len(combos) > 1 && !calculation.FormulaReferencesDims(*m.Formula, dimIDToName) {
		combos = []map[string]string{{}}
		collapsed = true
	}
	evalCombo := func(combo map[string]string) (float64, error) {
		values := make(map[string]float64, len(refs))
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
			v, _, err := rollup.ResolveTime(ctx, rollupDims, refM.ID, metricDimIDs[refM.ID], rollup.AggRule(refM.AggRule),
				rollup.TimeSummaryRule(refM.TimeSummary), at, fetchFor(refM.ID))
			if err != nil {
				return 0, err
			}
			values[refM.Name] = v
		}
		namedDims := make(map[string]string, len(combo))
		for dimID, code := range combo {
			if name, found := dimIDToName[dimID]; found {
				namedDims[name] = code
			}
		}
		// The member-metadata context (UNFILTERED): dim.property and PARENT
		// compute exactly as in the scheduler.
		return calculation.EvaluateMetricWithDimContext(sr.metadata(), m.Name, *m.Formula, values, namedDims, sr.cellContext(combo))
	}

	var leaves []leafValue
	anyWithheld := false
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
	if collapsed || isFormulaRule(m.AggRule) {
		axis = nil
	}
	if axis != nil {
		if v, ok := combineLeaves(leaves, axis, m.AggRule, m.TimeSummary); ok {
			totals[m.ID] = v
			working[m.ID] = v
		}
	} else if v, ok := combineLeaves(leaves, nil, m.AggRule, m.TimeSummary); ok {
		totals[m.ID] = v
		working[m.ID] = v
	}

	if !isFormulaRule(m.AggRule) {
		return
	}
	// Rollup cells for rules the client cannot combine — the scoped twin of
	// the scheduler's own rollup-row persistence: evaluated from the SAME
	// scoped inputs, so a restricted user's World row aggregates exactly the
	// members they may see.
	const rollupComboCap = 20000
	for _, combo := range rollup.RollupCombos(rollupDims, ownDims, rollupComboCap) {
		key := cellKeyOf(m.ID, ownDims, combo)
		v, err := evalCombo(combo)
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
	v, err := evalCombo(sr.totalCombo())
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
func (h *handler) loadScopedReads(
	ctx context.Context, modelID, revisionID string,
	unfiltered map[string]*rollup.Dimension, dimIDToName map[string]string,
	allMetrics, revisionMetrics []metricRow, metricDims map[string][]string,
	hiddenByDim map[string]map[string]bool, scopePinned map[string]string, scopeSubtrees map[string]map[string]bool,
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
	}
	for d := range scopeSubtrees {
		sr.Trimmed[d] = true
	}
	for _, m := range allMetrics {
		if m.IsInput || m.Formula == nil || !readset.Served(*m.Formula) {
			continue
		}
		rows, err := store.LoadCalcValueMap(ctx, modelID, revisionID, m.ID)
		if err != nil {
			return nil, err
		}
		sr.Served[m.ID] = &servedMetric{Rows: rows}
	}
	if len(hiddenByDim) > 0 {
		metrics := make([]readset.Metric, 0, len(revisionMetrics))
		for _, m := range revisionMetrics {
			rm := readset.Metric{ID: m.ID, Name: m.Name, IsInput: m.IsInput, Dims: metricDims[m.ID]}
			if m.Formula != nil {
				rm.Formula = *m.Formula
			}
			metrics = append(metrics, rm)
		}
		sr.Reads = readset.New(unfiltered, dimIDToName, meta, metrics, hiddenByDim)
	}
	return sr, nil
}
