package main

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

const (
	perfDimSize     = 100 // account members x cost_center members = 10,000 cells
	perfCellVal     = 1.0 // every cell = 1, so total_amount's expected sum is exactly perfDimSize*perfDimSize
	perfPollTimeout = 120 * time.Second
	perfPollEvery   = 500 * time.Millisecond
)

func runPerfTests(dev *api, _seedID string) {
	step("creating dimensions \"pf_account\" (%d members) and \"pf_cost_center\" (%d members)", perfDimSize, perfDimSize)
	acctDimID := createDimension(dev, "pf_account", "")
	ccDimID := createDimension(dev, "pf_cost_center", "")

	t0 := time.Now()
	for i := 1; i <= perfDimSize; i++ {
		createMember(dev, acctDimID, fmt.Sprintf("ACC%03d", i), fmt.Sprintf("Account %03d", i), "")
	}
	for i := 1; i <= perfDimSize; i++ {
		createMember(dev, ccDimID, fmt.Sprintf("CC%03d", i), fmt.Sprintf("Cost Center %03d", i), "")
	}
	step("created %d dimension members in %s (setup cost, not part of the import measurement)", 2*perfDimSize, time.Since(t0))

	step("creating grid \"Bulk Import Perf Test\" (dims: pf_account x pf_cost_center)")
	gridID := createGrid(dev, "Bulk Import Perf Test")
	assignDimension(dev, gridID, acctDimID)
	assignDimension(dev, gridID, ccDimID)

	amountID := createInputMetric(dev, "pf_amount")
	// depends on pf_amount -> forces the real per-combo recalc path
	// (RecalcAffected only visits metrics reachable from the changed input
	// via the reverse dependency graph; a metric with no dependents would
	// make this test artificially cheap and not representative of a real
	// bulk-import-feeds-a-rollup scenario).
	totalID := createCalcMetric(dev, "pf_total_amount", `pf_amount`, "sum")
	assignMetric(dev, gridID, amountID)
	assignMetric(dev, gridID, totalID)

	// ── baseline adjustment latency (grid still empty) ──────────────────────
	step("timing a single-cell adjustment BEFORE any bulk data exists (baseline)")
	baselineDims := map[string]string{acctDimID: "ACC001", ccDimID: "CC001"}
	t0 = time.Now()
	writeCell(dev, amountID, baselineDims, 42)
	baselineAdjustLatency := time.Since(t0)
	step("baseline single-cell adjustment: %s", baselineAdjustLatency)

	// ── generate a 10,000-row CSV (100 accounts x 100 cost centers) ────────
	step("generating a %d-row CSV (header: pf_account,pf_cost_center,pf_amount)", perfDimSize*perfDimSize)
	var sb strings.Builder
	sb.WriteString("pf_account,pf_cost_center,pf_amount\n")
	rowCount := 0
	for i := 1; i <= perfDimSize; i++ {
		for j := 1; j <= perfDimSize; j++ {
			fmt.Fprintf(&sb, "ACC%03d,CC%03d,%g\n", i, j, perfCellVal)
			rowCount++
		}
	}
	csvBytes := sb.String()
	step("CSV generated: %d rows, %d bytes", rowCount, len(csvBytes))

	// ── time the import commit itself (end-to-end HTTP round trip) ─────────
	step("uploading CSV via POST /api/import/upload (mode=replace) — timing the full HTTP round trip")
	t0 = time.Now()
	importResp := dev.call("POST", "/api/import/upload", map[string]any{
		"csv":         csvBytes,
		"revision_id": dev.revisionID,
		"import_mode": "replace",
	})
	importLatency := time.Since(t0)
	step("import HTTP round trip: %s — response: %+v", importLatency, importResp)
	record(fmt.Sprintf("import of %d cells completed", rowCount), true, importLatency.String())

	validRows, _ := importResp["valid_rows"].(float64)
	errorRows, _ := importResp["error_rows"].(float64)
	record("import reported 0 error rows", errorRows == 0, fmt.Sprintf("valid=%v error=%v", validRows, errorRows))
	record(fmt.Sprintf("import reported valid_rows == %d", rowCount), int(validRows) == rowCount, fmt.Sprintf("got %v", validRows))

	// The import commit's recalc is fire-and-forget (a goroutine, not
	// awaited by the HTTP response — see handler.go's importUpload). Poll
	// until pf_total_amount's total settles to the expected sum, timing how
	// long "the model is actually back in sync" takes from the caller's
	// point of view, not just "the upload call returned".
	expectedTotal := float64(rowCount) * perfCellVal
	step("polling GET /api/grid until pf_total_amount reflects the import (expected=%v)", expectedTotal)
	t0 = time.Now()
	var lastTotal any
	syncDeadline := time.Now().Add(perfPollTimeout)
	synced := false
	var pollCount int
	for time.Now().Before(syncDeadline) {
		pollCount++
		g := dev.call("GET", fmt.Sprintf("/api/grid?revision_id=%s", dev.revisionID), nil)
		totals, _ := g["totals"].(map[string]any)
		lastTotal = totals[totalID]
		if f, ok := lastTotal.(float64); ok && f == expectedTotal {
			synced = true
			break
		}
		time.Sleep(perfPollEvery)
	}
	syncLatency := time.Since(t0)
	if synced {
		step("model synchronized (pf_total_amount == %v) after %s (%d polls, includes import's own async recalc goroutine)", expectedTotal, syncLatency, pollCount)
	} else {
		warn("model did NOT synchronize within %s — last observed pf_total_amount=%v (expected %v)", perfPollTimeout, lastTotal, expectedTotal)
	}
	record("bulk-import recalc converges to correct total", synced, fmt.Sprintf("took %s (last=%v, expected=%v)", syncLatency, lastTotal, expectedTotal))

	// ── grid fetch latency at 10k-cell scale ────────────────────────────────
	step("timing GET /api/grid?grid_def_id=... at %d-cell scale", rowCount)
	t0 = time.Now()
	gridResp := dev.call("GET", fmt.Sprintf("/api/grid?grid_def_id=%s&revision_id=%s", gridID, dev.revisionID), nil)
	gridFetchLatency := time.Since(t0)
	cellCount := 0
	if cells, ok := gridResp["cells"].(map[string]any); ok {
		cellCount = len(cells)
	}
	step("grid fetch: %s (%d cells returned)", gridFetchLatency, cellCount)
	record("grid fetch at 10k-cell scale", true, gridFetchLatency.String())

	// ── adjustment latency AFTER bulk data + a dependent calc metric exist ──
	step("timing a single-cell adjustment AFTER the bulk import (this is the expensive one: RecalcAffected re-evaluates pf_total_amount over ALL %d combos, not just the changed cell)", rowCount)
	afterDims := map[string]string{acctDimID: "ACC050", ccDimID: "CC050"}
	t0 = time.Now()
	writeCell(dev, amountID, afterDims, 99)
	afterAdjustLatency := time.Since(t0)
	step("post-import single-cell adjustment: %s (baseline was %s)", afterAdjustLatency, baselineAdjustLatency)

	slowdown := float64(afterAdjustLatency) / float64(baselineAdjustLatency)
	record("single-cell adjustment latency after 10k rows", true,
		fmt.Sprintf("%s vs %s baseline (%.1fx)", afterAdjustLatency, baselineAdjustLatency, slowdown))
	if afterAdjustLatency > 2*time.Second {
		warn("post-import adjustment took %s — that's a user-perceptible stall for editing ONE cell", afterAdjustLatency)
	}

	// ── xlsx path sanity (same size, different encoding, quick check) ──────
	step("sanity-checking the .xlsx upload path still works at this scale is skipped (CSV path already proves the commit/recalc pipeline; xlsx only differs in parsing, covered by unit tests)")
	_ = base64.StdEncoding // (kept for potential future xlsx timing; CSV path is the one that matters for "10,000 uploaded rows")

	printPerfSummary(rowCount, importLatency, syncLatency, gridFetchLatency, baselineAdjustLatency, afterAdjustLatency)
}

func printPerfSummary(rows int, importLatency, syncLatency, gridFetchLatency, baselineAdjust, afterAdjust time.Duration) {
	fmt.Println()
	fmt.Println("  ── performance numbers ──────────────────────────────────────")
	fmt.Printf("  rows imported:                    %d\n", rows)
	fmt.Printf("  import HTTP round trip:           %s\n", importLatency)
	fmt.Printf("  full model sync (incl. async recalc): %s\n", syncLatency)
	fmt.Printf("  grid fetch @ 10k cells:           %s\n", gridFetchLatency)
	fmt.Printf("  single-cell adjustment, before:   %s\n", baselineAdjust)
	fmt.Printf("  single-cell adjustment, after:    %s\n", afterAdjust)
	fmt.Println("  ─────────────────────────────────────────────────────────────")
}
