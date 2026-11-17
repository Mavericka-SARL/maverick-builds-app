package gateway

import (
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// maxParentDimensionCells caps the cells addParentDimensionCells works out
// for one grid: past it the group rows stay blank, as before, rather than
// slow the grid down.
const maxParentDimensionCells = 20000

// addParentDimensionCells adds to cells the values of a grid's formula and
// rate metrics (AggregateEvaluated) at the members of the dimensions above a
// grid dimension (parent_dimension_id): a grid of territories is grouped by
// their regions, and a region's row now shows its utilisation or its quota
// over achievable where it showed "—" — a ratio's subtotal cannot be added
// up from its rows, and the read sent nothing at a parent member (Territory
// & Quota build, 2026-11-16). Each value is the engine's rule for a total
// above the leaves: a rate is its numerator's total over its denominator's,
// a formula is evaluated with each metric it reads totalled by that
// metric's own rule (a sum added up, an average averaged, a ratio worked out
// the same way) over the parent's leaf rows of cells — the cells of the read,
// every metric's, before the grid keeps its own. A sum is still added up by
// the grid itself. Each cell is keyed as the grid keys the row: the parent
// member's code in its child dimension's place, the other dimensions at the
// member a dashboard pins, else at each of their leaf members. A reader with
// members hidden there gets none (a total of what they see would stand as
// the region's), nor does a formula that reads members (LOOKUP, *IFS,
// dim.property), whose value at a parent has no rows to come from.
func addParentDimensionCells(dims, allDims []gridDimension, metrics, allMetrics []metricRow, metricDims map[string][]string,
	hiddenByDim map[string]map[string]bool, scopePinned map[string]string, cells map[string]float64) {
	byID := make(map[string]*gridDimension, len(allDims))
	for i := range allDims {
		byID[allDims[i].ID] = &allDims[i]
	}
	byName := make(map[string]*metricRow, len(allMetrics))
	byMetricID := make(map[string]*metricRow, len(allMetrics))
	for i := range allMetrics {
		byName[strings.ToUpper(allMetrics[i].Name)] = &allMetrics[i]
		byMetricID[allMetrics[i].ID] = &allMetrics[i]
	}
	budget := maxParentDimensionCells
	for di, d := range dims {
		if d.ParentDimensionID == nil || len(hiddenByDim[d.ID]) > 0 {
			continue
		}
		// The members of each dimension above d, and the leaves of d under
		// each (walking parent codes up through every level).
		parentOf := map[string]string{} // member code → parent code, d and above
		for _, m := range d.Members {
			parentOf[m.Code] = m.ParentCode
		}
		var above []gridDimMember
		hidden := false
		for p, depth := d.ParentDimensionID, 0; p != nil && depth < 10; depth++ {
			a := byID[*p]
			if a == nil {
				break
			}
			hidden = hidden || len(hiddenByDim[a.ID]) > 0
			for _, m := range a.Members {
				above = append(above, m)
				parentOf[m.Code] = m.ParentCode
			}
			p = a.ParentDimensionID
		}
		if len(above) == 0 || hidden {
			continue
		}
		leavesUnder := map[string][]string{}
		for _, m := range d.Members {
			for p, depth := m.ParentCode, 0; p != "" && depth < 20; p, depth = parentOf[p], depth+1 {
				leavesUnder[p] = append(leavesUnder[p], m.Code)
			}
		}
		var targets []*metricRow
		for i := range metrics {
			if metrics[i].AggregateEvaluated && containsString(metricDims[metrics[i].ID], d.ID) {
				targets = append(targets, &metrics[i])
			}
		}
		if len(targets) == 0 {
			continue
		}
		// The other grid dimensions: the pinned member, else each leaf.
		others := [][]string{{}}
		for j, o := range dims {
			if j == di {
				continue
			}
			var codes []string
			if code, ok := scopePinned[o.ID]; ok {
				codes = []string{code}
			} else {
				isParent := map[string]bool{}
				for _, m := range o.Members {
					if m.ParentCode != "" {
						isParent[m.ParentCode] = true
					}
				}
				for _, m := range o.Members {
					if !isParent[m.Code] {
						codes = append(codes, m.Code)
					}
				}
			}
			next := make([][]string, 0, len(others)*len(codes))
			for _, prefix := range others {
				for _, c := range codes {
					next = append(next, append(append([]string{}, prefix...), c))
				}
			}
			others = next
		}
		if n := len(above) * len(others) * len(targets); n > budget {
			continue
		} else {
			budget -= n
		}
		for _, a := range above {
			leaves := leavesUnder[a.Code]
			if len(leaves) == 0 {
				continue
			}
			for _, rest := range others {
				point := map[string]string{}
				key := make([]string, len(dims))
				k := 0
				for j, o := range dims {
					if j == di {
						key[j] = a.Code
						continue
					}
					point[o.ID] = rest[k]
					key[j] = rest[k]
					k++
				}
				agg := parentAggregator{d: d.ID, leaves: leaves, point: point, cells: cells, metricDims: metricDims, byName: byName, byID: byMetricID}
				for _, m := range targets {
					if v, ok := agg.total(m, 0); ok {
						cells[m.ID+":"+strings.Join(key, ":")] = v
					}
				}
			}
		}
	}
}

// parentAggregator totals metrics over the leaf members of dimension d
// (leaves) at the other dimensions' members in point.
type parentAggregator struct {
	d          string
	leaves     []string
	point      map[string]string
	cells      map[string]float64
	metricDims map[string][]string
	byName     map[string]*metricRow
	byID       map[string]*metricRow
}

// leafValues are m's cells at each leaf that has one.
func (p parentAggregator) leafValues(m *metricRow) ([]float64, bool) {
	ds := p.metricDims[m.ID]
	if !containsString(ds, p.d) {
		return nil, false
	}
	var out []float64
	codes := make([]string, len(ds))
	for _, leaf := range p.leaves {
		for i, id := range ds {
			switch {
			case id == p.d:
				codes[i] = leaf
			case p.point[id] != "":
				codes[i] = p.point[id]
			default:
				return nil, false // a dimension of m the grid does not show
			}
		}
		if v, ok := p.cells[m.ID+":"+strings.Join(codes, ":")]; ok {
			out = append(out, v)
		}
	}
	return out, true
}

// total is m's value over the leaves by its own rule: false when the rule
// has no total there or a metric it reads cannot be totalled.
func (p parentAggregator) total(m *metricRow, depth int) (float64, bool) {
	if depth > 8 {
		return 0, false
	}
	switch m.AggRule {
	case "", "sum":
		vals, ok := p.leafValues(m)
		sum := 0.0
		for _, v := range vals {
			sum += v
		}
		return sum, ok
	case "average":
		if !m.AggregateEvaluated {
			vals, ok := p.leafValues(m)
			if !ok || len(vals) == 0 {
				return 0, false
			}
			sum := 0.0
			for _, v := range vals {
				sum += v
			}
			return sum / float64(len(vals)), true
		}
	case "rate":
		num, den := p.byID[m.AggNumeratorMetricID], p.byID[m.AggDenominatorMetricID]
		if num == nil || den == nil {
			return 0, false
		}
		n, ok1 := p.total(num, depth+1)
		dv, ok2 := p.total(den, depth+1)
		if !ok1 || !ok2 || dv == 0 {
			return 0, false
		}
		return n / dv, true
	case "formula":
	default:
		return 0, false
	}
	// A formula (or a pure-ratio average): evaluated with what it reads
	// totalled by their own rules.
	if m.Formula == nil || strings.TrimSpace(*m.Formula) == "" {
		return 0, false
	}
	an, err := formula.Analyze(*m.Formula)
	if err != nil || len(an.DimensionalCalls) > 0 || len(an.PropertyRefs) > 0 || len(an.CellDims) > 0 || len(an.DimensionArgs) > 0 || an.UsesTimeSeries {
		return 0, false
	}
	vars := map[string]formula.Value{}
	for _, ref := range an.References {
		r := p.byName[strings.ToUpper(ref.Name)]
		if r == nil {
			return 0, false
		}
		v, ok := p.total(r, depth+1)
		if !ok {
			return 0, false
		}
		vars[strings.ToUpper(ref.Name)] = formula.NumberVal(v)
	}
	res, err := formula.EvalWithContext(*m.Formula, &formula.EvalContext{Vars: vars})
	if err != nil || res.IsError() {
		return 0, false
	}
	n, ok := res.Number()
	return n, ok && res.Kind() == formula.KindNumber
}
