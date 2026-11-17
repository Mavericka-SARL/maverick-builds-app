package calculation

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// storedReads lists, per dependency, the coordinates its fetch has a value
// at — the keys (dimKey) of the map the fetch looks up — so a read of the
// dependency over many leaf coordinates visits those only
// (rollup.WithRecorded). A dependency is listed only when its fetch is that
// lookup and nothing else: register a map in the same place the fetch over
// it is made. Partitions of a pass may read it at once.
type storedReads struct {
	mu     sync.Mutex
	maps   map[string]map[string]float64
	coords map[string][]map[string]string
}

func newStoredReads() *storedReads {
	return &storedReads{maps: map[string]map[string]float64{}, coords: map[string][]map[string]string{}}
}

// add registers values as what metricID's fetch looks up.
func (s *storedReads) add(metricID string, values map[string]float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maps[metricID] = values
	delete(s.coords, metricID)
}

// recorded is a rollup.Recorded over the registered maps; a metric with no
// map registered is not known, and its reads walk every leaf coordinate.
func (s *storedReads) recorded(_ context.Context, metricID string) ([]map[string]string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, ok := s.maps[metricID]
	if !ok {
		return nil, false, nil
	}
	if cs, ok := s.coords[metricID]; ok {
		return cs, true, nil
	}
	cs, err := decodeKeys(values)
	if err != nil {
		return nil, false, err
	}
	s.coords[metricID] = cs
	return cs, true, nil
}

// within is ctx for a read through the registered fetches (ctx itself on a
// nil registry).
func (s *storedReads) within(ctx context.Context) context.Context {
	if s == nil {
		return ctx
	}
	return rollup.WithRecorded(ctx, s.recorded)
}

// recordedOf is a rollup.Recorded for one metric whose fetch looks values
// up by dimKey: every other metric is not known.
func recordedOf(metricID string, values map[string]float64) rollup.Recorded {
	s := newStoredReads()
	s.add(metricID, values)
	return s.recorded
}

// decodeKeys turns dimKey keys back into the combos they encode (dimKey is
// json.Marshal's output, so a decoded combo keys the same entry again).
func decodeKeys(values map[string]float64) ([]map[string]string, error) {
	cs := make([]map[string]string, 0, len(values))
	for key := range values {
		var c map[string]string
		if err := json.Unmarshal([]byte(key), &c); err != nil {
			return nil, err
		}
		cs = append(cs, c)
	}
	return cs, nil
}
