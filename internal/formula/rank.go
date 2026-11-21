package formula

import (
	"fmt"
	"sort"
	"strings"
)

// RANK and RANKIFS (2026-11-21, from an inventory model that ranked items
// by value with three COUNTIFS): the place of the cell's value among the
// values of the members of a dimension, as Excel's RANK.EQ — the largest
// is 1 by default, equal values share a rank and the next rank is skipped.
//
//	RANK(metric, dimension [, order])
//	RANKIFS(metric, range1, criterion1 [, range2, criterion2 ...] [, order])
//
// RANK ranks among every leaf of one dimension, the cell's other
// coordinates kept; RANKIFS among the member combinations its criteria
// keep (the *IFS criteria): several dimensions (product, "*", location,
// "*") or a group (product.category, product.category). The order is 0 or
// "DESC" (largest first, the default), 1 or "ASC" (smallest first).
//
// A member with no recorded value is not ranked, and the cell has no rank
// (blank) when it is not in the ranked set: its own value not recorded, a
// total of a range dimension, a member the criteria leave out.

// RankedSet is what one RANK call ranks among, kept in
// DimEvalContext.Ranks: the recorded values in ascending order and each
// ranked member combination's value.
type RankedSet struct {
	values  []float64
	members map[string]float64
}

// IsRankFormula reports whether text is a RANK or RANKIFS call as a whole:
// a metric whose cells are ranks, which never add up to a total.
func IsRankFormula(text string) bool {
	node, err := Parse(text)
	if err != nil {
		return false
	}
	call, ok := node.(*CallExpr)
	return ok && (strings.EqualFold(call.Name, conditionalRank) || strings.EqualFold(call.Name, conditionalRankIfs))
}

// rankOrderArg is the order argument of a RANK or RANKIFS call, nil when
// it is left out.
func rankOrderArg(fn string, args []Node) Node {
	switch fn {
	case conditionalRank:
		if len(args) == 3 {
			return args[2]
		}
	case conditionalRankIfs:
		if len(args) >= 4 && len(args)%2 == 0 {
			return args[len(args)-1]
		}
	}
	return nil
}

// rankDescending reads an order: 0, FALSE, "DESC" or blank rank the
// largest first; any other number, TRUE or "ASC" the smallest first.
func rankDescending(fn string, v Value) (bool, *FormulaError) {
	switch v.Kind() {
	case KindBlank:
		return true, nil
	case KindNumber, KindBool:
		n, _ := v.Number()
		return n == 0, nil
	case KindString:
		switch strings.ToUpper(strings.TrimSpace(v.String())) {
		case "DESC", "":
			return true, nil
		case "ASC":
			return false, nil
		}
	case KindError:
	}
	return false, errValue(fmt.Sprintf(`%s: the order is 0 or "DESC" (largest first, the default) or 1 or "ASC" (smallest first), got %s`, fn, v.String()))
}

// rankKey names a member combination of the ranked set.
func rankKey(dimOrder []string, codes map[string]string) string {
	var sb strings.Builder
	for _, dim := range dimOrder {
		sb.WriteString(strings.ToLower(codes[dim]))
		sb.WriteString(memoKeySeparator)
	}
	return sb.String()
}

// rankCall evaluates RANK or RANKIFS at the cell.
func rankCall(ctx *EvalContext, d *DimEvalContext, fn string, args []Node, source string, dimOrder []string, crits []evaluatedCriterion) Value {
	desc := true
	if order := rankOrderArg(fn, args); order != nil {
		v := ctx.eval(order)
		if v.IsError() {
			return v
		}
		var ferr *FormulaError
		if desc, ferr = rankDescending(fn, v); ferr != nil {
			return ErrorVal(ferr)
		}
	}
	if d.Resolve == nil || d.Current == nil {
		return ErrorVal(dimCallbackMissing(fn, "dimensional reads"))
	}
	if fn == conditionalRank && crits[0].metricDims != nil {
		return ErrorVal(&FormulaError{Code: CodeDimensionArgRequired,
			Message: fmt.Sprintf("%s: %s is a metric, not a dimension — the second argument names the dimension whose members are ranked", fn, crits[0].rng.dim)})
	}
	if _, ok := d.picklist(source); ok {
		return ErrorVal(errValue(fmt.Sprintf("%s: %s is a pick-list, whose members cannot be ranked as numbers", fn, source)))
	}

	// The cell's place along every range dimension.
	own := make(map[string]string, len(dimOrder))
	for _, dim := range dimOrder {
		code, pinned, ferr := d.Current(dim)
		if ferr != nil {
			return ErrorVal(ferr)
		}
		if !pinned {
			return BlankVal()
		}
		own[dim] = code
	}

	set, ferr := rankedSet(d, fn, source, dimOrder, crits)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	v0, ok := set.members[rankKey(dimOrder, own)]
	if !ok {
		return BlankVal()
	}
	if desc {
		above := len(set.values) - sort.Search(len(set.values), func(i int) bool { return set.values[i] > v0 })
		return NumberVal(float64(1 + above))
	}
	return NumberVal(float64(1 + sort.SearchFloat64s(set.values, v0)))
}

// rankedSet returns the members a rank call ranks among with their
// recorded values, from DimEvalContext.Ranks when another cell of the set
// read it first in the pass.
func rankedSet(d *DimEvalContext, fn, source string, dimOrder []string, crits []evaluatedCriterion) (*RankedSet, *FormulaError) {
	memoKey := ""
	if d.Ranks != nil && d.CoordKey != nil {
		var sb strings.Builder
		sb.WriteString(strings.ToUpper(source))
		sb.WriteString(memoKeySeparator)
		for _, c := range crits {
			sb.WriteString(strings.ToUpper(c.rng.dim + dimPropertyDisplaySeparator + c.rng.prop))
			sb.WriteString(memoValueSeparator)
			sb.WriteString(valueKey(c.raw))
			sb.WriteString(memoKeySeparator)
		}
		sb.WriteString(d.CoordKey(dimOrder))
		memoKey = sb.String()
		if set, ok := d.Ranks[memoKey]; ok {
			return set, nil
		}
	}
	var metricCrits []evaluatedCriterion
	for _, c := range crits {
		if c.metricDims != nil {
			metricCrits = append(metricCrits, c)
		}
	}
	matches, tuples, ferr := conditionalMatches(d, fn, dimOrder, crits)
	if ferr != nil {
		return nil, ferr
	}
	set := &RankedSet{members: map[string]float64{}}
	if tuples > 0 {
		idx := make([]int, len(dimOrder))
		overrides := make(map[string]string, len(dimOrder))
		for {
			for i, dim := range dimOrder {
				overrides[dim] = matches[i][idx[i]]
			}
			keep, ferr := metricCriteriaMatch(d, metricCrits, overrides)
			if ferr != nil {
				return nil, ferr
			}
			if keep {
				v, found, ferr := d.Resolve(source, overrides)
				if ferr != nil {
					return nil, ferr
				}
				if found {
					set.values = append(set.values, v)
					set.members[rankKey(dimOrder, overrides)] = v
				}
			}
			k := len(idx) - 1
			for ; k >= 0; k-- {
				idx[k]++
				if idx[k] < len(matches[k]) {
					break
				}
				idx[k] = 0
			}
			if k < 0 {
				break
			}
		}
	}
	sort.Float64s(set.values)
	if memoKey != "" && len(d.Ranks) < conditionalMemoCap {
		d.Ranks[memoKey] = set
	}
	return set, nil
}
