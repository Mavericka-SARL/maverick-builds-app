package aiassistant

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// A workbook attached to an AI session is read as the model it describes:
// each sheet's values (rows numbered, so a cell address can be found), then
// its formulas, its drop-down lists (data validations) and its conditional
// highlights. Values alone said what a cell holds but never how it is
// computed, nor which options a drop-down offers: the AI Developer asked to
// rebuild a workbook had to reverse-engineer every formula from its results
// and listed only the options it happened to see in use.
//
// Formulas are compressed: a run of cells down a column whose formulas are
// the same once their references are made relative (the same R1C1 formula)
// is one line — "E5:E24 = B5*D5 (each row its own)" — under the column's
// header, so a 240-cell monthly block costs twelve lines, not 240.

func extractXLSX(data []byte) (string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("xlsx parse failed: %w", err)
	}
	defer func() { _ = f.Close() }()

	var sb strings.Builder
	for _, sheet := range f.GetSheetList() {
		// Values as stored, not as displayed: a sheet showing 13.4 holds
		// 13.36, and 6.0% is 0.06.
		rows, err := f.GetRows(sheet, excelize.Options{RawCellValue: true})
		if err != nil {
			continue
		}
		fmt.Fprintf(&sb, "## Sheet: %s (row number, then the cells from column A; values as stored — a percentage is a fraction)\n", sheet)
		for i, row := range rows {
			if i >= maxSheetRows {
				fmt.Fprintf(&sb, "... (%d more rows omitted)\n", len(rows)-maxSheetRows)
				break
			}
			if strings.TrimSpace(strings.Join(row, "")) == "" {
				continue
			}
			fmt.Fprintf(&sb, "%d\t%s\n", i+1, strings.Join(row, "\t"))
		}
		writeLayout(&sb, f, sheet, rows)
		writeFormulas(&sb, f, sheet, rows)
		writeValidations(&sb, f, sheet)
		writeConditionalFormats(&sb, f, sheet)
		writeComments(&sb, f, sheet)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// sheetRef finds the sheets a formula reads (Sheet!A1 or 'Sheet name'!A1).
var sheetRef = regexp.MustCompile(`(?:'([^']+)'|([A-Za-z_][A-Za-z0-9_.]*))!\$?[A-Z]{1,3}\$?[0-9]`)

// writeLayout says how a sheet is built, beside its values: the sheets its
// formulas read, and each column's header with what its rows hold — typed
// inputs (numbers), text (labels, choices, notes) or formulas — so a table's
// input columns, its calculated columns and its total rows can be told
// apart without re-deriving them from the values.
func writeLayout(sb *strings.Builder, f *excelize.File, sheet string, rows [][]string) {
	reads := map[string]bool{}
	type span struct {
		kind       string
		start, end int
	}
	width := 0
	for _, r := range rows {
		if len(r) > width {
			width = len(r)
		}
	}
	type column struct {
		col    int
		header string
		spans  []span
	}
	var cols []column
	for c := 1; c <= width; c++ {
		header, headerRow := "", 0
		var spans []span
		for r := 1; r <= len(rows) && r <= maxSheetRows; r++ {
			name, _ := excelize.CoordinatesToCellName(c, r)
			kind := ""
			if text, err := f.GetCellFormula(sheet, name); err == nil && strings.TrimSpace(text) != "" {
				kind = "formula"
				for _, m := range sheetRef.FindAllStringSubmatch(text, -1) {
					if n := m[1] + m[2]; n != sheet {
						reads[n] = true
					}
				}
			} else if c-1 < len(rows[r-1]) && strings.TrimSpace(rows[r-1][c-1]) != "" {
				v := strings.TrimSpace(rows[r-1][c-1])
				if _, err := strconv.ParseFloat(strings.NewReplacer(",", "", "%", "", "$", "", "(", "-", ")", "").Replace(v), 64); err == nil {
					kind = "input"
				} else {
					kind = "text"
				}
			}
			if kind == "" {
				continue
			}
			if kind == "text" && headerRow == 0 && len(spans) == 0 {
				header, headerRow = strings.TrimSpace(rows[r-1][c-1]), r
				continue // the column's header
			}
			if n := len(spans); n > 0 && spans[n-1].kind == kind && spans[n-1].end == r-1 {
				spans[n-1].end = r
			} else {
				spans = append(spans, span{kind, r, r})
			}
		}
		if len(spans) > 0 {
			cols = append(cols, column{c, header, spans})
		}
	}
	if len(cols) == 0 {
		return
	}
	fmt.Fprintf(sb, "### Layout of %s", sheet)
	if len(reads) > 0 {
		names := make([]string, 0, len(reads))
		for n := range reads {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintf(sb, " (its formulas read %s)", strings.Join(names, ", "))
	}
	sb.WriteString(": each column, its header and what its rows hold\n")
	for _, c := range cols {
		letter, _ := excelize.ColumnNumberToName(c.col)
		parts := make([]string, 0, len(c.spans))
		for _, sp := range c.spans {
			rng := strconv.Itoa(sp.start)
			if sp.end != sp.start {
				rng += "-" + strconv.Itoa(sp.end)
			}
			parts = append(parts, sp.kind+" "+rng)
		}
		header := c.header
		if header != "" {
			header = " [" + header + "]"
		}
		fmt.Fprintf(sb, "%s%s: %s\n", letter, header, strings.Join(parts, ", "))
	}
}

func writeComments(sb *strings.Builder, f *excelize.File, sheet string) {
	comments, err := f.GetComments(sheet)
	if err != nil || len(comments) == 0 {
		return
	}
	fmt.Fprintf(sb, "### Cell comments on %s\n", sheet)
	for _, c := range comments {
		text := c.Text
		if text == "" {
			for _, run := range c.Paragraph {
				text += run.Text
			}
		}
		fmt.Fprintf(sb, "%s: %s\n", c.Cell, strings.TrimSpace(strings.ReplaceAll(text, "\n", " ")))
	}
}

// formulaCell is one cell holding a formula.
type formulaCell struct {
	col, row int
	formula  string
	pattern  string // the formula with every reference made relative to the cell
}

func writeFormulas(sb *strings.Builder, f *excelize.File, sheet string, rows [][]string) {
	var cells []formulaCell
	for r := range rows {
		if r >= maxSheetRows {
			break
		}
		width := len(rows[r])
		if width < 30 {
			width = 30 // a formula cell can sit right of the last value GetRows keeps
		}
		for c := 0; c < width; c++ {
			name, _ := excelize.CoordinatesToCellName(c+1, r+1)
			text, err := f.GetCellFormula(sheet, name)
			if err != nil || strings.TrimSpace(text) == "" {
				continue
			}
			cells = append(cells, formulaCell{col: c + 1, row: r + 1, formula: text, pattern: relativeFormula(text, c+1, r+1)})
		}
	}
	if len(cells) == 0 {
		return
	}
	// Runs down each column of one pattern, then side-by-side runs over the
	// same rows whose formulas also agree once column references move with
	// the column (twelve month columns each reading their own month).
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].col != cells[j].col {
			return cells[i].col < cells[j].col
		}
		return cells[i].row < cells[j].row
	})
	type run struct {
		col, lastCol, row, lastRow int
		formula, pattern           string
		lastFormula                string
	}
	var runs []run
	for i := 0; i < len(cells); {
		j := i + 1
		for j < len(cells) && cells[j].col == cells[i].col && cells[j].row == cells[j-1].row+1 && cells[j].pattern == cells[i].pattern {
			j++
		}
		c := cells[i]
		runs = append(runs, run{col: c.col, lastCol: c.col, row: c.row, lastRow: cells[j-1].row, formula: c.formula,
			pattern: c.pattern, lastFormula: c.formula})
		i = j
	}
	var merged []run
	for _, r := range runs {
		joined := false
		for k := range merged {
			m := &merged[k]
			if m.lastCol+1 == r.col && m.row == r.row && m.lastRow == r.lastRow &&
				sameAcross(m.lastFormula, m.lastCol, r.formula, r.col, r.row) {
				m.lastCol, m.lastFormula = r.col, r.formula
				joined = true
				break
			}
		}
		if !joined {
			merged = append(merged, r)
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].row != merged[j].row {
			return merged[i].row < merged[j].row
		}
		return merged[i].col < merged[j].col
	})
	fmt.Fprintf(sb, "### Formulas on %s (a range shares the formula of its first cell; references move with the row and, across columns, with the column)\n", sheet)
	for _, r := range merged {
		first, _ := excelize.CoordinatesToCellName(r.col, r.row)
		where := first
		if r.lastCol != r.col || r.lastRow != r.row {
			last, _ := excelize.CoordinatesToCellName(r.lastCol, r.lastRow)
			where = first + ":" + last
		}
		header := columnHeader(rows, r.col, r.row)
		if r.lastCol != r.col {
			if h := columnHeader(rows, r.lastCol, r.row); h != "" && h != header {
				header += " … " + h
			}
		}
		if r.lastRow == r.row {
			// One row: its own label (a form's line, a table's total row).
			if l := rowLabel(rows, r.col, r.row); l != "" && l != header {
				if header != "" {
					header += " | "
				}
				header += l
			}
		}
		if header != "" {
			header = " [" + header + "]"
		}
		fmt.Fprintf(sb, "%s%s = %s\n", where, header, r.formula)
	}
}

func isLabel(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	n := strings.NewReplacer(",", "", "%", "", "$", "", "(", "-", ")", "").Replace(s)
	_, err := strconv.ParseFloat(n, 64)
	return err != nil
}

// columnHeader is the label of col's table above row: going up from the
// row, the text cell just above the column's first non-text cell of that
// stretch — so a choice typed in a cell above (B7 "Growth % vs LY") is not
// taken for the header of B10.
func columnHeader(rows [][]string, col, row int) string {
	cell := func(r int) string {
		if r < 0 || r >= len(rows) || col-1 >= len(rows[r]) {
			return ""
		}
		return strings.TrimSpace(rows[r][col-1])
	}
	r := row - 2
	// skip the stretch of data (numbers, blanks, text values) directly above
	for r >= 0 && r >= row-60 {
		v := cell(r)
		if isLabel(v) && (r == 0 || cell(r-1) == "" || !isLabel(cell(r-1)) && cell(r+1) != "" && !isLabel(cell(r+1))) {
			return v
		}
		r--
	}
	return ""
}

// rowLabel is the first text left of the cell in its own row (a form's
// line label, a total row's name).
func rowLabel(rows [][]string, col, row int) string {
	if row-1 >= len(rows) {
		return ""
	}
	for c := 0; c < col-1 && c < len(rows[row-1]); c++ {
		if isLabel(rows[row-1][c]) {
			return strings.TrimSpace(rows[row-1][c])
		}
	}
	return ""
}

// cellRef matches a cell reference (optionally sheet-qualified, optionally
// absolute) that is not a function name: the character after it must not
// be "(" or another name character.
var cellRef = regexp.MustCompile(`(\$?)([A-Z]{1,3})(\$?)([0-9]+)`)

// relativeFormula rewrites formula's references as R1C1 relative to the cell
// at (col, row), keeping absolute parts absolute and leaving text literals
// alone, so two cells computing "the same thing for their own row" compare
// equal.
func relativeFormula(formula string, col, row int) string {
	var out strings.Builder
	inString := false
	for i := 0; i < len(formula); {
		ch := formula[i]
		if ch == '"' {
			inString = !inString
			out.WriteByte(ch)
			i++
			continue
		}
		if inString {
			out.WriteByte(ch)
			i++
			continue
		}
		loc := cellRef.FindStringSubmatchIndex(formula[i:])
		if loc == nil || loc[0] != 0 || (i > 0 && isNameChar(formula[i-1])) {
			out.WriteByte(ch)
			i++
			continue
		}
		end := i + loc[1]
		if end < len(formula) && (formula[end] == '(' || isNameChar(formula[end])) {
			out.WriteString(formula[i:end])
			i = end
			continue
		}
		m := formula[i : i+loc[1]]
		sub := cellRef.FindStringSubmatch(m)
		c, _ := excelize.ColumnNameToNumber(sub[2])
		r, _ := strconv.Atoi(sub[4])
		if sub[3] == "$" {
			fmt.Fprintf(&out, "R%d", r)
		} else {
			fmt.Fprintf(&out, "R[%d]", r-row)
		}
		if sub[1] == "$" {
			fmt.Fprintf(&out, "C%d", c)
		} else {
			fmt.Fprintf(&out, "C[%d]", c-col)
		}
		i = end
	}
	return out.String()
}

// sameAcross reports whether formula b of the cell one column right of a's
// is a's formula carried across: the same text, every reference to the same
// rows, and every reference's column either the same (a key column such as
// the region in A, however it is written) or moved by exactly the cells'
// distance (Sales_Base_LY!$C$5:$C$24 becoming $D$5:$D$24: a month column
// reading its own month).
func sameAcross(a string, colA int, b string, colB, row int) bool {
	sa, ra := refSkeleton(a, colA, row)
	sb, rb := refSkeleton(b, colB, row)
	if sa != sb || len(ra) != len(rb) {
		return false
	}
	for i := range ra {
		x, y := ra[i], rb[i]
		if x.row != y.row || (y.col != x.col && y.col-x.col != colB-colA) {
			return false
		}
	}
	return true
}

// ref is one cell reference at its absolute position in its sheet.
type ref struct{ col, row int }

// refSkeleton splits a formula into its text with references blanked out
// and the references themselves.
func refSkeleton(formula string, _, _ int) (string, []ref) {
	var out strings.Builder
	var refs []ref
	inString := false
	for i := 0; i < len(formula); {
		ch := formula[i]
		if ch == '"' {
			inString = !inString
		}
		loc := cellRef.FindStringSubmatchIndex(formula[i:])
		if inString || loc == nil || loc[0] != 0 || (i > 0 && isNameChar(formula[i-1])) {
			out.WriteByte(ch)
			i++
			continue
		}
		end := i + loc[1]
		if end < len(formula) && (formula[end] == '(' || isNameChar(formula[end])) {
			out.WriteString(formula[i:end])
			i = end
			continue
		}
		sub := cellRef.FindStringSubmatch(formula[i:end])
		c, _ := excelize.ColumnNameToNumber(sub[2])
		r, _ := strconv.Atoi(sub[4])
		refs = append(refs, ref{col: c, row: r})
		out.WriteString("\x00")
		i = end
	}
	return out.String(), refs
}

func isNameChar(b byte) bool {
	return b == '_' || b == '.' || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func writeValidations(sb *strings.Builder, f *excelize.File, sheet string) {
	dvs, err := f.GetDataValidations(sheet)
	if err != nil || len(dvs) == 0 {
		return
	}
	fmt.Fprintf(sb, "### Drop-down lists and other input rules on %s (data validation)\n", sheet)
	for _, dv := range dvs {
		if dv == nil {
			continue
		}
		rule := dv.Type
		if dv.Type == "list" {
			rule = "one of " + strings.Trim(dv.Formula1, `"`)
		} else if dv.Formula1 != "" {
			rule = fmt.Sprintf("%s %s %s %s", dv.Type, dv.Operator, dv.Formula1, dv.Formula2)
		}
		fmt.Fprintf(sb, "%s: %s\n", dv.Sqref, strings.TrimSpace(rule))
	}
}

func writeConditionalFormats(sb *strings.Builder, f *excelize.File, sheet string) {
	cfs, err := f.GetConditionalFormats(sheet)
	if err != nil || len(cfs) == 0 {
		return
	}
	ranges := make([]string, 0, len(cfs))
	for r := range cfs {
		ranges = append(ranges, r)
	}
	sort.Strings(ranges)
	fmt.Fprintf(sb, "### Highlighted when (conditional formatting) on %s\n", sheet)
	for _, r := range ranges {
		for _, c := range cfs[r] {
			cond := c.Criteria
			if c.Type == "expression" || cond == "" {
				cond = c.Value
			} else if c.Value != "" {
				cond = cond + " " + c.Value
			}
			fmt.Fprintf(sb, "%s: %s\n", r, strings.TrimSpace(cond))
		}
	}
}
