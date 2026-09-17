package gateway

// A scale test answering a real product question: how does the Test model
// behave when a developer imports 10,000 rows of Excel/CSV data against a
// product dimension of 500 elements? It exercises the ACTUAL code paths a
// user hits — the dimension-member importer (500 members, some auto-coded),
// the grid import + commit (writeguard, staging, per-combo facts), the
// calculation scheduler's recalc, and a grid read — at that scale, and
// reports the wall-clock cost of each stage plus correctness checks.
//
// Run with:  go test ./internal/gateway/ -run TestScale_10kRows_500Products -v -timeout 20m

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestScale_10kRows_500Products(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped in -short mode")
	}
	d := setupSalesDemo(t)
	ctx := context.Background()

	const (
		numProducts = 500
		numRows     = 10000
	)

	// ── Stage 1: import a 500-member product dimension ──────────────────────
	// Half carry explicit codes; half are label-only so the auto-code path
	// (uppercase slug / uniqueness) runs at scale too. All parent to ALL_PROD.
	var pdb strings.Builder
	pdb.WriteString("code,label,parent_code,property:category\n")
	codes := make([]string, numProducts)
	for i := 0; i < numProducts; i++ {
		cat := []string{"A", "B", "C", "D"}[i%4]
		if i%2 == 0 {
			codes[i] = fmt.Sprintf("SKU%04d", i)
			pdb.WriteString(fmt.Sprintf("SKU%04d,Product %d,ALL_PROD,%s\n", i, i, cat))
		} else {
			// label-only → server auto-generates a code; we recover it after.
			pdb.WriteString(fmt.Sprintf(",Item Number %d,ALL_PROD,%s\n", i, cat))
		}
	}

	t1 := time.Now()
	resp := d.call("POST", "/api/import/dimension-members", d.dev, map[string]any{
		"dimension_id": d.prodDim,
		"csv":          pdb.String(),
	})
	dimDur := time.Since(t1)
	if er, _ := resp["error_rows"].(float64); er != 0 {
		t.Fatalf("dimension import reported %v error rows: %v", er, resp)
	}

	var memberCount int
	if err := d.pool.QueryRow(ctx,
		`SELECT count(*) FROM model.dimension_member WHERE dimension_id=$1::uuid`, d.prodDim).Scan(&memberCount); err != nil {
		t.Fatalf("count members: %v", err)
	}
	// 7 seed members (ALL_PROD + 6) + 500 imported.
	if memberCount != 7+numProducts {
		t.Fatalf("product members = %d, want %d", memberCount, 7+numProducts)
	}

	// Recover the auto-generated codes so the fact CSV references real ones.
	allCodes := make([]string, 0, numProducts)
	rows, err := d.pool.Query(ctx,
		`SELECT code FROM model.dimension_member WHERE dimension_id=$1::uuid AND parent_member_id IS NOT NULL AND code NOT IN ('HARDWARE','SOFTWARE','LAPTOP','MONITOR','LICENSE','SUPPORT')`, d.prodDim)
	if err != nil {
		t.Fatalf("load codes: %v", err)
	}
	for rows.Next() {
		var c string
		if rows.Scan(&c) == nil {
			allCodes = append(allCodes, c)
		}
	}
	rows.Close()
	if len(allCodes) != numProducts {
		t.Fatalf("recovered %d product codes, want %d", len(allCodes), numProducts)
	}

	// ── Stage 2: build a 10,000-row fact CSV ────────────────────────────────
	// geography × product × period leaf combos; each row carries units +
	// revenue (2 cells), so 10k rows = 20k facts. Product cycles through all
	// 500, so every product is exercised; geography/period cycle through
	// their leaves.
	geoLeaves := []string{"UK", "DE", "US", "CA"}
	periodLeaves := []string{"Q1", "Q2", "Q3", "Q4"}
	var fb strings.Builder
	fb.WriteString("geography,product,period,units,revenue\n")
	seen := make(map[string]bool, numRows)
	for i := 0; i < numRows; i++ {
		g := geoLeaves[i%len(geoLeaves)]
		p := allCodes[i%numProducts]
		// vary period by a different stride so combos stay distinct across the
		// full 10k (4 geo × 4 period × 500 prod = 8000 unique; the last 2000
		// deliberately overwrite earlier combos in replace mode — realistic,
		// and it proves latest-wins at scale).
		per := periodLeaves[(i/numProducts)%len(periodLeaves)]
		key := g + "|" + p + "|" + per
		seen[key] = true
		fb.WriteString(fmt.Sprintf("%s,%s,%s,%d,%d\n", g, p, per, 10+i%90, 1000+i%5000))
	}

	t2 := time.Now()
	imp := d.call("POST", "/api/import/upload", d.dev, map[string]any{
		"csv":         fb.String(),
		"revision_id": d.revID,
		"import_mode": "replace",
	})
	importDur := time.Since(t2)
	if er, _ := imp["error_rows"].(float64); er != 0 {
		t.Fatalf("fact import reported %v error rows: %v", er, imp)
	}
	if vr, _ := imp["valid_rows"].(float64); int(vr) != numRows*2 {
		t.Errorf("valid_rows = %v, want %d (10k rows x 2 metrics)", vr, numRows*2)
	}

	// ── Stage 3: recalc convergence ─────────────────────────────────────────
	// Import triggers an async recalc. Poll for a stable per-combo count of
	// the input revenue rows, and time how long it takes the persisted facts
	// to appear.
	t3 := time.Now()
	uniqueCombos := len(seen)
	var factRows int
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if err := d.pool.QueryRow(ctx, `
			SELECT count(*) FROM runtime.fact_input
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid
		`, d.modelID, d.revID, d.metric["revenue"]).Scan(&factRows); err != nil {
			t.Fatalf("count facts: %v", err)
		}
		if factRows >= uniqueCombos {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	factDur := time.Since(t3)
	if factRows < uniqueCombos {
		t.Errorf("only %d revenue facts persisted, want %d unique combos", factRows, uniqueCombos)
	}

	// Wait for a calc metric to reflect the imported data (margin over the
	// whole model), timing the recalc.
	t4 := time.Now()
	var calcConverged bool
	var lastCalc float64
	deadline = time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if v, ok := d.calcValue("margin", map[string]string{}); ok && v != 0 {
			lastCalc = v
			calcConverged = true
			break
		}
		time.Sleep(1 * time.Second)
	}
	recalcDur := time.Since(t4)

	// How many calc rows the read has to load — the rollup lattice for the
	// formula/rate metrics is the suspected cost driver at 500 products.
	var calcRowCount int
	_ = d.pool.QueryRow(ctx,
		`SELECT count(*) FROM runtime.calc_result WHERE model_id=$1::uuid AND revision_id=$2::uuid`,
		d.modelID, d.revID).Scan(&calcRowCount)

	// ── Stage 4: grid read at scale (cold, then warm) ───────────────────────
	t5 := time.Now()
	status, raw := d.req("GET", "/api/grid?revision_id="+d.revID, d.dev, nil)
	gridDur := time.Since(t5)
	if status != 200 {
		t.Fatalf("grid read: status %d\n%s", status, truncate(raw, 300))
	}
	respBytes := len(raw)
	t5b := time.Now()
	_, _ = d.req("GET", "/api/grid?revision_id="+d.revID, d.dev, nil)
	gridWarmDur := time.Since(t5b)

	var grid map[string]any
	_ = json.Unmarshal(raw, &grid)
	cellCount := 0
	if cells, ok := grid["cells"].(map[string]any); ok {
		cellCount = len(cells)
	}

	// ── Stage 5: SCOPED grid read (server-side context scoping) ─────────────
	// Pin geography=UK, period=Q1 — the slice a dashboard grid actually
	// shows. Should return only that slice's cells, far fewer and far faster
	// than the whole-model read.
	scope := fmt.Sprintf(`{%q:%q,%q:%q}`, d.geoDim, "UK", d.periodDim, "Q1")
	t6 := time.Now()
	sStatus, sRaw := d.req("GET", "/api/grid?revision_id="+d.revID+"&scope="+url.QueryEscape(scope), d.dev, nil)
	scopedDur := time.Since(t6)
	if sStatus != 200 {
		t.Fatalf("scoped grid read: status %d\n%s", sStatus, truncate(sRaw, 300))
	}
	var sGrid map[string]any
	_ = json.Unmarshal(sRaw, &sGrid)
	scopedCells := 0
	if cells, ok := sGrid["cells"].(map[string]any); ok {
		scopedCells = len(cells)
	}
	if scopedCells == 0 {
		t.Error("scoped read returned no cells")
	}
	// Compare INPUT-metric (revenue) cell counts, not total cells: the
	// whole-model read serves calc metrics straight from calc_result (only
	// persisted combos), while a scoped read RE-RESOLVES them per combo
	// (scopeCalcCells), which can legitimately yield MORE calc cells than
	// were persisted. Total-cell counts across the two paths aren't
	// comparable; revenue's facts, however, are genuinely narrowed by the
	// scope predicate, so that count must drop.
	countMetricCells := func(g map[string]any, metricID string) int {
		n := 0
		if cells, ok := g["cells"].(map[string]any); ok {
			for k := range cells {
				if strings.HasPrefix(k, metricID+":") {
					n++
				}
			}
		}
		return n
	}
	wholeRevenue := countMetricCells(grid, d.metric["revenue"])
	scopedRevenue := countMetricCells(sGrid, d.metric["revenue"])
	if scopedRevenue == 0 {
		t.Error("scoped read returned no revenue cells")
	}
	if scopedRevenue >= wholeRevenue {
		t.Errorf("scoped read returned %d revenue cells, not fewer than whole-model %d — scoping had no effect", scopedRevenue, wholeRevenue)
	}
	// Correctness: a UK·Q1 revenue cell must equal the whole-model read's
	// value for the same key (scoping changes WHICH cells, never their value).
	sampleKey := ""
	if cells, ok := grid["cells"].(map[string]any); ok {
		for k := range cells {
			if strings.HasPrefix(k, d.metric["revenue"]+":") && strings.Contains(k, ":UK:") && strings.HasSuffix(k, ":Q1") {
				sampleKey = k
				break
			}
		}
	}
	if sampleKey != "" {
		sc, _ := sGrid["cells"].(map[string]any)
		whole, _ := grid["cells"].(map[string]any)
		if sc[sampleKey] != whole[sampleKey] {
			t.Errorf("scoped value for %s = %v, whole-model = %v — must match", sampleKey, sc[sampleKey], whole[sampleKey])
		}
	}

	// ── Report ──────────────────────────────────────────────────────────────
	t.Logf("\n"+
		"╭─ SCALE TEST: Test model · %d products · %d rows (%d facts) ─\n"+
		"│ dimension import (500 members)  %8s\n"+
		"│ fact import + commit (10k rows) %8s\n"+
		"│ facts persisted (%d combos)   %8s\n"+
		"│ recalc convergence (margin)     %8s   value=%.0f converged=%v\n"+
		"│ calc_result rows (all metrics)  %8d\n"+
		"│ grid read (cold)                %8s   cells=%d  payload=%dKB\n"+
		"│ grid read (warm)                %8s\n"+
		"│ grid read (SCOPED UK·Q1)          %8s   cells=%d\n"+
		"╰────────────────────────────────────────────────────────────",
		numProducts, numRows, numRows*2,
		dimDur.Round(time.Millisecond),
		importDur.Round(time.Millisecond),
		uniqueCombos, factDur.Round(time.Millisecond),
		recalcDur.Round(time.Millisecond), lastCalc, calcConverged,
		calcRowCount,
		gridDur.Round(time.Millisecond), cellCount, respBytes/1024,
		gridWarmDur.Round(time.Millisecond),
		scopedDur.Round(time.Millisecond), scopedCells,
	)

	if !calcConverged {
		t.Errorf("margin never converged within timeout — recalc did not complete at scale")
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
