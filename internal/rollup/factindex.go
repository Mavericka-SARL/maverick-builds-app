package rollup

import (
	"context"
	"slices"
	"sync"
)

// factIndex answers factlessChildren for one metric's listed facts without
// visiting them all. factlessChildren asks, at every parent a read expands,
// which children have a fact beneath them within the other pins; visiting
// every fact at every parent made a scoped read of a dense model spend most
// of its time there — an OPEX budget of 2,366 facts over account × cost
// centre × period × version, read with a version and a year pinned, took
// 1–1.6 s on the Playground, 54% of it in that scan (2026-11-22). The index
// lists, for each dimension and member, the facts at or under the member:
// a child is covered when its list and the pins' lists share a fact, found
// from the shortest of them.
type factIndex struct {
	metricDims []string
	dims       []*Dimension // dims[metricDims[i]] the index was built over
	// under[i][code]: ascending positions in the listed facts of the facts
	// whose member of metricDims[i] is code or lies beneath it.
	under []map[string][]int32
}

// maxChainDepth is how far atOrUnder and childAbove climb a member's
// ancestors; a chain longer than that (a cycle) is not indexed.
const maxChainDepth = 64

// buildFactIndex indexes coords over dims; nil when a member's ancestors
// cannot be listed (a chain past maxChainDepth), where the scan answers.
func buildFactIndex(dims map[string]*Dimension, metricDimIDs []string, coords []map[string]string) *factIndex {
	idx := &factIndex{
		metricDims: slices.Clone(metricDimIDs),
		dims:       dimsOf(dims, metricDimIDs),
		under:      make([]map[string][]int32, len(metricDimIDs)),
	}
	for i := range metricDimIDs {
		idx.under[i] = map[string][]int32{}
	}
	for n, f := range coords {
		if len(f) != len(metricDimIDs) {
			continue // as the scan: a fact naming other dimensions is never within
		}
		for i, p := range metricDimIDs {
			d := idx.dims[i]
			if d == nil {
				continue // an unknown dimension constrains nothing; never expanded
			}
			code := f[p]
			for depth := 0; ; depth++ {
				if depth > maxChainDepth {
					return nil
				}
				idx.under[i][code] = append(idx.under[i][code], int32(n))
				m := FindMember(d, code)
				if m == nil || m.ParentCode == "" {
					break
				}
				code = m.ParentCode
			}
		}
	}
	return idx
}

// builtFor reports whether idx was built over the same dimensions.
func (idx *factIndex) builtFor(dims map[string]*Dimension, metricDimIDs []string) bool {
	if !slices.Equal(idx.metricDims, metricDimIDs) {
		return false
	}
	for i, p := range metricDimIDs {
		if idx.dims[i] != dims[p] {
			return false
		}
	}
	return true
}

// factless is factlessChildren's answer from the index: the scan's answer
// (factlessChildrenScan), with dims[dimID] known.
func (idx *factIndex) factless(
	dims map[string]*Dimension,
	metricDimIDs []string,
	dimID string,
	combo map[string]string,
	children []Member,
) map[string]bool {
	at := -1
	var pins [][]int32
	for i, p := range metricDimIDs {
		if p == dimID {
			at = i
			continue
		}
		code, pinned := combo[p]
		if !pinned || dims[p] == nil {
			continue
		}
		pins = append(pins, idx.under[i][code])
	}
	skip := make(map[string]bool, len(children))
	for _, c := range children {
		if !sharesFact(idx.under[at][c.Code], pins) {
			skip[c.Code] = true
		}
	}
	return skip
}

// sharesFact reports whether one position is in first and in every list of
// rest (each ascending): the shortest list is walked, the others searched.
func sharesFact(first []int32, rest [][]int32) bool {
	shortest := first
	for _, l := range rest {
		if len(l) < len(shortest) {
			shortest = l
		}
	}
	if len(shortest) == 0 {
		return false
	}
	for _, n := range shortest {
		if !contains32(first, n) {
			continue
		}
		in := true
		for _, l := range rest {
			if !contains32(l, n) {
				in = false
				break
			}
		}
		if in {
			return true
		}
	}
	return false
}

func contains32(sorted []int32, n int32) bool {
	_, found := slices.BinarySearch(sorted, n)
	return found
}

// factIndexes holds the indexes a read has built, by metric and listed facts.
type factIndexes struct {
	mu sync.Mutex
	m  map[factIndexKey][]*factIndex
	// asked counts the expansions of each list answered by a scan so far:
	// the index is built for the second (indexAfterScans), so a read that
	// expands one parent — a recalculation's read at one coordinate — scans
	// once, as before, and pays no build.
	asked map[factIndexKey]int
}

// indexAfterScans is how many expansions of a metric's facts are scanned
// before its index is built.
const indexAfterScans = 1

// factIndexKey names a metric's listed facts by the list itself: a Recorded
// answers the same slice for a metric each time it is asked (its contract
// in each implementation here), and the key keeps that slice alive, so its
// address names it as long as the index is held.
type factIndexKey struct {
	metricID string
	first    *map[string]string
	n        int
}

type factIndexesKey struct{}

// WithFactIndexes makes the recorded reads made with the returned context
// (WithRecorded and its variants) share one index of each metric's listed
// facts, built the first time a read expands a parent of the metric. A
// caller that makes many reads over the same facts — a scoped read working
// out every calculated metric at every combination — passes it once, above
// them; without it each WithRecorded call indexes the facts for itself.
func WithFactIndexes(ctx context.Context) context.Context {
	if _, ok := ctx.Value(factIndexesKey{}).(*factIndexes); ok {
		return ctx
	}
	return context.WithValue(ctx, factIndexesKey{}, &factIndexes{m: map[factIndexKey][]*factIndex{}, asked: map[factIndexKey]int{}})
}

// factIndexFor is the index of coords for metricID over dims, built when
// asked after indexAfterScans scans and kept on ctx (WithFactIndexes); nil —
// scan — before that, when ctx keeps none, or when the facts cannot be
// indexed.
func factIndexFor(ctx context.Context, dims map[string]*Dimension, metricID string, metricDimIDs []string, coords []map[string]string) *factIndex {
	cache, _ := ctx.Value(factIndexesKey{}).(*factIndexes)
	if cache == nil || len(coords) == 0 {
		return nil // no facts: the scan finds every child factless without visiting any
	}
	key := factIndexKey{metricID: metricID, first: &coords[0], n: len(coords)}
	cache.mu.Lock()
	for _, idx := range cache.m[key] {
		if idx.builtFor(dims, metricDimIDs) {
			cache.mu.Unlock()
			return idx.usable()
		}
	}
	if cache.asked[key] < indexAfterScans {
		cache.asked[key]++
		cache.mu.Unlock()
		return nil
	}
	cache.mu.Unlock()
	idx := buildFactIndex(dims, metricDimIDs, coords)
	if idx == nil {
		// Kept as unusable, so the next read scans at once instead of
		// trying to build it again.
		idx = &factIndex{metricDims: slices.Clone(metricDimIDs), dims: dimsOf(dims, metricDimIDs)}
	}
	cache.mu.Lock()
	cache.m[key] = append(cache.m[key], idx)
	cache.mu.Unlock()
	return idx.usable()
}

// usable is idx, or nil for one that could not be built (no lists).
func (idx *factIndex) usable() *factIndex {
	if idx.under == nil {
		return nil
	}
	return idx
}

func dimsOf(dims map[string]*Dimension, metricDimIDs []string) []*Dimension {
	out := make([]*Dimension, len(metricDimIDs))
	for i, p := range metricDimIDs {
		out[i] = dims[p]
	}
	return out
}
