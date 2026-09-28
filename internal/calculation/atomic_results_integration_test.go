package calculation_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// TestRecalcNeverExposesAnEmptyResultSet: a recalculation replaces a
// metric's rows (clear the previous per-combo set, write the new one) in
// one transaction, so a reader running alongside it always sees a full
// set — the previous one or the new one — never the metric with no cells.
// Before, the scalar and the time-series paths cleared and wrote in
// separate statements, and the live harness saw ~1 read in 7 with every
// DE/UK cell of a metric missing.
func TestRecalcNeverExposesAnEmptyResultSet(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000a7"
	revID := "00000000-0000-0000-0001-0000000000a7"

	regionID := regionFixture(t, store, ctx, modelID, revID)
	monthID, months := monthHierarchy(t, store, ctx, modelID, revID, 1)
	revenueID := insertMetric(t, store, ctx, modelID, revID, "revenue", "", true)
	salesID := insertMetric(t, store, ctx, modelID, revID, "sales", "", true)
	scalarID := insertCalc(t, store, ctx, modelID, revID, "doubled", "revenue * 2")
	seriesID := insertCalc(t, store, ctx, modelID, revID, "prev", "PREVIOUS(sales)")
	insertGridSetup(t, store, ctx, modelID, []string{regionID}, []string{revenueID, scalarID})
	insertGridSetup(t, store, ctx, modelID, []string{regionID, monthID}, []string{salesID, seriesID})
	for i, r := range []string{"DE", "FR", "US"} {
		insertFact(t, store, ctx, modelID, revID, revenueID, factJSON(regionID, r), float64(10*(i+1)))
		for _, m := range months[:3] {
			insertFact(t, store, ctx, modelID, revID, salesID, factJSON(regionID, r, monthID, m), float64(i+1))
		}
	}
	inputs := []string{revenueID, salesID}
	runRecalc(t, store, ctx, modelID, revID, inputs)
	c := rowChecker{t, store, ctx, modelID, revID, map[string]string{scalarID: "doubled", seriesID: "prev"}}
	c.want(scalarID, pins(regionID, "DE"), 20)
	c.want(seriesID, pins(regionID, "FR", monthID, months[1]), 2)

	count := func(metricID string) (int, error) {
		var n int
		err := store.Pool().QueryRow(ctx, `
			SELECT count(*) FROM runtime.calc_result
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid AND dim_members::text <> '{}'`,
			modelID, revID, metricID).Scan(&n)
		return n, err
	}
	var stop atomic.Bool
	var reads, empty atomic.Int64
	var wg sync.WaitGroup
	for _, id := range []string{scalarID, seriesID} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for !stop.Load() {
				n, err := count(id)
				if err != nil {
					t.Errorf("read: %v", err)
					return
				}
				reads.Add(1)
				if n == 0 {
					empty.Add(1)
				}
			}
		}(id)
	}
	for i := 0; i < 40; i++ {
		runRecalc(t, store, ctx, modelID, revID, inputs)
	}
	stop.Store(true)
	wg.Wait()

	if reads.Load() < 40 {
		t.Fatalf("only %d concurrent reads ran; the test proves nothing", reads.Load())
	}
	if n := empty.Load(); n > 0 {
		t.Errorf("%d of %d concurrent reads saw a metric with no cells during recalculation", n, reads.Load())
	}
	c.want(scalarID, pins(regionID, "US"), 60)
	c.want(seriesID, pins(regionID, "FR", monthID, months[1]), 2)
}
