package gateway

import (
	"context"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/fairshare"
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
