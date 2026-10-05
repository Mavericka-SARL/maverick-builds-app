package calculation_test

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/calculation"
)

// Results stored by an older engine are recalculated once at start-up and
// the new version is recorded; a database already on it is left alone.
func TestEngineUpgradeRecalculatesStoredResultsOnce(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	const (
		model = "00000000-0000-0000-0000-0000000000e9"
		rev   = "00000000-0000-0000-0001-0000000000e9"
	)
	aID := insertMetric(t, store, ctx, model, rev, "a", "", true)
	dblID := insertMetric(t, store, ctx, model, rev, "dbl", "a * 2", false)
	insertDep(t, store, ctx, dblID, aID)
	insertFact(t, store, ctx, model, rev, aID, "{}", 21)
	runRecalc(t, store, ctx, model, rev, []string{aID})
	assertCalc(t, store, ctx, model, rev, dblID, map[string]string{}, 42)

	pool := store.Pool()
	stale := func() {
		if _, err := pool.Exec(ctx, `UPDATE runtime.calc_result SET value = 7 WHERE metric_id = $1::uuid`, dblID); err != nil {
			t.Fatal(err)
		}
	}
	stale() // what an older engine left behind
	calculation.RunEngineUpgrade(ctx, pool, zerolog.Nop())
	assertCalc(t, store, ctx, model, rev, dblID, map[string]string{}, 42)
	var version int
	if err := pool.QueryRow(ctx, `SELECT version FROM runtime.calc_engine_state`).Scan(&version); err != nil || version != calculation.EngineVersion {
		t.Fatalf("recorded version = %d (err %v), want %d", version, err, calculation.EngineVersion)
	}

	stale()
	calculation.RunEngineUpgrade(ctx, pool, zerolog.Nop()) // already on this version: nothing to do
	var v float64
	_ = pool.QueryRow(ctx, `SELECT value::float8 FROM runtime.calc_result WHERE metric_id = $1::uuid AND dim_members = '{}'::jsonb`, dblID).Scan(&v)
	if v != 7 {
		t.Errorf("a database on the current version was recalculated again (dbl = %v)", v)
	}
}
