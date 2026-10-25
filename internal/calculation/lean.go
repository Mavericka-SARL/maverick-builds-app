package calculation

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// A metric with very many cells is calculated on a lean path. The ordinary
// path holds every leaf as a map of its members — about 500 bytes a cell,
// 2.4 GB for a tester's two metrics of 2.5 million cells — and writes the
// set as one statement. Here a leaf is its member indices and its value
// (20 bytes over three dimensions), each cell's members are built only to
// evaluate and to write it, the rows stream into COPY, and the pass cache
// does not keep the set (a metric reading it loads it). The totals and
// slices are reduced from the same leaves in the same order (leafRows), so
// the numbers are the ordinary path's.

// leanCellsAt is the number of cells from which a metric takes the lean
// path, its values are not kept in the pass cache, and a component holding
// or reading it runs alone (largeComponent).
var leanCellsAt int64 = 200_000

// largeComponent reports the components of a pass that hold a metric of
// leanCellsAt cells or more, or read one: their values are loaded whole
// for the evaluation, so they run one at a time (runComponents).
func largeComponent(defs map[string]*MetricDef, allDims map[string]*rollup.Dimension, metricDimIDs map[string][]string) func(metricformula.Component) bool {
	large := func(id string) bool {
		return id != "" && len(metricDimIDs[id]) > 0 && rollup.LeafComboCount(allDims, metricDimIDs[id]) >= leanCellsAt
	}
	return func(comp metricformula.Component) bool {
		for _, id := range comp.Members {
			def := defs[id]
			if def == nil || def.IsInput {
				continue
			}
			if large(id) || large(def.AggNumeratorID) || large(def.AggDenominatorID) {
				return true
			}
			for _, dep := range def.DependsOnID {
				if large(dep) {
					return true
				}
			}
		}
		return false
	}
}

// compactRows is a metric's leaves as member indices and values, in the
// order rollup.LeafCombos enumerates them: leafRows to the reductions,
// resultSource to the write.
type compactRows struct {
	dimIDs []string
	pos    map[string]int
	codes  [][]string // per dimension, its leaf codes
	idx    []uint32   // len(dimIDs) per leaf
	vals   []float64
}

func newCompactRows(allDims map[string]*rollup.Dimension, dimIDs []string) *compactRows {
	c := &compactRows{dimIDs: dimIDs, pos: make(map[string]int, len(dimIDs)), codes: make([][]string, len(dimIDs))}
	for p, id := range dimIDs {
		c.pos[id] = p
		c.codes[p] = rollup.LeafCodes(allDims[id])
	}
	return c
}

// each calls fn with every cell, first dimension outermost and last
// fastest (LeafCombos's order): its members, fresh for each cell, and its
// member indices, valid only during the call.
func (c *compactRows) each(fn func(combo map[string]string, at []int)) {
	nd := len(c.dimIDs)
	for _, codes := range c.codes {
		if len(codes) == 0 {
			return
		}
	}
	at := make([]int, nd)
	for {
		combo := make(map[string]string, nd)
		for p, id := range c.dimIDs {
			combo[id] = c.codes[p][at[p]]
		}
		fn(combo, at)
		p := nd - 1
		for ; p >= 0; p-- {
			if at[p]++; at[p] < len(c.codes[p]) {
				break
			}
			at[p] = 0
		}
		if p < 0 {
			return
		}
	}
}

func (c *compactRows) add(at []int, v float64) {
	for _, i := range at {
		c.idx = append(c.idx, uint32(i))
	}
	c.vals = append(c.vals, v)
}

func (c *compactRows) Len() int            { return len(c.vals) }
func (c *compactRows) Value(i int) float64 { return c.vals[i] }

func (c *compactRows) Member(i int, dimID string) (string, bool) {
	p, ok := c.pos[dimID]
	if !ok {
		return "", false
	}
	return c.codes[p][c.idx[i*len(c.dimIDs)+p]], true
}

func (c *compactRows) Row(i int) (map[string]string, float64, *string) {
	nd := len(c.dimIDs)
	dims := make(map[string]string, nd)
	for p, id := range c.dimIDs {
		dims[id] = c.codes[p][c.idx[i*nd+p]]
	}
	return dims, c.vals[i], nil
}

// executeLean is executePartition's evaluation, reduction and write for a
// metric of leanCellsAt cells or more, evaluated whole (no increment).
func (s *Scheduler) executeLean(ctx context.Context, def *MetricDef, modelID, revisionID string,
	allDims map[string]*rollup.Dimension, metricDimIDs map[string][]string, dimIDToName map[string]string, allDefs map[string]*MetricDef,
	axis *timeAxis, evalOne cellEval,
) error {
	dimIDs := metricDimIDs[def.ID]
	rule := EffectiveAggRule(def.AggRule, def.Formula, dimIDToName)
	cells := rollup.LeafComboCount(allDims, dimIDs)

	leaves := newCompactRows(allDims, dimIDs)
	var failures, skipped, memberNA int
	var firstErr error
	leaves.each(func(combo map[string]string, at []int) {
		v, noData, err := evalOne(combo)
		if err != nil {
			// As executePartition: a failure where every value read is zero
			// is no data here; any other is the formula's.
			if noData {
				skipped++
				return
			}
			failures++
			if formula.IsMemberNotAvailable(err) {
				memberNA++
			}
			if firstErr == nil {
				firstErr = err
			}
			s.log.Warn().Err(err).Str("metric", def.Name).Msg("per-combo eval failed")
			return
		}
		leaves.add(at, v)
	})

	// Rollup rows of the rules the client cannot combine, as
	// executePartition writes them (none past the cap — a metric this
	// large is past it).
	var extra []CalcResultRow
	if rule == string(rollup.AggFormula) || rule == string(rollup.AggRate) {
		const rollupComboCap = 20000
		for _, combo := range rollup.RollupCombos(allDims, dimIDs, rollupComboCap) {
			if v, _, err := evalOne(combo); err == nil {
				extra = append(extra, CalcResultRow{DimMembers: combo, Value: v})
			}
		}
	}

	if leaves.Len() == 0 && len(extra) == 0 {
		if failures == 0 || memberNA == failures {
			if err := s.store.ClearAllResults(ctx, modelID, revisionID, def.ID); err != nil {
				return fmt.Errorf("clear stale results: %w", err)
			}
			s.remember(ctx, def.ID, nil)
			if failures == 0 {
				return nil
			}
		}
		return fmt.Errorf("%d/%d combos failed to evaluate for metric %s (first error: %w)", failures, cells, def.Name, firstErr)
	}

	vals := make([]float64, 0, leaves.Len()+len(extra))
	vals = append(vals, leaves.vals...)
	for _, r := range extra {
		vals = append(vals, r.Value)
	}
	aggregate := rollup.CombineAgg(vals, rollup.AggRule(rule))
	writeAggregate := true
	if axis != nil && rule != string(rollup.AggFormula) && rule != string(rollup.AggRate) {
		aggregate, writeAggregate = summarizeOverTime(leaves, axis, rule, def.TimeSummary, nil)
	}
	if rule == string(rollup.AggFormula) {
		total, _, totalErr := evalOne(map[string]string{})
		switch {
		case errors.Is(totalErr, errBlankResult):
			writeAggregate = false
		case totalErr != nil:
			return fmt.Errorf("total-level evaluation failed for metric %s (agg_rule=formula): %w", def.Name, totalErr)
		default:
			aggregate = total
		}
	}
	if def.AggRule == string(rollup.AggRate) {
		total, rateErr := s.rateTotal(ctx, def, modelID, revisionID, allDims, metricDimIDs, allDefs)
		if rateErr != nil {
			return fmt.Errorf("ratio total failed for metric %s (agg_rule=rate): %w", def.Name, rateErr)
		}
		aggregate = total
	}
	if !rollup.Aggregates(rollup.AggRule(def.AggRule)) && !rollup.LeafCombo(allDims, dimIDs, map[string]string{}) {
		writeAggregate = false
	}

	tail := append([]CalcResultRow{}, extra...)
	dimConditional := FormulaReferencesDims(def.Formula, dimIDToName)
	tail = append(tail, oneDimSliceRows(rule, dimConditional, dimIDs, allDims, leaves, evalOne, axis, def.TimeSummary)...)
	tail = append(tail, aggregatePeriodRows(rule, dimIDs, leaves, axis, def.TimeSummary)...)
	if writeAggregate {
		tail = append(tail, CalcResultRow{DimMembers: map[string]string{}, Value: aggregate})
	}
	if err := s.store.InTx(ctx, func(tx pgx.Tx) error {
		return s.store.ReplaceResultSourceTx(ctx, tx, modelID, revisionID, def.ID, sources{leaves, rowsSource(tail)})
	}); err != nil {
		s.forget(ctx, def.ID)
		return fmt.Errorf("write results: %w", err)
	}
	// Not kept for the pass: a metric reading it loads it from the
	// database, once.
	s.forget(ctx, def.ID)

	if failures > 0 {
		return fmt.Errorf("%d/%d combos failed to evaluate for metric %s (first error: %w)", failures, cells-int64(skipped), def.Name, firstErr)
	}
	return nil
}
