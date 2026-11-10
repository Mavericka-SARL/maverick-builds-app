package calculation

import "github.com/mavericks-engine/mavericks/internal/rollup"

// Total rules in the scheduler's stored totals (migration 174,
// rollup/totalrules.go). A metric whose dimension totals as one member
// (first, last) or not at all (none) has its '{}' total, its one-dimension
// slices and its rollup rows read at that member: the leaves combined are
// the chosen member's, a formula is evaluated there, and where the rule is
// none no row is written — as every reader computes them (rollup.RuleCombo),
// so a KPI served from a stored row and a chart resolving the same total
// read one number.

// ruledAt is where the stored row at slice reads metricID's leaves: the
// rule-chosen member of each dimension the slice leaves open or pins to a
// member with members under it (chosen), and the rewritten combo to
// evaluate a formula at. ok=false: a rule is none there, no row.
func ruledAt(dims map[string]*rollup.Dimension, metricID string, dimIDs []string, slice map[string]string) (at map[string]string, chosen map[string]string, ok bool) {
	at, ok = rollup.RuleCombo(dims, metricID, dimIDs, slice)
	if !ok {
		return nil, nil, false
	}
	for d, code := range at {
		if pinned, was := slice[d]; !was || pinned != code {
			if chosen == nil {
				chosen = map[string]string{}
			}
			chosen[d] = code
		}
	}
	return at, chosen, true
}

// atChosen keeps the rows of leaves at every chosen member (nil: all).
func atChosen(leaves leafRows, chosen map[string]string) leafRows {
	if len(chosen) == 0 {
		return leaves
	}
	var idx []int
	for i, n := 0, leaves.Len(); i < n; i++ {
		match := true
		for d, code := range chosen {
			if c, _ := leaves.Member(i, d); c != code {
				match = false
				break
			}
		}
		if match {
			idx = append(idx, i)
		}
	}
	return subRows{base: leaves, idx: idx}
}

// rowValues lists leaves' values.
func rowValues(leaves leafRows) []float64 {
	vals := make([]float64, leaves.Len())
	for i := range vals {
		vals[i] = leaves.Value(i)
	}
	return vals
}

// ruledTotal is the '{}' total of a metric with a member-choosing total
// rule, for the rules that combine leaves (formula and rate are evaluated):
// its leaves at the chosen members, combined as the unruled total combines
// all of them. ok=false: no total row.
func ruledTotal(leaves leafRows, chosen map[string]string, axis *timeAxis, rule, timeSummary string) (float64, bool) {
	picked := atChosen(leaves, chosen)
	if axis != nil {
		return summarizeOverTime(picked, axis, rule, timeSummary, nil)
	}
	if picked.Len() == 0 {
		return 0, false
	}
	if !rollup.Aggregates(rollup.AggRule(rule)) && picked.Len() > 1 {
		return 0, false
	}
	return rollup.CombineAgg(rowValues(picked), rollup.AggRule(rule)), true
}
