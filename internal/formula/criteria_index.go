package formula

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Criteria indexes: a SUMIFS (COUNTIFS, AVERAGEIFS, ...) over one dimension
// whose first metric criterion is an equality — SUMIFS(clf, cl_project,
// project), each project's cost lines — tested every member of the
// dimension at every cell: 575 cost lines × 42,000 project-month cells, 3.8 s
// of an 18 s recalculation (2026-11-27). The range metric's values are read
// once per pass and grouped by what an equality compares; each cell then
// visits only the members holding its value, in the dimension's order, and
// tests them as before (metricCriteriaMatch) — the same members, the same
// order, the same sum.
//
// The index is used only where it cannot change an answer: one dimension
// iterated, no criterion on the dimension itself (every leaf a candidate),
// an "=" criterion with an operand and no wildcards, and no range value
// that is an error to read or not a number (NaN compares equal to every
// number). Anything else is evaluated member by member, as before.

// CriteriaIndexes are a metric pass's criteria indexes (DimEvalContext); nil
// disables them.
type CriteriaIndexes struct {
	m map[criteriaIndexKey]*criteriaIndex
}

// NewCriteriaIndexes returns an empty set for one metric's pass.
func NewCriteriaIndexes() *CriteriaIndexes {
	return &CriteriaIndexes{m: map[criteriaIndexKey]*criteriaIndex{}}
}

type criteriaIndexKey struct {
	metric, dim string
	leaves      *DimMember // the pass's leaf list (DimMetadata keeps one per dimension)
	n           int
}

type criteriaIndex struct {
	usable bool
	text   map[string][]int // folded text → leaf positions, ascending
	num    map[uint64][]int // a number to 15 significant digits → leaf positions, ascending
}

// criteriaIndexMinLeaves: below this a member-by-member test costs less than
// building the index.
const criteriaIndexMinLeaves = 32

// indexedPositions are the positions in matches[0] whose members can satisfy
// the first metric criterion, ascending; false when the index does not apply
// (the caller tests every combination).
func indexedPositions(d *DimEvalContext, dimOrder []string, matches [][]string, metricCrits []evaluatedCriterion) ([]int, bool) {
	if d.CriteriaIndexes == nil || len(dimOrder) != 1 || len(metricCrits) == 0 || len(matches) != 1 {
		return nil, false
	}
	c := metricCrits[0]
	if len(c.metricDims) != 1 || !strings.EqualFold(c.metricDims[0], dimOrder[0]) {
		return nil, false
	}
	if c.crit.op != opEq || c.crit.operand == "" || c.crit.wildcards || (c.crit.isNum && math.IsNaN(c.crit.num)) {
		return nil, false
	}
	leaves, ferr := d.Leaves(dimOrder[0])
	if ferr != nil || len(leaves) < criteriaIndexMinLeaves || len(leaves) != len(matches[0]) {
		return nil, false // a criterion on the dimension narrowed the members
	}
	key := criteriaIndexKey{metric: strings.ToUpper(c.rng.dim), dim: strings.ToUpper(dimOrder[0]), leaves: &leaves[0], n: len(leaves)}
	idx := d.CriteriaIndexes.m[key]
	if idx == nil {
		idx = buildCriteriaIndex(d, c, dimOrder[0], leaves)
		d.CriteriaIndexes.m[key] = idx
	}
	if !idx.usable {
		return nil, false
	}
	text := idx.text[foldKey(c.crit.literal)]
	if !c.crit.isNum {
		return text, true // a number never equals a text operand
	}
	num := idx.num[numKey(c.crit.num)]
	if len(text) == 0 {
		return num, true
	}
	out := make([]int, 0, len(text)+len(num))
	out = append(append(out, text...), num...)
	sort.Ints(out)
	return out, true
}

func buildCriteriaIndex(d *DimEvalContext, c evaluatedCriterion, dim string, leaves []DimMember) *criteriaIndex {
	idx := &criteriaIndex{usable: true, text: map[string][]int{}, num: map[uint64][]int{}}
	for p, leaf := range leaves {
		candidate, ferr := criteriaRangeValue(d, c, map[string]string{dim: leaf.Code})
		if ferr != nil {
			return &criteriaIndex{} // the member-by-member test reports it where it meets it
		}
		switch {
		case candidate.IsError(), candidate.IsBlank():
			// never equal to an operand (criterion.matches)
		case candidate.Kind() == KindNumber:
			n, _ := candidate.Number()
			if math.IsNaN(n) {
				return &criteriaIndex{}
			}
			k := numKey(n)
			idx.num[k] = append(idx.num[k], p)
		default:
			text, _ := memberCode(candidate)
			if text == "" && candidate.Kind() == KindString {
				continue // blank
			}
			k := foldKey(text)
			idx.text[k] = append(idx.text[k], p)
		}
	}
	return idx
}

// numKey is what criterion.matches compares a number by: its 15 significant
// digits (cmpFloat), zero's sign aside.
func numKey(n float64) uint64 {
	s := significant15(n)
	if s == 0 {
		s = 0
	}
	return math.Float64bits(s)
}

// foldKey is equal for two texts exactly when strings.EqualFold holds: each
// rune replaced by the smallest of its simple case folding orbit.
func foldKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < m {
				m = f
			}
		}
		b.WriteRune(m)
	}
	return b.String()
}
