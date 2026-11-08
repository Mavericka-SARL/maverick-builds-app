package calculation_test

import (
	"context"
	"fmt"
	"testing"
)

// TestEmptyTextLeavesANumberCellEmpty: a workbook's IF(closed, x, "") in a
// number metric leaves the open months empty — no row, never a 0 — and the
// metric calculates. Each such month used to fail with #VALUE!, the metric
// was marked failed and kept its stale values (2026-11-08).
func TestEmptyTextLeavesANumberCellEmpty(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-0000000000f2"
	revID := "00000000-0000-0000-0001-0000000000f2"
	monthID := insertTimeDim(t, store, ctx, modelID, revID, "month")
	months := insertMonths(t, store, ctx, monthID, 3)
	flagID := insertMetric(t, store, ctx, modelID, revID, "closed", "", true)
	xID := insertMetric(t, store, ctx, modelID, revID, "x", "", true)
	shownID := insertCalc(t, store, ctx, modelID, revID, "shown", `IF(closed = 1, x * 2, "")`)
	insertGridSetup(t, store, ctx, modelID, []string{monthID}, []string{flagID, xID, shownID})
	for i, code := range months {
		insertFact(t, store, ctx, modelID, revID, xID, fmt.Sprintf(`{"%s":"%s"}`, monthID, code), float64(10*(i+1)))
	}
	insertFact(t, store, ctx, modelID, revID, flagID, fmt.Sprintf(`{"%s":"%s"}`, monthID, months[0]), 1)
	runRecalc(t, store, ctx, modelID, revID, []string{flagID, xID})

	if got, ok := calcRow(t, store, ctx, modelID, revID, shownID, map[string]string{monthID: months[0]}); !ok || got != 20 {
		t.Errorf("closed month: %v ok=%v, want 20", got, ok)
	}
	for _, code := range months[1:] {
		if got, ok := calcRow(t, store, ctx, modelID, revID, shownID, map[string]string{monthID: code}); ok {
			t.Errorf("open month %s: %v, want no value", code, got)
		}
	}
	var status string
	if err := store.Pool().QueryRow(ctx, `SELECT status::text FROM runtime.metric_partition_state WHERE metric_id=$1::uuid`, shownID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status == "error" {
		t.Errorf("the metric failed: %s", status)
	}
}
