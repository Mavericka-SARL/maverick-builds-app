package reconcile

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"
)

// Propose drafts a map from the workbook and what the revision knows —
// metric names and labels, member codes and labels, the periods' dates —
// for the developer (or the AI Developer) to complete and confirm. It finds
// three layouts:
//   - a table whose header row names metrics, one row per item: the item's
//     members from the columns whose values name members (a key such as
//     "SKU001-DC" split into its parts);
//   - rows named by metrics under a row of dates, repeated in blocks when
//     the same labels come back at a fixed stride: the period from the date
//     above, the block's item from its title row;
//   - single values: a row named by a metric with no dimension, its value
//     the first number to the right.
//
// A dimension it cannot place stays unbound (read at its total); the notes
// say which, and that a scenario-like one is usually the case's.
func Propose(ctx context.Context, pool *pgxpool.Pool, modelID, revisionID string, data []byte) (Map, []string, error) {
	md, err := loadModel(ctx, pool, modelID, revisionID)
	if err != nil {
		return Map{}, nil, err
	}
	wb, err := openWorkbook(data)
	if err != nil {
		return Map{}, nil, err
	}
	p := proposer{md: md, wb: wb, byLabel: metricLabels(md)}
	var notes []string
	out := Map{Rules: []Rule{}}
	for _, sheet := range wb.names {
		rules, n := p.sheet(sheet)
		out.Rules = append(out.Rules, rules...)
		notes = append(notes, n...)
	}
	if len(out.Rules) == 0 {
		notes = append(notes, "No sheet has rows or columns named like the model's metrics (by name or label): write the rules by hand.")
	}
	return out, notes, nil
}

type proposer struct {
	md      *model
	wb      *workbook
	byLabel map[string]*metric
}

var nonWord = regexp.MustCompile(`[^a-z0-9%]+`)

func norm(s string) string {
	return strings.TrimSpace(nonWord.ReplaceAllString(strings.ToLower(s), " "))
}

// metricLabels indexes metrics by normalised name and label.
func metricLabels(md *model) map[string]*metric {
	out := map[string]*metric{}
	for _, m := range md.metrics {
		for _, k := range []string{m.name, m.label} {
			if n := norm(k); n != "" {
				if _, taken := out[n]; !taken {
					out[n] = m
				}
			}
		}
	}
	return out
}

// metricNamed is the metric a cell's text names: its name or label, or a
// label that ends with the text ("Check: net stock roll-forward" for "Net
// stock roll-forward") when only one does.
func (p *proposer) metricNamed(text string) *metric {
	n := norm(text)
	if n == "" || len(n) < 3 {
		return nil
	}
	if _, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
		return nil
	}
	if m := p.byLabel[n]; m != nil {
		return m
	}
	var hit *metric
	for k, m := range p.byLabel {
		if strings.HasSuffix(k, " "+n) || strings.HasPrefix(k, n+" ") {
			if hit != nil && hit != m {
				return nil
			}
			hit = m
		}
	}
	return hit
}

func (p *proposer) rows(sheet string) [][]string { return p.wb.sheets[strings.ToLower(sheet)] }

func colName(c int) string { n, _ := excelize.ColumnNumberToName(c); return n }

func (p *proposer) sheet(sheet string) ([]Rule, []string) {
	var rules []Rule
	var notes []string
	used := map[int]bool{} // rows a rule already covers
	if r, n, ok := p.dateRows(sheet, used); ok {
		rules = append(rules, r...)
		notes = append(notes, n...)
	}
	if r, n, ok := p.headerTable(sheet, used); ok {
		rules = append(rules, r)
		notes = append(notes, n...)
	}
	if r, ok := p.singleValues(sheet, used); ok {
		rules = append(rules, r)
	}
	return rules, notes
}

// ── A header row of metrics ────────────────────────────────────────────────

func (p *proposer) headerTable(sheet string, used map[int]bool) (Rule, []string, bool) {
	rows := p.rows(sheet)
	bestRow, best := 0, map[int]*metric{}
	for r := 1; r <= min(len(rows), 60); r++ {
		found := map[int]*metric{}
		for c, v := range rows[r-1] {
			if m := p.metricNamed(v); m != nil {
				found[c+1] = m
			}
		}
		if len(found) >= 2 && len(found) > len(best) {
			bestRow, best = r, found
		}
	}
	if bestRow == 0 {
		return Rule{}, nil, false
	}
	// The dimensions those metrics have, and the columns naming their members.
	dims := map[string]*dim{}
	for _, m := range best {
		for _, d := range m.dims {
			if dd := p.md.dimByID[d]; dd != nil {
				dims[dd.name] = dd
			}
		}
	}
	members := map[string]Binding{}
	keyCols := map[int]bool{}
	width := 0
	for r := bestRow + 1; r <= min(len(rows), bestRow+40); r++ {
		width = max(width, len(rows[r-1]))
	}
	names := sortedDimNames(dims)
	for c := 1; c <= width; c++ {
		if best[c] != nil {
			continue
		}
		sample := p.columnSample(sheet, c, bestRow+1, 30)
		if len(sample) == 0 {
			continue
		}
		for _, name := range names {
			if _, done := members[name]; done {
				continue
			}
			if resolvesMost(dims[name], sample, "", 0) {
				members[name] = Binding{Column: colName(c)}
				keyCols[c] = true
				break
			}
		}
		// A key of several members: "SKU001-DC".
		for _, sep := range []string{"-", "|", "/", "_", " "} {
			for part := 1; part <= 3; part++ {
				for _, name := range names {
					if _, done := members[name]; done {
						continue
					}
					if resolvesMost(dims[name], sample, sep, part) {
						members[name] = Binding{Column: colName(c), Split: sep, Part: part}
						keyCols[c] = true
					}
				}
			}
		}
	}
	if len(keyCols) == 0 && len(dims) > 0 {
		return Rule{}, nil, false
	}
	// Rows while a key column holds something.
	last := bestRow
	for r := bestRow + 1; r <= len(rows); r++ {
		filled := false
		for c := range keyCols {
			if p.wb.value(sheet, c, r) != "" {
				filled = true
			}
		}
		if !filled {
			break
		}
		last = r
	}
	if last == bestRow {
		return Rule{}, nil, false
	}
	byCol := map[string]string{}
	for c, m := range best {
		byCol[colName(c)] = m.name
	}
	for r := bestRow + 1; r <= last; r++ {
		used[r] = true
	}
	rule := Rule{Name: fmt.Sprintf("%s rows %d–%d", sheet, bestRow+1, last), Sheet: sheet, Rows: &Span{From: bestRow + 1, To: last},
		MetricsByColumn: byCol, Members: members}
	var ms []*metric
	for _, m := range best {
		ms = append(ms, m)
	}
	return rule, unboundNotes(rule.Name, ms, p.md, members), true
}

// columnSample is up to n non-blank values of a column from row on.
func (p *proposer) columnSample(sheet string, col, row, n int) []string {
	var out []string
	rows := p.rows(sheet)
	for r := row; r <= len(rows) && len(out) < n; r++ {
		if v := p.wb.value(sheet, col, r); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// resolvesMost: at least 80% of the sample names members of d (one part of
// each, split by sep, when sep is set).
func resolvesMost(d *dim, sample []string, sep string, part int) bool {
	if d == nil || len(sample) == 0 {
		return false
	}
	hit := 0
	for _, v := range sample {
		if sep != "" {
			ps := strings.Split(v, sep)
			if len(ps) < 2 || part > len(ps) {
				continue
			}
			v = ps[part-1]
		}
		if _, ok := d.resolve(v); ok {
			hit++
		}
	}
	return hit*5 >= len(sample)*4
}

func sortedDimNames(m map[string]*dim) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// unboundNotes names the dimensions most of a rule's metrics have that no
// binding places: one whose members are not added up (a scenario, total
// "first") is usually the case's; any other is read at its total.
func unboundNotes(rule string, metrics []*metric, md *model, members map[string]Binding) []string {
	count := map[string]int{}
	for _, m := range metrics {
		for _, d := range m.dims {
			count[d]++
		}
	}
	var ids []string
	for id, n := range count {
		if n*2 >= len(metrics) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return md.dimByID[ids[i]].name < md.dimByID[ids[j]].name })
	var notes []string
	for _, id := range ids {
		d := md.dimByID[id]
		if d == nil {
			continue
		}
		if _, ok := bindingFor(members, d.name); ok {
			continue
		}
		if d.agg != "" && d.agg != "sum" {
			notes = append(notes, fmt.Sprintf("%s: %s is not bound. Its members are not added up (total %q): if the workbook calculates one %s at a time, bind it to the case ({\"case\": true}) and list the cases.", rule, d.name, d.agg, d.name))
			continue
		}
		notes = append(notes, fmt.Sprintf("%s: %s is not bound, so its total is compared; bind it if the sheet shows single members.", rule, d.name))
	}
	return notes
}

// ── Metric rows under a row of dates ──────────────────────────────────────

func (p *proposer) dateRows(sheet string, used map[int]bool) ([]Rule, []string, bool) {
	rows := p.rows(sheet)
	// The row with the most dates of one dated dimension.
	var td *dim
	dateRow, dateCols := 0, []int(nil)
	for r := 1; r <= min(len(rows), 60); r++ {
		for _, d := range p.md.dimByID {
			if len(d.periods) == 0 {
				continue
			}
			var cols []int
			for c, v := range rows[r-1] {
				if _, err := strconv.ParseFloat(v, 64); err != nil {
					continue
				}
				if _, ok := d.resolve(v); ok {
					cols = append(cols, c+1)
				}
			}
			if len(cols) >= 3 && len(cols) > len(dateCols) {
				td, dateRow, dateCols = d, r, cols
			}
		}
	}
	if td == nil {
		return nil, nil, false
	}
	first := dateCols[0]
	// The label column: left of the dates, naming the most metrics below.
	labelCol, labelRows := 0, map[int]*metric{}
	for c := 1; c < first; c++ {
		found := map[int]*metric{}
		for r := dateRow + 1; r <= len(rows); r++ {
			if m := p.metricNamed(p.wb.value(sheet, c, r)); m != nil && hasDim(m, td.id) {
				found[r] = m
			}
		}
		if len(found) > len(labelRows) {
			labelCol, labelRows = c, found
		}
	}
	if len(labelRows) == 0 {
		return nil, nil, false
	}
	_ = labelCol
	sorted := make([]int, 0, len(labelRows))
	for r := range labelRows {
		sorted = append(sorted, r)
	}
	sort.Ints(sorted)
	members := map[string]Binding{td.name: {Row: dateRow}}
	cols := &ColumnSpan{From: colName(first), To: colName(dateCols[len(dateCols)-1])}
	// Blocks: the first label coming back at a fixed stride.
	firstMetric := labelRows[sorted[0]]
	var starts []int
	for _, r := range sorted {
		if labelRows[r] == firstMetric {
			starts = append(starts, r)
		}
	}
	dims := map[string]*dim{}
	for _, r := range sorted {
		for _, d := range labelRows[r].dims {
			if dd := p.md.dimByID[d]; dd != nil && dd.id != td.id {
				dims[dd.name] = dd
			}
		}
	}
	if len(starts) >= 2 {
		stride := starts[1] - starts[0]
		regular := true
		for i := 2; i < len(starts); i++ {
			if starts[i]-starts[i-1] != stride {
				regular = false
			}
		}
		if regular && stride > 1 {
			// The block starts on the row above its first label (the title).
			base := starts[0] - 1
			var offsets []int
			var byRow []string
			for _, r := range sorted {
				if r >= base+stride {
					break
				}
				offsets = append(offsets, r-base)
				byRow = append(byRow, labelRows[r].name)
			}
			zero := 0
			for _, name := range sortedDimNames(dims) {
				d := dims[name]
				for c := 1; c < first; c++ {
					var sample []string
					for _, s := range starts {
						if v := p.wb.value(sheet, c, s-1); v != "" {
							sample = append(sample, v)
						}
					}
					if len(sample) == 0 {
						continue
					}
					if resolvesMost(d, sample, "", 0) {
						members[name] = Binding{Column: colName(c), BlockRow: &zero}
						break
					}
					placed := false
					for _, sep := range []string{"-", "|", "/", "_", " "} {
						for part := 1; part <= 3 && !placed; part++ {
							if resolvesMost(d, sample, sep, part) {
								members[name] = Binding{Column: colName(c), BlockRow: &zero, Split: sep, Part: part}
								placed = true
							}
						}
					}
					if placed {
						break
					}
				}
			}
			for k := range starts {
				for _, off := range offsets {
					used[base+k*stride+off] = true
				}
			}
			rule := Rule{Name: fmt.Sprintf("%s blocks from row %d", sheet, base), Sheet: sheet, RowList: offsets,
				Blocks: &Blocks{First: base, Height: stride, Count: len(starts)}, Columns: cols, MetricsByRow: byRow, Members: members}
			return []Rule{rule}, unboundNotes(rule.Name, p.metricsOf(byRow), p.md, members), true
		}
	}
	var byRow []string
	for _, r := range sorted {
		byRow = append(byRow, labelRows[r].name)
		used[r] = true
	}
	rule := Rule{Name: fmt.Sprintf("%s rows by %s", sheet, td.name), Sheet: sheet, RowList: sorted, Columns: cols, MetricsByRow: byRow, Members: members}
	return []Rule{rule}, unboundNotes(rule.Name, p.metricsOf(byRow), p.md, members), true
}

func (p *proposer) metricsOf(names []string) []*metric {
	out := make([]*metric, 0, len(names))
	for _, n := range names {
		if m := p.md.metrics[strings.ToLower(n)]; m != nil {
			out = append(out, m)
		}
	}
	return out
}

func hasDim(m *metric, id string) bool {
	for _, d := range m.dims {
		if d == id {
			return true
		}
	}
	return false
}

// ── Single values ──────────────────────────────────────────────────────────

func (p *proposer) singleValues(sheet string, used map[int]bool) (Rule, bool) {
	rows := p.rows(sheet)
	byCol := map[int]map[int]*metric{} // value column → row → metric
	for r := 1; r <= len(rows); r++ {
		if used[r] {
			continue
		}
		for c, v := range rows[r-1] {
			m := p.metricNamed(v)
			if m == nil || !p.scalar(m) {
				continue
			}
			for vc := c + 2; vc <= len(rows[r-1]); vc++ {
				if _, err := strconv.ParseFloat(p.wb.value(sheet, vc, r), 64); err == nil {
					if byCol[vc] == nil {
						byCol[vc] = map[int]*metric{}
					}
					byCol[vc][r] = m
					break
				}
			}
			break
		}
	}
	bestCol := 0
	for c, rs := range byCol {
		if bestCol == 0 || len(rs) > len(byCol[bestCol]) {
			bestCol = c
		}
	}
	if bestCol == 0 || len(byCol[bestCol]) < 2 {
		return Rule{}, false
	}
	var list []int
	for r := range byCol[bestCol] {
		list = append(list, r)
	}
	sort.Ints(list)
	var byRow []string
	for _, r := range list {
		byRow = append(byRow, byCol[bestCol][r].name)
	}
	return Rule{Name: fmt.Sprintf("%s single values", sheet), Sheet: sheet, RowList: list, ColumnList: []string{colName(bestCol)}, MetricsByRow: byRow}, true
}

// scalar: a metric with no dimension but, at most, one bound to a case.
func (p *proposer) scalar(m *metric) bool {
	return len(m.dims) <= 1
}
