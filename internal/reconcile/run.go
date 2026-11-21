package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/query"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// MaxDifferences is how many differences a report keeps (the counts are
// always complete).
const MaxDifferences = 20000

// Report is what a run found.
type Report struct {
	RanAt      time.Time    `json:"ran_at"`
	RevisionID string       `json:"revision_id"`
	Compared   int          `json:"compared"`
	Matched    int          `json:"matched"`
	Cases      []CaseResult `json:"cases"`
	// MappingProblems: the map reads the workbook wrongly (a metric or a
	// member it names does not exist) — fixed in the map, not the model.
	MappingProblems  []string     `json:"mapping_problems"`
	Differences      []Difference `json:"differences"`
	DifferencesTotal int          `json:"differences_total"`
}

// CaseResult is one case's counts.
type CaseResult struct {
	Name     string   `json:"name"`
	Status   string   `json:"status"` // compared, skipped
	Reason   string   `json:"reason,omitempty"`
	Compared int      `json:"compared"`
	Matched  int      `json:"matched"`
	BySheet  []Counts `json:"by_sheet"`
	ByMetric []Counts `json:"by_metric"`
}

// Counts is compared and matched cells of one sheet or metric.
type Counts struct {
	Name     string `json:"name"`
	Compared int    `json:"compared"`
	Matched  int    `json:"matched"`
}

// Difference is one cell that differs.
type Difference struct {
	Case     string            `json:"case"`
	Sheet    string            `json:"sheet"`
	Cell     string            `json:"cell"`
	Metric   string            `json:"metric"`
	Members  map[string]string `json:"members"`
	Expected string            `json:"expected"`
	Actual   string            `json:"actual"`
}

// Options are a run's settings.
type Options struct {
	// Soffice is the LibreOffice program that recalculates a case with
	// edits ("" leaves such cases skipped).
	Soffice string
}

// Run compares the workbook (data) with the revision, case by case. With no
// cases it compares the values the workbook was saved with.
func Run(ctx context.Context, pool *pgxpool.Pool, modelID, revisionID string, data []byte, m Map, cases []Case, opts Options) (*Report, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		cases = []Case{{Name: "As saved"}}
	}
	if err := ValidateCases(cases); err != nil {
		return nil, err
	}
	md, err := loadModel(ctx, pool, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	reader, err := query.NewChartResolver(pool).NewRevisionReader(ctx, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	rep := &Report{RanAt: time.Now().UTC(), RevisionID: revisionID, MappingProblems: []string{}, Differences: []Difference{}}
	problems := map[string]bool{}
	problem := func(s string) {
		if !problems[s] && len(problems) < 200 {
			problems[s] = true
			rep.MappingProblems = append(rep.MappingProblems, s)
		}
	}
	var texts map[string]string
	for ci, c := range cases {
		res := CaseResult{Name: c.Name, Status: "compared", BySheet: []Counts{}, ByMetric: []Counts{}}
		caseData := data
		if len(c.Edits) > 0 {
			edited, err := applyEdits(data, c.Edits)
			if err == nil {
				caseData, err = recalculate(ctx, opts.Soffice, edited)
			}
			if err != nil {
				res.Status, res.Reason = "skipped", err.Error()
				rep.Cases = append(rep.Cases, res)
				continue
			}
		}
		wb, err := openWorkbook(caseData)
		if err != nil {
			return nil, err
		}
		cells := expand(md, wb, m, c, ci == 0, problem)
		if texts == nil {
			if texts, err = loadTexts(ctx, pool, modelID, revisionID, md); err != nil {
				return nil, err
			}
		}
		bySheet, byMetric := map[string]*Counts{}, map[string]*Counts{}
		for _, cl := range cells {
			want := wb.value(cl.sheet, cl.col, cl.row)
			var got formula.Value
			gotText := ""
			if cl.metric.text {
				gotText = texts[textKey(cl.metric.id, cl.metric.dims, cl.point)]
			} else {
				got, _ = reader.ValueAt(ctx, cl.metric.name, cl.point)
			}
			ok := md.same(cl.metric, want, got, gotText, cl.tol, m.BlankTexts)
			res.Compared++
			sc, mc := counter(bySheet, cl.sheet), counter(byMetric, cl.metric.name)
			sc.Compared++
			mc.Compared++
			if ok {
				res.Matched++
				sc.Matched++
				mc.Matched++
				continue
			}
			rep.DifferencesTotal++
			if len(rep.Differences) < MaxDifferences {
				actual := gotText
				if !cl.metric.text {
					actual = show(got)
				}
				rep.Differences = append(rep.Differences, Difference{Case: c.Name, Sheet: cl.sheet, Cell: cl.addr, Metric: cl.metric.name,
					Members: cl.members, Expected: want, Actual: actual})
			}
		}
		res.BySheet, res.ByMetric = sortedCounts(bySheet), sortedCounts(byMetric)
		rep.Compared += res.Compared
		rep.Matched += res.Matched
		rep.Cases = append(rep.Cases, res)
	}
	return rep, nil
}

func counter(m map[string]*Counts, name string) *Counts {
	c := m[name]
	if c == nil {
		c = &Counts{Name: name}
		m[name] = c
	}
	return c
}

// sortedCounts lists the most differing first, then by name.
func sortedCounts(m map[string]*Counts) []Counts {
	out := make([]Counts, 0, len(m))
	for _, c := range m {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := out[i].Compared-out[i].Matched, out[j].Compared-out[j].Matched
		if di != dj {
			return di > dj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// show writes a model value for a report.
func show(v formula.Value) string {
	switch v.Kind() {
	case formula.KindBlank:
		return ""
	case formula.KindError:
		return v.Err().Code
	case formula.KindNumber:
		n, _ := v.Number()
		return strconv.FormatFloat(n, 'g', 12, 64)
	case formula.KindString, formula.KindBool:
	}
	return v.String()
}

// ── The revision ───────────────────────────────────────────────────────────

type model struct {
	dims    map[string]*dim // lower-cased name
	dimByID map[string]*dim
	metrics map[string]*metric // lower-cased name
}

type dim struct {
	id, name string
	agg      string              // its total rule: sum, first, last, none
	byCode   map[string]string   // lower-cased code → code
	byLabel  map[string][]string // lower-cased label → codes
	labelOf  map[string]string   // code → label
	periods  []period
}

type period struct {
	code       string
	start, end time.Time
}

type metric struct {
	id, name, label, format string
	dims                    []string // dimension IDs
	text                    bool
	picklist                *dim
}

func loadModel(ctx context.Context, pool *pgxpool.Pool, modelID, revisionID string) (*model, error) {
	md := &model{dims: map[string]*dim{}, dimByID: map[string]*dim{}, metrics: map[string]*metric{}}
	rows, err := pool.Query(ctx, `
		SELECT d.id::text, d.name, COALESCE(d.agg_rule, 'sum'), m.code, COALESCE(m.label, ''), m.period_start, m.period_end,
		       NOT EXISTS (SELECT 1 FROM model.dimension_member c WHERE c.parent_member_id = m.id)
		FROM model.dimension_def d JOIN model.dimension_member m ON m.dimension_id = d.id
		WHERE d.model_id = $1::uuid AND (d.revision_id = $2::uuid OR d.revision_id IS NULL)`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, name, agg, code, label string
		var start, end *time.Time
		var leaf bool
		if err := rows.Scan(&id, &name, &agg, &code, &label, &start, &end, &leaf); err != nil {
			rows.Close()
			return nil, err
		}
		d := md.dimByID[id]
		if d == nil {
			d = &dim{id: id, name: name, agg: agg, byCode: map[string]string{}, byLabel: map[string][]string{}, labelOf: map[string]string{}}
			md.dimByID[id], md.dims[strings.ToLower(name)] = d, d
		}
		d.byCode[strings.ToLower(code)] = code
		d.labelOf[code] = label
		if l := strings.ToLower(strings.TrimSpace(label)); l != "" {
			d.byLabel[l] = append(d.byLabel[l], code)
		}
		if leaf && start != nil && end != nil {
			d.periods = append(d.periods, period{code: code, start: start.UTC(), end: end.UTC()})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	dimIDs, err := calculation.NewStore(pool).LoadMetricDimensionIDs(ctx, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	rows, err = pool.Query(ctx, `
		SELECT id::text, name, COALESCE(label, ''), COALESCE(format, ''), COALESCE(picklist_dimension_id::text, '')
		FROM model.metric_def WHERE model_id = $1::uuid AND (revision_id = $2::uuid OR revision_id IS NULL)`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, label, format, pick string
		if err := rows.Scan(&id, &name, &label, &format, &pick); err != nil {
			return nil, err
		}
		md.metrics[strings.ToLower(name)] = &metric{id: id, name: name, label: label, format: format, dims: dimIDs[id],
			text: format == "text", picklist: md.dimByID[pick]}
	}
	return md, rows.Err()
}

// resolve is the member a workbook value names: its code, its one label, or
// — on a dimension of dated periods — a date (a serial number or
// yyyy-mm-dd) the shortest period holds.
func (d *dim) resolve(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	if c, ok := d.byCode[strings.ToLower(v)]; ok {
		return c, true
	}
	if cs := d.byLabel[strings.ToLower(v)]; len(cs) == 1 {
		return cs[0], true
	}
	if len(d.periods) == 0 {
		// A number written with a decimal point names a code without one.
		if n, err := strconv.ParseFloat(v, 64); err == nil && n == math.Trunc(n) {
			if c, ok := d.byCode[strconv.FormatFloat(n, 'f', -1, 64)]; ok {
				return c, true
			}
		}
		return "", false
	}
	var day time.Time
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		day, _ = time.Parse("2006-01-02", formula.SerialDate(math.Floor(n)))
	} else if t, err := time.Parse("2006-01-02", v); err == nil {
		day = t
	} else {
		return "", false
	}
	best := -1
	for i, p := range d.periods {
		if !day.Before(p.start) && !day.After(p.end) && (best < 0 || p.end.Sub(p.start) < d.periods[best].end.Sub(d.periods[best].start)) {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	return d.periods[best].code, true
}

// ── Cells ──────────────────────────────────────────────────────────────────

type cell struct {
	sheet    string
	col, row int
	addr     string
	metric   *metric
	point    map[string]string // dimension ID → code
	members  map[string]string // dimension name → code, for reports
	tol      float64
}

// expand turns a case's rules into cells. A cell whose metric no binding
// ties to the case is the same in every case: it is compared in the first
// only.
func expand(md *model, wb *workbook, m Map, c Case, first bool, problem func(string)) []cell {
	var out []cell
	baseTol := m.Tolerance
	if baseTol <= 0 {
		baseTol = 1e-9
	}
	for _, r := range m.Rules {
		if !wb.has(r.Sheet) {
			problem(fmt.Sprintf("%s: the workbook has no sheet %q", r.Name, r.Sheet))
			continue
		}
		cols, _ := r.columns()
		rows := r.rows()
		blocks := 1
		if r.Blocks != nil {
			blocks = r.Blocks.Count
		}
		caseOf := map[string]string{}
		for k, v := range c.Members {
			caseOf[strings.ToLower(k)] = v
		}
		for b := 0; b < blocks; b++ {
			base := 0
			if r.Blocks != nil {
				base = r.Blocks.First + b*r.Blocks.Height
			}
			for ri, off := range rows {
				row := base + off
				if !rowHolds(wb, r, row, problem) {
					continue
				}
				for _, col := range cols {
					letter, _ := excelize.ColumnNumberToName(col)
					name := r.Metric
					switch {
					case len(r.MetricsByColumn) > 0:
						name = lookupFold(r.MetricsByColumn, letter)
					case len(r.MetricsByRow) > 0:
						name = r.MetricsByRow[ri]
					}
					if strings.TrimSpace(name) == "" {
						continue
					}
					mt := md.metrics[strings.ToLower(strings.TrimSpace(name))]
					if mt == nil {
						problem(fmt.Sprintf("%s: no metric named %q in this revision", r.Name, name))
						continue
					}
					cl := cell{sheet: r.Sheet, col: col, row: row, metric: mt, point: map[string]string{}, members: map[string]string{}, tol: baseTol}
					cl.addr, _ = excelize.CoordinatesToCellName(col, row)
					if t, ok := r.Tolerances[mt.name]; ok && t > 0 {
						cl.tol = t
					}
					caseBound, ok := bind(md, wb, r, base, row, col, mt, caseOf, c.Name, &cl, problem)
					if !ok || (!caseBound && !first) {
						continue
					}
					out = append(out, cl)
				}
			}
		}
	}
	return out
}

// bind fills a cell's point from the rule's bindings of its metric's
// dimensions; caseBound when one takes the case's member.
func bind(md *model, wb *workbook, r Rule, base, row, col int, mt *metric, caseOf map[string]string, caseName string, cl *cell, problem func(string)) (caseBound, ok bool) {
	for _, did := range mt.dims {
		d := md.dimByID[did]
		if d == nil {
			continue
		}
		b, has := bindingFor(r.Members, d.name)
		if !has || b.Total {
			continue
		}
		var raw, from string
		switch {
		case b.Code != "":
			raw, from = b.Code, "code"
		case b.Case:
			v, ok := caseOf[strings.ToLower(d.name)]
			if !ok {
				problem(fmt.Sprintf("%s: case %q gives no member of %s", r.Name, caseName, d.name))
				return false, false
			}
			raw, from, caseBound = v, "case "+caseName, true
		case b.Column != "":
			c, err := excelize.ColumnNameToNumber(strings.TrimSpace(b.Column))
			if err != nil {
				problem(fmt.Sprintf("%s: member %s: column %q", r.Name, d.name, b.Column))
				return false, false
			}
			at := row
			if b.BlockRow != nil {
				at = base + *b.BlockRow
			}
			raw = wb.value(r.Sheet, c, at)
			from, _ = excelize.CoordinatesToCellName(c, at)
		case b.Row > 0:
			raw = wb.value(r.Sheet, col, b.Row)
			from, _ = excelize.CoordinatesToCellName(col, b.Row)
		case b.Cell != "":
			v, err := wb.ref(r.Sheet, b.Cell)
			if err != nil {
				problem(fmt.Sprintf("%s: member %s: %v", r.Name, d.name, err))
				return false, false
			}
			raw, from = v, b.Cell
		}
		if b.Split != "" {
			parts := strings.Split(raw, b.Split)
			part := b.Part
			if part == 0 {
				part = 1
			}
			if part < 0 {
				part = len(parts) + 1 + part
			}
			if part < 1 || part > len(parts) {
				problem(fmt.Sprintf("%s: %s %q has no part %d split by %q", r.Name, from, raw, b.Part, b.Split))
				return false, false
			}
			raw = parts[part-1]
		}
		code, found := d.resolve(raw)
		if !found {
			problem(fmt.Sprintf("%s: %s %q names no member of %s", r.Name, from, raw, d.name))
			return false, false
		}
		cl.point[d.id] = code
		cl.members[d.name] = code
	}
	return caseBound, true
}

func bindingFor(m map[string]Binding, name string) (Binding, bool) {
	if b, ok := m[name]; ok {
		return b, true
	}
	for k, b := range m {
		if strings.EqualFold(strings.TrimSpace(k), name) {
			return b, true
		}
	}
	return Binding{}, false
}

func lookupFold(m map[string]string, key string) string {
	if v, ok := m[key]; ok {
		return v
	}
	for k, v := range m {
		if strings.EqualFold(strings.TrimSpace(k), key) {
			return v
		}
	}
	return ""
}

// rowHolds tests a row against the rule's only_when conditions.
func rowHolds(wb *workbook, r Rule, row int, problem func(string)) bool {
	for _, c := range r.OnlyWhen {
		col, err := excelize.ColumnNameToNumber(strings.TrimSpace(c.Column))
		if err != nil {
			problem(fmt.Sprintf("%s: only_when column %q", r.Name, c.Column))
			return false
		}
		v := wb.value(r.Sheet, col, row)
		if c.Op == "not_blank" {
			if v == "" {
				return false
			}
			continue
		}
		other := fmt.Sprint(c.Value)
		if c.Cell != "" {
			if other, err = wb.ref(r.Sheet, c.Cell); err != nil {
				problem(fmt.Sprintf("%s: only_when: %v", r.Name, err))
				return false
			}
		} else if c.Value == nil {
			other = ""
		}
		if !compare(v, c.Op, other) {
			return false
		}
	}
	return true
}

// compare tests a op b, as numbers when both are, else as texts.
func compare(a, op, b string) bool {
	x, e1 := strconv.ParseFloat(a, 64)
	y, e2 := strconv.ParseFloat(b, 64)
	if e1 == nil && e2 == nil {
		switch op {
		case "=":
			return x == y
		case "<>":
			return x != y
		case "<":
			return x < y
		case "<=":
			return x <= y
		case ">":
			return x > y
		case ">=":
			return x >= y
		}
		return false
	}
	switch op {
	case "=":
		return strings.EqualFold(a, b)
	case "<>":
		return !strings.EqualFold(a, b)
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	case ">=":
		return a >= b
	}
	return false
}

// same is whether a model value matches the workbook's: numbers within the
// tolerance (a percentage metric holds points, the workbook a fraction), a
// pick-list's member by code or label, texts as written, a blank (or one of
// the blank texts) by a blank or zero, an error by the same error.
func (md *model) same(mt *metric, want string, got formula.Value, gotText string, tol float64, blankTexts []string) bool {
	want = strings.TrimSpace(want)
	blank := want == ""
	for _, t := range blankTexts {
		if strings.EqualFold(want, strings.TrimSpace(t)) {
			blank = true
		}
	}
	if mt.text {
		if blank {
			return strings.TrimSpace(gotText) == ""
		}
		return strings.TrimSpace(gotText) == want
	}
	if got.IsError() {
		return strings.EqualFold(want, got.Err().Code)
	}
	if mt.picklist != nil {
		code := ""
		if got.Kind() == formula.KindString {
			code = strings.TrimSpace(got.String())
		}
		if blank {
			// A member that means nothing ("None", or one of the blank
			// texts) matches a blank cell.
			return code == "" || isBlankText(code, blankTexts) || isBlankText(mt.picklist.labelOf[code], blankTexts)
		}
		return strings.EqualFold(want, code) || strings.EqualFold(want, strings.TrimSpace(mt.picklist.labelOf[code]))
	}
	if blank {
		n, _ := got.Number()
		return got.IsBlank() || (got.Kind() == formula.KindNumber && n == 0) || (got.Kind() == formula.KindString && strings.TrimSpace(got.String()) == "")
	}
	w, err := strconv.ParseFloat(want, 64)
	if err != nil {
		switch strings.ToUpper(want) {
		case "TRUE":
			w = 1
		case "FALSE":
			w = 0
		default:
			return got.Kind() == formula.KindString && strings.TrimSpace(got.String()) == want
		}
	}
	g, ok := got.Number()
	if !ok {
		return false
	}
	if mt.format == "percentage" {
		g /= 100
	}
	return math.Abs(g-w) <= tol*math.Max(1, math.Abs(w))+1e-9
}

func isBlankText(s string, blankTexts []string) bool {
	s = strings.TrimSpace(s)
	for _, t := range blankTexts {
		if strings.EqualFold(s, strings.TrimSpace(t)) {
			return true
		}
	}
	return false
}

// ── Texts ──────────────────────────────────────────────────────────────────

// loadTexts reads the text of every cell of the revision's text metrics:
// typed (or, for a metric records post into, posted) texts of inputs, the
// formula's texts of calculated ones.
func loadTexts(ctx context.Context, pool *pgxpool.Pool, modelID, revisionID string, md *model) (map[string]string, error) {
	out := map[string]string{}
	byID := map[string]*metric{}
	var ids []string
	for _, m := range md.metrics {
		if m.text {
			byID[m.id] = m
			ids = append(ids, m.id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	read := func(rows pgx.Rows, err error) error {
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, dm, text string
			if err := rows.Scan(&id, &dm, &text); err != nil {
				return err
			}
			var point map[string]string
			if json.Unmarshal([]byte(dm), &point) != nil || byID[id] == nil {
				continue
			}
			out[textKey(id, byID[id].dims, point)] = text
		}
		return rows.Err()
	}
	if err := read(pool.Query(ctx, `
		WITH `+writeguard.FedMetricsCTE+`
		SELECT DISTINCT ON (metric_id, dim_members) metric_id::text, dim_members::text, COALESCE(text_value, '')
		FROM runtime.fact_input fi
		WHERE model_id = $1::uuid AND revision_id = $2::uuid AND metric_id::text = ANY($3::text[])
		  AND (source_ref IS NULL) = NOT EXISTS (SELECT 1 FROM fed f WHERE f.metric_id = fi.metric_id)
		ORDER BY metric_id, dim_members, entered_at DESC, id DESC`, modelID, revisionID, ids)); err != nil {
		return nil, err
	}
	if err := read(pool.Query(ctx, `
		SELECT metric_id::text, dim_members::text, text_value FROM runtime.calc_result
		WHERE model_id = $1::uuid AND revision_id = $2::uuid AND text_value IS NOT NULL AND metric_id::text = ANY($3::text[])`,
		modelID, revisionID, ids)); err != nil {
		return nil, err
	}
	return out, nil
}

// textKey keys a text cell by its metric and its members of the metric's
// dimensions.
func textKey(metricID string, dims []string, point map[string]string) string {
	var b strings.Builder
	b.WriteString(metricID)
	for _, d := range dims {
		b.WriteString("|")
		b.WriteString(point[d])
	}
	return b.String()
}
