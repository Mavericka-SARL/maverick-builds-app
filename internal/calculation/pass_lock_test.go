package calculation_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// Two gateways recalculated one revision at once: the pass queue and the
// revision lock were per process, so a pass that read its inputs before
// the other gateway's write could finish last and leave stale results. A
// pass now waits for the database's pass lock that another gateway's pass
// holds, and gives it back when it ends.
func TestPassWaitsForAnotherGatewaysPass(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	const modelID, revisionID = "00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0001-000000000001"
	id := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := store.Pool().QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	revenue := id(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'revenue', true) RETURNING id::text`, modelID, revisionID)
	cogs := id(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'cogs', true) RETURNING id::text`, modelID, revisionID)
	profit := id(`INSERT INTO model.metric_def (model_id, revision_id, name, formula, is_input) VALUES ($1::uuid, $2::uuid, 'gross_profit', '{revenue} - {cogs}', false) RETURNING id::text`, modelID, revisionID)
	if _, err := store.Pool().Exec(ctx, `INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid), ($1::uuid, $3::uuid)`, profit, revenue, cogs); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
		VALUES ($1::uuid, $2::uuid, $3::uuid, '{}', 1000), ($1::uuid, $2::uuid, $4::uuid, '{}', 600)`, modelID, revisionID, revenue, cogs); err != nil {
		t.Fatal(err)
	}

	// The other gateway: its own pool on the same database, so its pass
	// queue is not this one's.
	otherPool, err := pgxpool.NewWithConfig(ctx, store.Pool().Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer otherPool.Close()
	release, err := calculation.HoldPassLock(calculation.NewStore(otherPool), revisionID)
	if err != nil {
		t.Fatalf("the other gateway's pass lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- calculation.NewScheduler(logger.New("calc-test"), store).RecalcSpecific(ctx, modelID, revisionID, []string{profit})
	}()
	select {
	case err := <-done:
		release()
		t.Fatalf("the pass ran while another gateway's pass held the revision (err=%v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	var rows int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM runtime.calc_result WHERE metric_id = $1::uuid`, profit).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d results written while the other pass held the revision", rows)
	}

	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the pass after the other gateway's: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the pass never ran after the other gateway's pass ended")
	}
	if got, err := store.GetCalcValue(ctx, modelID, revisionID, profit, map[string]string{}); err != nil || got != 400 {
		t.Fatalf("gross_profit = %v err=%v, want 400", got, err)
	}

	// The pass gave its lock back: no advisory lock is left held once its
	// connection's backend has gone.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var held int
		if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted`).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d advisory locks still held after the passes", held)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
