package calculation

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// increment is one metric's part in a pass that follows written cells
// (region.go): in is the region of its cells to evaluate (unbounded:
// every cell), out the region that changed (nil: nothing), old and oldText
// the values and texts it held. A nil *increment is an ordinary pass:
// every cell evaluated, the whole set replaced.
type increment struct {
	in      *region
	out     *region
	old     map[string]float64
	oldText map[string]string
}

// partial reports whether only the cells in inc.in are evaluated.
func (inc *increment) partial() bool {
	return inc != nil && inc.in != nil && !inc.in.unbounded()
}

// incremental is a pass's bookkeeping of what changed.
type incremental struct {
	on           bool
	req          passRequest
	requested    map[string]bool
	status       map[string]string // metric ID → its partition's status before the pass
	allDims      map[string]*rollup.Dimension
	metricDimIDs map[string][]string
	dimIDToName  map[string]string
	defs         map[string]*MetricDef

	mu      sync.Mutex
	changed map[string]*region // metric ID → what changed in this pass
	shapes  map[string]readShape
}

// newIncremental starts the bookkeeping of a pass. A pass that knows no
// written cells, or cannot read the partitions' state, runs as an
// ordinary pass.
func (s *Scheduler) newIncremental(ctx context.Context, modelID, revisionID, timePartition string, req passRequest,
	allDims map[string]*rollup.Dimension, metricDimIDs map[string][]string, dimIDToName map[string]string, defs map[string]*MetricDef,
) *incremental {
	inc := &incremental{req: req, allDims: allDims, metricDimIDs: metricDimIDs, dimIDToName: dimIDToName, defs: defs,
		changed: map[string]*region{}, shapes: map[string]readShape{}, requested: map[string]bool{}}
	if len(req.cells) == 0 {
		return inc
	}
	status, err := s.store.LoadPartitionStatuses(ctx, modelID, revisionID, timePartition)
	if err != nil {
		s.log.Warn().Err(err).Msg("load partition states: recalculating every cell")
		return inc
	}
	inc.on, inc.status = true, status
	for _, id := range req.ids {
		inc.requested[id] = true
	}
	for _, c := range req.cells {
		inc.changed[c.MetricID] = inc.changed[c.MetricID].union(cellRegion(allDims, metricDimIDs[c.MetricID], c.Combo))
	}
	return inc
}

// plan decides whether comp runs and, in a pass following cells, each
// member's increment. A component runs whole when it is forced (asked for
// without cells), was not calculated cleanly before (never, or failing), or
// is a recurrence, a text or a time-series calculation (evaluated whole,
// written as a difference). It does not run when nothing it reads changed:
// a prerequisite Plan pulled in, or a metric the written cells reach only
// through values that came out the same.
func (inc *incremental) plan(comp metricformula.Component) (map[string]*increment, bool) {
	if !inc.on {
		return nil, true
	}
	inPass, forced := false, false
	members := map[string]bool{}
	for _, id := range comp.Members {
		members[id] = true
		d := inc.defs[id]
		if d == nil || d.IsInput {
			continue
		}
		if inc.requested[id] {
			inPass = true
		}
		if (inc.requested[id] && inc.req.forcedID(id)) || inc.status[id] != "clean" {
			forced = true
		}
	}
	if !inPass && !forced {
		return nil, false
	}
	var in *region
	if forced {
		in = everywhere()
	} else {
		inc.mu.Lock()
		for _, id := range comp.Members {
			def := inc.defs[id]
			if def == nil || def.IsInput {
				continue
			}
			shape, ok := inc.shapes[id]
			if !ok {
				shape = shapeOf(def, inc.metricDimIDs[id], inc.allDims, inc.dimIDToName)
				inc.shapes[id] = shape
			}
			for _, depID := range dependencyIDs(def) {
				if members[depID] {
					continue
				}
				dep := inc.defs[depID]
				if dep == nil {
					continue
				}
				changed := inc.changed[depID]
				if changed == nil {
					continue
				}
				rule := EffectiveAggRule(dep.AggRule, dep.Formula, inc.dimIDToName)
				readerRule := EffectiveAggRule(def.AggRule, def.Formula, inc.dimIDToName)
				in = in.union(project(changed, dep, inc.metricDimIDs[depID], rule, shape, inc.metricDimIDs[id], readerRule))
			}
		}
		inc.mu.Unlock()
		if in == nil {
			return nil, false
		}
		if comp.Recurrence {
			in = everywhere()
		}
	}
	incs := make(map[string]*increment, len(comp.Members))
	for _, id := range comp.Members {
		incs[id] = &increment{in: in}
	}
	return incs, true
}

// done records what comp's members changed.
func (inc *incremental) done(comp metricformula.Component, incs map[string]*increment) {
	if !inc.on {
		return
	}
	inc.mu.Lock()
	defer inc.mu.Unlock()
	for _, id := range comp.Members {
		if i := incs[id]; i != nil {
			inc.changed[id] = i.out
		}
	}
}

// dependencyIDs are the metrics def reads: its formula's references and a
// ratio's numerator and denominator.
func dependencyIDs(def *MetricDef) []string {
	ids := append([]string{}, def.DependsOnID...)
	if def.AggNumeratorID != "" {
		ids = append(ids, def.AggNumeratorID)
	}
	if def.AggDenominatorID != "" {
		ids = append(ids, def.AggDenominatorID)
	}
	return ids
}

// loadHeld reads the values and texts def holds before inc's pass writes it.
func (s *Scheduler) loadHeld(ctx context.Context, def *MetricDef, modelID, revisionID string, inc *increment) error {
	old, err := s.valueMap(ctx, def, modelID, revisionID)
	if err != nil {
		return fmt.Errorf("load the values %s holds: %w", def.Name, err)
	}
	inc.old, inc.oldText = old, map[string]string{}
	if def.Text {
		if inc.oldText, err = s.textMap(ctx, def, modelID, revisionID); err != nil {
			return fmt.Errorf("load the texts %s holds: %w", def.Name, err)
		}
	}
	return nil
}

// cleared records in inc that def's results were all deleted.
func (inc *increment) cleared(dimIDs []string, allDims map[string]*rollup.Dimension) {
	if inc != nil && inc.old != nil {
		inc.out = diffResults(nil, inc.old, inc.oldText, dimIDs, allDims).changed
	}
}

// persistRows replaces def's result set with rows in a transaction of its
// own: in an ordinary pass the whole set (ReplaceResultsTx), in a pass
// following cells only the difference from what it held, which inc
// records. A difference of nothing writes nothing.
func (s *Scheduler) persistRows(ctx context.Context, def *MetricDef, modelID, revisionID string, rows []CalcResultRow,
	inc *increment, dimIDs []string, allDims map[string]*rollup.Dimension,
) error {
	if inc == nil || inc.old == nil {
		return s.store.InTx(ctx, func(tx pgx.Tx) error {
			if err := s.store.ReplaceResultsTx(ctx, tx, modelID, revisionID, def.ID, rows); err != nil {
				return fmt.Errorf("write results: %w", err)
			}
			return nil
		})
	}
	diff := diffResults(rows, inc.old, inc.oldText, dimIDs, allDims)
	if len(diff.del) == 0 && len(diff.ins) == 0 {
		inc.out = nil
		return nil
	}
	return s.store.InTx(ctx, func(tx pgx.Tx) error {
		return s.writeDiffTx(ctx, tx, def, modelID, revisionID, rows, diff, inc)
	})
}

// writeRowsTx is persistRows inside tx (a recurrence writes its members in
// one transaction).
func (s *Scheduler) writeRowsTx(ctx context.Context, tx pgx.Tx, def *MetricDef, modelID, revisionID string, rows []CalcResultRow,
	inc *increment, dimIDs []string, allDims map[string]*rollup.Dimension,
) error {
	if inc == nil || inc.old == nil {
		if err := s.store.ReplaceResultsTx(ctx, tx, modelID, revisionID, def.ID, rows); err != nil {
			return fmt.Errorf("write results: %w", err)
		}
		return nil
	}
	diff := diffResults(rows, inc.old, inc.oldText, dimIDs, allDims)
	if len(diff.del) == 0 && len(diff.ins) == 0 {
		inc.out = nil
		return nil
	}
	return s.writeDiffTx(ctx, tx, def, modelID, revisionID, rows, diff, inc)
}

// writeDiffTx writes diff. When the metric then holds another number of
// rows than rows — the table held what the pass did not read: a
// duplicate, a row written meanwhile — the whole set is replaced instead,
// and its change is unbounded.
func (s *Scheduler) writeDiffTx(ctx context.Context, tx pgx.Tx, def *MetricDef, modelID, revisionID string, rows []CalcResultRow, diff resultDiff, inc *increment) error {
	inc.out = diff.changed
	ok, err := s.store.ApplyResultsDiffTx(ctx, tx, modelID, revisionID, def.ID, diff.del, diff.ins, len(rows))
	if err != nil {
		return fmt.Errorf("write results: %w", err)
	}
	if !ok {
		s.log.Warn().Str("metric", def.Name).Msg("results differed from what the pass read: replacing them whole")
		inc.out = everywhere()
		if err := s.store.ReplaceResultsTx(ctx, tx, modelID, revisionID, def.ID, rows); err != nil {
			return fmt.Errorf("write results: %w", err)
		}
	}
	return nil
}

// prefetchHeld reads, in dependency order on a goroutine of its own, the
// values each metric of a pass following cells holds, so a metric finds
// them in the pass cache instead of reading them when its turn comes: the
// education model's largest metric spent 60 ms of its turn reading its
// 19,500 rows (2026-10-20). A metric that writes first keeps what it wrote
// (valueMap never replaces a cached set). The returned func stops it and
// waits for it.
func (s *Scheduler) prefetchHeld(ctx context.Context, components []metricformula.Component, inc *incremental, modelID, revisionID string) func() {
	if !inc.on || s.store.pool == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, comp := range components {
			for _, id := range comp.Members {
				if ctx.Err() != nil {
					return
				}
				def := inc.defs[id]
				if def == nil || def.IsInput || !inc.requested[id] {
					continue
				}
				// A metric of very many cells is evaluated whole and not
				// kept (lean.go): nothing to prefetch.
				if rollup.LeafComboCount(inc.allDims, inc.metricDimIDs[id]) >= leanCellsAt {
					continue
				}
				_, _ = s.valueMap(ctx, def, modelID, revisionID)
				if def.Text {
					_, _ = s.textMap(ctx, def, modelID, revisionID)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
