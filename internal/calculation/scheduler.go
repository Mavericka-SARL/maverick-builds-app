package calculation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

const (
	subjectFactsCommitted = "facts.committed"
	streamName            = "MAVERICKS"
	consumerName          = "calc-engine"
)

// FactsCommittedEvent mirrors the event published by the Query Service.
type FactsCommittedEvent struct {
	ModelID    string   `json:"model_id"`
	RevisionID string   `json:"revision_id"`
	MetricIDs  []string `json:"metric_ids"` // input metrics that changed
	UserID     string   `json:"user_id"`
}

// Scheduler subscribes to NATS and drives the incremental calculation loop.
type Scheduler struct {
	log   zerolog.Logger
	store *Store
	nc    *nats.Conn
}

func NewScheduler(log zerolog.Logger, store *Store, nc *nats.Conn) *Scheduler {
	return &Scheduler{log: log, store: store, nc: nc}
}

// Run starts the NATS JetStream consumer and blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	js, err := s.nc.JetStream()
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}

	// Ensure stream exists (idempotent)
	_, err = js.AddStream(&nats.StreamConfig{
		Name:     streamName,
		Subjects: []string{"facts.*", "model.changed.*"},
		MaxAge:   7 * 24 * time.Hour,
	})
	if err != nil && !isAlreadyExists(err) {
		return fmt.Errorf("ensure stream: %w", err)
	}

	sub, err := js.PullSubscribe(subjectFactsCommitted, consumerName,
		nats.BindStream(streamName),
		nats.ManualAck(),
	)
	if err != nil {
		// Durable consumer may not exist yet — create without bind
		sub, err = js.PullSubscribe(subjectFactsCommitted, consumerName,
			nats.Durable(consumerName),
			nats.ManualAck(),
		)
		if err != nil {
			return fmt.Errorf("subscribe: %w", err)
		}
	}
	defer sub.Unsubscribe() //nolint:errcheck

	s.log.Info().Msg("calculation scheduler started")

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		msgs, err := sub.Fetch(10, nats.MaxWait(2*time.Second))
		if err != nil {
			if err == nats.ErrTimeout {
				continue
			}
			s.log.Error().Err(err).Msg("fetch messages")
			continue
		}

		for _, msg := range msgs {
			if err := s.handleMessage(ctx, msg); err != nil {
				s.log.Error().Err(err).Msg("handle message")
				msg.Nak() //nolint:errcheck
			} else {
				msg.Ack() //nolint:errcheck
			}
		}
	}
}

func (s *Scheduler) handleMessage(ctx context.Context, msg *nats.Msg) error {
	var evt FactsCommittedEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		return fmt.Errorf("unmarshal event: %w", err)
	}

	s.log.Info().
		Str("model_id", evt.ModelID).
		Str("revision_id", evt.RevisionID).
		Int("input_metrics", len(evt.MetricIDs)).
		Msg("facts.committed received")

	return s.RecalcAffected(ctx, evt.ModelID, evt.RevisionID, evt.MetricIDs)
}

// RecalcAffected marks all transitively dependent partitions dirty and then executes them
// in topological order (leaves → dependents).
func (s *Scheduler) RecalcAffected(ctx context.Context, modelID, revisionID string, changedInputIDs []string) error {
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
func (s *Scheduler) RecalcSpecific(ctx context.Context, modelID, revisionID string, metricIDs []string) error {
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
// of deriving one from AffectedMetricIDs's graph walk.
func (s *Scheduler) recalcMetricIDs(ctx context.Context, modelID, revisionID string, defs map[string]*MetricDef, affectedIDs []string) error {
	defer lockRevision(modelID, revisionID)()
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
	graph := make(metricformula.Graph, len(affectedIDs))
	names := make(map[string]string, len(defs))
	for id, def := range defs {
		names[id] = def.Name
	}
	for _, id := range affectedIDs {
		if def, ok := defs[id]; ok {
			graph[id] = def.DependsOn
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

	for _, comp := range components {
		if comp.Recurrence {
			s.runRecurrence(ctx, comp, modelID, revisionID, timePartition, allDims, metricDimIDs, dimIDToName, defs)
			continue
		}
		metricID := comp.Members[0]
		def, ok := defs[metricID]
		if !ok || def.IsInput {
			continue
		}

		pk := BuildPartitionKey(modelID, revisionID, metricID, timePartition)

		// Mark dirty
		if err := s.store.MarkDirty(ctx, []string{pk}, modelID, metricID, revisionID, timePartition); err != nil {
			s.log.Error().Err(err).Str("partition_key", pk).Msg("mark dirty")
			continue
		}

		// Claim and execute
		claimed, err := s.store.ClaimForCalculation(ctx, pk)
		if err != nil || !claimed {
			continue
		}

		calcErr := s.executePartition(ctx, def, modelID, revisionID, pk, allDims, metricDimIDs, dimIDToName, defs)
		if calcErr != nil {
			s.log.Error().Err(calcErr).Str("metric", def.Name).Msg("calculation failed")
			s.store.MarkError(ctx, pk, calcErr.Error()) //nolint:errcheck
		} else {
			s.store.MarkClean(ctx, pk) //nolint:errcheck
		}
	}
	return nil
}

// runRecurrence claims every member of a recurrence component, executes it
// as one unit, and marks all members clean or all in error together.
func (s *Scheduler) runRecurrence(
	ctx context.Context, comp metricformula.Component, modelID, revisionID, timePartition string,
	allDims map[string]*rollup.Dimension, metricDimIDs map[string][]string, dimIDToName map[string]string, defs map[string]*MetricDef,
) {
	keys := make([]string, 0, len(comp.Members))
	for _, id := range comp.Members {
		def, ok := defs[id]
		if !ok || def.IsInput {
			continue
		}
		pk := BuildPartitionKey(modelID, revisionID, id, timePartition)
		if err := s.store.MarkDirty(ctx, []string{pk}, modelID, id, revisionID, timePartition); err != nil {
			s.log.Error().Err(err).Str("partition_key", pk).Msg("mark dirty")
			return
		}
		claimed, err := s.store.ClaimForCalculation(ctx, pk)
		if err != nil || !claimed {
			return
		}
		keys = append(keys, pk)
	}
	calcErr := s.executeRecurrence(ctx, comp, modelID, revisionID, BuildPartitionKey(modelID, revisionID, comp.Members[0], timePartition), allDims, metricDimIDs, dimIDToName, defs)
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

// executePartition resolves all dependency values and evaluates the formula
// once per declared leaf-level dimensional intersection of def's own
// dimensions (config-driven — grid_metric ⋈ grid_dimension, enumerated via
// rollup.LeafCombos — not whatever dim_members happen to already exist in
// fact_input), persisting one calc_result row per intersection in addition
// to the existing '{}' aggregate row every other consumer already reads.
func (s *Scheduler) executePartition(
	ctx context.Context, def *MetricDef, modelID, revisionID, partitionKey string,
	allDims map[string]*rollup.Dimension, metricDimIDs map[string][]string, dimIDToName map[string]string, allDefs map[string]*MetricDef,
) error {
	// A time-series formula (PREVIOUS, LAG, MOVINGSUM, ...) on a declared
	// time dimension takes the time-series path: one evaluation per leaf
	// period with a time context, time_summary for its totals. A time
	// function on a metric WITHOUT a time dimension is left to the scalar
	// path below, where it fails every cell with TIME_CONTEXT_REQUIRED — a
	// validation error, never a silent zero. An ordinary formula on a time
	// dimension stays on the scalar path: its leaves are period-independent,
	// and only how they reduce across time changes (time_summary, below).
	axis, err := timeAxisFor(allDims, metricDimIDs[def.ID])
	if err != nil {
		return err
	}
	if axis != nil && usesTimeSeries(def.Formula) {
		return s.executeTimeSeries(ctx, def, modelID, revisionID, partitionKey, axis, allDims, metricDimIDs, dimIDToName, allDefs)
	}

	// Bulk-prefetch each dependency's recorded values ONCE (not once per
	// leaf combo) — this is what makes full leaf-combo enumeration viable
	// instead of the old fact-driven "only combos that already have data"
	// shortcut. RawValue's ok=false ⇒ value=0 contract (satisfied naturally
	// here: a map miss returns the zero value) is what lets rollup.Resolve's
	// aggregation tiers sum/average/count a genuinely-missing dependency as
	// 0 without any special-casing in this function.
	fetch := make(map[string]rollup.RawValue, len(def.DependsOnID))
	for _, depID := range def.DependsOnID {
		depDef, ok := allDefs[depID]
		if !ok {
			return fmt.Errorf("dependency %s not found in model", depID)
		}
		var valueMap map[string]float64
		var err error
		if depDef.IsInput {
			valueMap, err = s.store.LoadInputValueMap(ctx, modelID, revisionID, depID)
		} else {
			valueMap, err = s.store.LoadCalcValueMap(ctx, modelID, revisionID, depID)
		}
		if err != nil {
			return fmt.Errorf("load values for %s: %w", depDef.Name, err)
		}
		fetch[depID] = func(_ context.Context, _ string, combo map[string]string) (float64, bool, error) {
			v, ok := valueMap[dimKey(combo)]
			return v, ok, nil
		}
	}

	// evalOne's second return is only meaningful alongside an error: true
	// means the formula failed while EVERY referenced metric resolved to
	// zero at this combo — the signature of an intersection that simply has
	// no data (absence resolves to 0, and calc dependencies persist literal
	// 0 rows at empty intersections, so absence cannot be told from zero
	// here and doesn't need to be). The caller skips such combos instead of
	// recording them as calculation failures.
	evalOne := func(combo map[string]string) (float64, bool, error) {
		values := make(map[string]float64, len(def.DependsOnID))
		for _, depID := range def.DependsOnID {
			depDef := allDefs[depID]
			v, _, err := rollup.ResolveTime(ctx, allDims, depID, metricDimIDs[depID], rollup.AggRule(depDef.AggRule),
				rollup.TimeSummaryRule(depDef.TimeSummary), combo, fetch[depID])
			if err != nil {
				return 0, false, fmt.Errorf("resolve %s: %w", depDef.Name, err)
			}
			values[depDef.Name] = v // bind unconditionally: preserves "0 on miss" formula-variable semantics
		}
		// Translate stored {dim_id: member_code} into named vars for the formula.
		namedDims := make(map[string]string, len(combo))
		for dimID, memberCode := range combo {
			if name, ok := dimIDToName[dimID]; ok {
				namedDims[name] = memberCode
			}
		}
		v, err := EvaluateWithDims(def.Formula, values, namedDims)
		if err != nil {
			noData := len(def.DependsOnID) > 0
			for _, dv := range values {
				if dv != 0 {
					noData = false
					break
				}
			}
			return 0, noData, err
		}
		return v, false, nil
	}

	leafCombos := rollup.LeafCombos(allDims, metricDimIDs[def.ID])
	if len(leafCombos) == 0 {
		leafCombos = []map[string]string{{}}
	}

	// A pure ratio (agg_rule 'average', e.g. a utilization %) has no
	// well-defined per-intersection value — persisting identical per-combo
	// rows for it wouldn't be "per-intersection" in any real sense — unless
	// the formula is itself dimension-conditional and must run per combo,
	// in which case per-combo results are legitimately distinct and are
	// averaged below as usual.
	if def.AggRule == "average" && len(leafCombos) > 1 && !FormulaReferencesDims(def.Formula, dimIDToName) {
		leafCombos = []map[string]string{{}}
	}

	results := make([]CalcResultRow, 0, len(leafCombos))
	var failures, skipped int
	var firstErr error
	for _, combo := range leafCombos {
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
	if def.AggRule == string(rollup.AggFormula) || def.AggRule == string(rollup.AggRate) {
		const rollupComboCap = 20000
		for _, combo := range rollup.RollupCombos(allDims, metricDimIDs[def.ID], rollupComboCap) {
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
			if err := s.store.ClearPerComboResults(ctx, modelID, revisionID, def.ID); err != nil {
				return fmt.Errorf("clear stale per-combo results: %w", err)
			}
			return nil
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
	// is the difference from the 'average' shortcut further up, which throws
	// the per-member rows away entirely.
	aggregate := rollup.CombineAgg(vals, rollup.AggRule(def.AggRule))
	writeAggregate := true
	// On a time dimension, a combining rule (sum/average/count) reduces the
	// non-time dimensions first and time last, by the metric's time_summary
	// — a closing balance totals as its LAST period, not the sum of every
	// month's balance. 'formula' and 'rate' are untouched: their total is
	// the formula re-evaluated at the aggregate, across time as across any
	// other dimension (Anaplan's Formula summary), so a margin percentage
	// stays total_margin / total_revenue rather than a sum of quarterly
	// percentages.
	if axis != nil && def.AggRule != string(rollup.AggFormula) && def.AggRule != string(rollup.AggRate) {
		aggregate, writeAggregate = summarizeOverTime(results, axis, def.AggRule, def.TimeSummary, nil)
	}
	if def.AggRule == string(rollup.AggFormula) {
		total, _, totalErr := evalOne(map[string]string{})
		if totalErr != nil {
			// Falling back to the combined value would quietly publish the very
			// number this rule exists to avoid, so the metric gets no fresh row
			// instead — the same stance the all-combos-failed branch takes.
			return fmt.Errorf("total-level evaluation failed for metric %s (agg_rule=formula): %w", def.Name, totalErr)
		}
		aggregate = total
	}
	if def.AggRule == string(rollup.AggRate) {
		total, rateErr := s.rateTotal(ctx, def, modelID, revisionID, allDims, metricDimIDs, allDefs)
		if rateErr != nil {
			return fmt.Errorf("ratio total failed for metric %s (agg_rule=rate): %w", def.Name, rateErr)
		}
		aggregate = total
	}
	// Write the aggregate FIRST: every existing external reader (/api/metrics,
	// grid()'s totals) depends only on this '{}' row, so if a per-combo write
	// below fails partway through, the one row everything else already reads
	// is safely persisted regardless. (time_summary 'none' writes no total.)
	if writeAggregate {
		if err := s.store.WriteCalcResult(ctx, modelID, revisionID, def.ID, partitionKey, map[string]string{}, aggregate); err != nil {
			return fmt.Errorf("write aggregate: %w", err)
		}
	}

	// The single-combo case (leafCombos collapsed to just {} above, or the
	// metric has no declared dims at all) is already fully covered by the
	// aggregate write above and must not be double-written.
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
	dimConditional := FormulaReferencesDims(def.Formula, dimIDToName)
	perComboRows = append(perComboRows, oneDimSliceRows(def.AggRule, dimConditional, metricDimIDs[def.ID], allDims, results, evalOne, axis, def.TimeSummary)...)
	perComboRows = append(perComboRows, aggregatePeriodRows(def.AggRule, metricDimIDs[def.ID], results, axis, def.TimeSummary)...)
	// This recompute's per-combo set is authoritative: clear the previous
	// set first, or intersections that lost their data since the last run
	// (skipped above, so absent from perComboRows) would keep serving their
	// old values forever.
	if err := s.store.ClearPerComboResults(ctx, modelID, revisionID, def.ID); err != nil {
		return fmt.Errorf("clear stale per-combo results: %w", err)
	}
	if err := s.store.WriteCalcResults(ctx, modelID, revisionID, def.ID, partitionKey, perComboRows); err != nil {
		return fmt.Errorf("write per-intersection results: %w", err)
	}

	if failures > 0 {
		return fmt.Errorf("%d/%d combos failed to evaluate for metric %s (first error: %w)", failures, len(leafCombos)-skipped, def.Name, firstErr)
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

	var out []CalcResultRow
	for _, dimID := range dimIDs {
		d := dims[dimID]
		if d == nil {
			continue
		}
		childrenOf := map[string][]string{}
		for _, m := range d.Members {
			if m.ParentCode != "" {
				childrenOf[m.ParentCode] = append(childrenOf[m.ParentCode], m.Code)
			}
		}
		for _, m := range d.Members {
			slice := map[string]string{dimID: m.Code}
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
			if axis != nil && (dimID != axis.dim.ID || !m.IsLeafPeriod()) {
				// A non-time member reduces time by the summary; so does an
				// AGGREGATE period (H1 = its quarters reduced). A leaf period
				// is a single period and falls through to the plain combine.
				if v, ok := summarizeOverTime(leafResults, axis, aggRule, timeSummaryRule, map[string]map[string]bool{dimID: sub}); ok {
					out = append(out, CalcResultRow{DimMembers: slice, Value: v})
				}
				continue
			}
			vals := make([]float64, 0)
			for _, r := range leafResults {
				if code, ok := r.DimMembers[dimID]; ok && sub[code] {
					vals = append(vals, r.Value)
				}
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
		var valueMap map[string]float64
		var err error
		if d.IsInput {
			valueMap, err = s.store.LoadInputValueMap(ctx, modelID, revisionID, id)
		} else {
			valueMap, err = s.store.LoadCalcValueMap(ctx, modelID, revisionID, id)
		}
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

// FormulaReferencesDims reports whether expr's formula text references any
// of the given dimension names as a bare identifier — used to decide whether
// a pure-ratio (agg_rule "average") metric's formula is itself
// dimension-conditional and must be evaluated per leaf combo, or whether it
// collapses to one revision-wide ratio-of-sums evaluation. Exported so
// internal/gateway's scopeCalcCells (the hidden-member-restricted grid path)
// can apply the exact same collapse rule executePartition does below —
// duplicating this logic there previously caused the two paths to disagree
// on a metric's value for a restricted vs. unrestricted caller.
func FormulaReferencesDims(expr string, dimIDToName map[string]string) bool {
	if len(dimIDToName) == 0 {
		return false
	}
	names := make(map[string]bool, len(dimIDToName))
	for _, n := range dimIDToName {
		names[n] = true
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
		if names[expr[i:j]] {
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

func isAlreadyExists(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "stream name already in use"))
}
