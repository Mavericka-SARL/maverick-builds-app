package gateway

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
)

// gridMetricIDs is the ids of the metrics a grid read shows.
func gridMetricIDs(metrics []metricRow) []string {
	ids := make([]string, 0, len(metrics))
	for _, m := range metrics {
		ids = append(ids, m.ID)
	}
	return ids
}

// gridMetricSet is gridMetricIDs as a set.
func gridMetricSet(metrics []metricRow) map[string]bool {
	set := make(map[string]bool, len(metrics))
	for _, m := range metrics {
		set[m.ID] = true
	}
	return set
}

// keepGridMetrics drops from a grid read's answer every cell, text, total
// and withheld marker of a metric outside on (keys are "<metric id>" or
// "<metric id>:<member codes>") and returns the withheld markers kept.
func keepGridMetrics(on map[string]bool, cells map[string]float64, texts map[string]string, totals map[string]float64, withheld []string) []string {
	metricOf := func(key string) string {
		id, _, _ := strings.Cut(key, ":")
		return id
	}
	for k := range cells {
		if !on[metricOf(k)] {
			delete(cells, k)
		}
	}
	for k := range texts {
		if !on[metricOf(k)] {
			delete(texts, k)
		}
	}
	for k := range totals {
		if !on[k] {
			delete(totals, k)
		}
	}
	kept := withheld[:0]
	for _, k := range withheld {
		if on[metricOf(k)] {
			kept = append(kept, k)
		}
	}
	return kept
}

// gridReadsAtOnce is how many full grid reads a gateway answers at once
// (GRID_READ_CONCURRENCY, default 4). Reads beyond it wait their turn rather
// than fail: they are bound by the processor anyway, so a burst takes as
// long as before, without its memory. Measured 2026-10-15: each read of a
// 108,000-cell grid held ~55 MB, sixteen at once ~900 MB; both production
// gateways were OOM-killed at 512 MiB while a tester's reads ran in
// parallel, and the ingress answered 503 until they came back.
func gridReadsAtOnce() int {
	if n, err := strconv.Atoi(os.Getenv("GRID_READ_CONCURRENCY")); err == nil && n > 0 {
		return n
	}
	return 4
}

var (
	gridReadSlotsOnce sync.Once
	gridReadSlots     chan struct{}
)

// gridReadSlot waits for a turn to read a grid in full and returns its
// release; an error when ctx ends first (the caller went away).
func gridReadSlot(ctx context.Context) (func(), error) {
	gridReadSlotsOnce.Do(func() { gridReadSlots = make(chan struct{}, gridReadsAtOnce()) })
	select {
	case gridReadSlots <- struct{}{}:
		return func() { <-gridReadSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
