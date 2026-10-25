package calculation_test

import (
	"context"
	"strings"
	"testing"
)

// A metric with more cells than CALC_MAX_METRIC_CELLS is not calculated —
// its partition says why, naming its size — nor is a metric that reads it;
// the rest of the model is. Two metrics of 2.5 million cells took the
// gateway past its memory limit, every tenant's requests with it
// (2026-10-22).
func TestAMetricWithTooManyCellsIsRefusedWithItsReaders(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	t.Setenv("CALC_MAX_METRIC_CELLS", "10")
	modelID := "00000000-0000-0000-0000-0000000000e7"
	revID := "00000000-0000-0000-0001-0000000000e7"

	storeID := insertDimRev(t, store, ctx, modelID, revID, "store")
	for _, c := range []string{"S1", "S2", "S3"} {
		addMember(t, store, ctx, storeID, c, "", nil)
	}
	skuID := insertDimRev(t, store, ctx, modelID, revID, "sku")
	for _, c := range []string{"A", "B", "C", "D"} {
		addMember(t, store, ctx, skuID, c, "", nil)
	}
	revenueID := insertMetric(t, store, ctx, modelID, revID, "revenue", "", true)
	bigID := insertCalc(t, store, ctx, modelID, revID, "big", "revenue * 2")
	insertGridSetup(t, store, ctx, modelID, []string{storeID, skuID}, []string{revenueID, bigID})
	readerID := insertCalc(t, store, ctx, modelID, revID, "reader", "big + 1")
	smallID := insertCalc(t, store, ctx, modelID, revID, "small", "revenue + 1")
	insertGridSetup(t, store, ctx, modelID, []string{storeID}, []string{readerID, smallID})
	insertFact(t, store, ctx, modelID, revID, revenueID, factJSON(storeID, "S1", skuID, "A"), 5)

	runRecalc(t, store, ctx, modelID, revID, []string{revenueID})

	if status, msg := partitionState(t, store, ctx, bigID); status != "error" || !strings.Contains(msg, "big has 12 cells (sku 4 × store 3), more than the 10 a metric may have") {
		t.Errorf("big: %s %q, want an error naming its 12 cells", status, msg)
	}
	if status, msg := partitionState(t, store, ctx, readerID); status != "error" || !strings.Contains(msg, "it reads big, which has too many cells") {
		t.Errorf("reader: %s %q, want refused for reading big", status, msg)
	}
	var bigRows int
	_ = store.Pool().QueryRow(ctx, `SELECT count(*) FROM runtime.calc_result WHERE metric_id = $1::uuid`, bigID).Scan(&bigRows)
	if bigRows != 0 {
		t.Errorf("big has %d result rows, want none", bigRows)
	}
	if v, ok := calcRow(t, store, ctx, modelID, revID, smallID, pins(storeID, "S1")); !ok || v != 6 {
		t.Errorf("small @ S1 = %v (present %v), want 6: the rest of the model is calculated", v, ok)
	}
}
