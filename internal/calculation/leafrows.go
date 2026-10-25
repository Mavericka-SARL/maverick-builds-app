package calculation

// leafRows is the leaves a metric's totals and slices are reduced from
// (summarizeOverTime, oneDimSliceRows, aggregatePeriodRows): each leaf's
// member on a dimension and its value. A pass holds them as rows
// (resultRows), or, for a metric with very many cells, as member indices
// and values (lean.go) — the reductions read both the same way, in the
// same order.
type leafRows interface {
	Len() int
	// Member is leaf i's member on dimID; false when it has none there.
	Member(i int, dimID string) (string, bool)
	Value(i int) float64
}

// resultRows is leafRows over rows.
type resultRows []CalcResultRow

func (r resultRows) Len() int { return len(r) }
func (r resultRows) Member(i int, dimID string) (string, bool) {
	code, ok := r[i].DimMembers[dimID]
	return code, ok
}
func (r resultRows) Value(i int) float64 { return r[i].Value }

// subRows is the leaves of base at idx, in idx's order.
type subRows struct {
	base leafRows
	idx  []int
}

func (s subRows) Len() int                                  { return len(s.idx) }
func (s subRows) Member(i int, dimID string) (string, bool) { return s.base.Member(s.idx[i], dimID) }
func (s subRows) Value(i int) float64                       { return s.base.Value(s.idx[i]) }
