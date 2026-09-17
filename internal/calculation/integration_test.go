package calculation_test

import (
	"context"
	"embed"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

//go:embed testdata/*.sql
var testMigrations embed.FS

// setupDB spins up a postgres container, runs migrations, and returns a Store.
func setupDB(t *testing.T) (*calculation.Store, func()) {
	t.Helper()
	ctx := context.Background()

	pgc, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("mavericks"),
		tcpostgres.WithUsername("mavericks"),
		tcpostgres.WithPassword("mavericks"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}

	dsn, err := pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = pgc.Terminate(ctx)
		t.Fatalf("connection string: %v", err)
	}

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		_ = pgc.Terminate(ctx)
		t.Fatalf("connect: %v", err)
	}

	if err := migrate.Run(ctx, pool, testMigrations, "testdata"); err != nil {
		pool.Close()
		_ = pgc.Terminate(ctx)
		t.Fatalf("migrate: %v", err)
	}

	cleanup := func() {
		pool.Close()
		_ = pgc.Terminate(ctx)
	}
	return calculation.NewStore(pool), cleanup
}

// TestRecalcAffected verifies the full calculation pipeline:
//
//	fact_input rows → RecalcAffected → calc_result row with correct value
func TestRecalcAffected(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()

	ctx := context.Background()
	log := logger.New("calc-test")

	modelID := "00000000-0000-0000-0000-000000000001"
	revisionID := "00000000-0000-0000-0001-000000000001"

	var revenueID, cogsID, grossProfitID string
	err := store.Pool().QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, revision_id, name, is_input)
		VALUES ($1::uuid, $2::uuid, 'revenue', true)
		RETURNING id::text
	`, modelID, revisionID).Scan(&revenueID)
	if err != nil {
		t.Fatalf("insert revenue: %v", err)
	}

	err = store.Pool().QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, revision_id, name, is_input)
		VALUES ($1::uuid, $2::uuid, 'cogs', true)
		RETURNING id::text
	`, modelID, revisionID).Scan(&cogsID)
	if err != nil {
		t.Fatalf("insert cogs: %v", err)
	}

	err = store.Pool().QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, revision_id, name, formula, is_input)
		VALUES ($1::uuid, $2::uuid, 'gross_profit', '{revenue} - {cogs}', false)
		RETURNING id::text
	`, modelID, revisionID).Scan(&grossProfitID)
	if err != nil {
		t.Fatalf("insert gross_profit: %v", err)
	}

	_, err = store.Pool().Exec(ctx, `
		INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id)
		VALUES ($1::uuid, $2::uuid), ($1::uuid, $3::uuid)
	`, grossProfitID, revenueID, cogsID)
	if err != nil {
		t.Fatalf("insert dependencies: %v", err)
	}

	_, err = store.Pool().Exec(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
		VALUES ($1::uuid, $2::uuid, $3::uuid, '{}', 1000),
		       ($1::uuid, $2::uuid, $4::uuid, '{}', 600)
	`, modelID, revisionID, revenueID, cogsID)
	if err != nil {
		t.Fatalf("insert facts: %v", err)
	}

	scheduler := calculation.NewScheduler(log, store, nil)
	if err := scheduler.RecalcAffected(ctx, modelID, revisionID, []string{revenueID, cogsID}); err != nil {
		t.Fatalf("RecalcAffected: %v", err)
	}

	got, err := store.GetCalcValue(ctx, modelID, revisionID, grossProfitID, map[string]string{})
	if err != nil {
		t.Fatalf("GetCalcValue: %v", err)
	}
	const want = 400.0
	if got != want {
		t.Errorf("gross_profit = %v, want %v", got, want)
	}
}

// TestRecalcPartitionState checks that the partition state machine transitions correctly.
func TestRecalcPartitionState(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()

	ctx := context.Background()
	log := logger.New("calc-test")

	modelID := "00000000-0000-0000-0000-000000000002"
	revisionID := "00000000-0000-0000-0002-000000000002"

	var inputID, calcID string
	_ = store.Pool().QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, revision_id, name, is_input)
		VALUES ($1::uuid, $2::uuid, 'sales', true) RETURNING id::text
	`, modelID, revisionID).Scan(&inputID)

	_ = store.Pool().QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, revision_id, name, formula, is_input)
		VALUES ($1::uuid, $2::uuid, 'double_sales', '{sales} * 2', false) RETURNING id::text
	`, modelID, revisionID).Scan(&calcID)

	_, _ = store.Pool().Exec(ctx, `
		INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id)
		VALUES ($1::uuid, $2::uuid)
	`, calcID, inputID)

	_, _ = store.Pool().Exec(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
		VALUES ($1::uuid, $2::uuid, $3::uuid, '{}', 50)
	`, modelID, revisionID, inputID)

	scheduler := calculation.NewScheduler(log, store, nil)
	if err := scheduler.RecalcAffected(ctx, modelID, revisionID, []string{inputID}); err != nil {
		t.Fatalf("RecalcAffected: %v", err)
	}

	// Partition key format: modelID:rev:revisionID:metricID:timePartition
	timePartition := time.Now().Format("2006-01")
	pk := modelID + ":rev:" + revisionID + ":" + calcID + ":" + timePartition

	state, err := store.GetPartitionState(ctx, pk)
	if err != nil {
		t.Fatalf("GetPartitionState: %v", err)
	}
	if state.Status.String() != "PARTITION_STATUS_CLEAN" {
		t.Errorf("partition status = %v, want PARTITION_STATUS_CLEAN", state.Status)
	}

	got, err := store.GetCalcValue(ctx, modelID, revisionID, calcID, map[string]string{})
	if err != nil {
		t.Fatalf("GetCalcValue: %v", err)
	}
	if got != 100.0 {
		t.Errorf("double_sales = %v, want 100", got)
	}
}

// TestRecalcAffected_FormulaErrorWritesNoRow is a regression test: a calc
// metric whose formula errors on every evaluation attempt (here, division
// by zero) must not get a calc_result row at all — writing one with a
// fabricated value (CombineAgg of zero successful combos is 0 for both
// "sum" and "average") would make a real formula error (div-by-zero, an
// unknown function, ...) silently look like a genuinely-computed result to
// every reader that trusts calc_result (/api/metrics, grid() totals).
// GetCalcValue can't distinguish "no row" from "a row whose value is 0", so
// this asserts directly against runtime.calc_result's row count instead.
func TestRecalcAffected_FormulaErrorWritesNoRow(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()

	ctx := context.Background()
	log := logger.New("calc-test")

	modelID := "00000000-0000-0000-0000-000000000003"
	revisionID := "00000000-0000-0000-0003-000000000003"

	var zeroID, brokenID string
	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, revision_id, name, is_input)
		VALUES ($1::uuid, $2::uuid, 'zero_input', true)
		RETURNING id::text
	`, modelID, revisionID).Scan(&zeroID); err != nil {
		t.Fatalf("insert zero_input: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, revision_id, name, formula, is_input)
		VALUES ($1::uuid, $2::uuid, 'broken_ratio', '1/{zero_input}', false)
		RETURNING id::text
	`, modelID, revisionID).Scan(&brokenID); err != nil {
		t.Fatalf("insert broken_ratio: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id)
		VALUES ($1::uuid, $2::uuid)
	`, brokenID, zeroID); err != nil {
		t.Fatalf("insert dependency: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
		VALUES ($1::uuid, $2::uuid, $3::uuid, '{}', 0)
	`, modelID, revisionID, zeroID); err != nil {
		t.Fatalf("insert fact: %v", err)
	}

	scheduler := calculation.NewScheduler(log, store, nil)
	// The per-metric evaluation failure is caught and logged inside
	// RecalcAffected's own loop, not propagated as its return value — this
	// call succeeding is expected, not a gap in the test.
	if err := scheduler.RecalcAffected(ctx, modelID, revisionID, []string{zeroID}); err != nil {
		t.Fatalf("RecalcAffected: %v", err)
	}

	var rowCount int
	if err := store.Pool().QueryRow(ctx, `
		SELECT count(*) FROM runtime.calc_result WHERE model_id = $1::uuid AND metric_id = $2::uuid
	`, modelID, brokenID).Scan(&rowCount); err != nil {
		t.Fatalf("count calc_result rows: %v", err)
	}
	if rowCount != 0 {
		t.Errorf("broken_ratio has %d calc_result row(s), want 0 — a div-by-zero formula error must not fabricate a value", rowCount)
	}
}

// TestRecalcSpecificForcesRecomputeIgnoringGraphReachability is a
// regression test for the gap RecalcAffected has that RecalcSpecific
// exists to close: when a metric is deleted, its dependents'
// model.calc_dependency edges cascade away in the same statement — so
// RecalcAffected's own AffectedMetricIDs graph walk (rooted at the deleted
// metric) can no longer discover them afterward, and they'd stay silently
// frozen at their stale pre-deletion value forever. RecalcSpecific must
// force-recompute an explicitly given metric ID regardless of whether the
// current dependency graph still reaches it from anything — surfacing the
// #NAME? error via MarkError, not silently no-op'ing.
func TestRecalcSpecificForcesRecomputeIgnoringGraphReachability(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()

	ctx := context.Background()
	log := logger.New("calc-test")

	modelID := "00000000-0000-0000-0000-000000000004"
	revisionID := "00000000-0000-0000-0004-000000000004"

	var travelCostID, totalCostID string
	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, revision_id, name, is_input)
		VALUES ($1::uuid, $2::uuid, 'travel_cost', true)
		RETURNING id::text
	`, modelID, revisionID).Scan(&travelCostID); err != nil {
		t.Fatalf("insert travel_cost: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, revision_id, name, formula, is_input)
		VALUES ($1::uuid, $2::uuid, 'total_cost', '{travel_cost} * 2', false)
		RETURNING id::text
	`, modelID, revisionID).Scan(&totalCostID); err != nil {
		t.Fatalf("insert total_cost: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id)
		VALUES ($1::uuid, $2::uuid)
	`, totalCostID, travelCostID); err != nil {
		t.Fatalf("insert dependency: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
		VALUES ($1::uuid, $2::uuid, $3::uuid, '{}', 100)
	`, modelID, revisionID, travelCostID); err != nil {
		t.Fatalf("insert fact: %v", err)
	}

	scheduler := calculation.NewScheduler(log, store, nil)
	if err := scheduler.RecalcAffected(ctx, modelID, revisionID, []string{travelCostID}); err != nil {
		t.Fatalf("initial RecalcAffected: %v", err)
	}
	if got, err := store.GetCalcValue(ctx, modelID, revisionID, totalCostID, map[string]string{}); err != nil || got != 200.0 {
		t.Fatalf("total_cost before deletion = %v, err = %v, want 200 — fixture assumption broken", got, err)
	}

	// Simulate deleting travel_cost: the DB's ON DELETE CASCADE on
	// calc_dependency.depends_on_metric_id removes total_cost's edge to it
	// in the same statement as the metric_def row itself.
	if _, err := store.Pool().Exec(ctx, `DELETE FROM model.metric_def WHERE id=$1::uuid`, travelCostID); err != nil {
		t.Fatalf("delete travel_cost: %v", err)
	}
	var depCount int
	_ = store.Pool().QueryRow(ctx, `SELECT count(*) FROM model.calc_dependency WHERE metric_id=$1::uuid`, totalCostID).Scan(&depCount)
	if depCount != 0 {
		t.Fatalf("expected the cascade to remove total_cost's dependency edge, %d remain", depCount)
	}

	// A normal RecalcAffected call has nothing to root the walk from
	// anymore — total_cost is no longer graph-reachable from any input, so
	// this is a deliberate no-op, not a bug in RecalcAffected itself.
	if err := scheduler.RecalcAffected(ctx, modelID, revisionID, []string{travelCostID}); err != nil {
		t.Fatalf("RecalcAffected after deletion: %v", err)
	}

	// RecalcSpecific, given total_cost's ID explicitly (as
	// developerMetricAction's DELETE case now captures before deleting),
	// must still attempt it — the formula text still says "{travel_cost}",
	// which can no longer resolve, so this must surface as an error, not
	// silently leave the stale pre-deletion calc_result untouched.
	if err := scheduler.RecalcSpecific(ctx, modelID, revisionID, []string{totalCostID}); err != nil {
		t.Fatalf("RecalcSpecific: %v", err)
	}

	timePartition := time.Now().Format("2006-01")
	pk := modelID + ":rev:" + revisionID + ":" + totalCostID + ":" + timePartition
	state, err := store.GetPartitionState(ctx, pk)
	if err != nil {
		t.Fatalf("GetPartitionState: %v", err)
	}
	if state.Status.String() != "PARTITION_STATUS_ERROR" {
		t.Errorf("partition status = %v, want PARTITION_STATUS_ERROR (formula references a deleted metric)", state.Status)
	}
	if state.Error == "" {
		t.Error("expected a non-empty error message referencing the unresolvable travel_cost reference")
	}
}
