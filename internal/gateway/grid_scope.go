package gateway

import (
	"context"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/fairshare"
	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// gridMetricIDs is the ids of the metrics a grid read shows.
func gridMetricIDs(metrics []metricRow) []string {
	ids := make([]string, 0, len(metrics))
	for _, m := range metrics {
		ids = append(ids, m.ID)
	}
	return ids
}

// readsClosure is targets and every metric their formulas read, at any
// depth: the metrics a scoped read must work out to answer for targets. A
// scoped read worked out every calculated metric of the model and kept the
// grid's own — 60 of them for a six-metric grid of an OPEX budget, on
// every poll of every widget (2026-11-22). Names match as the evaluator
// binds them, ignoring case; a reference that names no metric (a function,
// a dimension) adds nothing.
func readsClosure(targets []string, universe []metricRow) map[string]bool {
	byID := make(map[string]*metricRow, len(universe))
	byName := make(map[string]*metricRow, len(universe))
	for i := range universe {
		byID[universe[i].ID] = &universe[i]
		byName[strings.ToUpper(universe[i].Name)] = &universe[i]
	}
	on := make(map[string]bool, len(targets))
	queue := make([]*metricRow, 0, len(targets))
	for _, id := range targets {
		if m := byID[id]; m != nil && !on[id] {
			on[id] = true
			queue = append(queue, m)
		}
	}
	for len(queue) > 0 {
		m := queue[0]
		queue = queue[1:]
		if m.Formula == nil || *m.Formula == "" {
			continue
		}
		refs, err := formula.ExtractRefs(*m.Formula)
		if err != nil {
			continue // a formula that does not parse resolves nothing, read or not
		}
		for _, ref := range refs {
			if dep := byName[strings.ToUpper(ref)]; dep != nil && !on[dep.ID] {
				on[dep.ID] = true
				queue = append(queue, dep)
			}
		}
	}
	return on
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

// heavyTurn waits for a turn of heavy work — a grid or chart read — under
// the tenant the request reads (fairshare): a tenant's reads beyond its share
// wait behind its own, never behind another tenant's. The tenant is the
// routed one (a dedicated database); in a shared database it is the actor's
// own tenant, else the actor. The returned ctx holds the turn, so a read
// that reads again within itself does not wait for a second one.
func (h *handler) heavyTurn(ctx context.Context, act *actor) (context.Context, func(), error) {
	return fairshare.Default().Acquire(ctx, heavyKey(ctx, act))
}

func heavyKey(ctx context.Context, act *actor) string {
	if id := tenantdb.TenantFrom(ctx); id != "" {
		return id
	}
	if act != nil && act.CustomerID != "" {
		return act.CustomerID
	}
	if act != nil {
		return "user:" + act.UserID
	}
	return ""
}
