package dataexport

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Snapshot is one user's view of a grid: its structure without the members
// and metrics hidden from that user, and its values keyed the way /api/grid
// keys `cells` ("metricID:code1:code2…" in the metric's DimensionIDs order).
// Values withheld from the user are simply absent.
type Snapshot struct {
	Grid
	Cells map[string]float64
}

// Value is one cell of a rendered table: text (a member code, label or
// metric name), a number, or empty.
type Value struct {
	Text  string
	Num   float64
	IsNum bool
}

func (v Value) Empty() bool { return !v.IsNum && v.Text == "" }

// Table is a rendered export, before it is written in its file format.
type Table struct {
	Header []string
	// DefaultHeader is Header before column_names renamed it: the keys
	// column_names takes.
	DefaultHeader []string
	Rows          [][]Value
	Warnings      []string
	spec          Spec
}

// Spec is the normalized spec the table was rendered with.
func (t *Table) Spec() Spec { return t.spec }

// defaultHeader is the export's columns before column_names renames them.
func (p *plan) defaultHeader(g Grid) []string {
	s := p.spec
	var h []string
	for _, d := range p.rowDims {
		h = append(h, d.Name)
		if s.MemberDisplay == DisplayCodeAndLabel {
			h = append(h, d.Name+" label")
		}
	}
	switch s.Layout {
	case LayoutLong:
		h = append(h, "metric", "value")
	case LayoutPivot:
		if len(p.metrics) > 1 {
			h = append(h, "metric")
		}
		if p.pivotDim != nil {
			for _, m := range p.pivotLeaves() {
				h = append(h, p.memberHeader(m))
			}
		}
	default:
		for _, m := range p.metrics {
			h = append(h, p.metricText(m))
		}
	}
	return h
}

func (p *plan) header(g Grid) []string {
	h := p.defaultHeader(g)
	out := make([]string, len(h))
	for i, col := range h {
		out[i] = p.spec.rename(col)
	}
	return out
}

func (p *plan) metricText(m Metric) string {
	if p.spec.MetricDisplay == DisplayLabel {
		if m.Label != "" {
			return m.Label
		}
		return MetricLabel(m.Name)
	}
	return m.Name
}

func (p *plan) memberHeader(m Member) string {
	if p.spec.MemberDisplay == DisplayLabel && m.Label != "" {
		return m.Label
	}
	return m.Code
}

// allowedLeaves is a dimension's leaves that pass the spec's filter, in
// display order.
func (p *plan) allowedLeaves(d Dimension) []Member {
	allowed := p.allowed[d.ID]
	var out []Member
	for _, m := range d.leaves() {
		if allowed[m.Code] {
			out = append(out, m)
		}
	}
	return out
}

func (p *plan) pivotLeaves() []Member {
	if p.pivotDim == nil {
		return nil
	}
	return p.allowedLeaves(*p.pivotDim)
}

// splitCodes recovers a cell key's member codes. Codes are joined with ":",
// which a code may itself contain, so a key that does not split cleanly is
// matched against each dimension's known codes in turn.
func splitCodes(rest string, dims []Dimension) ([]string, bool) {
	if parts := strings.Split(rest, ":"); len(parts) == len(dims) {
		return parts, true
	}
	codes := make([]string, len(dims))
	var match func(i int, s string) bool
	match = func(i int, s string) bool {
		if i == len(dims)-1 {
			for _, m := range dims[i].Members {
				if m.Code == s {
					codes[i] = s
					return true
				}
			}
			return false
		}
		for _, m := range dims[i].Members {
			if strings.HasPrefix(s, m.Code+":") && match(i+1, s[len(m.Code)+1:]) {
				codes[i] = m.Code
				return true
			}
		}
		return false
	}
	if len(dims) == 0 || !match(0, rest) {
		return nil, false
	}
	return codes, true
}

type rowData struct {
	codes map[string]string  // dimension ID -> leaf code
	vals  map[string]float64 // metric ID, or metric ID + "\x1f" + pivot code
}

// Render lays a snapshot out as the spec describes. A spec naming a metric
// or member the snapshot lacks — hidden from this user — renders without
// it, as that user's grid does; a spec that is structurally invalid is an
// error.
func Render(spec Spec, snap Snapshot) (*Table, error) {
	s := spec.Normalized()
	p, problems := resolve(s, snap.Grid, false)
	if len(problems) > 0 {
		return nil, fmt.Errorf("export spec: %s", strings.Join(problems, "; "))
	}
	t := &Table{Header: p.header(snap.Grid), DefaultHeader: p.defaultHeader(snap.Grid), spec: s}

	dimByID := map[string]Dimension{}
	gridDims := map[string]bool{}
	for _, d := range snap.Dimensions {
		dimByID[d.ID] = d
		gridDims[d.ID] = true
	}

	// Which metrics' cells can be placed: a metric keyed by exactly this
	// grid's dimensions. One that also spans another grid's dimensions has
	// no value per THIS grid's leaf combination.
	placeable := map[string]Metric{}
	for _, m := range p.metrics {
		ok := len(m.DimensionIDs) == len(snap.Dimensions)
		for _, id := range m.DimensionIDs {
			ok = ok && gridDims[id]
		}
		if !ok {
			t.Warnings = append(t.Warnings, fmt.Sprintf("metric %q is keyed by dimensions outside this grid; its values are not exported", m.Name))
			continue
		}
		placeable[m.ID] = m
	}

	rows := map[string]*rowData{}
	rowKey := func(codes map[string]string) string {
		parts := make([]string, len(p.rowDims))
		for i, d := range p.rowDims {
			parts[i] = codes[d.ID]
		}
		return strings.Join(parts, "\x1f")
	}
	for key, v := range snap.Cells {
		i := strings.IndexByte(key, ':')
		if i <= 0 {
			continue
		}
		m, ok := placeable[key[:i]]
		if !ok {
			continue
		}
		dims := make([]Dimension, len(m.DimensionIDs))
		for j, id := range m.DimensionIDs {
			dims[j] = dimByID[id]
		}
		codeList, ok := splitCodes(key[i+1:], dims)
		if !ok {
			continue
		}
		codes := make(map[string]string, len(codeList))
		keep := true
		for j, id := range m.DimensionIDs {
			// allowed holds leaves only, so this also drops the rollup
			// rows /api/grid serves for calculated metrics.
			if !p.allowed[id][codeList[j]] {
				keep = false
				break
			}
			codes[id] = codeList[j]
		}
		if !keep {
			continue
		}
		for id, pinned := range p.pinned {
			if codes[id] != pinned {
				keep = false
			}
		}
		if !keep {
			continue
		}
		rk := rowKey(codes)
		r := rows[rk]
		if r == nil {
			r = &rowData{codes: codes, vals: map[string]float64{}}
			rows[rk] = r
		}
		col := m.ID
		if p.pivotDim != nil {
			col += "\x1f" + codes[p.pivotDim.ID]
		}
		r.vals[col] = v
	}

	// Row order: each row dimension's display order, outermost first.
	order := make([]map[string]int, len(p.rowDims))
	for i, d := range p.rowDims {
		order[i] = map[string]int{}
		for j, m := range d.Members {
			order[i][m.Code] = j
		}
	}
	var list []*rowData
	if s.IncludeEmptyRows {
		total := 1
		for _, d := range p.rowDims {
			total *= len(p.allowedLeaves(d))
			if total > MaxRows {
				return nil, fmt.Errorf("this export would have more than %d rows — filter its dimensions or turn off include_empty_rows", MaxRows)
			}
		}
		var walk func(i int, codes map[string]string)
		walk = func(i int, codes map[string]string) {
			if i == len(p.rowDims) {
				cp := make(map[string]string, len(codes))
				for k, v := range codes {
					cp[k] = v
				}
				if r := rows[rowKey(cp)]; r != nil {
					list = append(list, r)
				} else {
					list = append(list, &rowData{codes: cp, vals: map[string]float64{}})
				}
				return
			}
			for _, m := range p.allowedLeaves(p.rowDims[i]) {
				codes[p.rowDims[i].ID] = m.Code
				walk(i+1, codes)
			}
			delete(codes, p.rowDims[i].ID)
		}
		walk(0, map[string]string{})
	} else {
		for _, r := range rows {
			list = append(list, r)
		}
		sort.Slice(list, func(a, b int) bool {
			for i, d := range p.rowDims {
				oa, ob := order[i][list[a].codes[d.ID]], order[i][list[b].codes[d.ID]]
				if oa != ob {
					return oa < ob
				}
			}
			return false
		})
	}

	labelOf := map[string]map[string]string{}
	for _, d := range snap.Dimensions {
		labelOf[d.ID] = map[string]string{}
		for _, m := range d.Members {
			labelOf[d.ID][m.Code] = m.Label
		}
	}
	dimCells := func(r *rowData) []Value {
		var out []Value
		for _, d := range p.rowDims {
			code := r.codes[d.ID]
			switch s.MemberDisplay {
			case DisplayLabel:
				label := labelOf[d.ID][code]
				if label == "" {
					label = code
				}
				out = append(out, Value{Text: label})
			case DisplayCodeAndLabel:
				out = append(out, Value{Text: code}, Value{Text: labelOf[d.ID][code]})
			default:
				out = append(out, Value{Text: code})
			}
		}
		return out
	}
	num := func(v float64, ok bool) Value {
		if !ok {
			return Value{}
		}
		if s.Decimals != nil {
			f := math.Pow(10, float64(*s.Decimals))
			v = math.Round(v*f) / f
		}
		return Value{Num: v, IsNum: true}
	}

	var metricsInOrder []Metric
	for _, m := range p.metrics {
		if _, ok := placeable[m.ID]; ok {
			metricsInOrder = append(metricsInOrder, m)
		}
	}
	emit := func(row []Value) error {
		if len(t.Rows) >= MaxRows {
			return fmt.Errorf("this export would have more than %d rows — filter its dimensions", MaxRows)
		}
		t.Rows = append(t.Rows, row)
		return nil
	}
	for _, r := range list {
		switch s.Layout {
		case LayoutLong:
			for _, m := range metricsInOrder {
				v, ok := r.vals[m.ID]
				if !ok && !s.IncludeEmptyRows {
					continue
				}
				row := append(dimCells(r), Value{Text: p.metricText(m)}, num(v, ok))
				if err := emit(row); err != nil {
					return nil, err
				}
			}
		case LayoutPivot:
			for _, m := range metricsInOrder {
				row := dimCells(r)
				if len(p.metrics) > 1 {
					row = append(row, Value{Text: p.metricText(m)})
				}
				found := false
				for _, leaf := range p.pivotLeaves() {
					v, ok := r.vals[m.ID+"\x1f"+leaf.Code]
					found = found || ok
					row = append(row, num(v, ok))
				}
				if !found && !s.IncludeEmptyRows {
					continue
				}
				if err := emit(row); err != nil {
					return nil, err
				}
			}
		default:
			row := dimCells(r)
			for _, m := range p.metrics {
				v, ok := r.vals[m.ID]
				row = append(row, num(v, ok))
			}
			if err := emit(row); err != nil {
				return nil, err
			}
		}
	}
	return t, nil
}

// FormatNumber writes a value the way the spec's text formats do: fixed
// decimals when set, full precision otherwise, with the chosen decimal
// separator.
func FormatNumber(v float64, s Spec) string {
	var out string
	if s.Decimals != nil {
		out = strconv.FormatFloat(v, 'f', *s.Decimals, 64)
	} else {
		out = strconv.FormatFloat(v, 'f', -1, 64)
	}
	if s.DecimalSeparator == "," {
		out = strings.Replace(out, ".", ",", 1)
	}
	return out
}

// TextRows is the table as text, the first n rows (n <= 0 = all) — for a
// preview.
func (t *Table) TextRows(n int) [][]string {
	if n <= 0 || n > len(t.Rows) {
		n = len(t.Rows)
	}
	out := make([][]string, n)
	for i := 0; i < n; i++ {
		row := make([]string, len(t.Rows[i]))
		for j, v := range t.Rows[i] {
			if v.IsNum {
				row[j] = FormatNumber(v.Num, t.spec)
			} else {
				row[j] = v.Text
			}
		}
		out[i] = row
	}
	return out
}
