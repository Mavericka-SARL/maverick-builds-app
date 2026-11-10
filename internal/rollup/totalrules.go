package rollup

// Total rules (migration 174; owner's decision 2026-11-09): how a
// dimension's members total. "sum", the default, adds them up — each metric
// by its own rule (agg_rule, time_summary). A dimension of alternatives
// (scenarios: Base, Downside) totals otherwise:
//
//   - "first": a total is the value at the dimension's first leaf member
//     (Build's order) under what the read pins — Base;
//   - "last": at its last leaf member;
//   - "none": there is no total; a read names a member.
//
// The dimension's rule is every metric's over it unless the metric sets its
// own for that dimension (model.metric_dimension_rule). A time dimension's
// totals stay each metric's time summary.
//
// Each rule but sum is a choice of member, so a reader applies it by
// rewriting the coordinate it reads (RuleCombo) before any aggregation: the
// read then pins the dimension to one leaf, and no aggregation in this
// package or its callers ever adds that dimension's members up.

// The total rules.
const (
	TotalSum   = "sum"
	TotalFirst = "first"
	TotalLast  = "last"
	TotalNone  = "none"
)

// TotalRules lists the rules a dimension or a metric may have.
var TotalRules = []string{TotalSum, TotalFirst, TotalLast, TotalNone}

// ValidTotalRule reports whether rule is one of TotalRules ("" is sum).
func ValidTotalRule(rule string) bool {
	switch rule {
	case "", TotalSum, TotalFirst, TotalLast, TotalNone:
		return true
	}
	return false
}

// RuleFor is metricID's total rule over d: its own when it sets one, else
// d's; "" or "sum" adds the members up. A time dimension has none.
func (d *Dimension) RuleFor(metricID string) string {
	if d == nil || d.IsTime {
		return ""
	}
	if r, ok := d.MetricRules[metricID]; ok {
		return r
	}
	return d.TotalRule
}

// choosesMember reports whether rule picks a member (or none) instead of
// adding the members up.
func choosesMember(rule string) bool {
	return rule == TotalFirst || rule == TotalLast || rule == TotalNone
}

// RevisionTotalRules are a revision's total rules as loaded: each
// dimension's rule other than sum, and each metric's own rules by dimension.
type RevisionTotalRules struct {
	Dimension map[string]string            // dimension id → rule
	Metric    map[string]map[string]string // dimension id → metric id → rule
}

// Apply sets the rules on dims.
func (r RevisionTotalRules) Apply(dims map[string]*Dimension) {
	for id, d := range dims {
		if d == nil {
			continue
		}
		d.TotalRule = r.Dimension[id]
		d.MetricRules = r.Metric[id]
	}
}

// HasMemberRules reports whether metricID totals over any of its own
// dimensions by a rule that chooses a member — the readers that total from
// leaf rows themselves ask before doing the work.
func HasMemberRules(dims map[string]*Dimension, metricID string, metricDimIDs []string) bool {
	for _, id := range metricDimIDs {
		if choosesMember(dims[id].RuleFor(metricID)) {
			return true
		}
	}
	return false
}

// RuleCombo rewrites combo for metricID's total rules over its own
// dimensions: a dimension whose rule chooses a member, left unpinned or
// pinned to a member with members under it, is pinned to the first or last
// leaf under it — among the members related to what combo pins elsewhere,
// when a pinned dimension relates to it. ok=false when a rule is none there,
// or no leaf is left: the read has no value. combo itself is never changed;
// without such a rule it is returned as it is.
func RuleCombo(dims map[string]*Dimension, metricID string, metricDimIDs []string, combo map[string]string) (map[string]string, bool) {
	var out map[string]string
	for _, id := range metricDimIDs {
		d := dims[id]
		rule := d.RuleFor(metricID)
		if !choosesMember(rule) {
			continue
		}
		code, pinned := combo[id]
		var leaves []string
		if pinned {
			if FindMember(d, code) == nil || len(childrenOf(d, code)) == 0 {
				continue // a leaf (or a code it does not know): read as it is
			}
			leaves = LeafDescendants(d, code)
		} else {
			leaves = allLeafCodes(d)
			if related := relate(dims, id, combo); related != nil {
				in := make(map[string]bool, len(related))
				for _, c := range related {
					in[c] = true
				}
				kept := leaves[:0:0]
				for _, c := range leaves {
					if in[c] {
						kept = append(kept, c)
					}
				}
				leaves = kept
			}
		}
		if rule == TotalNone || len(leaves) == 0 {
			return nil, false
		}
		if out == nil {
			out = make(map[string]string, len(combo)+1)
			for k, v := range combo {
				out[k] = v
			}
		}
		if rule == TotalFirst {
			out[id] = leaves[0]
		} else {
			out[id] = leaves[len(leaves)-1]
		}
	}
	if out == nil {
		return combo, true
	}
	return out, true
}
