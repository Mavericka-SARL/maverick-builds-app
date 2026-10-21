package calculation

import (
	"encoding/json"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// A cell write changes a few cells, and a pass used to recalculate every
// cell of every metric they reach: a driver typed for one scenario of the
// education FP&A model recalculated all three scenarios of 73 metrics,
// two thirds of them to the values they already had (2026-10-20). A pass
// that knows which cells were written follows them instead:
//
//   - a written input's region is the members its cells pin;
//   - a metric's cells to evaluate are its dependencies' changed regions
//     carried onto its own dimensions (project), and only those are
//     evaluated, the others keep the values they hold;
//   - what a metric then writes is the difference from what it held, and
//     the cells that differ are the region its dependents follow.
//
// A region only ever over-states what changed: a formula that reads
// across a dimension (LOOKUP, the criteria functions, a time offset, a
// pinned reference) leaves that dimension unbounded, and whatever cannot
// be followed (an unparsed formula, a criteria range) is everywhere.

// region is the part of a metric that may have changed: per dimension ID,
// the leaf member codes; a dimension it does not bound is every member. A
// nil *region is "nothing changed".
type region struct {
	bound map[string]map[string]bool
	// aggregates: a row above the leaves changed. For a formula or rate
	// metric those rows are not their leaves combined, so a reader that
	// evaluates its own totals from them evaluates every total again.
	aggregates bool
}

func everywhere() *region { return &region{} }

// unbounded reports whether r bounds no dimension: anything may have changed.
func (r *region) unbounded() bool { return r != nil && len(r.bound) == 0 }

// union is every cell in r or o.
func (r *region) union(o *region) *region {
	if r == nil {
		return o
	}
	if o == nil {
		return r
	}
	out := &region{bound: map[string]map[string]bool{}, aggregates: r.aggregates || o.aggregates}
	for d, a := range r.bound {
		b, ok := o.bound[d]
		if !ok {
			continue // unbounded in o: unbounded in the union
		}
		u := make(map[string]bool, len(a)+len(b))
		for c := range a {
			u[c] = true
		}
		for c := range b {
			u[c] = true
		}
		out.bound[d] = u
	}
	return out
}

// contains reports whether the leaf combo lies in r.
func (r *region) contains(combo map[string]string) bool {
	for d, codes := range r.bound {
		if !codes[combo[d]] {
			return false
		}
	}
	return true
}

// cellRegion is the region of one written cell of a metric on dimIDs: on
// each of them the leaves at or under the member it names; a dimension it
// does not name is unbounded.
func cellRegion(allDims map[string]*rollup.Dimension, dimIDs []string, combo map[string]string) *region {
	r := &region{bound: map[string]map[string]bool{}}
	for _, d := range dimIDs {
		code, ok := combo[d]
		dim := allDims[d]
		if !ok || dim == nil {
			continue
		}
		set := map[string]bool{}
		for _, c := range rollup.LeafDescendants(dim, code) {
			set[c] = true
		}
		if len(set) == 0 {
			set[code] = true
		}
		r.bound[d] = set
	}
	return r
}

// readShape is how a metric reads its dependencies, for project.
type readShape struct {
	// everywhere: what it reads cannot be followed (an unparsed formula, a
	// criteria range read over all its cells).
	everywhere bool
	// moving are the dimensions it reads at members other than the cell's:
	// LOOKUP's and PARENT's dimensions, criteria ranges, the dimensions a
	// reference is pinned along, and time under a time offset.
	moving map[string]bool
}

// shapeOf analyses def's formula. dimIDs are its dimensions.
func shapeOf(def *MetricDef, dimIDs []string, allDims map[string]*rollup.Dimension, dimIDToName map[string]string) readShape {
	an, err := formula.Analyze(def.Formula)
	if err != nil {
		return readShape{everywhere: true}
	}
	if len(an.RangeNames) > 0 {
		return readShape{everywhere: true}
	}
	for _, c := range an.DimensionalCalls {
		if !strings.EqualFold(c.Func, "LOOKUP") {
			return readShape{everywhere: true}
		}
	}
	byName := map[string][]string{}
	for id, name := range dimIDToName {
		byName[strings.ToUpper(name)] = append(byName[strings.ToUpper(name)], id)
	}
	sh := readShape{moving: map[string]bool{}}
	alongTime := an.UsesTimeSeries || len(an.TimeSums) > 0
	move := func(name string) {
		for _, id := range byName[strings.ToUpper(strings.TrimSpace(name))] {
			sh.moving[id] = true
		}
	}
	for _, ref := range an.References {
		if ref.MinTimeOffset != 0 || ref.MaxTimeOffset != 0 || ref.UnboundedPast || ref.UnboundedFuture {
			alongTime = true
		}
		if ref.Dimensional && len(ref.OverriddenDims) == 0 {
			alongTime = true // the *VALUE family reads along time
		}
		for _, d := range ref.OverriddenDims {
			move(d)
		}
	}
	for _, d := range an.DimensionArgs {
		move(d)
	}
	for _, c := range an.DimensionalCalls {
		for _, d := range c.Dims {
			move(d)
		}
	}
	if alongTime {
		for _, id := range dimIDs {
			if d := allDims[id]; d != nil && d.IsTime {
				sh.moving[id] = true
			}
		}
	}
	return sh
}

// project carries a dependency's changed region onto the cells of the
// metric reading it: a cell's read pins the dimensions both share at the
// cell's own members, so a dimension both have stays bounded unless the
// reader moves along it; the reader's other dimensions are unbounded. A
// formula or rate dependency read above its leaves (the reader lacks one
// of its dimensions, or moves along one) answers from rows its leaves do
// not determine: everywhere.
func project(changed *region, dep *MetricDef, depDims []string, depRule string, reader readShape, readerDims []string, readerRule string) *region {
	if changed == nil {
		return nil
	}
	if reader.everywhere || changed.unbounded() {
		return everywhere()
	}
	out := &region{bound: map[string]map[string]bool{}}
	if exactTotals(depRule, dep) {
		own := make(map[string]bool, len(readerDims))
		for _, d := range readerDims {
			own[d] = true
		}
		for _, d := range depDims {
			if !own[d] || reader.moving[d] {
				return everywhere()
			}
		}
		// The reader's own totals, a formula's, read the dependency's.
		out.aggregates = changed.aggregates && exactTotals(readerRule, nil)
	}
	for _, d := range readerDims {
		if reader.moving[d] {
			continue
		}
		if codes, ok := changed.bound[d]; ok {
			out.bound[d] = codes
		}
	}
	return out
}

// exactTotals reports whether a metric's rows above its leaves are its
// formula's own answers there (agg_rule formula or rate, a calculated
// text), not its leaves combined. dep may be nil (a rule alone).
func exactTotals(rule string, def *MetricDef) bool {
	return rule == string(rollup.AggFormula) || rule == string(rollup.AggRate) || (def != nil && def.Text && !def.IsInput)
}

// coverTest reports whether a combo above the leaves covers part of inc's
// region: on every dimension the region bounds, the member it pins has a
// leaf of the region at or under it. A total that covers none of it reads
// nothing that changed and keeps the value it holds.
func coverTest(inc *increment, allDims map[string]*rollup.Dimension) func(map[string]string) bool {
	if !inc.partial() || inc.in.aggregates {
		return func(map[string]string) bool { return true }
	}
	memo := map[string]bool{}
	return func(combo map[string]string) bool {
		for d, set := range inc.in.bound {
			code, pinned := combo[d]
			if !pinned {
				continue
			}
			k := d + "\x00" + code
			hit, ok := memo[k]
			if !ok {
				hit = set[code]
				if dim := allDims[d]; !hit && dim != nil {
					for _, c := range rollup.LeafDescendants(dim, code) {
						if set[c] {
							hit = true
							break
						}
					}
				}
				memo[k] = hit
			}
			if !hit {
				return false
			}
		}
		return true
	}
}

// resultDiff compares the rows a metric writes with the rows it held:
// the keys to delete (changed or gone), the rows to insert (changed or
// new), and the leaf region that changed.
type resultDiff struct {
	del     []string
	ins     []CalcResultRow
	changed *region
}

// diffResults is resultDiff for def over dimIDs. old and oldText are its
// held values and texts by dimKey.
func diffResults(rows []CalcResultRow, old map[string]float64, oldText map[string]string, dimIDs []string, allDims map[string]*rollup.Dimension) resultDiff {
	leaves := make(map[string]map[string]bool, len(dimIDs))
	for _, d := range dimIDs {
		set := map[string]bool{}
		if dim := allDims[d]; dim != nil {
			for _, c := range rollup.LeafCodes(dim) {
				set[c] = true
			}
		}
		leaves[d] = set
	}
	var diff resultDiff
	box := map[string]map[string]bool{}
	aggregates := false
	mark := func(combo map[string]string) {
		if len(combo) != len(dimIDs) {
			aggregates = true // above the leaves
			return
		}
		for _, d := range dimIDs {
			if !leaves[d][combo[d]] {
				aggregates = true
				return
			}
		}
		for _, d := range dimIDs {
			if box[d] == nil {
				box[d] = map[string]bool{}
			}
			box[d][combo[d]] = true
		}
		if diff.changed == nil {
			diff.changed = &region{bound: box}
		}
	}
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		k := dimKey(r.DimMembers)
		seen[k] = true
		v, had := old[k]
		t, hadText := oldText[k]
		same := had && v == r.Value && hadText == (r.Text != nil) && (r.Text == nil || t == *r.Text)
		if same {
			continue
		}
		if had {
			diff.del = append(diff.del, k)
		}
		diff.ins = append(diff.ins, r)
		mark(r.DimMembers)
	}
	for k := range old {
		if seen[k] {
			continue
		}
		diff.del = append(diff.del, k)
		var combo map[string]string
		if json.Unmarshal([]byte(k), &combo) == nil {
			mark(combo)
		}
	}
	if diff.changed == nil && (len(diff.del) > 0 || len(diff.ins) > 0) {
		// Only rows above the leaves changed (a formula total): no leaf
		// box, but a change — a reader above the leaves sees it (project).
		diff.changed = &region{bound: map[string]map[string]bool{}}
		for _, d := range dimIDs {
			diff.changed.bound[d] = map[string]bool{}
		}
	}
	if diff.changed != nil {
		diff.changed.aggregates = aggregates
	}
	return diff
}
