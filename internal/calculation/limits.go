package calculation

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// A pass holds a metric's every cell in memory while it calculates and
// writes it, and the gateway it runs in serves every tenant. Two metrics of
// a tester's model spanning 21 customers × 1,858 days × 64 SKUs — 2.5
// million cells each — took the gateway past its 3 GiB limit, and the
// kernel killed it with every request it was serving (2026-10-22; measured
// again on a copy, 3.4 GB). A metric over MaxMetricCells is refused with a
// message naming it and its size, and so is every metric that reads it,
// instead of being calculated from values that are not there.

// defaultMaxMetricCells is MaxMetricCells without CALC_MAX_METRIC_CELLS.
const defaultMaxMetricCells = 1_000_000

// MaxMetricCells is how many cells (leaf intersections of its dimensions)
// one metric may have to be calculated: CALC_MAX_METRIC_CELLS, else
// defaultMaxMetricCells.
func MaxMetricCells() int64 {
	if n, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("CALC_MAX_METRIC_CELLS")), 10, 64); err == nil && n > 0 {
		return n
	}
	return defaultMaxMetricCells
}

// TooLargeError is a metric a pass did not calculate because it, or a
// metric it reads (Via), has more cells than MaxMetricCells.
type TooLargeError struct {
	Metric string
	Cells  int64
	Max    int64
	// Dims describes the dimensions that make the cells: "customer 21 ×
	// period 1858 × sku 64".
	Dims string
	// Via names the too-large metric this one reads; "" when it is the
	// metric itself.
	Via string
}

func (e *TooLargeError) Error() string {
	if e.Via != "" {
		return fmt.Sprintf("not calculated: it reads %s, which has too many cells to calculate", e.Via)
	}
	return fmt.Sprintf("not calculated: %s has %d cells (%s), more than the %d a metric may have — give it fewer dimensions or members, or a coarser time dimension",
		e.Metric, e.Cells, e.Dims, e.Max)
}

// MetricCells is how many leaf cells a metric over dimIDs has, and the
// dimensions that make them (by name), for a message.
func MetricCells(allDims map[string]*rollup.Dimension, dimIDs []string, dimIDToName map[string]string) (int64, string) {
	parts := make([]string, 0, len(dimIDs))
	for _, id := range dimIDs {
		name := dimIDToName[id]
		if name == "" {
			name = id
		}
		parts = append(parts, fmt.Sprintf("%s %d", name, rollup.LeafComboCount(allDims, []string{id})))
	}
	sort.Strings(parts)
	return rollup.LeafComboCount(allDims, dimIDs), strings.Join(parts, " × ")
}

// checkSize refuses def when it has more cells than MaxMetricCells or reads
// a metric this pass refused, and records the refusal for its dependents.
func (s *Scheduler) checkSize(c *passCache, def *MetricDef, allDims map[string]*rollup.Dimension, dimIDs []string, dimIDToName map[string]string) error {
	if c != nil {
		deps := append(append([]string{}, def.DependsOnID...), def.AggNumeratorID, def.AggDenominatorID)
		for _, dep := range deps {
			if via, refused := c.refusedName(dep); refused {
				c.refuse(def.ID, def.Name)
				return &TooLargeError{Metric: def.Name, Via: via}
			}
		}
	}
	max := MaxMetricCells()
	cells, dims := MetricCells(allDims, dimIDs, dimIDToName)
	if cells <= max {
		return nil
	}
	if c != nil {
		c.refuse(def.ID, def.Name)
	}
	return &TooLargeError{Metric: def.Name, Cells: cells, Max: max, Dims: dims}
}
