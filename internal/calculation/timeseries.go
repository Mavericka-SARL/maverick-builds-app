package calculation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// Time-series execution (spec §8).
//
// A metric dimensioned by a declared time dimension is evaluated once per
// leaf period with every non-time coordinate held fixed; time functions in
// its formula reach other periods through formula.TimeEvalContext.EvalAt,
// which this file fulfils. Its aggregate rows reduce non-time dimensions
// first (agg_rule) and time last (time_summary). A recurrence — a
// dependency cycle broken by time, such as an opening/closing balance — is
// evaluated period by period across all of its members and persisted as one
// transaction.

// timeAxis is a metric's time dimension in evaluation shape.
type timeAxis struct {
	dim     *rollup.Dimension
	periods []formula.TimePeriod
	index   map[string]int // member code → position
}

// timeAxisFor finds the one time dimension among dimIDs. nil when there is
// none; an error when there is more than one (a grid must not carry two).
func timeAxisFor(allDims map[string]*rollup.Dimension, dimIDs []string) (*timeAxis, error) {
	var axis *timeAxis
	for _, id := range dimIDs {
		d := allDims[id]
		if d == nil || !d.IsTime {
			continue
		}
		if axis != nil {
			return nil, fmt.Errorf("%s: metric is dimensioned by more than one time dimension", formula.CodeMultipleTimeDimensions)
		}
		axis = &timeAxis{dim: d, index: make(map[string]int, len(d.Members))}
		// The axis is the LEAF periods in time_index order; aggregate
		// periods (H1, FY26) are not positions, they are reductions of the
		// leaves beneath them (see leafPositions).
		var members []rollup.Member
		for _, m := range d.Members {
			if m.IsLeafPeriod() {
				members = append(members, m)
			}
		}
		sort.SliceStable(members, func(i, j int) bool { return members[i].TimeIndex < members[j].TimeIndex })
		for i, m := range members {
			axis.periods = append(axis.periods, formula.TimePeriod{Code: m.Code, Index: i, Start: m.PeriodStart, End: m.PeriodEnd})
			axis.index[m.Code] = i
		}
	}
	return axis, nil
}

func (a *timeAxis) timeCtx(pos int) *formula.TimeEvalContext {
	return &formula.TimeEvalContext{
		DimensionID:          a.dim.ID,
		Granularity:          a.dim.TimeGranularity,
		FiscalYearStartMonth: a.dim.FiscalYearStartMonth,
		Position:             pos,
		Periods:              a.periods,
	}
}

// leafPositions returns the axis positions of the leaf periods at or below
// code (the code itself when it is a leaf), in chronological order; nil for
// an unknown code or an empty aggregate.
func (a *timeAxis) leafPositions(code string) []int {
	if pos, ok := a.index[code]; ok {
		return []int{pos}
	}
	sub := subtreeCodes(a.dim, code)
	var out []int
	for c := range sub {
		if pos, ok := a.index[c]; ok {
			out = append(out, pos)
		}
	}
	sort.Ints(out)
	return out
}

// nonTimeDims returns dimIDs without the axis dimension.
func (a *timeAxis) nonTimeDims(dimIDs []string) []string {
	out := make([]string, 0, len(dimIDs))
	for _, id := range dimIDs {
		if id != a.dim.ID {
			out = append(out, id)
		}
	}
	return out
}

// TimeSummary reduces one metric's per-period values across time. ok=false
// for 'none': a time total is meaningless for the metric and no row is
// written (renders "—"). Exported so the gateway's scoped grid read combines
// persisted leaf rows exactly as the scheduler does.
func TimeSummary(method string, vals []float64) (float64, bool) {
	if len(vals) == 0 {
		return 0, method != "none"
	}
	switch method {
	case "none":
		return 0, false
	case "average":
		var s float64
		for _, v := range vals {
			s += v
		}
		return s / float64(len(vals)), true
	case "min":
		m := vals[0]
		for _, v := range vals[1:] {
			if v < m {
				m = v
			}
		}
		return m, true
	case "max":
		m := vals[0]
		for _, v := range vals[1:] {
			if v > m {
				m = v
			}
		}
		return m, true
	case "first":
		return vals[0], true
	case "last":
		return vals[len(vals)-1], true
	default: // sum
		var s float64
		for _, v := range vals {
			s += v
		}
		return s, true
	}
}

// TimeSummaryOver is TimeSummary over a reduction of `periods` periods, of
// which vals are those with a value: an average counts a period with no
// value as 0 (rollup.CombineTimeOver).
func TimeSummaryOver(method string, vals []float64, periods int) (float64, bool) {
	if method == "average" && len(vals) > 0 {
		return rollup.CombineTimeOver(vals, periods, "average"), true
	}
	return TimeSummary(method, vals)
}

// usesTimeSeries reports whether a formula calls a time function. A formula
// that does not parse is not time series: it fails later with the parser's
// own error.
func usesTimeSeries(formulaText string) bool {
	an, err := formula.Analyze(formulaText)
	return err == nil && an.UsesTimeSeries
}

// summarizeOverTime reduces leaf results to one value the way every
// time-dimensioned total does: non-time dimensions first (aggRule, per
// period), then time (timeSummary; an average counts every period of the
// reduction, one with no row as 0). subtrees optionally restricts the leaves
// to member subtrees per dimension (a slice). ok=false when there is nothing
// to reduce or the time summary is 'none'.
func summarizeOverTime(leaves []CalcResultRow, axis *timeAxis, aggRule, timeSummaryRule string, subtrees map[string]map[string]bool) (float64, bool) {
	if !rollup.Aggregates(rollup.AggRule(aggRule)) {
		return 0, false
	}
	perPeriod := map[int][]float64{}
	for _, r := range leaves {
		pos, ok := axis.index[r.DimMembers[axis.dim.ID]]
		if !ok {
			continue
		}
		match := true
		for dimID, sub := range subtrees {
			if !sub[r.DimMembers[dimID]] {
				match = false
				break
			}
		}
		if match {
			perPeriod[pos] = append(perPeriod[pos], r.Value)
		}
	}
	if len(perPeriod) == 0 {
		return 0, false
	}
	vals := make([]float64, 0, len(axis.periods))
	periods := 0
	for pos, p := range axis.periods {
		if sub, ok := subtrees[axis.dim.ID]; ok && !sub[p.Code] {
			continue
		}
		periods++
		if v, ok := perPeriod[pos]; ok {
			vals = append(vals, rollup.CombineAgg(v, rollup.AggRule(aggRule)))
		}
	}
	return TimeSummaryOver(timeSummaryRule, vals, periods)
}

// memoKey identifies one evaluation of one AST node at one time position
// for one non-time coordinate (spec §6 item 7).
type memoKey struct {
	node    formula.Node
	pos     int
	nonTime string
}

// tsEvaluator evaluates one time-dimensioned metric at any (combo, period).
type tsEvaluator struct {
	s           *Scheduler
	ctx         context.Context
	def         *MetricDef
	node        formula.Node
	modelID     string
	revisionID  string
	axis        *timeAxis
	allDims     map[string]*rollup.Dimension
	metricDims  map[string][]string
	dimIDToName map[string]string
	allDefs     map[string]*MetricDef
	fetch       map[string]rollup.RawValue
	// rows: persisted rows of formula/rate calculated dependencies
	// (exactRows); a recurrence peer has none.
	rows map[string]*exactSource
	memo map[memoKey]formula.Value
	// varsMemo caches the resolved dependency values per coordinate. It is
	// nil for a recurrence member: vars binds EVERY dependency eagerly, so
	// caching would freeze a peer's value at a coordinate before the peer
	// had been computed there (opening_cash binds closing_cash at t while
	// only ever reading it at t-1). A pinned-leaf Resolve is one map lookup,
	// so the recurrence path simply re-resolves.
	varsMemo map[string]map[string]float64
	// plain holds the UPPER-CASE names the formula reads as plain values;
	// a dependency read only as a LOOKUP / *IFS / *VALUE source is never
	// bound. nil (a formula that does not parse) binds every dependency.
	plain map[string]bool
	// reads fulfils the dimensional context's Resolve and records whether
	// any value read through it (or through Summarize) was non-zero.
	reads *dimReads
	// hasConditional: the formula reads through the dimensional context
	// (LOOKUP, the conditional aggregations) or Summarize (the *VALUE
	// family) — reads a memo hit (the conditional memo, or EvalAt's) skips,
	// so "nothing read" must be re-checked without the memos.
	hasConditional bool
	// noMemo disables both memos for one evaluation (the no-data recheck).
	noMemo bool
}

// overlay holds a recurrence component's own results as they are computed,
// so a member's read of a peer (or of itself at another period) sees this
// pass's value and never a stale persisted one.
type overlay map[string]map[string]float64

func (s *Scheduler) newTSEvaluator(
	ctx context.Context, def *MetricDef, modelID, revisionID string, axis *timeAxis,
	allDims map[string]*rollup.Dimension, metricDimIDs map[string][]string, dimIDToName map[string]string,
	meta *DimMetadata, allDefs map[string]*MetricDef, ov overlay,
) (*tsEvaluator, error) {
	node, err := formula.Parse(def.Formula)
	if err != nil {
		return nil, fmt.Errorf("parse formula for %s: %w", def.Name, err)
	}
	e := &tsEvaluator{
		s: s, ctx: ctx, def: def, node: node, modelID: modelID, revisionID: revisionID, axis: axis,
		allDims: allDims, metricDims: metricDimIDs, dimIDToName: dimIDToName, allDefs: allDefs,
		fetch: make(map[string]rollup.RawValue, len(def.DependsOnID)),
		rows:  map[string]*exactSource{},
		memo:  map[memoKey]formula.Value{}, varsMemo: map[string]map[string]float64{},
	}
	env := newEvalEnv(s, modelID, revisionID, allDims, metricDimIDs, dimIDToName, meta, allDefs)
	for _, depID := range def.DependsOnID {
		depDef, ok := allDefs[depID]
		if !ok {
			return nil, fmt.Errorf("dependency %s not found in model", depID)
		}
		if peer, inComponent := ov[depID]; inComponent {
			// A peer's values exist only in this pass. Causal validation
			// guarantees every legitimate read was computed before it.
			e.varsMemo = nil
			e.fetch[depID] = func(_ context.Context, _ string, combo map[string]string) (float64, bool, error) {
				v, ok := peer[dimKey(combo)]
				return v, ok, nil
			}
			continue
		}
		var valueMap map[string]float64
		if depDef.IsInput {
			valueMap, err = s.store.LoadInputValueMap(ctx, modelID, revisionID, depID)
		} else {
			valueMap, err = s.store.LoadCalcValueMap(ctx, modelID, revisionID, depID)
		}
		if err != nil {
			return nil, fmt.Errorf("load values for %s: %w", depDef.Name, err)
		}
		e.fetch[depID] = func(_ context.Context, _ string, combo map[string]string) (float64, bool, error) {
			v, ok := valueMap[dimKey(combo)]
			return v, ok, nil
		}
		if src := env.exactSource(ctx, depDef, valueMap); src != nil {
			e.rows[depID] = src
		}
	}
	e.plain = map[string]bool{}
	plainReferences(node, e.plain)
	if an, anErr := formula.Analyze(def.Formula); anErr == nil {
		e.hasConditional = len(an.DimensionalCalls) > 0
		for _, fn := range an.Calls {
			switch strings.ToUpper(fn) {
			case "YEARVALUE", "HALFYEARVALUE", "QUARTERVALUE", "MONTHVALUE":
				e.hasConditional = true
			}
		}
	}
	// The conditional-aggregation memo is off for a recurrence member
	// (varsMemo == nil): its peers' values appear during the pass, so a
	// memoised read could freeze one before it was computed. The scheduler
	// refuses dimensional functions in a recurrence anyway
	// (executeRecurrence); this is the second guard.
	e.reads = newDimReads(ctx, meta, def, allDefs, metricDimIDs, e.fetch, e.rows, e.varsMemo != nil)
	return e, nil
}

// vars resolves every dependency at combo (which may pin non-leaf members —
// rollup.Resolve aggregates them) and binds dimension names to member codes.
func (e *tsEvaluator) vars(combo map[string]string) (map[string]formula.Value, map[string]float64, error) {
	key := dimKey(combo)
	values, ok := e.varsMemo[key]
	if !ok {
		values = make(map[string]float64, len(e.def.DependsOnID))
		for _, depID := range e.def.DependsOnID {
			depDef := e.allDefs[depID]
			if e.plain != nil && !e.plain[strings.ToUpper(depDef.Name)] {
				continue // read only through LOOKUP / *IFS / *VALUE
			}
			v, _, err := resolveDependency(e.ctx, e.allDims, depDef, e.metricDims[depID], combo, e.fetch[depID], e.rows[depID])
			if err != nil {
				return nil, nil, fmt.Errorf("resolve %s: %w", depDef.Name, err)
			}
			values[depDef.Name] = v
		}
		if e.varsMemo != nil {
			e.varsMemo[key] = values
		}
	}
	vars := make(map[string]formula.Value, len(values)+len(combo))
	for k, v := range values {
		vars[strings.ToUpper(k)] = formula.NumberVal(v)
	}
	for dimID, code := range combo {
		if name, ok := e.dimIDToName[dimID]; ok {
			vars[strings.ToUpper(name)] = formula.StringVal(code)
		}
	}
	return vars, values, nil
}

// evalCtx builds the evaluation context at combo/pos. Its EvalAt re-enters
// here with the time member swapped and everything else unchanged.
func (e *tsEvaluator) evalCtx(combo map[string]string, pos int, parent *formula.TimeEvalContext) (*formula.EvalContext, map[string]float64, error) {
	shifted := make(map[string]string, len(combo))
	for k, v := range combo {
		shifted[k] = v
	}
	shifted[e.axis.dim.ID] = e.axis.periods[pos].Code
	vars, raw, err := e.vars(shifted)
	if err != nil {
		return nil, nil, err
	}
	var tc *formula.TimeEvalContext
	if parent == nil {
		tc = e.axis.timeCtx(pos)
	} else {
		tc = parent.Child(pos)
	}
	nonTime := e.nonTimeCombo(shifted)
	nonTimeKey := dimKey(nonTime)
	tc.EvalAt = func(node formula.Node, position int) formula.Value {
		k := memoKey{node: node, pos: position, nonTime: nonTimeKey}
		if !e.noMemo {
			if v, ok := e.memo[k]; ok {
				return v
			}
		}
		child, _, cerr := e.evalCtx(shifted, position, tc)
		if cerr != nil {
			return formula.ErrorVal(&formula.FormulaError{Code: "#REF!", Message: cerr.Error()})
		}
		v := formula.EvalNode(child, node)
		if !e.noMemo {
			e.memo[k] = v
		}
		return v
	}
	tc.Summarize = func(metric string, positions []int) (float64, bool, *formula.FormulaError) {
		return e.summarize(nonTime, metric, positions)
	}
	tc.Span = e.span
	// The dimensional context of THIS (shifted) cell: dim.property on the
	// time dimension and PARENT of a leaf period see the period evaluated,
	// and a LOOKUP inside LAG reads at the lagged period.
	dim := e.reads.context(shifted, !e.noMemo)
	return &formula.EvalContext{Vars: vars, Time: tc, Dim: dim}, raw, nil
}

// summarize fulfils TimeEvalContext.Summarize (YEARVALUE and its
// siblings): the bare source metric at each given leaf position with the
// non-time coordinates of the cell held, reduced by the source's own
// time_summary — periods with no recorded value skipped, except that an
// average counts them as 0. 'none' is blank.
func (e *tsEvaluator) summarize(nonTime map[string]string, metric string, positions []int) (float64, bool, *formula.FormulaError) {
	depID, ferr := e.reads.source(metric)
	if ferr != nil {
		return 0, false, ferr
	}
	src := e.allDefs[depID]
	srcDims := e.metricDims[depID]
	onAxis := false
	for _, id := range srcDims {
		if id == e.axis.dim.ID {
			onAxis = true
			break
		}
	}
	if !onAxis {
		return 0, false, &formula.FormulaError{Code: formula.CodeTimeDimensionMismatch,
			Message: fmt.Sprintf("%s is not dimensioned by the time dimension, so it has no value per period to summarise", metric)}
	}
	if src.TimeSummary == "none" {
		return 0, false, nil
	}
	// Pins on dimensions unrelated to the source are dropped, as for LOOKUP:
	// a source that does not carry a dimension is never rolled up along it.
	base, err := rollup.NormalizeCombo(e.allDims, srcDims, nonTime, nil)
	if err != nil {
		return 0, false, &formula.FormulaError{Code: formula.ErrRef.Code, Message: fmt.Sprintf("reading %s: %v", metric, err)}
	}
	vals := make([]float64, 0, len(positions))
	periods := 0
	for _, pos := range positions {
		if pos < 0 || pos >= len(e.axis.periods) {
			continue
		}
		periods++
		c := make(map[string]string, len(base)+1)
		for k, v := range base {
			c[k] = v
		}
		c[e.axis.dim.ID] = e.axis.periods[pos].Code
		var v float64
		var ok bool
		var err error
		if exact := e.rows[depID]; exact != nil {
			v, ok, err = resolveDependency(e.ctx, e.allDims, src, srcDims, c, e.fetch[depID], exact)
		} else {
			// At a parent member (EMEA), ResolveTime answers the sum of
			// nothing for a month no leaf recorded; that period is skipped
			// like an empty month at a leaf.
			v, ok, err = rollup.ResolveTimeRecorded(e.ctx, e.allDims, depID, srcDims, rollup.AggRule(src.AggRule),
				rollup.TimeSummaryRule(src.TimeSummary), c, e.fetch[depID])
		}
		if err != nil {
			return 0, false, &formula.FormulaError{Code: formula.ErrRef.Code, Message: fmt.Sprintf("reading %s: %v", metric, err)}
		}
		if !ok {
			continue // no recorded value (an average counts it as 0)
		}
		if v != 0 {
			e.reads.data = true
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return 0, false, nil
	}
	return rollup.CombineTimeOver(vals, periods, rollup.TimeSummaryRule(src.TimeSummary)), true, nil
}

// span fulfils TimeEvalContext.Span (TIMESUM): a period code — leaf or
// aggregate — to the first and last leaf positions it covers.
func (e *tsEvaluator) span(code string) (int, int, bool) {
	positions := e.axis.leafPositions(code)
	if len(positions) == 0 {
		return 0, 0, false
	}
	return positions[0], positions[len(positions)-1], true
}

func (e *tsEvaluator) nonTimeCombo(combo map[string]string) map[string]string {
	out := make(map[string]string, len(combo))
	for k, v := range combo {
		if k != e.axis.dim.ID {
			out[k] = v
		}
	}
	return out
}

// evalAt evaluates the metric at combo with the time dimension pinned to
// period pos. The second return mirrors executePartition's evalOne: true
// alongside an error means every dependency resolved to zero (no data).
func (e *tsEvaluator) evalAt(combo map[string]string, pos int) (float64, bool, error) {
	v, noData, err := e.evalAtOnce(combo, pos)
	if err != nil && noData && e.hasConditional && !e.noMemo && !errors.Is(err, errBlankResult) {
		// A memo hit reads nothing, so "nothing read" may only mean "memo
		// hit": decide no-data from an evaluation that really reads.
		e.noMemo = true
		_, noData, _ = e.evalAtOnce(combo, pos)
		e.noMemo = false
	}
	return v, noData, err
}

// evalAtOnce is one evaluation of evalAt. noData (alongside an error) means
// every plain reference at the cell and every value read through the
// dimensional context or Summarize was zero or absent.
func (e *tsEvaluator) evalAtOnce(combo map[string]string, pos int) (float64, bool, error) {
	e.reads.data = false
	ctx, raw, err := e.evalCtx(combo, pos, nil)
	if err != nil {
		return 0, false, err
	}
	v := e.reads.meta.EncodeResult(e.def.Name, formula.EvalNode(ctx, e.node))
	if v.IsError() {
		// A blank or unknown member is never "no data" (see evalCell).
		noData := len(e.def.DependsOnID) > 0 && !e.reads.data && !formula.IsMemberNotAvailable(v.Err())
		for _, dv := range raw {
			if dv != 0 {
				noData = false
				break
			}
		}
		return 0, noData, v.Err()
	}
	if v.IsBlank() {
		return 0, true, errBlankResult // no value: no row, never a 0
	}
	n, ok := v.Number()
	if !ok {
		return 0, false, formula.ErrValue
	}
	return n, false, nil
}

// evalCombo evaluates a combo that must already pin the time dimension.
func (e *tsEvaluator) evalCombo(combo map[string]string) (float64, bool, error) {
	code, ok := combo[e.axis.dim.ID]
	if !ok {
		return 0, false, fmt.Errorf("%s: no current period at this coordinate", formula.CodeTimeContextRequired)
	}
	if pos, ok := e.axis.index[code]; ok {
		return e.evalAt(e.nonTimeCombo(combo), pos)
	}
	// An aggregate period: the formula per leaf beneath it, reduced by the
	// time summary — a time-relative formula never runs at an aggregate.
	positions := e.axis.leafPositions(code)
	if len(positions) == 0 {
		return 0, false, fmt.Errorf("%s: %q is not a period of the time dimension", formula.CodeInvalidTimeMember, code)
	}
	vals := make([]float64, 0, len(positions))
	for _, pos := range positions {
		v, _, err := e.evalAt(e.nonTimeCombo(combo), pos)
		if errors.Is(err, errBlankResult) {
			continue // a blank period has no value (an average counts it as 0)
		}
		if err != nil {
			return 0, false, err
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return 0, true, errBlankResult
	}
	v, ok := TimeSummaryOver(e.def.TimeSummary, vals, len(positions))
	if !ok {
		return 0, false, fmt.Errorf("time_summary is none: no aggregate over periods")
	}
	return v, false, nil
}

// tsResults is one metric's complete result set for a pass.
type tsResults struct {
	leaf      []CalcResultRow
	aggregate *float64
	rows      []CalcResultRow // per-combo rows: leaves, rollups, slices
	failures  int
	memberNA  int // failures naming a member that does not exist
	skipped   int
	firstErr  error
}

// evalLeaves evaluates every (non-time leaf combo × period), in the period
// order given, recording each value into ov (when non-nil) as it goes so
// same-pass readers see it.
func (e *tsEvaluator) evalLeaves(order []int, ov overlay, res *tsResults) {
	nonTime := e.axis.nonTimeDims(e.metricDims[e.def.ID])
	groups := rollup.LeafCombos(e.allDims, nonTime)
	if len(groups) == 0 {
		groups = []map[string]string{{}}
	}
	for _, pos := range order {
		for _, g := range groups {
			v, noData, err := e.evalAt(g, pos)
			combo := make(map[string]string, len(g)+1)
			for k, val := range g {
				combo[k] = val
			}
			combo[e.axis.dim.ID] = e.axis.periods[pos].Code
			if err != nil {
				if noData {
					res.skipped++
					continue
				}
				res.failures++
				if formula.IsMemberNotAvailable(err) {
					res.memberNA++
				}
				if res.firstErr == nil {
					res.firstErr = err
				}
				e.s.log.Warn().Err(err).Str("metric", e.def.Name).Str("period", e.axis.periods[pos].Code).Msg("per-combo eval failed")
				continue
			}
			res.leaf = append(res.leaf, CalcResultRow{DimMembers: combo, Value: v})
			if ov != nil {
				ov[e.def.ID][dimKey(combo)] = v
			}
		}
	}
}

// finish derives the aggregate, rollup and slice rows from the leaves: for
// any coordinate that leaves time unpinned, non-time dimensions reduce
// first (agg_rule) and time last (time_summary).
func (e *tsEvaluator) finish(res *tsResults) {
	dimIDs := e.metricDims[e.def.ID]
	useEval := e.def.AggRule == string(rollup.AggFormula) || e.def.AggRule == string(rollup.AggRate) ||
		(e.def.AggRule == "average" && !FormulaReferencesDims(e.def.Formula, e.dimIDToName))
	timeID := e.axis.dim.ID

	// periodValue: the metric at one period with the given non-time pins
	// (possibly none), non-time dimensions reduced by agg_rule.
	periodValue := func(pins map[string]string, pos int) (float64, bool) {
		if useEval {
			v, _, err := e.evalAt(pins, pos)
			return v, err == nil
		}
		// Subtree filter per pinned non-time dimension.
		subs := map[string]map[string]bool{}
		for dimID, code := range pins {
			subs[dimID] = subtreeCodes(e.allDims[dimID], code)
		}
		code := e.axis.periods[pos].Code
		var vals []float64
		for _, r := range res.leaf {
			if r.DimMembers[timeID] != code {
				continue
			}
			match := true
			for dimID, sub := range subs {
				if !sub[r.DimMembers[dimID]] {
					match = false
					break
				}
			}
			if match {
				vals = append(vals, r.Value)
			}
		}
		if len(vals) == 0 {
			return 0, false
		}
		return rollup.CombineAgg(vals, rollup.AggRule(e.def.AggRule)), true
	}
	// overTime: time_summary across the given periods of periodValue(pins).
	overTime := func(pins map[string]string, positions []int) (float64, bool) {
		vals := make([]float64, 0, len(positions))
		for _, pos := range positions {
			if v, ok := periodValue(pins, pos); ok {
				vals = append(vals, v)
			}
		}
		if len(vals) == 0 {
			return 0, false
		}
		return TimeSummaryOver(e.def.TimeSummary, vals, len(positions))
	}
	allPositions := make([]int, len(e.axis.periods))
	for i := range allPositions {
		allPositions[i] = i
	}

	res.rows = append(res.rows, res.leaf...)

	// Aggregate '{}' row.
	if v, ok := overTime(map[string]string{}, allPositions); ok {
		res.aggregate = &v
	}

	// Rollup combos (formula/rate): at least one non-leaf member pinned.
	// Time has no hierarchy in Phase 1, so the time member is always a leaf
	// there and the formula evaluates directly.
	if e.def.AggRule == string(rollup.AggFormula) || e.def.AggRule == string(rollup.AggRate) {
		const rollupComboCap = 20000
		for _, combo := range rollup.RollupCombos(e.allDims, dimIDs, rollupComboCap) {
			if v, _, err := e.evalCombo(combo); err == nil {
				res.rows = append(res.rows, CalcResultRow{DimMembers: combo, Value: v})
			}
		}
	}

	// Aggregate-period rows on a single-time-dimension metric ({H1}, {FY26})
	// are what the grid reads for those rows; with more dimensions they are
	// among the slice rows below.
	if len(dimIDs) == 1 {
		formulaRule := e.def.AggRule == string(rollup.AggFormula) || e.def.AggRule == string(rollup.AggRate)
		for _, m := range e.axis.dim.Members {
			if m.IsLeafPeriod() || formulaRule { // formula/rate: written by the RollupCombos pass above
				continue
			}
			if v, ok := overTime(map[string]string{}, e.axis.leafPositions(m.Code)); ok {
				res.rows = append(res.rows, CalcResultRow{DimMembers: map[string]string{timeID: m.Code}, Value: v})
			}
		}
		return
	}
	// One-dimension slice rows, for metrics with two or more dimensions.
	for _, dimID := range dimIDs {
		d := e.allDims[dimID]
		if d == nil {
			continue
		}
		for _, m := range d.Members {
			slice := map[string]string{dimID: m.Code}
			var v float64
			var ok bool
			if dimID == timeID {
				// A leaf period is one position; an aggregate period is its
				// leaves reduced by the time summary.
				v, ok = overTime(map[string]string{}, e.axis.leafPositions(m.Code))
			} else {
				v, ok = overTime(slice, allPositions)
			}
			if ok {
				res.rows = append(res.rows, CalcResultRow{DimMembers: slice, Value: v})
			}
		}
	}
}

// subtreeCodes returns code and every descendant code within dim.
func subtreeCodes(dim *rollup.Dimension, code string) map[string]bool {
	out := map[string]bool{code: true}
	if dim == nil {
		return out
	}
	childrenOf := map[string][]string{}
	for _, m := range dim.Members {
		if m.ParentCode != "" {
			childrenOf[m.ParentCode] = append(childrenOf[m.ParentCode], m.Code)
		}
	}
	queue := []string{code}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		for _, child := range childrenOf[c] {
			if !out[child] {
				out[child] = true
				queue = append(queue, child)
			}
		}
	}
	return out
}

// forwardOrder / backwardOrder are the period orders for the two causal
// directions.
func (a *timeAxis) order(dir metricformula.Direction) []int {
	n := len(a.periods)
	out := make([]int, n)
	for i := range out {
		if dir == metricformula.DirectionBackward {
			out[i] = n - 1 - i
		} else {
			out[i] = i
		}
	}
	return out
}

// executeTimeSeries is executePartition's counterpart for a metric with a
// time dimension: leaves per period, then aggregates with time_summary, then
// the same write sequence executePartition uses.
func (s *Scheduler) executeTimeSeries(
	ctx context.Context, def *MetricDef, modelID, revisionID, partitionKey string, axis *timeAxis,
	allDims map[string]*rollup.Dimension, metricDimIDs map[string][]string, dimIDToName map[string]string, meta *DimMetadata, allDefs map[string]*MetricDef,
) error {
	if len(axis.periods) == 0 {
		return fmt.Errorf("%s: time dimension has no periods", formula.CodeInvalidTimeMember)
	}
	e, err := s.newTSEvaluator(ctx, def, modelID, revisionID, axis, allDims, metricDimIDs, dimIDToName, meta, allDefs, nil)
	if err != nil {
		return err
	}
	var res tsResults
	e.evalLeaves(axis.order(metricformula.DirectionForward), nil, &res)
	if len(res.leaf) == 0 {
		if res.failures == 0 {
			// No data anywhere: every row goes, the '{}' total included.
			return s.store.ClearAllResults(ctx, modelID, revisionID, def.ID)
		}
		if res.memberNA == res.failures {
			// Every cell names a member that does not exist: the last
			// rows were read from that member's data (see executePartition).
			if err := s.store.ClearAllResults(ctx, modelID, revisionID, def.ID); err != nil {
				return fmt.Errorf("clear stale results: %w", err)
			}
		}
		return fmt.Errorf("%d combos failed to evaluate for metric %s (first error: %w)", res.failures, def.Name, res.firstErr)
	}
	e.finish(&res)
	// One transaction, as the scalar path: a reader never sees the metric
	// between the clear of its rows and the write of the new ones.
	if err := s.store.InTx(ctx, func(tx pgx.Tx) error {
		return s.persistTimeSeries(ctx, tx, def, modelID, revisionID, partitionKey, &res)
	}); err != nil {
		return err
	}
	if res.failures > 0 {
		return fmt.Errorf("%d combos failed to evaluate for metric %s (first error: %w)", res.failures, def.Name, res.firstErr)
	}
	return nil
}

// persistTimeSeries replaces one metric's results inside tx: its '{}'
// total (or the clear of a stale one), the clear of its previous per-combo
// rows and the new ones.
func (s *Scheduler) persistTimeSeries(ctx context.Context, tx pgx.Tx, def *MetricDef, modelID, revisionID, partitionKey string, res *tsResults) error {
	if res.aggregate != nil {
		if err := s.store.WriteCalcResultTx(ctx, tx, modelID, revisionID, def.ID, partitionKey, map[string]string{}, *res.aggregate); err != nil {
			return fmt.Errorf("write aggregate: %w", err)
		}
	} else if err := s.store.ClearAggregateResultTx(ctx, tx, modelID, revisionID, def.ID); err != nil {
		// No total this pass (time_summary 'none', no period with a value):
		// an earlier pass's total must not keep answering for it.
		return fmt.Errorf("clear stale aggregate: %w", err)
	}
	if err := s.store.ClearPerComboResultsTx(ctx, tx, modelID, revisionID, def.ID); err != nil {
		return fmt.Errorf("clear stale per-combo results: %w", err)
	}
	if err := s.store.WriteCalcResultsTx(ctx, tx, modelID, revisionID, def.ID, partitionKey, res.rows); err != nil {
		return fmt.Errorf("write per-intersection results: %w", err)
	}
	return nil
}

// executeRecurrence evaluates a causal component (spec §8.2): every member
// shares one time axis; periods run in the component's direction; within a
// period members run in zero-offset order; each value is recorded in the
// overlay the moment it is computed. Results for all members are persisted
// in one transaction.
func (s *Scheduler) executeRecurrence(
	ctx context.Context, comp metricformula.Component, modelID, revisionID, partitionKey string,
	allDims map[string]*rollup.Dimension, metricDimIDs map[string][]string, dimIDToName map[string]string, meta *DimMetadata, allDefs map[string]*MetricDef,
) error {
	var axis *timeAxis
	ov := overlay{}
	evals := make([]*tsEvaluator, 0, len(comp.Members))
	for _, id := range comp.Members {
		def := allDefs[id]
		if def == nil || def.IsInput {
			return fmt.Errorf("recurrence member %s is not a calculated metric", id)
		}
		a, err := timeAxisFor(allDims, metricDimIDs[id])
		if err != nil {
			return err
		}
		if a == nil {
			return fmt.Errorf("%s: %s is part of a dependency cycle but has no time dimension", formula.CodeTimeDimensionRequired, def.Name)
		}
		if axis == nil {
			axis = a
		} else if axis.dim.ID != a.dim.ID {
			return fmt.Errorf("%s: %s and %s are in one recurrence but use different time dimensions",
				formula.CodeTimeDimensionMismatch, allDefs[comp.Members[0]].Name, def.Name)
		}
		// Runtime guard (contract C4; metricformula.Plan refuses this at
		// save): LOOKUP and the conditional aggregations read other cells
		// along a dimension, which a period-by-period recurrence cannot
		// order causally.
		if an, anErr := formula.Analyze(def.Formula); anErr == nil && len(an.DimensionalCalls) > 0 {
			return fmt.Errorf("%s: %s is part of a recurrence (a cycle broken by time) and calls %s, which reads other cells along a dimension; that is not allowed in a recurrence",
				formula.CodeTemporalCycleNotCausal, def.Name, an.DimensionalCalls[0].Func)
		}
		ov[id] = map[string]float64{}
	}
	if len(axis.periods) == 0 {
		return fmt.Errorf("%s: time dimension has no periods", formula.CodeInvalidTimeMember)
	}
	for _, id := range comp.Members {
		e, err := s.newTSEvaluator(ctx, allDefs[id], modelID, revisionID, axis, allDims, metricDimIDs, dimIDToName, meta, allDefs, ov)
		if err != nil {
			return err
		}
		evals = append(evals, e)
	}

	results := make([]tsResults, len(evals))
	for _, pos := range axis.order(comp.Direction) {
		for i, e := range evals {
			e.evalLeaves([]int{pos}, ov, &results[i])
		}
	}
	for i, e := range evals {
		if results[i].failures > 0 {
			return fmt.Errorf("%d combos failed to evaluate for metric %s (first error: %w)", results[i].failures, e.def.Name, results[i].firstErr)
		}
		e.finish(&results[i])
	}

	tx, err := s.store.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	for i, e := range evals {
		if len(results[i].leaf) == 0 {
			if err := s.store.ClearAllResultsTx(ctx, tx, modelID, revisionID, e.def.ID); err != nil {
				return err
			}
			continue
		}
		if err := s.persistTimeSeries(ctx, tx, e.def, modelID, revisionID, partitionKey, &results[i]); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
