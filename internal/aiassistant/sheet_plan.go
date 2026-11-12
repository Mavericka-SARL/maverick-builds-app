package aiassistant

// plan_sheet: the server translates a workbook sheet's formulas into the
// model's terms and writes the sheet's plan itself (owner, 2026-11-12:
// "translate formulas on the server", "one plan per sheet, generated").
// With the target model mapping each row to a metric, a cell reference
// becomes a metric (the same period), LAG (an earlier period), CUMULATE (a
// running total), a month number or the period's date; another sheet's row
// becomes that sheet's metric. The AI reviews the plan and writes only the
// rows the rules cannot translate. Most calls of the 2026-11-12 build went
// into reading rows and translating them one by one.

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// SheetRow is one row of a sheet as the plan reads it.
type SheetRow struct {
	Row        int
	Label      string
	Section    string // the text-only row above it, if any
	Metric     string
	Input      bool   // typed numbers, no formulas
	Formula    string // the translated formula ("" for an input)
	Excel      string // the workbook formula translated (typical period column)
	Problem    string // why it could not be translated
	Percentage bool
}

// sheetGrid is a sheet read: values, formulas, layout.
type sheetGrid struct {
	name        string
	values      [][]string
	formulas    map[[2]int]string // (row, col) 1-based → formula
	periodStart int               // 1-based column of the first period, 0 when none
	periodEnd   int
	header      int
	monthRow    int // the row counting periods 1, 2, 3 …
	dateRow     int // the row of period dates (the header)
	labels      map[int]string
	sections    map[int]string
	rowMetric   map[int]string
	rowMember   map[int][2]string // row → (dimension, member code) for a row that is one member of a metric
}

func readSheetGrid(f *excelize.File, name string) (*sheetGrid, error) {
	rows, err := f.GetRows(name)
	if err != nil {
		return nil, err
	}
	g := &sheetGrid{name: name, values: rows, formulas: map[[2]int]string{}, labels: map[int]string{}, sections: map[int]string{}, rowMetric: map[int]string{}, rowMember: map[int][2]string{}}
	section := ""
	for r, cells := range rows {
		label, nums, fmls := "", 0, 0
		for c, v := range cells {
			axis, _ := excelize.CoordinatesToCellName(c+1, r+1)
			if fml, _ := f.GetCellFormula(name, axis); fml != "" {
				g.formulas[[2]int{r + 1, c + 1}] = fml
				fmls++
				continue
			}
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if looksNumber(v) {
				nums++
			} else if label == "" && c <= 2 {
				label = v
			}
		}
		if label != "" {
			if nums == 0 && fmls == 0 {
				section = label
			} else {
				g.labels[r+1] = label
				g.sections[r+1] = section
			}
		}
	}
	// The period columns: the row (in the first 15) with the most month
	// headers; the month-number row: 1, 2, 3 … across them.
	best := 0
	for r := 1; r <= len(rows) && r <= 15; r++ {
		start, end, n := 0, 0, 0
		for c, v := range rows[r-1] {
			if periodOf(v) != "" {
				if start == 0 {
					start = c + 1
				}
				end = c + 1
				n++
			}
		}
		if n > best && n >= 3 {
			best, g.header, g.periodStart, g.periodEnd = n, r, start, end
		}
	}
	g.dateRow = g.header
	if g.periodStart > 0 {
		for r := 1; r <= len(rows) && r <= 15; r++ {
			if r == g.header || len(rows[r-1]) < g.periodStart+1 {
				continue
			}
			a, b := strings.TrimSpace(rows[r-1][g.periodStart-1]), strings.TrimSpace(rows[r-1][g.periodStart])
			if a == "1" && b == "2" {
				g.monthRow = r
				break
			}
		}
	}
	return g, nil
}

// mapRows matches the sheet's labelled rows to the target sheet's metrics:
// overrides first, then the workbook map, then the target's order when the
// counts agree, then the best name match (label and section words).
func (g *sheetGrid) mapRows(metrics []string, known map[int]string, overrides map[int]string, labels map[string]string, members map[string]map[string]string) {
	var rows []int
	for r := range g.labels {
		if r != g.monthRow && r != g.dateRow {
			rows = append(rows, r)
		}
	}
	sort.Ints(rows)
	used := map[string]bool{}
	assign := func(r int, m string) {
		if m != "" && !used[strings.ToLower(m)] {
			g.rowMetric[r] = m
			used[strings.ToLower(m)] = true
		}
	}
	for r, m := range overrides {
		assign(r, m)
	}
	for r, m := range known {
		if _, done := g.rowMetric[r]; !done {
			assign(r, m)
		}
	}
	// The model's own labels: a metric already built carries the row's text.
	byLabel := map[string]string{}
	for _, m := range metrics {
		if l := labels[strings.ToLower(m)]; l != "" {
			byLabel[normItem(l)] = m
		}
	}
	for _, r := range rows {
		if _, done := g.rowMetric[r]; !done {
			assign(r, byLabel[normItem(g.labels[r])])
		}
	}
	// A row naming a member of one of the sheet's dimensions is that member
	// of a metric the section names (FTE by function: one row per function).
	for _, r := range rows {
		if _, done := g.rowMetric[r]; done {
			continue
		}
		for dim, ms := range members {
			code, ok := ms[strings.ToLower(strings.TrimSpace(g.labels[r]))]
			if !ok {
				continue
			}
			st := tokens(g.sections[r])
			bestM, bestS := "", 0.0
			for _, m := range metrics {
				if s := jaccard(st, tokens(m)); s > bestS {
					bestM, bestS = m, s
				}
			}
			if bestM != "" {
				g.rowMetric[r] = bestM
				g.rowMember[r] = [2]string{dim, code}
				used[strings.ToLower(bestM)] = true
			}
			break
		}
	}
	var free []int
	for _, r := range rows {
		if _, done := g.rowMetric[r]; !done {
			free = append(free, r)
		}
	}
	var left []string
	for _, m := range metrics {
		if !used[strings.ToLower(m)] {
			left = append(left, m)
		}
	}
	if len(free) == len(left) && len(free) > 0 {
		agree := 0
		for i, r := range free {
			if jaccard(tokens(g.labels[r]+" "+g.sections[r]), tokens(left[i])) > 0 {
				agree++
			}
		}
		if agree*10 >= len(free)*7 {
			for i, r := range free {
				assign(r, left[i])
			}
			return
		}
	}
	for _, r := range free {
		lt := tokens(g.labels[r])
		st := tokens(g.labels[r] + " " + g.sections[r])
		bestM, bestS := "", 0.0
		for _, m := range left {
			if used[strings.ToLower(m)] {
				continue
			}
			s := jaccard(lt, tokens(m))
			if s2 := jaccard(st, tokens(m)); s2 > s {
				s = s2
			}
			if SnakeName(g.labels[r]) == strings.ToLower(m) {
				s = 2
			}
			if s > bestS {
				bestM, bestS = m, s
			}
		}
		if bestS >= 0.34 {
			assign(r, bestM)
		}
	}
}

var (
	refRE   = regexp.MustCompile(`(?:'((?:[^']|'')+)'|([A-Za-z_][A-Za-z0-9_.]*))?(!)?(\$?)([A-Z]{1,3})(\$?)(\d+)(?::(?:'(?:[^']|'')+'!|[A-Za-z_][A-Za-z0-9_.]*!)?(\$?)([A-Z]{1,3})(\$?)(\d+))?`)
	strRE   = regexp.MustCompile(`"(?:[^"]|"")*"`)
	pctRE   = regexp.MustCompile(`(\d+(?:\.\d+)?)%`)
	fnRE    = regexp.MustCompile(`([A-Z][A-Z0-9.]*)\s*\(`)
	colRefs = regexp.MustCompile(`(?:'[^']+'|[A-Za-z_][A-Za-z0-9_.]*)?!?\$?[A-Z]{1,3}:\$?[A-Z]{1,3}\b`)
)

// formulaFunctions are the Excel functions the model's formulas share.
var formulaFunctions = map[string]bool{
	"IF": true, "AND": true, "OR": true, "NOT": true, "MAX": true, "MIN": true, "ROUND": true, "ROUNDUP": true,
	"ROUNDDOWN": true, "ABS": true, "IFERROR": true, "SUM": true, "AVERAGE": true, "INT": true, "MOD": true,
	"POWER": true, "SQRT": true, "CEILING": true, "FLOOR": true, "EOMONTH": true, "EDATE": true, "DATE": true,
	"YEAR": true, "MONTH": true, "DAY": true, "IFS": true, "SWITCH": true, "TRUE": true, "FALSE": true,
}

// planContext is what a translation reads.
type planContext struct {
	sheets   map[string]*sheetGrid // lower-cased sheet name → grid
	cur      *sheetGrid
	col      int // the period column translated
	percent  map[string]bool
	scenario string // the scenario dimension's name ("" when none)
	y0, m0   int    // the first period
	firstCol bool
	curRow   int
}

func colNumber(s string) int {
	n := 0
	for _, ch := range s {
		n = n*26 + int(ch-'A'+1)
	}
	return n
}

// translate rewrites one Excel formula (of the current sheet, at column
// col) in the model's terms; problem says why it could not.
func (pc *planContext) translate(fml string) (string, string) {
	fml = strings.TrimPrefix(strings.TrimSpace(fml), "=")
	if colRefs.MatchString(strRE.ReplaceAllString(fml, `""`)) {
		return "", "reads whole columns (a lookup or a sum over an import table)"
	}
	for _, m := range fnRE.FindAllStringSubmatch(strRE.ReplaceAllString(fml, `""`), -1) {
		if !formulaFunctions[m[1]] {
			return "", "uses " + m[1] + ", which the rules do not translate"
		}
	}
	// Strings are kept aside so references inside them stay text.
	var lits []string
	work := strRE.ReplaceAllStringFunc(fml, func(s string) string {
		lits = append(lits, s)
		return fmt.Sprintf("\x00%d\x00", len(lits)-1)
	})
	problem := ""
	out := refRE.ReplaceAllStringFunc(work, func(ref string) string {
		if problem != "" {
			return ref
		}
		m := refRE.FindStringSubmatch(ref)
		sheetName := m[1]
		if sheetName == "" && m[3] == "!" {
			sheetName = m[2]
		} else if m[3] == "" && m[2] != "" {
			// A name glued to a cell reference without "!" is not a reference
			// (a function or a word): leave it.
			return ref
		}
		sheetName = strings.ReplaceAll(sheetName, "''", "'")
		g := pc.cur
		if sheetName != "" {
			if g = pc.sheets[strings.ToLower(sheetName)]; g == nil {
				problem = "reads sheet " + sheetName + ", which the target does not map"
				return ref
			}
		}
		col := colNumber(m[5])
		row, _ := strconv.Atoi(m[7])
		if m[9] != "" { // a range
			col2, row2 := colNumber(m[9]), 0
			row2, _ = strconv.Atoi(m[11])
			if col == col2 && row != row2 { // down one column: the rows' metrics
				var names []string
				for r := min(row, row2); r <= max(row, row2); r++ {
					if _, isRow := g.labels[r]; !isRow {
						continue
					}
					x, p := pc.cell(g, col, m[4] == "$", r)
					if p != "" {
						problem = p
						return ref
					}
					names = append(names, x)
				}
				if len(names) == 0 {
					return "0"
				}
				return strings.Join(names, ", ")
			}
			if row == row2 && m[4] == "$" && g.periodStart > 0 && col == g.periodStart && pc.col >= col2 {
				// $C12:C12 — every period so far: a running total.
				metric := g.rowMetric[row]
				if metric == "" {
					problem = fmt.Sprintf("a running total over %s row %d, which has no metric", g.name, row)
					return ref
				}
				return "CUMULATE(" + metric + ")"
			}
			problem = "reads a range across periods"
			return ref
		}
		x, p := pc.cell(g, col, m[4] == "$", row)
		if p != "" {
			problem = p
			return ref
		}
		return x
	})
	if problem != "" {
		return "", problem
	}
	for i, l := range lits {
		out = strings.Replace(out, fmt.Sprintf("\x00%d\x00", i), l, 1)
	}
	out = pctRE.ReplaceAllString(out, "($1 / 100)")
	return strings.TrimSpace(out), ""
}

// cell is one cell reference in the model's terms.
func (pc *planContext) cell(g *sheetGrid, col int, absCol bool, row int) (string, string) {
	// The month-number row and the period-date row read the period itself.
	offset := 0
	monthly := g.periodStart > 0 && col >= g.periodStart && col <= g.periodEnd
	if monthly && pc.cur.periodStart > 0 {
		offset = (col - g.periodStart) - (pc.col - pc.cur.periodStart)
	}
	if monthly && row == g.monthRow && g.monthRow > 0 {
		return fmt.Sprintf("((YEAR(START()) - %d) * 12 + MONTH(START()) - %d + %d)", pc.y0, pc.m0, 1+offset), ""
	}
	if monthly && row == g.dateRow && g.dateRow > 0 {
		if offset == 0 {
			return "START()", ""
		}
		return fmt.Sprintf("EDATE(START(), %d)", offset), ""
	}
	metric := g.rowMetric[row]
	label := strings.ToLower(g.labels[row])
	if metric == "" && pc.scenario != "" && strings.Contains(label, "scenario") && !monthly {
		return pc.scenario, ""
	}
	if metric == "" {
		if v := cellValue(g, row, col); v != "" && looksNumber(v) && !monthly {
			return "", fmt.Sprintf("reads %s!%s%d (%s), a number with no metric", g.name, colName(col), row, v)
		}
		return "", fmt.Sprintf("reads %s row %d (%q), which has no metric", g.name, row, g.labels[row])
	}
	if mem, ok := g.rowMember[row]; ok {
		if cm, same := pc.cur.rowMember[pc.curRow]; !same || g != pc.cur || cm != mem {
			metric = fmt.Sprintf("LOOKUP(%s, %s, \"%s\")", metric, mem[0], mem[1])
		}
	}
	x := metric
	if pc.percent[strings.ToLower(g.rowMetric[row])] {
		x = "(" + metric + " / 100)"
	}
	if !monthly {
		return x, "" // a setting or a driver: the same value in every period
	}
	if absCol && offset != 0 {
		return "", fmt.Sprintf("reads %s's fixed period %s", metric, colName(col))
	}
	switch {
	case offset == 0:
		return x, ""
	case offset < 0:
		if pc.firstCol && pc.col+offset < pc.cur.periodStart {
			return "", "the first period reads the column before the periods"
		}
		return fmt.Sprintf("LAG(%s, %d, 0)", x, -offset), ""
	default:
		return fmt.Sprintf("LEAD(%s, %d, 0)", x, offset), ""
	}
}

func cellValue(g *sheetGrid, row, col int) string {
	if row < 1 || row > len(g.values) || col < 1 || col > len(g.values[row-1]) {
		return ""
	}
	return strings.TrimSpace(g.values[row-1][col-1])
}

func colName(n int) string {
	s := ""
	for n > 0 {
		n--
		s = string(rune('A'+n%26)) + s
		n /= 26
	}
	return s
}

// SheetPlanInput is what PlanSheet needs besides the workbook.
type SheetPlanInput struct {
	Sheet     string
	Target    *TargetModel
	Known     map[string]map[int]string // sheet (lower) → row → metric (the workbook map)
	Overrides map[int]string
	Percent   map[string]bool // metric (lower) → a Percentage metric
	Scenario  string
	FirstYear int
	FirstMon  int
	// Labels: metric (lower) → its label in the model; Members: dimension →
	// member label or code (lower) → code.
	Labels  map[string]string
	Members map[string]map[string]string
}

// PlanSheet reads the sheet and every sheet its target maps, and translates
// the sheet's rows.
func PlanSheet(raw []byte, in SheetPlanInput) ([]SheetRow, error) {
	f, err := excelize.OpenReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	pc := &planContext{sheets: map[string]*sheetGrid{}, percent: in.Percent, scenario: in.Scenario, y0: in.FirstYear, m0: in.FirstMon}
	names := map[string]string{}
	for _, n := range f.GetSheetList() {
		names[strings.ToLower(n)] = n
	}
	var target *TargetSheet
	for i := range in.Target.Sheets {
		s := &in.Target.Sheets[i]
		name, ok := names[strings.ToLower(s.Sheet)]
		if !ok || len(s.Metrics) == 0 {
			if ok && strings.EqualFold(s.Sheet, in.Sheet) {
				target = s
			}
			continue
		}
		g, err := readSheetGrid(f, name)
		if err != nil {
			continue
		}
		var ov map[int]string
		if strings.EqualFold(s.Sheet, in.Sheet) {
			ov, target = in.Overrides, s
		}
		dims := map[string]map[string]string{}
		for _, d := range s.Dimensions {
			for name, ms := range in.Members {
				if strings.EqualFold(name, d) {
					dims[name] = ms
				}
			}
		}
		g.mapRows(s.Metrics, in.Known[strings.ToLower(s.Sheet)], ov, in.Labels, dims)
		pc.sheets[strings.ToLower(name)] = g
	}
	if target == nil {
		return nil, fmt.Errorf("the target model does not place sheet %q", in.Sheet)
	}
	g := pc.sheets[strings.ToLower(target.Sheet)]
	if g == nil {
		return nil, fmt.Errorf("sheet %q has no metrics in the target model", in.Sheet)
	}
	if g.periodStart == 0 {
		return nil, fmt.Errorf("sheet %q has no period columns: plan_sheet translates monthly sheets; build this one with load_sheet (inputs) or by hand", in.Sheet)
	}
	pc.cur = g
	var rows []int
	for r := range g.labels {
		if r != g.monthRow && r != g.dateRow {
			rows = append(rows, r)
		}
	}
	sort.Ints(rows)
	var out []SheetRow
	for _, r := range rows {
		sr := SheetRow{Row: r, Label: g.labels[r], Section: g.sections[r], Metric: g.rowMetric[r]}
		pc.curRow = r
		if mem, ok := g.rowMember[r]; ok {
			sr.Problem = fmt.Sprintf("member row: %s for %s = %s -- one formula of %s serves all its members", sr.Metric, mem[0], mem[1], sr.Metric)
			sr.Excel = g.formulas[[2]int{r, g.periodStart + 1}]
			out = append(out, sr)
			continue
		}
		if sr.Metric == "" {
			sr.Problem = "no metric of the target matches this row"
			out = append(out, sr)
			continue
		}
		sr.Percentage = in.Percent[strings.ToLower(sr.Metric)] || strings.Contains(sr.Label, "%")
		typical := g.periodStart + 1
		if typical > g.periodEnd {
			typical = g.periodStart
		}
		fTyp, fFirst := g.formulas[[2]int{r, typical}], g.formulas[[2]int{r, g.periodStart}]
		if fTyp == "" && fFirst == "" {
			sr.Input = true
			out = append(out, sr)
			continue
		}
		sr.Excel = fTyp
		pc.col, pc.firstCol = typical, false
		tTyp, p1 := pc.translate(fTyp)
		pc.col, pc.firstCol = g.periodStart, true
		tFirst, p2 := pc.translate(fFirst)
		switch {
		case p1 != "":
			sr.Problem = p1
		case tFirst == tTyp || fFirst == "":
			sr.Formula = tTyp
		case p2 != "":
			sr.Problem = "the first period differs: " + p2
		default:
			// The first period differs (an opening value): the first month
			// takes its own formula, the others the later one.
			sr.Formula = fmt.Sprintf("IF(((YEAR(START()) - %d) * 12 + MONTH(START()) - %d + 1) = 1, %s, %s)", in.FirstYear, in.FirstMon, tFirst, tTyp)
		}
		if sr.Formula != "" && sr.Percentage && !strings.Contains(sr.Formula, "* 100") {
			sr.Formula = "(" + sr.Formula + ") * 100"
		}
		out = append(out, sr)
	}
	return out, nil
}
