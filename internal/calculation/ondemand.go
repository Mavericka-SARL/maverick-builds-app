package calculation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// ── Reading a formula/rate source above its leaves ──────────────────────────
//
// A calculated metric whose agg_rule is formula or rate has no value above
// its leaves that a combination of children can reproduce: its value at a
// parent member, a slice or any other partial combination is its formula
// evaluated there. The scheduler persists some of those combinations
// (rollup combos, one-dimension slices, the total), but not every
// combination another metric can ask for: a LOOKUP from a cell with fewer
// dimensions than the source pins only some of them, a pin on a related
// dimension has no row of its own, and past the rollup-combo cap no rollup
// rows exist at all. Such a read evaluates the source's formula at the
// coordinates on demand — exactly what the scheduler does for the rows it
// persists — and never falls back to rollup's mean of children, a number
// nobody computed.

// exactSource reads one formula/rate calculated dependency: its persisted
// rows first, then the formula evaluated on demand.
type exactSource struct {
	dims map[string]*rollup.Dimension
	rows map[string]float64
	// eval evaluates the source's formula at a combo (see cellEvaluator);
	// built lazily, on the first read that needs it.
	eval func() (cellEval, error)
	memo map[string]onDemandResult
}

type onDemandResult struct {
	v   float64
	ok  bool
	err error
}

// cellEval evaluates a metric's formula at one combo: value, whether the
// failure (if any) is "no data at this intersection", error.
type cellEval func(combo map[string]string) (float64, bool, error)

// at reads the source at norm, a combo already normalised onto the source
// (rollup.NormalizeCombo). handled=false leaves the read to the caller's
// plain path: a nil source, or a combo pinning every one of the source's
// dimensions at a leaf member, where a missing row simply means no value.
func (src *exactSource) at(srcDims []string, norm map[string]string) (v float64, found, handled bool, err error) {
	if src == nil {
		return 0, false, false, nil
	}
	ownOnly := true
	for id := range norm {
		own := false
		for _, s := range srcDims {
			if s == id {
				own = true
				break
			}
		}
		if !own {
			ownOnly = false
			break
		}
	}
	if ownOnly {
		if v, ok := src.rows[dimKey(norm)]; ok {
			return v, true, true, nil
		}
		if src.allLeaves(srcDims, norm) {
			return 0, false, false, nil
		}
	}
	key := dimKey(norm)
	if r, ok := src.memo[key]; ok {
		return r.v, r.ok, true, r.err
	}
	var r onDemandResult
	evalOne, buildErr := src.eval()
	if buildErr != nil {
		r.err = buildErr
	} else {
		val, noData, evalErr := evalOne(norm)
		switch {
		case evalErr == nil:
			r.v, r.ok = val, true
		case errors.Is(evalErr, errBlankResult), noData:
			// no value there: reads like an intersection with nothing recorded
		default:
			r.err = evalErr
		}
	}
	src.memo[key] = r
	return r.v, r.ok, true, r.err
}

// allLeaves reports whether norm pins every one of srcDims at a leaf member.
func (src *exactSource) allLeaves(srcDims []string, norm map[string]string) bool {
	for _, id := range srcDims {
		code, pinned := norm[id]
		if !pinned {
			return false
		}
		leaves := rollup.LeafDescendants(src.dims[id], code)
		if len(leaves) != 1 || leaves[0] != code {
			return false
		}
	}
	return true
}

// evalEnv is what evaluating any metric of one revision needs during one
// partition's pass, shared by the metric being computed and every source
// it evaluates on demand.
type evalEnv struct {
	s            *Scheduler
	modelID      string
	revisionID   string
	allDims      map[string]*rollup.Dimension
	metricDimIDs map[string][]string
	dimIDToName  map[string]string
	meta         *DimMetadata
	allDefs      map[string]*MetricDef
	// sources caches one exactSource per formula/rate dependency for the
	// pass, so a source evaluated on demand is built and memoised once.
	sources map[string]*exactSource
	// building guards against a (validation-refused) cycle.
	building map[string]bool
}

func newEvalEnv(s *Scheduler, modelID, revisionID string, allDims map[string]*rollup.Dimension,
	metricDimIDs map[string][]string, dimIDToName map[string]string, meta *DimMetadata, allDefs map[string]*MetricDef,
) *evalEnv {
	return &evalEnv{
		s: s, modelID: modelID, revisionID: revisionID, allDims: allDims, metricDimIDs: metricDimIDs,
		dimIDToName: dimIDToName, meta: meta, allDefs: allDefs,
		sources: map[string]*exactSource{}, building: map[string]bool{},
	}
}

// exactSource returns the reader of dependency dep over its persisted
// values, or nil when dep is not a formula/rate calculated metric (its
// rows above the leaves combine the leaves, which rollup.ResolveTime
// reproduces).
func (env *evalEnv) exactSource(ctx context.Context, dep *MetricDef, valueMap map[string]float64) *exactSource {
	if exactRows(dep, valueMap) == nil {
		return nil
	}
	if src, ok := env.sources[dep.ID]; ok {
		return src
	}
	var built cellEval
	var buildErr error
	done := false
	src := &exactSource{
		dims: env.allDims,
		rows: valueMap,
		memo: map[string]onDemandResult{},
	}
	src.eval = func() (cellEval, error) {
		if done {
			return built, buildErr
		}
		if env.building[dep.ID] {
			return nil, fmt.Errorf("%s depends on itself", dep.Name)
		}
		env.building[dep.ID] = true
		defer delete(env.building, dep.ID)
		axis, err := timeAxisFor(env.allDims, env.metricDimIDs[dep.ID])
		switch {
		case err != nil:
			buildErr = err
		case axis != nil && usesTimeSeries(dep.Formula):
			buildErr = fmt.Errorf("%s (agg_rule %s) has no computed value at these coordinates, and a time-series formula is not evaluated on demand", dep.Name, dep.AggRule)
		default:
			built, buildErr = env.cellEvaluator(ctx, dep)
		}
		done = true
		return built, buildErr
	}
	env.sources[dep.ID] = src
	return src
}

// loadDependencies prefetches each dependency's recorded values once.
func (env *evalEnv) loadDependencies(ctx context.Context, def *MetricDef) (map[string]rollup.RawValue, map[string]*exactSource, error) {
	fetch := make(map[string]rollup.RawValue, len(def.DependsOnID))
	sources := make(map[string]*exactSource, len(def.DependsOnID))
	for _, depID := range def.DependsOnID {
		depDef, ok := env.allDefs[depID]
		if !ok {
			return nil, nil, fmt.Errorf("dependency %s not found in model", depID)
		}
		valueMap, err := env.s.valueMap(ctx, depDef, env.modelID, env.revisionID)
		if err != nil {
			return nil, nil, fmt.Errorf("load values for %s: %w", depDef.Name, err)
		}
		fetch[depID] = func(_ context.Context, _ string, combo map[string]string) (float64, bool, error) {
			v, ok := valueMap[dimKey(combo)]
			return v, ok, nil
		}
		if src := env.exactSource(ctx, depDef, valueMap); src != nil {
			sources[depID] = src
		}
	}
	return fetch, sources, nil
}

// cellEvaluator returns def's scalar (non-time-series) evaluation at one
// combo — the evaluation executePartition runs at every leaf, rollup combo,
// slice and total.
func (env *evalEnv) cellEvaluator(ctx context.Context, def *MetricDef) (cellEval, error) {
	evalValue, err := env.valueEvaluator(ctx, def)
	if err != nil {
		return nil, err
	}
	return func(combo map[string]string) (float64, bool, error) {
		v, noData, err := evalValue(combo)
		if err != nil {
			return 0, noData, err
		}
		n, ok := v.Number()
		if !ok {
			return 0, noData, formula.ErrValue
		}
		return n, false, nil
	}, nil
}

// textEvaluator is cellEvaluator for a text calculation: the formula's
// result as text (a number as the spreadsheet writes it). "" is no text.
func (env *evalEnv) textEvaluator(ctx context.Context, def *MetricDef) (func(combo map[string]string) (string, bool, error), error) {
	evalValue, err := env.valueEvaluator(ctx, def)
	if err != nil {
		return nil, err
	}
	return func(combo map[string]string) (string, bool, error) {
		v, noData, err := evalValue(combo)
		if err != nil {
			return "", noData, err
		}
		return v.String(), false, nil
	}, nil
}

// valueEvaluator evaluates def's formula at one combo and gives its value as
// the formula left it; the bool reports a failure where nothing was read.
func (env *evalEnv) valueEvaluator(ctx context.Context, def *MetricDef) (func(combo map[string]string) (formula.Value, bool, error), error) {
	allDims, allDefs, metricDimIDs, dimIDToName := env.allDims, env.allDefs, env.metricDimIDs, env.dimIDToName

	// Bulk-prefetch each dependency's recorded values ONCE (not once per
	// leaf combo) — this is what makes full leaf-combo enumeration viable
	// instead of the old fact-driven "only combos that already have data"
	// shortcut. RawValue's ok=false ⇒ value=0 contract (satisfied naturally
	// here: a map miss returns the zero value) is what lets rollup.Resolve's
	// aggregation tiers sum/average/count a genuinely-missing dependency as
	// 0 without any special-casing in this function.
	fetch, rows, err := env.loadDependencies(ctx, def)
	if err != nil {
		return nil, err
	}

	// The formula is parsed ONCE per metric and its AST evaluated per
	// combo (the conditional-aggregation memo is keyed on it). A parse
	// error still surfaces per combo, exactly as re-parsing did.
	node, parseErr := formula.Parse(def.Formula)
	plain := map[string]bool{}
	hasConditional := false
	if parseErr == nil {
		plainReferences(node, plain)
		if an, anErr := formula.Analyze(def.Formula); anErr == nil {
			for _, call := range an.DimensionalCalls {
				if call.Func != "LOOKUP" {
					hasConditional = true
				}
			}
		}
	}
	reads := newDimReads(ctx, env.meta, def, allDefs, metricDimIDs, fetch, rows, true)

	// A text calculation reads a text metric by a plain reference
	// (wa_employee_id & "|" & wa_type): an input's note or a calculation's
	// text at the cell's own members of that metric's dimensions. Validation
	// allows it nowhere else.
	texts := map[string]map[string]string{}
	for _, depID := range def.DependsOnID {
		if dep := allDefs[depID]; def.Text && dep.Text {
			m, err := env.s.textMap(ctx, dep, env.modelID, env.revisionID)
			if err != nil {
				return nil, fmt.Errorf("load the text of %s: %w", dep.Name, err)
			}
			texts[depID] = m
		}
	}
	noteAt := func(depID string, combo map[string]string) string {
		own := make(map[string]string, len(metricDimIDs[depID]))
		for _, dimID := range metricDimIDs[depID] {
			code, ok := combo[dimID]
			if !ok {
				return ""
			}
			own[dimID] = code
		}
		return texts[depID][dimKey(own)]
	}

	// evalCell evaluates the formula at combo. Its second return is only
	// meaningful alongside an error: true means the formula failed while
	// EVERY value it read — its plain references at this combo and every
	// value LOOKUP or a conditional aggregation read through the
	// dimensional context — was zero or absent: the signature of an
	// intersection that simply has no data (absence resolves to 0, and calc
	// dependencies persist literal 0 rows at empty intersections, so
	// absence cannot be told from zero here and doesn't need to be). The
	// caller skips such combos instead of recording them as calculation
	// failures.
	// bind resolves the formula's variables at combo: its plain references,
	// the text of text metrics it reads, and the cell's own member codes.
	bind := func(combo map[string]string) (map[string]formula.Value, map[string]float64, error) {
		values := make(map[string]float64, len(def.DependsOnID))
		notes := map[string]string{}
		for _, depID := range def.DependsOnID {
			depDef := allDefs[depID]
			if parseErr == nil && !plain[strings.ToUpper(depDef.Name)] {
				continue // read only through LOOKUP / *IFS / *VALUE: never bound as a value
			}
			if _, ok := texts[depID]; ok {
				notes[depDef.Name] = noteAt(depID, combo)
				continue
			}
			v, _, err := resolveDependency(ctx, allDims, depDef, metricDimIDs[depID], combo, fetch[depID], rows[depID])
			if err != nil {
				return nil, nil, fmt.Errorf("resolve %s: %w", depDef.Name, err)
			}
			values[depDef.Name] = v // bind unconditionally: preserves "0 on miss" formula-variable semantics
		}
		// Upper-cased keys, as EvalWithContext does: EvalNode does not.
		vars := make(map[string]formula.Value, len(values)+len(combo))
		for name, v := range values {
			vars[strings.ToUpper(name)] = formula.NumberVal(v)
		}
		for name, note := range notes {
			vars[strings.ToUpper(name)] = formula.StringVal(note)
		}
		// Translate stored {dim_id: member_code} into named vars for the formula.
		for dimID, memberCode := range combo {
			if name, ok := dimIDToName[dimID]; ok {
				vars[strings.ToUpper(name)] = formula.StringVal(memberCode)
			}
		}
		return vars, values, nil
	}

	// A text calculation on a time dimension is evaluated at its period
	// (START(), END(), PREVIOUS of a number it reads), as the time-series
	// path evaluates a number; another period's value is the formula's
	// variables bound at that period. Number calculations with time
	// functions never reach here (executeTimeSeries).
	var axis *timeAxis
	if def.Text {
		axis, _ = timeAxisFor(allDims, metricDimIDs[def.ID])
	}
	var timeAt func(combo map[string]string, parent *formula.TimeEvalContext, useMemo bool) *formula.TimeEvalContext
	timeAt = func(combo map[string]string, parent *formula.TimeEvalContext, useMemo bool) *formula.TimeEvalContext {
		if axis == nil {
			return nil
		}
		pos, ok := axis.index[combo[axis.dim.ID]]
		if !ok {
			return nil // an aggregate period: no current period
		}
		tc := axis.timeCtx(pos)
		if parent != nil {
			tc = parent.Child(pos)
		}
		tc.EvalAt = func(n formula.Node, at int) formula.Value {
			if !tc.InRange(at) {
				return formula.BlankVal()
			}
			shifted := make(map[string]string, len(combo))
			for k, v := range combo {
				shifted[k] = v
			}
			shifted[axis.dim.ID] = axis.periods[at].Code
			vars, _, err := bind(shifted)
			if err != nil {
				return formula.ErrorVal(&formula.FormulaError{Code: "#REF!", Message: err.Error()})
			}
			return formula.EvalNode(&formula.EvalContext{Vars: vars, Dim: reads.context(shifted, useMemo), Time: timeAt(shifted, tc, useMemo)}, n)
		}
		tc.Span = axis.span
		return tc
	}

	evalCell := func(combo map[string]string, useMemo bool) (formula.Value, bool, error) {
		reads.data = false
		vars, values, bindErr := bind(combo)
		if bindErr != nil {
			return formula.Value{}, false, bindErr
		}
		noData := func() bool {
			if len(def.DependsOnID) == 0 || reads.data {
				return false
			}
			for _, dv := range values {
				if dv != 0 {
					return false
				}
			}
			return true
		}
		if parseErr != nil {
			return formula.Value{}, noData(), parseErr
		}
		result := reads.meta.EncodeResult(def.Name, formula.EvalNode(&formula.EvalContext{Vars: vars, Dim: reads.context(combo, useMemo), Time: timeAt(combo, nil, useMemo)}, node))
		if result.IsError() {
			// A blank or unknown member is a configuration problem at this
			// cell, never an absence of data — even when nothing was read
			// before the member check failed (a LOOKUP-only formula).
			return formula.Value{}, noData() && !formula.IsMemberNotAvailable(result.Err()), result.Err()
		}
		if result.IsBlank() {
			// Blank (YEARVALUE of a time_summary 'none' source, a bare
			// unset property): no value, so no row — never a persisted 0.
			return formula.Value{}, true, errBlankResult
		}
		if _, ok := result.Number(); !ok && !def.Text {
			return formula.Value{}, noData(), formula.ErrValue
		}
		return result, false, nil
	}
	return func(combo map[string]string) (formula.Value, bool, error) {
		v, noData, err := evalCell(combo, true)
		if err != nil && noData && hasConditional && !errors.Is(err, errBlankResult) {
			// A memoised conditional aggregation answers without reading
			// anything, so "nothing read" may only mean "memo hit". Decide
			// no-data from an evaluation that really reads.
			_, noData, _ = evalCell(combo, false)
		}
		return v, noData, err
	}, nil
}
