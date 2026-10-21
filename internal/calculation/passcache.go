package calculation

import (
	"context"
	"sync"
)

// A recalculation pass reads each metric's values many times: every
// dependent of an input loaded the input again, and every metric read back
// the rows its own dependencies had just written. On the education FP&A
// model a driver change recalculated 97 metrics with 161 such reads, 1.5 s
// of a 4 s pass (2026-10-20). The pass cache holds, for the length of one
// pass, the values each metric was read with or written as, so a metric is
// read from the database at most once per pass and never after this pass
// wrote it.
//
// What it holds equals what the database holds: a pass writes a metric's
// whole result set (ReplaceResultsTx) or clears it, and the cache takes
// that set only once the write is committed. Inputs are not written during
// a pass, and passes over one revision run one at a time (lockRevision).
type passCache struct {
	mu     sync.Mutex
	values map[string]map[string]float64 // metric ID → dimKey → value
	texts  map[string]map[string]string  // metric ID → dimKey → text
}

type passCacheKey struct{}

// withPassCache gives ctx a fresh pass cache.
func withPassCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, passCacheKey{}, &passCache{
		values: map[string]map[string]float64{},
		texts:  map[string]map[string]string{},
	})
}

func passCacheFrom(ctx context.Context) *passCache {
	c, _ := ctx.Value(passCacheKey{}).(*passCache)
	return c
}

// valueMap is LoadInputValueMap or LoadCalcValueMap for dep, through the
// pass cache when ctx has one. The map is shared: callers only read it.
func (s *Scheduler) valueMap(ctx context.Context, dep *MetricDef, modelID, revisionID string) (map[string]float64, error) {
	c := passCacheFrom(ctx)
	if c != nil {
		c.mu.Lock()
		m, ok := c.values[dep.ID]
		c.mu.Unlock()
		if ok {
			return m, nil
		}
	}
	var m map[string]float64
	var err error
	if dep.IsInput {
		m, err = s.store.LoadInputValueMap(ctx, modelID, revisionID, dep.ID)
	} else {
		m, err = s.store.LoadCalcValueMap(ctx, modelID, revisionID, dep.ID)
	}
	if err != nil || c == nil {
		return m, err
	}
	c.mu.Lock()
	if prev, ok := c.values[dep.ID]; ok {
		m = prev // another worker of this pass loaded or wrote it first
	} else {
		c.values[dep.ID] = m
	}
	c.mu.Unlock()
	return m, nil
}

// textMap is LoadInputTextMap or LoadCalcTextMap for dep, through the pass
// cache when ctx has one.
func (s *Scheduler) textMap(ctx context.Context, dep *MetricDef, modelID, revisionID string) (map[string]string, error) {
	c := passCacheFrom(ctx)
	if c != nil {
		c.mu.Lock()
		m, ok := c.texts[dep.ID]
		c.mu.Unlock()
		if ok {
			return m, nil
		}
	}
	load := s.store.LoadInputTextMap
	if !dep.IsInput {
		load = s.store.LoadCalcTextMap
	}
	m, err := load(ctx, modelID, revisionID, dep.ID)
	if err != nil || c == nil {
		return m, err
	}
	c.mu.Lock()
	if prev, ok := c.texts[dep.ID]; ok {
		m = prev
	} else {
		c.texts[dep.ID] = m
	}
	c.mu.Unlock()
	return m, nil
}

// remember records rows as metricID's committed result set (nil rows: its
// results were cleared). Two rows at one intersection with different
// contents are left to the database to settle: the metric is forgotten
// and read again.
func (s *Scheduler) remember(ctx context.Context, metricID string, rows []CalcResultRow) {
	c := passCacheFrom(ctx)
	if c == nil {
		return
	}
	values := make(map[string]float64, len(rows))
	texts := map[string]string{}
	for _, r := range rows {
		k := dimKey(r.DimMembers)
		if prev, dup := values[k]; dup {
			prevText, hadText := texts[k]
			if prev != r.Value || hadText != (r.Text != nil) || (r.Text != nil && prevText != *r.Text) {
				s.forget(ctx, metricID)
				return
			}
			continue
		}
		values[k] = r.Value
		if r.Text != nil {
			texts[k] = *r.Text
		}
	}
	c.mu.Lock()
	c.values[metricID], c.texts[metricID] = values, texts
	c.mu.Unlock()
}

// forget drops metricID from the pass cache: its next read goes to the
// database (a write that failed, or one whose rows are not known here).
func (s *Scheduler) forget(ctx context.Context, metricID string) {
	if c := passCacheFrom(ctx); c != nil {
		c.mu.Lock()
		delete(c.values, metricID)
		delete(c.texts, metricID)
		c.mu.Unlock()
	}
}
