package calculation

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// Scheduler drives the incremental calculation of a revision: the gateway
// runs it in-process after every change that can move a calculated value.
type Scheduler struct {
	log   zerolog.Logger
	store *Store
}

func NewScheduler(log zerolog.Logger, store *Store) *Scheduler {
	return &Scheduler{log: log, store: store}
}

// RecalcTimeout bounds a recalculation that runs after its request has
// returned (the gateway's background recalculations), so a stuck one
// gives up its revision lock instead of holding it forever.
const RecalcTimeout = 30 * time.Minute

// recoverAsError turns a panic in a recalculation entry point into its
// returned error. Recalculations run in background goroutines — the
// gateway's `go h.recalc…`, the calculation service's RPCs, the
// facts.committed consumer — where an unrecovered panic stops the whole
// process, for every tenant. Deferred directly, so recover sees the panic.
func (s *Scheduler) recoverAsError(err *error, what string) {
	if r := recover(); r != nil {
		s.log.Error().Str("calculation", what).Interface("panic", r).Str("stack", string(debug.Stack())).
			Msg("calculation panicked; recovered")
		*err = fmt.Errorf("internal error during %s: %v", what, r)
	}
}

// guarded runs one metric's (or one recurrence's) calculation and returns a
// panic as its error, so that metric is marked failed — the console's
// warning — and the rest of the pass still runs.
func (s *Scheduler) guarded(what string, fn func() error) (err error) {
	defer s.recoverAsError(&err, what)
	return fn()
}

// RecalcAffected marks all transitively dependent partitions dirty and then executes them
// in topological order (leaves → dependents).
func (s *Scheduler) RecalcAffected(ctx context.Context, modelID, revisionID string, changedInputIDs []string) (err error) {
	defer s.recoverAsError(&err, "recalculation")
	if revisionID == "" {
		return fmt.Errorf("revisionID is required")
	}

	defs, err := s.store.LoadModelMetrics(ctx, modelID, revisionID)
	if err != nil {
		return fmt.Errorf("load metrics: %w", err)
	}

	affectedIDs := AffectedMetricIDs(defs, changedInputIDs)
	affectedIDs = withDependencyFree(defs, affectedIDs)
	if len(affectedIDs) == 0 {
		s.log.Debug().Str("model_id", modelID).Msg("no calculated metrics affected by writeback")
		return nil
	}

	return s.recalcMetricIDs(ctx, modelID, revisionID, defs, affectedIDs)
}

// ChangedCell is one input cell a write changed: its metric and its
// coordinate, {dimension ID: member code}.
type ChangedCell struct {
	MetricID string
	Combo    map[string]string
}

// RecalcAffectedCells is RecalcAffected for written cells: knowing which
// cells changed, the pass recalculates the cells they reach and writes
// what differs (region.go), instead of every cell of every metric they
// reach.
func (s *Scheduler) RecalcAffectedCells(ctx context.Context, modelID, revisionID string, cells []ChangedCell) (err error) {
	defer s.recoverAsError(&err, "recalculation")
	if revisionID == "" {
		return fmt.Errorf("revisionID is required")
	}
	defs, err := s.store.LoadModelMetrics(ctx, modelID, revisionID)
	if err != nil {
		return fmt.Errorf("load metrics: %w", err)
	}
	seen := map[string]bool{}
	var inputs []string
	for _, c := range cells {
		if !seen[c.MetricID] {
			seen[c.MetricID] = true
			inputs = append(inputs, c.MetricID)
		}
	}
	affectedIDs := withDependencyFree(defs, AffectedMetricIDs(defs, inputs))
	if len(affectedIDs) == 0 {
		return nil
	}
	return s.recalcRequest(ctx, modelID, revisionID, defs, passRequest{ids: affectedIDs, cells: cells})
}

// RecalcSpecific force-recomputes exactly the given metric IDs (plus any
// prerequisite calc metrics metricformula.Plan pulls in via their still-current
// dependency edges), regardless of whether the model's present-day
// dependency graph still reaches them from any input.
//
// This is needed when a metric is deleted: its former dependents'
// model.calc_dependency edges are cascade-removed by the delete itself, so
// RecalcAffected's own AffectedMetricIDs graph walk can no longer discover
// them from any changed input — even though their formula text still
// references the now-gone name and must be re-evaluated to surface the
// resulting error (via MarkError) instead of staying silently frozen at a
// stale pre-deletion value. Callers are expected to have captured the
// affected metric IDs themselves, before the deletion removed the edges
// that would otherwise make them discoverable.
func (s *Scheduler) RecalcSpecific(ctx context.Context, modelID, revisionID string, metricIDs []string) (err error) {
	defer s.recoverAsError(&err, "recalculation")
	if revisionID == "" {
		return fmt.Errorf("revisionID is required")
	}
	if len(metricIDs) == 0 {
		return nil
	}

	defs, err := s.store.LoadModelMetrics(ctx, modelID, revisionID)
	if err != nil {
		return fmt.Errorf("load metrics: %w", err)
	}

	return s.recalcMetricIDs(ctx, modelID, revisionID, defs, metricIDs)
}

// revisionLocks serializes recalculation passes per (model, revision)
// within this process. Passes used to overlap freely: each site that
// triggers one (a cell write, a member insert, a metric edit) builds its own
// Scheduler, and two passes over the same revision raced — the one that
// loaded its inputs BEFORE the latest write could finish LAST and, via
// ClearPerComboResults + write, replace fresh rows with stale ones (a newly
// added period's rows vanishing until the next edit; the load-sensitive
// form-mapping convergence test). A pass that waits its turn sees every
// write made before it started, so the last pass to run is the current one.
var revisionLocks sync.Map // "model:revision" → *sync.Mutex

func lockRevision(modelID, revisionID string) func() {
	v, _ := revisionLocks.LoadOrStore(modelID+":"+revisionID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// recalcMetricIDs is RecalcAffected's execution core, factored out so
// RecalcSpecific can drive it directly with an explicit target list instead
// of deriving one from AffectedMetricIDs's graph walk. On a pool, requests
// that arrive while a pass runs share the next pass (passQueue).
func (s *Scheduler) recalcMetricIDs(ctx context.Context, modelID, revisionID string, defs map[string]*MetricDef, affectedIDs []string) error {
	return s.recalcRequest(ctx, modelID, revisionID, defs, passRequest{ids: affectedIDs})
}

// passRequest is what a pass calculates: ids, and the written cells that
// made them stale when the request knows them (RecalcAffectedCells).
// Requests folded into one pass (passQueue) merge: a metric some request
// named without cells is forced — calculated whole.
type passRequest struct {
	ids    []string
	cells  []ChangedCell
	forced map[string]bool // set by the queue; nil: every id without cells is forced
}

// forcedID reports whether the request calculates id whole.
func (r passRequest) forcedID(id string) bool {
	if r.forced != nil {
		return r.forced[id]
	}
	return len(r.cells) == 0
}

func (s *Scheduler) recalcRequest(ctx context.Context, modelID, revisionID string, defs map[string]*MetricDef, req passRequest) error {
	if s.store.pool == nil {
		return s.recalcPass(ctx, modelID, revisionID, defs, req)
	}
	return queueFor(s.store.pool, modelID, revisionID).runRequest(req, func(merged passRequest) error {
		// The pass may calculate other requests' metrics as well: it runs
		// on when this caller's request is cancelled, bounded as a
		// background recalculation is.
		passCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RecalcTimeout)
		defer cancel()
		return s.recalcPass(passCtx, modelID, revisionID, defs, merged)
	})
}

// recalcPass is one recalculation pass over req.ids.
func (s *Scheduler) recalcPass(ctx context.Context, modelID, revisionID string, defs map[string]*MetricDef, req passRequest) error {
	defer lockRevision(modelID, revisionID)()
	ctx = withPassCache(ctx)
	affectedIDs := req.ids
	// Definitions were loaded before the lock; reload so a pass that queued
	// behind another sees the graph as it is now.
	if fresh, err := s.store.LoadModelMetrics(ctx, modelID, revisionID); err == nil {
		defs = fresh
	}
	// Dependency-ordered components. A component is one metric, or a
	// recurrence (a cycle broken by time — opening/closing balances) whose
	// members are evaluated together. Plan rejects any cycle that time does
	// not break; save-time validation already refused it, so reaching that
	// here means the graph was written around the validator.
	// The graph also carries which metrics call LOOKUP or a conditional
	// aggregation (re-analysed from the formula), so Plan refuses them in a
	// recurrence exactly as save-time validation does (contract C4).
	graph := metricformula.NewGraph()
	names := make(map[string]string, len(defs))
	for id, def := range defs {
		names[id] = def.Name
	}
	for _, id := range affectedIDs {
		if def, ok := defs[id]; ok {
			graph.SetMetric(id, def.DependsOn, def.Formula)
		}
	}
	components, err := metricformula.Plan(graph, affectedIDs, names)
	if err != nil {
		return fmt.Errorf("dependency order: %w", err)
	}

	timePartition := PartitionMonth(time.Now())
	// Load dimension ID→name mapping so formulas can reference dimensions by name.
	dimIDToName, err := s.store.LoadDimIDToName(ctx, modelID, revisionID)
	if err != nil {
		s.log.Warn().Err(err).Msg("load dim names")
		dimIDToName = map[string]string{}
	}

	// Load the revision-wide dimension universe and each metric's own
	// declared dimensions (grid_metric ⋈ grid_dimension) — config-driven,
	// not fact-driven: a metric's applicable intersections are enumerated
	// from its declared dims' full leaf-member set (rollup.LeafCombos),
	// not from whatever dim_members happen to already exist in fact_input.
	allDims, err := s.store.LoadAllDimensions(ctx, modelID, revisionID)
	if err != nil {
		return fmt.Errorf("load dimensions: %w", err)
	}
	metricDimIDs, err := s.store.LoadMetricDimensionIDs(ctx, modelID, revisionID)
	if err != nil {
		return fmt.Errorf("load metric dimension ids: %w", err)
	}
	// Member metadata for dim.property, PARENT, LOOKUP and the conditional
	// aggregations, over the same unfiltered dimensions. Without the
	// property declarations every dim.property fails UNKNOWN_PROPERTY
	// (loudly, per cell) while every other formula still computes.
	schema, err := s.store.LoadDimensionSchema(ctx, modelID, revisionID)
	if err != nil {
		s.log.Warn().Err(err).Msg("load dimension property declarations")
		schema = nil
	}
	meta := NewDimMetadata(allDims, dimIDToName, schema)
	DescribeMetrics(meta, defs, metricDimIDs)

	inc := s.newIncremental(ctx, modelID, revisionID, timePartition, req, allDims, metricDimIDs, dimIDToName, defs)
	defer s.prefetchHeld(ctx, components, inc, modelID, revisionID)()

	s.runComponents(ctx, components, defs, func(comp metricformula.Component) {
		incs, run := inc.plan(comp)
		if !run {
			return
		}
		if comp.Recurrence {
			s.runRecurrence(ctx, comp, modelID, revisionID, timePartition, allDims, metricDimIDs, dimIDToName, meta, defs, incs)
			inc.done(comp, incs)
			return
		}
		metricID := comp.Members[0]
		def, ok := defs[metricID]
		if !ok || def.IsInput {
			return
		}
		defer inc.done(comp, incs)

		pk := BuildPartitionKey(modelID, revisionID, metricID, timePartition)
		if err := s.store.MarkCalculating(ctx, pk, modelID, metricID, revisionID, timePartition); err != nil {
			s.log.Error().Err(err).Str("partition_key", pk).Msg("mark calculating")
			return
		}

		calcErr := s.guarded(def.Name, func() error {
			return s.executePartition(ctx, def, modelID, revisionID, pk, allDims, metricDimIDs, dimIDToName, meta, defs, incs[metricID])
		})
		if calcErr != nil {
			s.log.Error().Err(calcErr).Str("metric", def.Name).Msg("calculation failed")
			s.store.MarkError(ctx, pk, calcErr.Error()) //nolint:errcheck
		} else {
			s.store.MarkClean(ctx, pk) //nolint:errcheck
		}
	})
	return nil
}

// runComponents runs fn for every component, each once all the components
// it reads have finished, on up to passWorkers goroutines. One after
// another, a driver change on the education FP&A model recalculated 97
// metrics in a 2 s chain of evaluation and writes, most of which do not
// read each other (2026-10-20). A component reads what its members depend
// on (DependsOnID, and a ratio's numerator and denominator); Plan orders
// those before it, so a dependency on a later component cannot occur and
// is ignored. A finished component releases its dependents whether it
// failed or not, as the sequential order did. On a transaction (no pool:
// the AI plan check's dry run) the components run in order on this
// goroutine — a transaction serves one statement at a time.
func (s *Scheduler) runComponents(ctx context.Context, components []metricformula.Component, defs map[string]*MetricDef, fn func(metricformula.Component)) {
	workers := passWorkers(s.store.pool)
	if workers <= 1 || len(components) < 2 {
		for _, comp := range components {
			fn(comp)
		}
		return
	}
	owner := make(map[string]int, len(components))
	for i, comp := range components {
		for _, id := range comp.Members {
			owner[id] = i
		}
	}
	waiting := make([]int, len(components))
	dependents := make([][]int, len(components))
	for i, comp := range components {
		seen := map[int]bool{}
		for _, id := range comp.Members {
			def := defs[id]
			if def == nil {
				continue
			}
			reads := append(append([]string{}, def.DependsOnID...), def.AggNumeratorID, def.AggDenominatorID)
			for _, dep := range reads {
				j, ok := owner[dep]
				if !ok || j >= i || seen[j] {
					continue
				}
				seen[j] = true
				waiting[i]++
				dependents[j] = append(dependents[j], i)
			}
		}
	}
	ready := make(chan int, len(components))
	for i := range components {
		if waiting[i] == 0 {
			ready <- i
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	left := len(components)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ready {
				fn(components[i])
				mu.Lock()
				for _, d := range dependents[i] {
					if waiting[d]--; waiting[d] == 0 {
						ready <- d
					}
				}
				if left--; left == 0 {
					close(ready)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}

// passWorkers is how many components of a pass run at once on pool: two of
// its connections stay free for the reads the console makes meanwhile (a
// tenant pool has 5 by default), and at most 4 run.
func passWorkers(pool *pgxpool.Pool) int {
	if pool == nil {
		return 1
	}
	n := int(pool.Config().MaxConns) - 2
	if n > 4 {
		n = 4
	}
	if n < 1 {
		n = 1
	}
	return n
}

// runRecurrence claims every member of a recurrence component, executes it
// as one unit, and marks all members clean or all in error together.
func (s *Scheduler) runRecurrence(
	ctx context.Context, comp metricformula.Component, modelID, revisionID, timePartition string,
	allDims map[string]*rollup.Dimension, metricDimIDs map[string][]string, dimIDToName map[string]string, meta *DimMetadata, defs map[string]*MetricDef,
	incs map[string]*increment,
) {
	keys := make([]string, 0, len(comp.Members))
	for _, id := range comp.Members {
		def, ok := defs[id]
		if !ok || def.IsInput {
			continue
		}
		pk := BuildPartitionKey(modelID, revisionID, id, timePartition)
		if err := s.store.MarkCalculating(ctx, pk, modelID, id, revisionID, timePartition); err != nil {
			s.log.Error().Err(err).Str("partition_key", pk).Msg("mark calculating")
			return
		}
		keys = append(keys, pk)
	}
	calcErr := s.guarded("recurrence", func() error {
		return s.executeRecurrence(ctx, comp, modelID, revisionID, BuildPartitionKey(modelID, revisionID, comp.Members[0], timePartition), allDims, metricDimIDs, dimIDToName, meta, defs, incs)
	})
	for _, pk := range keys {
		if calcErr != nil {
			s.store.MarkError(ctx, pk, calcErr.Error()) //nolint:errcheck
		} else {
			s.store.MarkClean(ctx, pk) //nolint:errcheck
		}
	}
	if calcErr != nil {
		s.log.Error().Err(calcErr).Strs("metrics", comp.Members).Msg("recurrence calculation failed")
	}
}

// ErrBlankResult is an evaluation whose result is blank: no value at that
// coordinate, so no row (skipped like an intersection with no data), never
// a persisted 0 — and, from EvaluateWithDimContext, no cell and no chart
// point, never a 0 entering a total.
var ErrBlankResult = errors.New("the formula's result is blank")

var errBlankResult = ErrBlankResult

// executePartition resolves all dependency values and evaluates the formula
// once per declared leaf-level dimensional intersection of def's own
// dimensions (config-driven — grid_metric ⋈ grid_dimension, enumerated via
// rollup.LeafCombos — not whatever dim_members happen to already exist in
// fact_input), persisting one calc_result row per intersection in addition
// to the existing '{}' aggregate row every other consumer already reads.
func (s *Scheduler) executePartition(
	ctx context.Context, def *MetricDef, modelID, revisionID, partitionKey string,
	allDims map[string]*rollup.Dimension, metricDimIDs map[string][]string, dimIDToName map[string]string, meta *DimMetadata, allDefs map[string]*MetricDef,
	inc *increment,
) error {
	if inc != nil {
		if err := s.loadHeld(ctx, def, modelID, revisionID, inc); err != nil {
			return err
		}
	}
	// A time-series formula (PREVIOUS, LAG, MOVINGSUM, ...) on a declared
	// time dimension takes the time-series path: one evaluation per leaf
	// period with a time context, time_summary for its totals. A time
	// function on a metric WITHOUT a time dimension is left to the scalar
	// path below, where it fails every cell with TIME_CONTEXT_REQUIRED — a
	// validation error, never a silent zero. An ordinary formula on a time
	// dimension stays on the scalar path: its leaves are period-independent,
	// and only how they reduce across time changes (time_summary, below).
	if def.Text && !def.IsInput {
		return s.executeText(ctx, def, modelID, revisionID, partitionKey, newEvalEnv(s, modelID, revisionID, allDims, metricDimIDs, dimIDToName, meta, allDefs), inc)
	}
	axis, err := timeAxisFor(allDims, metricDimIDs[def.ID])
	if err != nil {
		return err
	}
	if axis != nil && usesTimeSeries(def.Formula) {
		return s.executeTimeSeries(ctx, def, modelID, revisionID, partitionKey, axis, allDims, metricDimIDs, dimIDToName, meta, allDefs, inc)
	}

	env := newEvalEnv(s, modelID, revisionID, allDims, metricDimIDs, dimIDToName, meta, allDefs)
	evalOne, err := env.cellEvaluator(ctx, def)
	if err != nil {
		return err
	}

	leafCombos := rollup.LeafCombos(allDims, metricDimIDs[def.ID])

	if len(leafCombos) == 0 {
		leafCombos = []map[string]string{{}}
	}

	// A pure ratio (agg_rule 'average' whose formula reads no member, e.g. a
	// utilization %) totals like agg_rule 'formula': the formula evaluated
	// on the totalled inputs at every total. Its cells are still each
	// intersection's own value: the leaves used to be collapsed into that one
	// revision-wide evaluation, so a [month, scenario] grid of
	// (opening + closing) / 2 had no monthly values at all.
	rule := EffectiveAggRule(def.AggRule, def.Formula, dimIDToName)

	results := make([]CalcResultRow, 0, len(leafCombos))
	var failures, skipped, memberNA int
	var firstErr error
	// In a pass following written cells, a cell outside the region they
	// reach keeps the value it holds (region.go).
	partial := inc.partial()
	for _, combo := range leafCombos {
		if partial && !inc.in.contains(combo) {
			if v, ok := inc.old[dimKey(combo)]; ok {
				results = append(results, CalcResultRow{DimMembers: combo, Value: v})
			}
			continue
		}
		v, noData, err := evalOne(combo)
		if err != nil {
			// A failure at an intersection where every referenced value is
			// zero is "no data here", not "calculation failing": with a
			// sparse plan, every division metric fails #DIV/0! at every
			// EMPTY intersection, which drowned three metrics of the sales
			// demo in 58-60/64 error badges while their actual numbers were
			// all correct. Skip quietly — no row (same as before, a failed
			// combo never produced one) and no error state. A failure with
			// any nonzero operand (e.g. revenue present, units genuinely 0)
			// is a real formula problem and still flags.
			if noData {
				skipped++
				s.log.Debug().Err(err).Str("metric", def.Name).Msg("combo skipped: no data at intersection")
				continue
			}
			failures++
			if formula.IsMemberNotAvailable(err) {
				memberNA++
			}
			if firstErr == nil {
				firstErr = err
			}
			s.log.Warn().Err(err).Str("metric", def.Name).Msg("per-combo eval failed")
			continue
		}
		results = append(results, CalcResultRow{DimMembers: combo, Value: v})
	}

	// Rollup rows for rules the client cannot combine: agg_rule "formula"
	// must re-evaluate the expression against aggregated inputs, and "rate"
	// is a ratio of aggregated operands — both of which evalOne already does
	// for a combo pinned to non-leaf members (rollup.Resolve recurses each
	// parent's subtree). Persisting every rollup combo lets the grid answer
	// a World row under a Q1 context with a REAL number instead of the "—"
	// the no-false-numbers display contract otherwise requires. Failures
	// here never fail the partition — a missing rollup row renders as "—",
	// the contract's safe state. Capped so a pathological member lattice
	// degrades to leaves+grand-total rather than an explosion of rows.
	if rule == string(rollup.AggFormula) || rule == string(rollup.AggRate) {
		const rollupComboCap = 20000
		covers := coverTest(inc, allDims)
		for _, combo := range rollup.RollupCombos(allDims, metricDimIDs[def.ID], rollupComboCap) {
			if partial && !covers(combo) {
				if v, ok := inc.old[dimKey(combo)]; ok {
					results = append(results, CalcResultRow{DimMembers: combo, Value: v})
				}
				continue
			}
			v, _, evalErr := evalOne(combo)
			if evalErr != nil {
				continue // empty subtree or genuine per-combo error — no row, renders "—"
			}
			results = append(results, CalcResultRow{DimMembers: combo, Value: v})
		}
	}

	// If every combo failed to evaluate, there is nothing genuine to
	// persist — CombineAgg([], ...) returns 0 for "sum"/"average" (a
	// mathematically correct sum/average of an empty set), and writing
	// that would fabricate a seemingly-valid computed value for a metric
	// whose only evaluation attempt actually errored (div-by-zero, an
	// unknown function, ...), silently turning a real formula error into
	// a value that shows up in every reader that trusts calc_result. Bail
	// out before either write — the metric simply gets no fresh row this
	// pass (any prior successful row, if one exists, is left as-is; a
	// brand-new metric whose formula has never once evaluated correctly
	// gets no row at all, matching "absent" rather than "computed to 0").
	// All-combos-SKIPPED (failures == 0) takes the same no-write exit but
	// without the error: a model with no data yet has nothing failing.
	if len(results) == 0 && len(leafCombos) > 0 {
		if failures == 0 {
			// Every combo was skipped for lack of data — which includes the
			// case where data USED to exist and was deleted (full_reload,
			// member removal). The stale per-combo rows from that earlier
			// data must go, or every reader keeps serving values whose
			// inputs no longer exist. Real failures below deliberately do
			// NOT clear: an erroring metric keeps its last good rows.
			// The '{}' total goes with them: a share-of-total metric must
			// not keep saying 100% after all its data was deleted.
			if err := s.store.ClearAllResults(ctx, modelID, revisionID, def.ID); err != nil {
				return fmt.Errorf("clear stale results: %w", err)
			}
			s.remember(ctx, def.ID, nil)
			inc.cleared(metricDimIDs[def.ID], allDims)
			return nil
		}
		if memberNA == failures {
			// Every cell names a member that does not exist (a LOOKUP of a
			// deleted member): the last rows were read from that member's
			// data, which is gone. Keeping them would serve numbers nobody
			// can recompute; the cells render "—" and the error names the
			// member.
			if err := s.store.ClearAllResults(ctx, modelID, revisionID, def.ID); err != nil {
				return fmt.Errorf("clear stale results: %w", err)
			}
			s.remember(ctx, def.ID, nil)
			inc.cleared(metricDimIDs[def.ID], allDims)
		}
		return fmt.Errorf("%d/%d combos failed to evaluate for metric %s (first error: %w)", failures, len(leafCombos), def.Name, firstErr)
	}

	vals := make([]float64, len(results))
	for i, r := range results {
		vals[i] = r.Value
	}
	// agg_rule 'formula' means the total is the formula's own answer at the
	// total level, not a combination of the per-member answers. evalOne({})
	// resolves every dependency with an empty combo, i.e. fully aggregated, so
	// a variance percentage totals as total_variance/total_target rather than
	// as a sum or a mean of each member's percentage.
	//
	// The per-member rows above are still computed and still written: they are
	// individually meaningful, and this only replaces how they roll up. That
	// holds for a pure ratio under 'average' too (rule above).
	aggregate := rollup.CombineAgg(vals, rollup.AggRule(rule))
	writeAggregate := true
	// On a time dimension, a combining rule (sum/average/count) reduces the
	// non-time dimensions first and time last, by the metric's time_summary
	// — a closing balance totals as its LAST period, not the sum of every
	// month's balance. 'formula' and 'rate' are untouched: their total is
	// the formula re-evaluated at the aggregate, across time as across any
	// other dimension (Anaplan's Formula summary), so a margin percentage
	// stays total_margin / total_revenue rather than a sum of quarterly
	// percentages.
	if axis != nil && rule != string(rollup.AggFormula) && rule != string(rollup.AggRate) {
		aggregate, writeAggregate = summarizeOverTime(results, axis, rule, def.TimeSummary, nil)
	}
	if rule == string(rollup.AggFormula) {
		total, _, totalErr := evalOne(map[string]string{})
		switch {
		case errors.Is(totalErr, errBlankResult):
			writeAggregate = false // blank at the total: no total row
		case totalErr != nil:
			// Falling back to the combined value would quietly publish the very
			// number this rule exists to avoid, so the metric gets no fresh row
			// instead — the same stance the all-combos-failed branch takes.
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
	if !rollup.Aggregates(rollup.AggRule(def.AggRule)) && !rollup.LeafCombo(allDims, metricDimIDs[def.ID], map[string]string{}) {
		// agg_rule none: values at the leaves only. A metric with no
		// dimensions has one value, its '{}' row, and keeps it; one whose
		// only dimension is time totals by its time summary (above).
		writeAggregate = false
	}
	// The single-combo case (the metric has no declared dims at all) is
	// already fully covered by the aggregate row and must not be
	// double-written.
	var perComboRows []CalcResultRow
	for _, r := range results {
		if len(r.DimMembers) > 0 {
			perComboRows = append(perComboRows, r)
		}
	}
	// Single-dimension slice rows ({oneDim: member}, all other dims
	// aggregated) — the shape a dashboard pins when the user drills ONE
	// context selector. Persisting them lets the grid read one row instead of
	// re-resolving the whole slice on the fly (the multi-second scoped read on
	// a large model). Combined exactly as scopeCalcCells (the live scoped
	// read) does, so the fast path and the recompute fallback never disagree.
	// agg_rule none gets the slices that pin every non-time dimension to a
	// leaf: there time still totals by the time summary (a closing balance
	// typed per scenario has its FY value).
	dimConditional := FormulaReferencesDims(def.Formula, dimIDToName)
	perComboRows = append(perComboRows, oneDimSliceRows(rule, dimConditional, metricDimIDs[def.ID], allDims, results, evalOne, axis, def.TimeSummary)...)
	perComboRows = append(perComboRows, aggregatePeriodRows(rule, metricDimIDs[def.ID], results, axis, def.TimeSummary)...)

	// Replace the metric's result set in one transaction: the '{}' total
	// (none when this pass has none — time_summary 'none', a blank total)
	// and every intersection. The new set is authoritative: intersections
	// that lost their data since the last run (skipped above, absent from
	// perComboRows) stop serving their old values. A reader sees the old
	// set or the new one, and a failed write keeps the last good rows.
	rows := perComboRows
	if writeAggregate {
		rows = append(rows, CalcResultRow{DimMembers: map[string]string{}, Value: aggregate})
	}
	if err := s.persistRows(ctx, def, modelID, revisionID, rows, inc, metricDimIDs[def.ID], allDims); err != nil {
		return err
	}
	s.remember(ctx, def.ID, rows)

	if failures > 0 {
		return fmt.Errorf("%d/%d combos failed to evaluate for metric %s (first error: %w)", failures, len(leafCombos)-skipped, def.Name, firstErr)
	}
	return nil
}

// executeText calculates a text metric (format text): its formula's text at
// every leaf, stored in calc_result.text_value beside a 0. Text is never
// added up: agg_rule "formula" evaluates the formula at every total too (a
// status at the total), any other rule gives leaves only. A cell whose text
// is "" (or blank, or without data) has no row.
func (s *Scheduler) executeText(ctx context.Context, def *MetricDef, modelID, revisionID, partitionKey string, env *evalEnv, inc *increment) error {
	evalText, err := env.textEvaluator(ctx, def)
	if err != nil {
		return err
	}
	dimIDs := env.metricDimIDs[def.ID]
	combos := rollup.LeafCombos(env.allDims, dimIDs)
	if len(combos) == 0 {
		combos = []map[string]string{{}}
	}
	if def.AggRule == string(rollup.AggFormula) && len(dimIDs) > 0 {
		combos = append(combos, rollup.RollupCombos(env.allDims, dimIDs, 20000)...)
		combos = append(combos, map[string]string{})
	}
	var rows []CalcResultRow
	var failures int
	var firstErr error
	for _, combo := range combos {
		text, noData, err := evalText(combo)
		if err != nil {
			if noData || errors.Is(err, errBlankResult) {
				continue
			}
			failures++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if text == "" {
			continue
		}
		rows = append(rows, CalcResultRow{DimMembers: combo, Text: &text})
	}
	if len(rows) == 0 && failures > 0 {
		// Every cell failed: the last good rows stay, as for a number.
		return fmt.Errorf("%d/%d combos failed to evaluate for metric %s (first error: %w)", failures, len(combos), def.Name, firstErr)
	}
	if err := s.persistRows(ctx, def, modelID, revisionID, rows, inc, dimIDs, env.allDims); err != nil {
		return err
	}
	s.remember(ctx, def.ID, rows)
	if failures > 0 {
		return fmt.Errorf("%d/%d combos failed to evaluate for metric %s (first error: %w)", failures, len(combos), def.Name, firstErr)
	}
	return nil
}

// oneDimSliceRows computes, for a metric with two or more dimensions, one
// calc_result row per (dimension, member) with just that dimension pinned and
// every other dimension aggregated — the "slice" a dashboard produces when the
// user drills a single context selector (period=Q1, all geographies/products).
// Reading such a row is O(1) versus re-resolving the entire slice per request.
//
// The value MUST equal what scopeCalcCells (the live scoped grid read) computes
// for the same pin, or the precomputed fast path and the recompute fallback
// would disagree. That function's rule, mirrored here:
//   - formula, rate, and average whose formula is NOT dimension-conditional:
//     the formula evaluated once against inputs aggregated over the slice,
//     i.e. evalOne(slice) — rollup.Resolve already aggregates the unpinned
//     dims. For a calc rate metric the formula IS the ratio (num/den), so
//     evalOne(slice) is exactly Anaplan Ratio at that slice: numerator's
//     slice total over denominator's slice total.
//   - sum / count / dimension-conditional average: CombineAgg over the slice's
//     leaf results (already computed this pass).
//
// Single-dimension metrics are skipped: their one-dim "slice" is just a leaf or
// rollup combo, already handled by the leaf/RollupCombos writes above.
//
// With a time axis, a non-time member's slice for a combining rule reduces
// the periods by timeSummary (see summarizeOverTime); the time member's own
// slice is one period and needs no time reduction.
func oneDimSliceRows(
	aggRule string,
	dimConditional bool,
	dimIDs []string,
	dims map[string]*rollup.Dimension,
	leafResults []CalcResultRow,
	evalOne func(map[string]string) (float64, bool, error),
	axis *timeAxis,
	timeSummaryRule string,
) []CalcResultRow {
	if len(dimIDs) < 2 {
		return nil
	}
	useEval := aggRule == string(rollup.AggFormula) ||
		aggRule == string(rollup.AggRate) ||
		(aggRule == "average" && !dimConditional)

	// Each member's leaves, found once per dimension: a slice used to scan
	// every leaf, so a node × day metric's 420 slices read its 43,200
	// leaves 420 times over (2026-10-23). In ascending leaf order, a slice
	// reduces its values in the order the scan met them.
	var out []CalcResultRow
	for _, dimID := range dimIDs {
		d := dims[dimID]
		if d == nil {
			continue
		}
		rowsOf := map[string][]int{}
		for i, r := range leafResults {
			if code, ok := r.DimMembers[dimID]; ok {
				rowsOf[code] = append(rowsOf[code], i)
			}
		}
		childrenOf := map[string][]string{}
		for _, m := range d.Members {
			if m.ParentCode != "" {
				childrenOf[m.ParentCode] = append(childrenOf[m.ParentCode], m.Code)
			}
		}
		for _, m := range d.Members {
			slice := map[string]string{dimID: m.Code}
			if !rollup.Aggregates(rollup.AggRule(aggRule)) && !rollup.LeafCombo(dims, dimIDs, slice) {
				continue // agg_rule none: no value across members
			}
			if useEval {
				v, _, err := evalOne(slice)
				if err != nil {
					continue // empty subtree / per-combo error — no row, renders "—"
				}
				out = append(out, CalcResultRow{DimMembers: slice, Value: v})
				continue
			}
			// Subtree of m (m and every descendant), so a rollup member's
			// slice sums all its leaves.
			sub := map[string]bool{}
			queue := []string{m.Code}
			for len(queue) > 0 {
				c := queue[0]
				queue = queue[1:]
				if sub[c] {
					continue
				}
				sub[c] = true
				queue = append(queue, childrenOf[c]...)
			}
			var picked []int
			lists := 0
			for c := range sub {
				if rows := rowsOf[c]; len(rows) > 0 {
					picked = append(picked, rows...)
					lists++
				}
			}
			if lists > 1 {
				sort.Ints(picked)
			}
			sliceLeaves := make([]CalcResultRow, len(picked))
			for k, i := range picked {
				sliceLeaves[k] = leafResults[i]
			}
			if axis != nil && (dimID != axis.dim.ID || !m.IsLeafPeriod()) {
				// A non-time member reduces time by the summary; so does an
				// AGGREGATE period (H1 = its quarters reduced). A leaf period
				// is a single period and falls through to the plain combine.
				if v, ok := summarizeOverTime(sliceLeaves, axis, aggRule, timeSummaryRule, map[string]map[string]bool{dimID: sub}); ok {
					out = append(out, CalcResultRow{DimMembers: slice, Value: v})
				}
				continue
			}
			vals := make([]float64, 0, len(sliceLeaves))
			for _, r := range sliceLeaves {
				vals = append(vals, r.Value)
			}
			if len(vals) == 0 {
				continue
			}
			out = append(out, CalcResultRow{DimMembers: slice, Value: rollup.CombineAgg(vals, rollup.AggRule(aggRule))})
		}
	}
	return out
}

// aggregatePeriodRows writes {H1}, {FY26} rows for a metric whose ONLY
// dimension is a time hierarchy (oneDimSliceRows covers the multi-dimension
// case): each aggregate period is its leaves reduced by the time summary,
// except under formula/rate rules, whose aggregate rows come from
// RollupCombos (formula re-evaluated at the aggregate).
func aggregatePeriodRows(aggRule string, dimIDs []string, leafResults []CalcResultRow, axis *timeAxis, timeSummaryRule string) []CalcResultRow {
	if axis == nil || len(dimIDs) != 1 || aggRule == string(rollup.AggFormula) || aggRule == string(rollup.AggRate) {
		return nil
	}
	var out []CalcResultRow
	for _, m := range axis.dim.Members {
		if m.IsLeafPeriod() {
			continue
		}
		sub := subtreeCodes(axis.dim, m.Code)
		if v, ok := summarizeOverTime(leafResults, axis, aggRule, timeSummaryRule, map[string]map[string]bool{axis.dim.ID: sub}); ok {
			out = append(out, CalcResultRow{DimMembers: map[string]string{axis.dim.ID: m.Code}, Value: v})
		}
	}
	return out
}

// formulaReferencesDims reports whether the formula mentions any dimension
// name as a bare identifier — the marker of a dimension-conditional formula
// that needs per-member-combo evaluation.
// rateTotal computes agg_rule 'rate' — Anaplan's Ratio summary method.
//
// The parent is numerator_total / denominator_total, taken from two nominated
// metrics rather than from this metric's own member values. That is what makes
// it different from 'formula', which re-evaluates this metric's expression:
// a ratio needs no expression of its own, so it works on input metrics too. A
// price typed per product totals as revenue/volume, never as a sum of prices.
//
// Each operand contributes its OWN total, not a sum of its leaves. For a
// calculated operand that total is the '{}' row this scheduler already wrote,
// which is authoritative and already reflects whatever rule that operand uses
// — reading it back beats recomputing it here with a rule this function would
// have to guess at. Input operands have no such row, so they are resolved.
func (s *Scheduler) rateTotal(
	ctx context.Context,
	def *MetricDef,
	modelID, revisionID string,
	allDims map[string]*rollup.Dimension,
	metricDimIDs map[string][]string,
	allDefs map[string]*MetricDef,
) (float64, error) {
	if def.AggNumeratorID == "" || def.AggDenominatorID == "" {
		return 0, fmt.Errorf("rate needs both a numerator and a denominator metric")
	}

	operandTotal := func(id string) (float64, error) {
		d, ok := allDefs[id]
		if !ok {
			return 0, fmt.Errorf("operand metric %s is not in this model/revision", id)
		}
		valueMap, err := s.valueMap(ctx, d, modelID, revisionID)
		if err != nil {
			return 0, fmt.Errorf("load values for %s: %w", d.Name, err)
		}
		if total, found := valueMap[dimKey(map[string]string{})]; found {
			return total, nil
		}
		v, _, resolveErr := rollup.ResolveTime(ctx, allDims, id, metricDimIDs[id], rollup.AggRule(d.AggRule), rollup.TimeSummaryRule(d.TimeSummary),
			map[string]string{}, func(_ context.Context, _ string, combo map[string]string) (float64, bool, error) {
				val, hit := valueMap[dimKey(combo)]
				return val, hit, nil
			})
		if resolveErr != nil {
			return 0, fmt.Errorf("resolve %s: %w", d.Name, resolveErr)
		}
		return v, nil
	}

	num, err := operandTotal(def.AggNumeratorID)
	if err != nil {
		return 0, err
	}
	den, err := operandTotal(def.AggDenominatorID)
	if err != nil {
		return 0, err
	}
	if den == 0 {
		// Publishing 0 would be indistinguishable from a genuine zero ratio.
		// Erroring leaves the metric without a fresh row, the same stance the
		// all-combos-failed branch takes for a formula that cannot evaluate.
		return 0, fmt.Errorf("denominator %s totals zero", allDefs[def.AggDenominatorID].Name)
	}
	return num / den, nil
}

// EffectiveAggRule is how a metric's totals are made: its agg_rule, except
// that a pure ratio — agg_rule "average" whose formula reads no member —
// totals as "formula" (the formula on the totalled inputs), as the Formulas
// Manual says. Shared by the scheduler and the scoped read so both give the
// same totals.
func EffectiveAggRule(aggRule, expr string, dimIDToName map[string]string) string {
	if aggRule == string(rollup.AggAverage) && !FormulaReferencesDims(expr, dimIDToName) {
		return string(rollup.AggFormula)
	}
	return aggRule
}

// FormulaReferencesDims reports whether expr's value depends on the cell's
// members — used to decide whether a pure-ratio (agg_rule "average")
// metric's formula is itself dimension-conditional and must be evaluated per
// leaf combo, or whether it collapses to one revision-wide ratio-of-sums
// evaluation. Exported so internal/gateway's scopeCalcCells (the
// hidden-member-restricted grid path) can apply the exact same collapse rule
// executePartition does below — duplicating this logic there previously
// caused the two paths to disagree on a metric's value for a restricted vs.
// unrestricted caller.
//
// A formula is dimension-conditional when it names one of the dimensions
// as a bare identifier, reads a dim.property, names a dimension as an
// argument (PARENT, LOOKUP, a criteria range), or calls LOOKUP or a
// conditional aggregation — every one of which answers differently per
// cell. Dimension names match case-insensitively (contract C1): REGION.weight
// is region.weight. A formula that does not analyse falls back to a
// case-insensitive scan of its words.
func FormulaReferencesDims(expr string, dimIDToName map[string]string) bool {
	if len(dimIDToName) == 0 {
		return false
	}
	names := make(map[string]bool, len(dimIDToName))
	for _, n := range dimIDToName {
		names[strings.ToUpper(n)] = true
	}
	if an, err := formula.Analyze(expr); err == nil {
		if len(an.PropertyRefs) > 0 || len(an.DimensionArgs) > 0 || len(an.DimensionalCalls) > 0 {
			return true
		}
		for _, ref := range an.References {
			if names[strings.ToUpper(ref.Name)] {
				return true
			}
		}
		return false
	}
	isWord := func(r byte) bool {
		return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
	}
	for i := 0; i < len(expr); {
		if !isWord(expr[i]) {
			i++
			continue
		}
		j := i
		for j < len(expr) && isWord(expr[j]) {
			j++
		}
		if names[strings.ToUpper(expr[i:j])] {
			return true
		}
		i = j
	}
	return false
}

// ── helpers ───────────────────────────────────────────────────────────────────

// AffectedMetricIDs returns every calculated metric transitively depending on
// any of changedInputIDs, walking defs' DependsOnID graph in reverse
// (breadth-first). Pure/in-memory, no DB access — callers that need to mark
// partitions dirty ahead of the actual claim-and-execute pass (e.g. inside
// a caller's own transaction, before this scheduler's pool-based
// RecalcAffected ever runs) can reuse the exact same affected-set logic
// without duplicating the graph walk.
func AffectedMetricIDs(defs map[string]*MetricDef, changedInputIDs []string) []string {
	reverse := make(map[string][]string)
	for id, def := range defs {
		for _, depID := range def.DependsOnID {
			reverse[depID] = append(reverse[depID], id)
		}
	}

	affected := make(map[string]bool)
	queue := append([]string(nil), changedInputIDs...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, dependent := range reverse[cur] {
			if !affected[dependent] {
				affected[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}

	out := make([]string, 0, len(affected))
	for id := range affected {
		out = append(out, id)
	}
	return out
}

// withDependencyFree adds every calculated metric that references no other
// metric at all.
//
// Such a metric can never be reached by AffectedMetricIDs, which walks the
// dependency graph outwards from the inputs that changed: with no edges it is
// never anyone's dependent, so it is never "affected" and never computed. It
// saves cleanly, produces no rows, and reports no error — the only symptom is
// a permanently blank column.
//
// A formula reading only a dimension is the case that reaches this in
// practice: =IF(OR(period = "Q1", period = "Q2"), 1, 0) is a legitimate thing
// to write and depends on nothing. Its value can still change, because the
// member set it reads can change, so "nothing it depends on moved" is not a
// safe conclusion to draw from an empty edge list.
//
// The cost is real and accepted: these are re-evaluated on every recalculation
// of their model, whether or not anything they read has moved. There are very
// few of them in a typical model, and computing a handful of dimension-driven
// flags too often is cheaper than the silence they produce otherwise.
func withDependencyFree(defs map[string]*MetricDef, affected []string) []string {
	seen := make(map[string]bool, len(affected))
	for _, id := range affected {
		seen[id] = true
	}
	out := append([]string(nil), affected...)
	for id, def := range defs {
		if def.IsInput || len(def.DependsOnID) > 0 || seen[id] {
			continue
		}
		// An input metric has no formula; a calculated one with no formula
		// has nothing to evaluate either way.
		if strings.TrimSpace(def.Formula) == "" {
			continue
		}
		out = append(out, id)
	}
	return out
}

func BuildPartitionKey(modelID, revisionID, metricID, timePartition string) string {
	return strings.Join([]string{modelID, "rev", revisionID, metricID, timePartition}, ":")
}

func PartitionMonth(t time.Time) string {
	return fmt.Sprintf("%d-%02d", t.Year(), t.Month())
}
