package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// Two people changing one cell (cell_conflict.go): a save naming the cell as
// read is refused once someone changed it, a save without it is made as
// before, a batch is all or nothing, and of several checked saves of the
// same read only one is made.
func TestTwoPeopleChangingOneCell(t *testing.T) {
	d := setupSalesDemo(t)
	codes := map[string]string{d.geoDim: "UK", d.prodDim: "LAPTOP", d.periodDim: "Q1"}
	save := func(persona string, value float64, expected map[string]any) (int, map[string]any) {
		body := map[string]any{"model_id": d.modelID, "revision_id": d.revID, "metric_id": d.metric["revenue"],
			"dim_codes": codes, "value": value}
		if expected != nil {
			body["expected"] = expected
		}
		status, raw := d.req("POST", "/api/cells", persona, body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return status, out
	}
	// What the cell holds as typed: its latest direct entry, nil when none.
	held := func() any {
		dm, _ := json.Marshal(codes)
		var v float64
		if err := d.pool.QueryRow(context.Background(), `
			SELECT value FROM runtime.fact_input
			WHERE revision_id = $1::uuid AND metric_id = $2::uuid AND dim_members = $3::jsonb AND source_ref IS NULL
			ORDER BY entered_at DESC, id DESC LIMIT 1`, d.revID, d.metric["revenue"], string(dm)).Scan(&v); err != nil {
			return nil
		}
		return v
	}
	start := held()
	read := map[string]any{"blank": true}
	if v, ok := start.(float64); ok {
		read = map[string]any{"value": v}
	}

	if st, out := save(d.dev, 100, read); st != http.StatusOK {
		t.Fatalf("the first save, as read: %d %v", st, out)
	}
	st, out := save(d.admin, 200, read)
	if st != http.StatusConflict || out["code"] != "cell_changed" || out["current"] != float64(100) ||
		!strings.Contains(out["error"].(string), "someone changed this cell since you opened it") {
		t.Fatalf("the second save of the same read: %d %v, want 409 cell_changed holding 100", st, out)
	}
	if got := held(); got != float64(100) {
		t.Fatalf("after the refused save the cell holds %v, want 100", got)
	}
	if st, out := save(d.admin, 200, map[string]any{"value": 100}); st != http.StatusOK {
		t.Fatalf("saved again over what it now holds: %d %v", st, out)
	}
	if st, out := save(d.dev, 300, nil); st != http.StatusOK || held() != float64(300) {
		t.Fatalf("a save without expected is made as before: %d %v, holds %v", st, out, held())
	}

	// A batch with one stale cell is written not at all.
	other := map[string]string{d.geoDim: "UK", d.prodDim: "LAPTOP", d.periodDim: "Q2"}
	status, raw := d.req("POST", "/api/cells/batch", d.admin, map[string]any{"model_id": d.modelID, "revision_id": d.revID, "cells": []any{
		map[string]any{"metric_id": d.metric["revenue"], "dim_codes": other, "value": 7},
		map[string]any{"metric_id": d.metric["revenue"], "dim_codes": codes, "value": 8, "expected": map[string]any{"value": 100}},
	}})
	if status != http.StatusConflict || !strings.Contains(string(raw), `"index":1`) {
		t.Fatalf("a batch with a stale cell: %d %s, want 409 naming cell 1", status, raw)
	}
	if got := held(); got != float64(300) {
		t.Fatalf("after the refused batch the cell holds %v, want 300", got)
	}

	// Ten saves of the same read at once: one is made.
	var wg sync.WaitGroup
	var mu sync.Mutex
	made := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			persona := d.dev
			if i%2 == 1 {
				persona = d.admin
			}
			if st, _ := save(persona, float64(1000+i), map[string]any{"value": 300}); st == http.StatusOK {
				mu.Lock()
				made++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if made != 1 {
		t.Errorf("ten checked saves of the same read: %d made, want 1", made)
	}
}
