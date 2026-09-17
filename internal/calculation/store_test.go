package calculation_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// TestLoadModelMetricsIsRevisionIsolated proves LoadModelMetrics (and, by
// extension, the recalc it feeds) only ever sees one revision's own metric
// definitions — never a same-named metric's UUID from a sibling revision.
// Mirrors migration 027's own scenario: two revisions of the same model,
// each with its own "revenue" metric_def row under a distinct UUID.
func TestLoadModelMetricsIsRevisionIsolated(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const model = "00000000-0000-0000-0000-000000000020"
	const revA = "00000000-0000-0000-0020-0000000000a0"
	const revB = "00000000-0000-0000-0020-0000000000b0"

	revenueA := insertMetric(t, store, ctx, model, revA, "revenue", "", true)
	revenueB := insertMetric(t, store, ctx, model, revB, "revenue", "", true)

	defsA, err := store.LoadModelMetrics(ctx, model, revA)
	if err != nil {
		t.Fatalf("LoadModelMetrics(revA): %v", err)
	}
	if _, ok := defsA[revenueA]; !ok {
		t.Errorf("revA's own revenue metric %s missing from %v", revenueA, defsA)
	}
	if _, ok := defsA[revenueB]; ok {
		t.Errorf("revB's revenue metric %s leaked into revA's LoadModelMetrics result", revenueB)
	}

	defsB, err := store.LoadModelMetrics(ctx, model, revB)
	if err != nil {
		t.Fatalf("LoadModelMetrics(revB): %v", err)
	}
	if _, ok := defsB[revenueB]; !ok {
		t.Errorf("revB's own revenue metric %s missing from %v", revenueB, defsB)
	}
	if _, ok := defsB[revenueA]; ok {
		t.Errorf("revA's revenue metric %s leaked into revB's LoadModelMetrics result", revenueA)
	}
}

// TestRecalcAffectedIsolatesRevisions proves a full recalc against one
// revision doesn't write calc_result rows visible under a sibling revision
// sharing the same metric name — the same isolation, exercised end to end
// through RecalcAffected rather than just the loader.
func TestRecalcAffectedIsolatesRevisions(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const model = "00000000-0000-0000-0000-000000000021"
	const revA = "00000000-0000-0000-0021-0000000000a1"
	const revB = "00000000-0000-0000-0021-0000000000b1"

	baseA := insertMetric(t, store, ctx, model, revA, "base", "", true)
	doubleA := insertMetric(t, store, ctx, model, revA, "double", `=base * 2`, false)
	insertDep(t, store, ctx, doubleA, baseA)
	insertFact(t, store, ctx, model, revA, baseA, "{}", 10)

	baseB := insertMetric(t, store, ctx, model, revB, "base", "", true)
	doubleB := insertMetric(t, store, ctx, model, revB, "double", `=base * 2`, false)
	insertDep(t, store, ctx, doubleB, baseB)
	insertFact(t, store, ctx, model, revB, baseB, "{}", 1000)

	sched := calculation.NewScheduler(logger.New("calc-test"), store, nil)
	if err := sched.RecalcAffected(ctx, model, revA, []string{baseA}); err != nil {
		t.Fatalf("RecalcAffected(revA): %v", err)
	}

	assertCalc(t, store, ctx, model, revA, doubleA, map[string]string{}, 20)
	// revB's own "double" must still read as 0 — revA's recalc must not have
	// touched it, and revB's own recalc hasn't run yet.
	assertCalc(t, store, ctx, model, revB, doubleB, map[string]string{}, 0)

	if err := sched.RecalcAffected(ctx, model, revB, []string{baseB}); err != nil {
		t.Fatalf("RecalcAffected(revB): %v", err)
	}
	assertCalc(t, store, ctx, model, revA, doubleA, map[string]string{}, 20)
	assertCalc(t, store, ctx, model, revB, doubleB, map[string]string{}, 2000)
}

// TestPartitionMarkedErrorOnPerComboFailure proves a per-combo evaluation
// failure now surfaces as a real, visible partition error (MarkError, not
// MarkClean) instead of silently proceeding with an incomplete aggregate —
// while still persisting every combo that DID evaluate successfully.
func TestPartitionMarkedErrorOnPerComboFailure(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const model = "00000000-0000-0000-0000-000000000022"
	const rev = "00000000-0000-0000-0022-000000000022"

	dimID := insertDim(t, store, ctx, model, "department")
	insertMember(t, store, ctx, dimID, "OK", "OK")
	insertMember(t, store, ctx, dimID, "BAD", "BAD")
	baseID := insertMetric(t, store, ctx, model, rev, "base", "", true)
	// Divides by a dimension-conditional denominator that is zero (and
	// unguarded by IFERROR) for the BAD department only, so exactly one
	// combo's evaluation genuinely errors while OK's own succeeds.
	riskyID := insertMetric(t, store, ctx, model, rev, "risky",
		`=base / IF(department = "BAD", 0, 1)`, false)
	insertDep(t, store, ctx, riskyID, baseID)
	insertGridSetup(t, store, ctx, model, []string{dimID}, []string{baseID, riskyID})

	insertFact(t, store, ctx, model, rev, baseID, `{"`+dimID+`": "OK"}`, 50)
	insertFact(t, store, ctx, model, rev, baseID, `{"`+dimID+`": "BAD"}`, 99)

	sched := calculation.NewScheduler(logger.New("calc-test"), store, nil)
	if err := sched.RecalcAffected(ctx, model, rev, []string{baseID}); err != nil {
		t.Fatalf("RecalcAffected: %v", err)
	}

	timePartition := calculation.PartitionMonth(time.Now())
	pk := calculation.BuildPartitionKey(model, rev, riskyID, timePartition)
	state, err := store.GetPartitionState(ctx, pk)
	if err != nil {
		t.Fatalf("GetPartitionState: %v", err)
	}
	if state.Status.String() != "PARTITION_STATUS_ERROR" {
		t.Errorf("partition status = %v, want PARTITION_STATUS_ERROR", state.Status)
	}

	assertCalc(t, store, ctx, model, rev, riskyID, map[string]string{dimID: "OK"}, 50)
}

// TestLoadAllCalcValueMaps proves the bulk read returns every calc metric's
// every combo in one call — grid()'s read path needs this precisely because
// LoadCalcValueMap (its one-metric-at-a-time sibling, used by the scheduler's
// own dependency prefetch) would otherwise need one query per calc metric on
// a grid. Two independent calc metrics sharing the same input dependency
// prove the result is correctly partitioned by metric, not flattened
// together.
func TestLoadAllCalcValueMaps(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	const model = "00000000-0000-0000-0000-000000000023"
	const rev = "00000000-0000-0000-0023-000000000023"

	dimID := insertDim(t, store, ctx, model, "department")
	insertMember(t, store, ctx, dimID, "OK", "OK")
	insertMember(t, store, ctx, dimID, "BAD", "BAD")
	baseID := insertMetric(t, store, ctx, model, rev, "base", "", true)
	doubleID := insertMetric(t, store, ctx, model, rev, "double", `=base * 2`, false)
	tripleID := insertMetric(t, store, ctx, model, rev, "triple", `=base * 3`, false)
	insertDep(t, store, ctx, doubleID, baseID)
	insertDep(t, store, ctx, tripleID, baseID)
	insertGridSetup(t, store, ctx, model, []string{dimID}, []string{baseID, doubleID, tripleID})

	insertFact(t, store, ctx, model, rev, baseID, `{"`+dimID+`": "OK"}`, 10)
	insertFact(t, store, ctx, model, rev, baseID, `{"`+dimID+`": "BAD"}`, 20)
	runRecalc(t, store, ctx, model, rev, []string{baseID})

	all, err := store.LoadAllCalcValueMaps(ctx, model, rev)
	if err != nil {
		t.Fatalf("LoadAllCalcValueMaps: %v", err)
	}

	key := func(dims map[string]string) string {
		b, _ := json.Marshal(dims)
		return string(b)
	}

	double, ok := all[doubleID]
	if !ok {
		t.Fatalf("expected an entry for double (%s), got keys %v", doubleID, mapKeys(all))
	}
	if v := double[key(map[string]string{dimID: "OK"})]; v != 20 {
		t.Errorf("double[OK] = %v, want 20", v)
	}
	if v := double[key(map[string]string{dimID: "BAD"})]; v != 40 {
		t.Errorf("double[BAD] = %v, want 40", v)
	}
	if v := double[key(map[string]string{})]; v != 60 {
		t.Errorf("double[{}] (aggregate) = %v, want 60", v)
	}

	triple, ok := all[tripleID]
	if !ok {
		t.Fatalf("expected an entry for triple (%s), got keys %v", tripleID, mapKeys(all))
	}
	if v := triple[key(map[string]string{dimID: "OK"})]; v != 30 {
		t.Errorf("triple[OK] = %v, want 30", v)
	}
	if v := triple[key(map[string]string{dimID: "BAD"})]; v != 60 {
		t.Errorf("triple[BAD] = %v, want 60", v)
	}
	if v := triple[key(map[string]string{})]; v != 90 {
		t.Errorf("triple[{}] (aggregate) = %v, want 90", v)
	}
}

func mapKeys(m map[string]map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
