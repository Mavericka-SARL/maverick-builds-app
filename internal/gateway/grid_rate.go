package gateway

import (
	"maps"
	"strings"
)

// A metric totalled as a rate (agg_rule "rate", Anaplan's Ratio summary) is,
// above its leaves, its numerator over its denominator, each read there by
// its own rules (calculation.RatioFormula). A calculated one has the
// scheduler's rows for that; an input has none — its subtotals and total are
// worked out by the read itself, from the operands, below. Before
// 2026-11-24 a grid totalled such an input as the plain sum of its cells
// (prices 1,000 × 4 read 4,000 against revenue / units = 1,520), and showed
// "—" on every subtotal.

// inputRates are the input metrics totalled as a rate, with both operands
// named, among shown — every one when shown is nil.
func inputRates(metrics []metricRow, shown map[string]bool) []string {
	var ids []string
	for _, m := range metrics {
		if m.IsInput && rateOperands(m) != nil && (shown == nil || shown[m.ID]) {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// ratioTotals sets each of ids' totals to its numerator's total over its
// denominator's — on a totals-only read, where both are totals already —
// and leaves none where either has none or the denominator is 0.
func ratioTotals(ids []string, metrics []metricRow, totals map[string]float64) {
	if len(ids) == 0 {
		return
	}
	byID := make(map[string]metricRow, len(metrics))
	for _, m := range metrics {
		byID[m.ID] = m
	}
	for _, id := range ids {
		ops := rateOperands(byID[id])
		if ops == nil {
			continue
		}
		n, okN := totals[ops[0]]
		d, okD := totals[ops[1]]
		if okN && okD && d != 0 {
			totals[id] = n / d
		} else {
			delete(totals, id)
		}
	}
}

// mergeInputRates replaces what a read gave the input rate metrics ids:
// their plain-sum totals go, and the subtotal cells and totals worked out
// for them (scopeCalcCells; nil maps: none yet) come in. Their own cells,
// as entered, stay.
func mergeInputRates(ids []string, rateCells, rateTotals, cells, totals map[string]float64) {
	if len(ids) == 0 {
		return
	}
	prefixes := make([]string, 0, len(ids))
	for _, id := range ids {
		delete(totals, id)
		if v, ok := rateTotals[id]; ok {
			totals[id] = v
		}
		prefixes = append(prefixes, id+":")
	}
	for k, v := range rateCells {
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				cells[k] = v
				break
			}
		}
	}
}

// withDimensionlessInputs is cells with each dimensionless input's value
// under its bare id, as scopeCalcCells reads it (cellKeyOf). Such an input
// has no cell key, so a read keeps it in totals only; left out, every plain
// formula reading it saw 0 in a filtered read (opening_ppe + … / life: 208
// instead of 32,351, found 2026-10-31). cells itself is not changed.
func withDimensionlessInputs(cells, totals map[string]float64, metrics []metricRow, metricDims map[string][]string) map[string]float64 {
	out, cloned := cells, false
	for _, m := range metrics {
		if v, ok := totals[m.ID]; ok && m.IsInput && len(metricDims[m.ID]) == 0 {
			if !cloned {
				out, cloned = maps.Clone(cells), true
			}
			out[m.ID] = v
		}
	}
	return out
}
