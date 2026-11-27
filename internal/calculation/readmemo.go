package calculation

import (
	"context"
	"strings"
	"sync"
)

// Read memo: the answers of whole-revision aggregate reads (each calculated
// metric's total, a slice, a metric's stored values, the input totals),
// shared by every read of the same model at the same data generation
// (migration 198). A dashboard's widgets each asked the database the same
// questions: the development-programme Portfolio ran 82 statements of
// 25 ms or more — 24 total lookups, 19 input-total sums — for one
// opening, 13 s of database time behind a 6 s wait (2026-11-27).
//
// A reader names its generation before it reads (WithReadGeneration, taken
// as the read's ETag is): a change committing meanwhile may be in the
// answer kept under the older generation, never the other way round — the
// argument read_etag.go makes for 304s. Without a generation in the
// context nothing is kept: recalculation, writes and every other caller
// read the database as before.

type readGenKey struct{}

// WithReadGeneration marks ctx's reads as answerable from the memo for
// scope: the routed database, the model and its data generation, as the
// gateway's grid read takes them.
func WithReadGeneration(ctx context.Context, scope string) context.Context {
	if scope == "" {
		return ctx
	}
	return context.WithValue(ctx, readGenKey{}, scope)
}

func readGenFrom(ctx context.Context) string {
	s, _ := ctx.Value(readGenKey{}).(string)
	return s
}

// readMemoEntries bounds what the memo holds, in map entries over every kept
// answer: a few dashboards' worth. A larger answer is not kept.
const (
	readMemoEntries   = 400_000
	readMemoLargest   = 100_000
	readMemoMaxAnswer = 4096
)

type readMemoEntry struct {
	done chan struct{}
	val  any
	err  error
	size int
}

type readMemoCache struct {
	mu      sync.Mutex
	entries map[string]*readMemoEntry
	order   []string
	size    int
}

var readMemo = &readMemoCache{entries: map[string]*readMemoEntry{}}

// memoRead answers kind(args) from the memo when ctx names a generation,
// loading it once for every concurrent asker; size reports an answer's
// entries. Each caller gets its own copy (clone), as callers may change what
// they are given.
func memoRead[T any](ctx context.Context, kind string, args []string, load func() (T, error), size func(T) int, clone func(T) T) (T, error) {
	scope := readGenFrom(ctx)
	if scope == "" {
		return load()
	}
	key := scope + "\x00" + kind + "\x00" + strings.Join(args, "\x00")
	m := readMemo
	m.mu.Lock()
	if e, ok := m.entries[key]; ok {
		m.mu.Unlock()
		select {
		case <-e.done:
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		}
		if e.err == nil {
			if v, ok := e.val.(T); ok {
				return clone(v), nil
			}
		}
		return load() // the first asker failed: read on our own
	}
	e := &readMemoEntry{done: make(chan struct{})}
	m.entries[key] = e
	m.mu.Unlock()

	v, err := load()
	e.val, e.err = v, err
	if err == nil {
		e.size = size(v) + 1
	}
	close(e.done)

	m.mu.Lock()
	if err != nil || e.size > readMemoLargest {
		delete(m.entries, key) // not kept: a failure, or too large
	} else {
		m.order = append(m.order, key)
		m.size += e.size
		for (m.size > readMemoEntries || len(m.order) > readMemoMaxAnswer) && len(m.order) > 0 {
			old := m.order[0]
			m.order = m.order[1:]
			if oe, ok := m.entries[old]; ok {
				m.size -= oe.size
				delete(m.entries, old)
			}
		}
	}
	m.mu.Unlock()
	if err != nil {
		return v, err
	}
	return clone(v), nil
}

func floatMapSize(m map[string]float64) int { return len(m) }

func cloneFloatMap(m map[string]float64) map[string]float64 {
	if m == nil {
		return nil
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func nestedFloatMapSize(m map[string]map[string]float64) int {
	n := 0
	for _, inner := range m {
		n += len(inner) + 1
	}
	return n
}

func cloneNestedFloatMap(m map[string]map[string]float64) map[string]map[string]float64 {
	if m == nil {
		return nil
	}
	out := make(map[string]map[string]float64, len(m))
	for k, inner := range m {
		out[k] = cloneFloatMap(inner)
	}
	return out
}

// MemoFloats is memoRead for a caller outside this package with a
// map[string]float64 answer (the gateway's input totals).
func MemoFloats(ctx context.Context, kind string, args []string, load func() (map[string]float64, error)) (map[string]float64, error) {
	return memoRead(ctx, kind, args, load, floatMapSize, cloneFloatMap)
}

// InputCellRow is one input cell as the grid read sums it: its metric, its
// members' JSON and its value.
type InputCellRow struct {
	MetricID, DimMembers string
	Value                float64
}

// MemoRows is memoRead for rows every caller only reads: they are shared,
// not copied, and must not be changed.
func MemoRows[T any](ctx context.Context, kind string, args []string, load func() ([]T, error)) ([]T, error) {
	return memoRead(ctx, kind, args, load, func(rows []T) int { return len(rows) }, func(rows []T) []T { return rows })
}
