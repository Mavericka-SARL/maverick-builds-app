package gateway

import (
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// pinReach reports, for a totals_only read pinning one dimension, which
// metrics the pin can change. A metric is reached when it, or anything it
// reads — directly or through other metrics — has the pinned dimension or a
// dimension related to it (a parent dimension, a property grouping), or when
// its formula names such a dimension (a criteria range, a LOOKUP argument). A
// formula that does not parse counts as reached.
//
// A calculated metric the pin cannot reach has the same total pinned as
// unpinned, so the slice fast path serves its whole-model ('{}') total
// instead of recomputing the whole model under the scope. Before 2026-11-21
// every such metric with a non-zero total forced that recomputation: a
// scenario selector over a model with product statistics (no scenario) read
// its KPIs in 1.8 s instead of milliseconds, every two seconds.
func pinReach(metrics []metricRow, metricDims map[string][]string, dims map[string]*rollup.Dimension, dimNames map[string]string, pinned string) map[string]bool {
	related := func(d string) bool {
		return d == pinned || rollup.Relates(dims, d, pinned) || rollup.Relates(dims, pinned, d)
	}
	relatedNames := map[string]bool{}
	for id, name := range dimNames {
		if related(id) {
			relatedNames[strings.ToLower(name)] = true
		}
	}
	byName := make(map[string]string, len(metrics))
	for _, m := range metrics {
		byName[strings.ToLower(m.Name)] = m.ID
	}
	reached := map[string]bool{}
	readers := map[string][]string{} // metric id → the metrics whose formulas read it
	var queue []string
	seed := func(id string) {
		if !reached[id] {
			reached[id] = true
			queue = append(queue, id)
		}
	}
	for _, m := range metrics {
		for _, d := range metricDims[m.ID] {
			if related(d) {
				seed(m.ID)
				break
			}
		}
		if m.IsInput || m.Formula == nil || strings.TrimSpace(*m.Formula) == "" {
			continue
		}
		an, err := formula.Analyze(*m.Formula)
		if err != nil {
			seed(m.ID)
			continue
		}
		// Every name the formula uses: metric and bare-dimension references,
		// dimension arguments and criteria ranges, dim.property reads, the
		// sources of LOOKUP and the *IFS family.
		var refs []string
		for _, r := range an.References {
			refs = append(refs, r.Name)
		}
		refs = append(refs, an.DimensionArgs...)
		for _, p := range an.PropertyRefs {
			refs = append(refs, p.Dim)
		}
		for _, c := range an.CellDims {
			refs = append(refs, c.Dim)
		}
		for _, c := range an.DimensionalCalls {
			refs = append(refs, c.Source)
			refs = append(refs, c.Dims...)
		}
		for _, ref := range refs {
			name := strings.ToLower(ref)
			if relatedNames[name] {
				seed(m.ID)
			}
			if dep, ok := byName[name]; ok && dep != m.ID {
				readers[dep] = append(readers[dep], m.ID)
			}
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, r := range readers[id] {
			seed(r)
		}
	}
	return reached
}
