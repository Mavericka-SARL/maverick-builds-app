package importpkg

// Reshaping a spreadsheet laid out for people into the rows an import reads.
//
// Real workbooks rarely arrive as one header row over one row per value: a
// title sits above the header, months run across the columns, a group label
// is written once and left blank below, "Total" rows sit between the data,
// the scenario is in the file name rather than a column, amounts are "in
// thousands" or written "1 234,50". A Reshape describes, declaratively, how
// to turn such a sheet into an importable one; nothing in it is code. It is
// applied between reading the file and the column_map, by every path that
// imports a whole file: the AI Developer's attachment import and conversion,
// and the runs of a saved file integration (a business user's dashboard
// upload button included).
//
// The steps run in a fixed order, each seeing the previous one's columns:
// header_row → fill_down → skip_rows → unpivot → constants → value_map →
// numbers (number_columns, decimal_comma, scale).

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// Reshape is the saved or proposed shape of a file. The zero value changes
// nothing: the header is the first row and every column is kept as it is.
type Reshape struct {
	// Delimiter separates a CSV file's fields: "," (default), ";", "\t" or
	// "|". Spreadsheets saved as CSV in many European locales use ";".
	Delimiter string `json:"delimiter,omitempty"`
	// HeaderRow is the 1-based row holding the column names (default 1);
	// rows above it — titles, notes, blank lines — are dropped.
	HeaderRow int `json:"header_row,omitempty"`
	// FillDown names columns whose blank cells take the value above them:
	// a group label written once over the rows it heads.
	FillDown []string `json:"fill_down,omitempty"`
	// SkipRows drops every row that matches any of these.
	SkipRows []RowFilter `json:"skip_rows,omitempty"`
	// Unpivot turns columns into rows: months across the top become a
	// Month column and a value column.
	Unpivot *Unpivot `json:"unpivot,omitempty"`
	// Constants adds columns holding the same value on every row: what the
	// file means but does not say ("Scenario": "Budget").
	Constants map[string]string `json:"constants,omitempty"`
	// ValueMap rewrites values in a column: {"Region": {"EMEA": "Europe"}}.
	// Matching ignores case and surrounding spaces.
	ValueMap map[string]map[string]string `json:"value_map,omitempty"`
	// NumberColumns are read as numbers written for people: thousands
	// separators, "(123)" for -123, "12%" for 0.12, currency signs. The
	// unpivot's value column and every Scale column are read so as well.
	NumberColumns []string `json:"number_columns,omitempty"`
	// DecimalComma reads "1.234,5" as 1234.5 (and "1,5" as 1.5).
	DecimalComma bool `json:"decimal_comma,omitempty"`
	// Scale multiplies a column's numbers: 1000 for a file "in thousands".
	Scale map[string]float64 `json:"scale,omitempty"`
}

// RowFilter matches a row by one of its cells. Exactly one of Equals,
// Contains and Blank is set. Column "" means any cell (not with Blank).
type RowFilter struct {
	Column   string `json:"column,omitempty"`
	Equals   string `json:"equals,omitempty"`
	Contains string `json:"contains,omitempty"`
	Blank    bool   `json:"blank,omitempty"`
}

// Unpivot names the wide columns — as a list, or as the run of adjacent
// columns From…To — and the two columns that replace them: NameColumn takes
// each wide column's header, ValueColumn its cell. A blank cell makes no row.
type Unpivot struct {
	Columns     []string `json:"columns,omitempty"`
	From        string   `json:"from,omitempty"`
	To          string   `json:"to,omitempty"`
	NameColumn  string   `json:"name_column"`
	ValueColumn string   `json:"value_column"`
}

// IsZero reports whether r changes nothing.
func (r *Reshape) IsZero() bool {
	return r == nil || (r.Delimiter == "" && r.HeaderRow <= 1 && len(r.FillDown) == 0 && len(r.SkipRows) == 0 &&
		r.Unpivot == nil && len(r.Constants) == 0 && len(r.ValueMap) == 0 && len(r.NumberColumns) == 0 &&
		!r.DecimalComma && len(r.Scale) == 0)
}

// Validate checks what can be checked without the file. Column names are
// checked against the file when it is shaped (ShapeGrid).
func (r *Reshape) Validate() error {
	if r == nil {
		return nil
	}
	if _, err := r.Comma(); err != nil {
		return err
	}
	if r.HeaderRow < 0 {
		return fmt.Errorf("reshape.header_row must be 1 or more")
	}
	for i, f := range r.SkipRows {
		set := 0
		if f.Equals != "" {
			set++
		}
		if f.Contains != "" {
			set++
		}
		if f.Blank {
			set++
		}
		if set != 1 {
			return fmt.Errorf("reshape.skip_rows[%d] needs exactly one of equals, contains or blank", i)
		}
		if f.Blank && strings.TrimSpace(f.Column) == "" {
			return fmt.Errorf("reshape.skip_rows[%d]: blank needs a column (blank rows are always skipped)", i)
		}
	}
	if u := r.Unpivot; u != nil {
		if strings.TrimSpace(u.NameColumn) == "" || strings.TrimSpace(u.ValueColumn) == "" {
			return fmt.Errorf("reshape.unpivot needs name_column (receives each column's header) and value_column (receives its cell)")
		}
		if strings.EqualFold(strings.TrimSpace(u.NameColumn), strings.TrimSpace(u.ValueColumn)) {
			return fmt.Errorf("reshape.unpivot: name_column and value_column must differ")
		}
		hasList, hasRange := len(u.Columns) > 0, u.From != "" || u.To != ""
		if hasList == hasRange {
			return fmt.Errorf("reshape.unpivot needs either columns (a list) or from and to (a run of adjacent columns)")
		}
		if hasRange && (u.From == "" || u.To == "") {
			return fmt.Errorf("reshape.unpivot needs both from and to")
		}
	}
	for col := range r.Constants {
		if strings.TrimSpace(col) == "" {
			return fmt.Errorf("reshape.constants has a column with no name")
		}
	}
	for col, f := range r.Scale {
		if f == 0 || math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("reshape.scale[%q] must be a non-zero number", col)
		}
	}
	return nil
}

// Comma is the CSV field separator r names (',' when it names none).
func (r *Reshape) Comma() (rune, error) {
	if r == nil || r.Delimiter == "" {
		return ',', nil
	}
	switch r.Delimiter {
	case ",", ";", "|":
		return rune(r.Delimiter[0]), nil
	case "\t", `\t`, "tab":
		return '\t', nil
	}
	return 0, fmt.Errorf("reshape.delimiter must be \",\", \";\", \"\\t\" or \"|\"")
}

// ReadGrid reads every row of a .csv file or of one .xlsx/.xlsm sheet (sheet
// "" = the first), header included, cells as stored. delim applies to CSV.
func ReadGrid(filename string, data []byte, sheet string, delim rune) ([][]string, error) {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".csv":
		if sheet != "" {
			return nil, fmt.Errorf("%s is a CSV file and has no sheets", filename)
		}
		return readCSVGrid(data, delim)
	case ".xlsx", ".xlsm":
		if delim != ',' {
			return nil, fmt.Errorf("%s is a workbook; reshape.delimiter applies to CSV files only", filename)
		}
		return readXLSXGrid(data, sheet, true)
	default:
		return nil, fmt.Errorf("%s is not a spreadsheet — import reads .csv, .xlsx and .xlsm files", filename)
	}
}

// ReadShaped reads a file and applies r (nil changes nothing).
func ReadShaped(filename string, data []byte, sheet string, r *Reshape) (header []string, rows []RawRow, err error) {
	if err := r.Validate(); err != nil {
		return nil, nil, err
	}
	delim, _ := r.Comma()
	grid, err := ReadGrid(filename, data, sheet, delim)
	if err != nil {
		return nil, nil, err
	}
	return ShapeGrid(grid, r)
}

// ShapeCSV reads CSV text and applies r.
func ShapeCSV(data []byte, r *Reshape) (header []string, rows []RawRow, err error) {
	return ReadShaped("file.csv", data, "", r)
}

// readCSVGrid reads CSV text. Rows may differ in length (a title row above
// the header is shorter), and a stray quote inside a field is kept as text;
// any other malformation is an error naming its line, never a silent end of
// the file.
func readCSVGrid(data []byte, delim rune) ([][]string, error) {
	cr := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	cr.Comma = delim
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.TrimLeadingSpace = true
	var grid [][]string
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read CSV: %w", err)
		}
		grid = append(grid, rec)
	}
	if len(grid) == 0 {
		return nil, fmt.Errorf("read header: the file is empty")
	}
	return grid, nil
}

func readXLSXGrid(data []byte, sheet string, rawValues bool) ([][]string, error) {
	_, _, grid, err := readXLSXSheetGrid(data, sheet, rawValues)
	return grid, err
}

// ReadXLSXSheet is a workbook's sheet names, the sheet read (the first when
// sheet is empty) and its cells as the upload reads them (ParseXLSXRows:
// as displayed, plain numbers as stored) — what the Import Wizard previews
// and maps, so the browser parses no workbook.
func ReadXLSXSheet(data []byte, sheet string) (sheets []string, name string, grid [][]string, err error) {
	return readXLSXSheetGrid(data, sheet, false)
}

func readXLSXSheetGrid(data []byte, sheet string, rawValues bool) (sheets []string, name string, grid [][]string, err error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return nil, "", nil, fmt.Errorf("open xlsx: %w", err)
	}
	defer func() { _ = f.Close() }()

	sheets = f.GetSheetList()
	if len(sheets) == 0 {
		return nil, "", nil, fmt.Errorf("workbook has no sheets")
	}
	name = sheets[0]
	if sheet != "" {
		name = ""
		for _, s := range sheets {
			if strings.EqualFold(s, sheet) {
				name = s
				break
			}
		}
		if name == "" {
			return sheets, "", nil, fmt.Errorf("workbook has no sheet %q — its sheets are: %s", sheet, strings.Join(sheets, ", "))
		}
	}
	if rawValues {
		grid, err = f.GetRows(name, excelize.Options{RawCellValue: true})
		if err == nil {
			isoDates(f, name, grid)
		}
	} else {
		grid, err = f.GetRows(name)
		if err == nil {
			var raw [][]string
			if raw, err = f.GetRows(name, excelize.Options{RawCellValue: true}); err == nil {
				storedNumbers(f, name, grid, raw)
			}
		}
	}
	if err != nil {
		return sheets, name, nil, fmt.Errorf("read sheet %q: %w", name, err)
	}
	if len(grid) == 0 {
		return sheets, name, nil, fmt.Errorf("sheet %q has no header row", name)
	}
	return sheets, name, grid, nil
}

// ShapeGrid turns a sheet's rows into an import's header and rows, applying
// r. Row numbers count data rows from 1 after the header row; every row an
// unpivot makes from one keeps its number, so an error names the line a
// person can find.
func ShapeGrid(grid [][]string, r *Reshape) (header []string, rows []RawRow, err error) {
	if r == nil {
		r = &Reshape{}
	}
	if err := r.Validate(); err != nil {
		return nil, nil, err
	}
	h := r.HeaderRow
	if h == 0 {
		h = 1
	}
	if h > len(grid) {
		return nil, nil, fmt.Errorf("the sheet has %d row(s); reshape.header_row is %d", len(grid), h)
	}
	header = append([]string(nil), grid[h-1]...)
	rowNum := 0
	for _, record := range grid[h:] {
		rowNum++
		if isBlankRecord(record) {
			continue
		}
		rows = append(rows, recordToRawRow(header, record, rowNum))
	}
	s := &shaper{header: header, rows: rows}
	steps := []func(*Reshape) error{s.fillDown, s.skipRows, s.unpivot, s.constants, s.valueMap, s.numbers}
	for _, step := range steps {
		if err := step(r); err != nil {
			return nil, nil, err
		}
	}
	return s.header, s.rows, nil
}

type shaper struct {
	header []string
	rows   []RawRow
}

// col finds a column by name, ignoring case and surrounding spaces, and
// returns the header's own spelling (the key cells are stored under).
func (s *shaper) col(step, name string) (string, error) {
	want := strings.TrimSpace(name)
	for _, h := range s.header {
		if h == name {
			return h, nil
		}
	}
	for _, h := range s.header {
		if strings.EqualFold(strings.TrimSpace(h), want) {
			return h, nil
		}
	}
	return "", fmt.Errorf("reshape.%s names column %q, which the file does not have at that step — its columns are: %s",
		step, name, strings.Join(s.header, ", "))
}

func (s *shaper) fillDown(r *Reshape) error {
	for _, name := range r.FillDown {
		c, err := s.col("fill_down", name)
		if err != nil {
			return err
		}
		last := ""
		for i := range s.rows {
			if v := strings.TrimSpace(s.rows[i].Cells[c]); v != "" {
				last = s.rows[i].Cells[c]
			} else if last != "" {
				s.rows[i].Cells[c] = last
			}
		}
	}
	return nil
}

func (s *shaper) skipRows(r *Reshape) error {
	if len(r.SkipRows) == 0 {
		return nil
	}
	type filter struct {
		col string
		RowFilter
	}
	filters := make([]filter, 0, len(r.SkipRows))
	for _, f := range r.SkipRows {
		c := ""
		if strings.TrimSpace(f.Column) != "" {
			var err error
			if c, err = s.col("skip_rows", f.Column); err != nil {
				return err
			}
		}
		filters = append(filters, filter{col: c, RowFilter: f})
	}
	matches := func(f filter, v string) bool {
		v = strings.TrimSpace(v)
		switch {
		case f.Blank:
			return v == ""
		case f.Equals != "":
			return strings.EqualFold(v, strings.TrimSpace(f.Equals))
		default:
			return strings.Contains(strings.ToLower(v), strings.ToLower(strings.TrimSpace(f.Contains)))
		}
	}
	kept := s.rows[:0]
	for _, row := range s.rows {
		skip := false
		for _, f := range filters {
			if f.col != "" {
				skip = matches(f, row.Cells[f.col])
			} else {
				for _, v := range row.Cells {
					if matches(f, v) {
						skip = true
						break
					}
				}
			}
			if skip {
				break
			}
		}
		if !skip {
			kept = append(kept, row)
		}
	}
	s.rows = kept
	return nil
}

func (s *shaper) unpivot(r *Reshape) error {
	u := r.Unpivot
	if u == nil {
		return nil
	}
	var wide []string
	if len(u.Columns) > 0 {
		for _, name := range u.Columns {
			c, err := s.col("unpivot.columns", name)
			if err != nil {
				return err
			}
			wide = append(wide, c)
		}
	} else {
		from, err := s.col("unpivot.from", u.From)
		if err != nil {
			return err
		}
		to, err := s.col("unpivot.to", u.To)
		if err != nil {
			return err
		}
		fi, ti := indexOf(s.header, from), indexOf(s.header, to)
		if fi > ti {
			return fmt.Errorf("reshape.unpivot: from %q comes after to %q in the file", u.From, u.To)
		}
		wide = append(wide, s.header[fi:ti+1]...)
	}
	isWide := map[string]bool{}
	for _, c := range wide {
		isWide[c] = true
	}
	name, value := strings.TrimSpace(u.NameColumn), strings.TrimSpace(u.ValueColumn)
	var kept []string
	for _, h := range s.header {
		if isWide[h] {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(h), name) || strings.EqualFold(strings.TrimSpace(h), value) {
			return fmt.Errorf("reshape.unpivot: the file already has a column %q — choose another name_column or value_column", h)
		}
		kept = append(kept, h)
	}
	var out []RawRow
	for _, row := range s.rows {
		for _, w := range wide {
			v := strings.TrimSpace(row.Cells[w])
			if v == "" {
				continue
			}
			cells := make(map[string]string, len(kept)+2)
			for _, k := range kept {
				if cv, ok := row.Cells[k]; ok {
					cells[k] = cv
				}
			}
			cells[name] = strings.TrimSpace(w)
			cells[value] = v
			out = append(out, RawRow{RowNumber: row.RowNumber, Cells: cells})
		}
	}
	s.header = append(kept, name, value)
	s.rows = out
	return nil
}

func (s *shaper) constants(r *Reshape) error {
	cols := make([]string, 0, len(r.Constants))
	for c := range r.Constants {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	for _, c := range cols {
		name := strings.TrimSpace(c)
		if _, err := s.col("constants", name); err == nil {
			return fmt.Errorf("reshape.constants: the file already has a column %q — use value_map to change its values", name)
		}
		s.header = append(s.header, name)
		for i := range s.rows {
			s.rows[i].Cells[name] = r.Constants[c]
		}
	}
	return nil
}

func (s *shaper) valueMap(r *Reshape) error {
	for name, m := range r.ValueMap {
		c, err := s.col("value_map", name)
		if err != nil {
			return err
		}
		lookup := make(map[string]string, len(m))
		for from, to := range m {
			lookup[strings.ToLower(strings.TrimSpace(from))] = to
		}
		for i := range s.rows {
			if to, ok := lookup[strings.ToLower(strings.TrimSpace(s.rows[i].Cells[c]))]; ok {
				s.rows[i].Cells[c] = to
			}
		}
	}
	return nil
}

func (s *shaper) numbers(r *Reshape) error {
	cols := map[string]float64{} // column -> factor (1 = read only)
	add := func(step, name string, factor float64) error {
		c, err := s.col(step, name)
		if err != nil {
			return err
		}
		if _, ok := cols[c]; !ok || factor != 1 {
			cols[c] = factor
		}
		return nil
	}
	for _, name := range r.NumberColumns {
		if err := add("number_columns", name, 1); err != nil {
			return err
		}
	}
	if r.Unpivot != nil {
		if err := add("unpivot", r.Unpivot.ValueColumn, 1); err != nil {
			return err
		}
	}
	for name, f := range r.Scale {
		if err := add("scale", name, f); err != nil {
			return err
		}
	}
	for c, factor := range cols {
		for i := range s.rows {
			v := strings.TrimSpace(s.rows[i].Cells[c])
			if v == "" {
				continue
			}
			// A cell that is not a number is left as written: the import's
			// own number check reports it with its row and column.
			if n, ok := ParseHumanNumber(v, r.DecimalComma); ok {
				s.rows[i].Cells[c] = strconv.FormatFloat(n*factor, 'f', -1, 64)
			}
		}
	}
	return nil
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

var (
	thousandsComma = regexp.MustCompile(`^\d{1,3}(,\d{3})+(\.\d*)?$`)
	thousandsDot   = regexp.MustCompile(`^\d{1,3}(\.\d{3})+(,\d*)?$`)
)

// ParseHumanNumber reads a number as people write it: "1,234.50",
// "1 234,50" (decimalComma), "(123)" for -123, "12%" for 0.12, with
// currency signs. A separator that cannot be a thousands separator is not
// guessed at: "1,5" without decimalComma is not a number, rather than 15.
func ParseHumanNumber(s string, decimalComma bool) (float64, bool) {
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		neg, s = true, s[1:len(s)-1]
	}
	pct := false
	if strings.HasSuffix(s, "%") {
		pct, s = true, strings.TrimSuffix(s, "%")
	}
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\u00a0', '\u202f', '\'', '’', '$', '€', '£', '¥', '₽', '₹':
			return -1
		}
		return r
	}, s)
	if strings.HasPrefix(s, "-") {
		neg, s = !neg, s[1:]
	} else {
		s = strings.TrimPrefix(s, "+")
	}
	if decimalComma {
		if strings.Contains(s, ".") {
			if !thousandsDot.MatchString(s) {
				return 0, false
			}
			s = strings.ReplaceAll(s, ".", "")
		}
		if strings.Count(s, ",") > 1 {
			return 0, false
		}
		s = strings.Replace(s, ",", ".", 1)
	} else if strings.Contains(s, ",") {
		if !thousandsComma.MatchString(s) {
			return 0, false
		}
		s = strings.ReplaceAll(s, ",", "")
	}
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if pct {
		f /= 100
	}
	if neg {
		f = -f
	}
	return f, true
}
