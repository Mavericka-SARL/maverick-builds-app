package aiassistant

// load_sheet: "load this sheet" as one server step (owner, 2026-11-12). The
// server reads the sheet's layout — a dimension's members across the columns
// (scenarios, months), the row labels down one column — matches each label to
// a metric of the model, and writes the reshape and column map itself; the AI
// confirms the mapping and proposes one import step. Hand-written reshape
// JSON was where the AI failed and then typed the sheet's numbers itself.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// LoadMember is one member of a dimension; Start is a time leaf's first day.
type LoadMember struct{ Code, Label, Start string }

// LoadDimension is a dimension with its leaf members.
type LoadDimension struct {
	Name    string
	Time    bool
	Members []LoadMember
}

// LoadMetric is a metric of the working revision, with its grid if any.
type LoadMetric struct{ Name, Label, Format, Grid, GridID string }

// LoadInventory is what a sheet is matched against.
type LoadInventory struct {
	Metrics    []LoadMetric
	Dimensions []LoadDimension
	GridDims   map[string][]string // grid id → its dimensions' names
}

// LoadRow is one data row of the sheet and the metric it loads into.
type LoadRow struct {
	Row       int
	Label     string
	Metric    string // "" when none matched
	Match     string // exact | label | nearest | none
	Values    int
	Suggested string // a name for a metric to create, when none matched
	Fraction  bool   // every value is between -1 and 1
}

// SheetLoad is the server's reading of a sheet and the import it makes.
type SheetLoad struct {
	HeaderRow   int
	LabelColumn string
	Dimension   string
	Columns     []string          // the value columns' headers, in order
	MemberOf    map[string]string // value column header → member code
	Rows        []LoadRow
	Grid        string
	GridID      string
	Reshape     *importpkg.Reshape
	ColumnMap   map[string]string
	Percent     bool // a percentage metric gets fractions: import with percent_fractions
	Notes       []string
}

var (
	monthWords = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	monYearRE  = regexp.MustCompile(`(?i)^([a-z]{3})[a-z]*[\s\-/']*(\d{2}|\d{4})$`)
	isoMonthRE = regexp.MustCompile(`^(\d{4})-(\d{2})(?:-\d{2})?`)
	usDateRE   = regexp.MustCompile(`^(\d{1,2})/(\d{1,2})/(\d{2,4})$`)
)

// periodOf reads a header cell as a month, "YYYY-MM" (or ""): Jan-27, Jan
// 2027, 2027-01, 2027-01-31, 1/31/27, an Excel date serial.
func periodOf(v string) string {
	v = strings.TrimSpace(v)
	if m := monYearRE.FindStringSubmatch(v); m != nil {
		mo, ok := monthWords[strings.ToLower(m[1])]
		if !ok {
			return ""
		}
		y, _ := strconv.Atoi(m[2])
		if y < 100 {
			y += 2000
		}
		return fmt.Sprintf("%04d-%02d", y, mo)
	}
	if m := isoMonthRE.FindStringSubmatch(v); m != nil {
		return m[1] + "-" + m[2]
	}
	if m := usDateRE.FindStringSubmatch(v); m != nil {
		mo, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[3])
		if y < 100 {
			y += 2000
		}
		return fmt.Sprintf("%04d-%02d", y, mo)
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil && n > 36000 && n < 60000 && n == float64(int(n)) {
		return time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(n)).Format("2006-01")
	}
	return ""
}

func loadNumber(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	neg := strings.HasPrefix(v, "(") && strings.HasSuffix(v, ")")
	pct := strings.HasSuffix(v, "%")
	v = strings.NewReplacer(",", "", "%", "", "€", "", "$", "", "£", "", "(", "", ")", "", " ", "").Replace(v)
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	if pct {
		n /= 100
	}
	return n, true
}

// SnakeName is a label as a metric name: lower case, words joined by _.
func SnakeName(s string) string {
	var b strings.Builder
	us := false
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			us = false
		} else if !us && b.Len() > 0 {
			b.WriteByte('_')
			us = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.Split(SnakeName(s), "_") {
		if t != "" && t != "per" && t != "of" && t != "the" && t != "and" && t != "a" {
			out[t] = true
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	in := 0
	for t := range a {
		if b[t] {
			in++
		}
	}
	return float64(in) / float64(len(a)+len(b)-in)
}

// memberIndex maps a header cell to a member code of d ("" when none).
func memberIndex(d LoadDimension) func(string) string {
	byText := map[string]string{}
	byMonth := map[string]string{}
	for _, m := range d.Members {
		byText[strings.ToLower(strings.TrimSpace(m.Code))] = m.Code
		if m.Label != "" {
			byText[strings.ToLower(strings.TrimSpace(m.Label))] = m.Code
		}
		if d.Time && len(m.Start) >= 7 {
			byMonth[m.Start[:7]] = m.Code
		}
	}
	return func(v string) string {
		if c, ok := byText[strings.ToLower(strings.TrimSpace(v))]; ok {
			return c
		}
		if d.Time {
			if p := periodOf(v); p != "" {
				return byMonth[p]
			}
		}
		return ""
	}
}

// PlanSheetLoad reads a sheet (grid: its cells as read, row 1 first) for
// loading into the model. headerRow > 0 fixes the header row.
func PlanSheetLoad(grid [][]string, inv LoadInventory, headerRow int, gridHint string) (*SheetLoad, error) {
	cell := func(r, c int) string {
		if r < 1 || r > len(grid) || c < 0 || c >= len(grid[r-1]) {
			return ""
		}
		return strings.TrimSpace(grid[r-1][c])
	}
	// 1. The header: the row whose cells name the most members of one
	//    dimension (two or more).
	bestRow, bestN := 0, 1
	var bestDim *LoadDimension
	var bestCols []int
	last := len(grid)
	if last > 40 {
		last = 40
	}
	for di := range inv.Dimensions {
		d := &inv.Dimensions[di]
		idx := memberIndex(*d)
		for r := 1; r <= last; r++ {
			if headerRow > 0 && r != headerRow {
				continue
			}
			var cols []int
			for c := range grid[r-1] {
				if v := cell(r, c); v != "" && idx(v) != "" {
					cols = append(cols, c)
				}
			}
			if len(cols) > bestN {
				bestRow, bestN, bestDim, bestCols = r, len(cols), d, cols
			}
		}
	}
	if bestDim == nil {
		return nil, fmt.Errorf("no row of this sheet names the members of a dimension of the model across its columns (scenarios, months, …): " +
			"load_sheet reads that layout. For a few settings use write_input_values; for a list of records, a table import")
	}
	idx := memberIndex(*bestDim)
	sl := &SheetLoad{HeaderRow: bestRow, Dimension: bestDim.Name, MemberOf: map[string]string{}}
	for _, c := range bestCols {
		h := cell(bestRow, c)
		sl.Columns = append(sl.Columns, h)
		sl.MemberOf[h] = idx(h)
	}
	// 2. The label column: left of the value columns, the one holding the
	//    most text in the rows below.
	labelCol, bestText := -1, 0
	for c := 0; c < bestCols[0]; c++ {
		n := 0
		for r := bestRow + 1; r <= len(grid); r++ {
			if v := cell(r, c); v != "" {
				if _, num := loadNumber(v); !num {
					n++
				}
			}
		}
		if n > bestText {
			labelCol, bestText = c, n
		}
	}
	if labelCol < 0 {
		return nil, fmt.Errorf("no column left of the %s columns holds row labels", bestDim.Name)
	}
	sl.LabelColumn = cell(bestRow, labelCol)
	if sl.LabelColumn == "" {
		return nil, fmt.Errorf("the label column has no header in row %d: give the header row (header_row)", bestRow)
	}
	// 3. The data rows and their metrics.
	byName, byLabel := map[string]*LoadMetric{}, map[string]*LoadMetric{}
	for i := range inv.Metrics {
		m := &inv.Metrics[i]
		byName[strings.ToLower(m.Name)] = m
		if m.Label != "" {
			byLabel[strings.ToLower(strings.TrimSpace(m.Label))] = m
		}
	}
	used := map[string]bool{}
	gridVotes := map[string]int{}
	for r := bestRow + 1; r <= len(grid); r++ {
		label := cell(r, labelCol)
		if label == "" {
			continue
		}
		n, fractions := 0, true
		for _, c := range bestCols {
			if v, ok := loadNumber(cell(r, c)); ok {
				n++
				if v < -1 || v > 1 {
					fractions = false
				}
			}
		}
		if n == 0 {
			continue // a heading
		}
		lr := LoadRow{Row: r, Label: label, Values: n, Match: "none", Fraction: fractions}
		var m *LoadMetric
		switch {
		case byName[SnakeName(label)] != nil:
			m, lr.Match = byName[SnakeName(label)], "exact"
		case byLabel[strings.ToLower(label)] != nil:
			m, lr.Match = byLabel[strings.ToLower(label)], "label"
		default:
			lt, best := tokens(label), 0.0
			for i := range inv.Metrics {
				cand := &inv.Metrics[i]
				s := jaccard(lt, tokens(cand.Name))
				if cand.Label != "" {
					if s2 := jaccard(lt, tokens(cand.Label)); s2 > s {
						s = s2
					}
				}
				if s > best {
					best, m = s, cand
				}
			}
			if best >= 0.6 {
				lr.Match = "nearest"
			} else {
				m = nil
			}
		}
		if m != nil && used[strings.ToLower(m.Name)] {
			m, lr.Match = nil, "none"
		}
		if m != nil {
			used[strings.ToLower(m.Name)] = true
			lr.Metric = m.Name
			if m.GridID != "" {
				gridVotes[m.GridID+"\x00"+m.Grid]++
			}
			if m.Format == "percentage" && fractions {
				sl.Percent = true
			}
		} else {
			lr.Suggested = SnakeName(label)
		}
		sl.Rows = append(sl.Rows, lr)
	}
	if len(sl.Rows) == 0 {
		return nil, fmt.Errorf("no row under the header (row %d) holds numbers in the %s columns", bestRow, bestDim.Name)
	}
	// 4. The grid: the one most matched metrics are on, or the one named.
	type vote struct {
		key string
		n   int
	}
	var votes []vote
	for k, n := range gridVotes {
		votes = append(votes, vote{k, n})
	}
	sort.Slice(votes, func(i, j int) bool {
		return votes[i].n > votes[j].n || votes[i].n == votes[j].n && votes[i].key < votes[j].key
	})
	for _, v := range votes {
		id, name, _ := strings.Cut(v.key, "\x00")
		if gridHint == "" || strings.EqualFold(gridHint, name) || gridHint == id {
			sl.GridID, sl.Grid = id, name
			break
		}
	}
	if sl.GridID == "" && gridHint != "" {
		sl.Grid = gridHint
	}
	if sl.GridID != "" {
		dims := inv.GridDims[sl.GridID]
		if len(dims) != 1 || !strings.EqualFold(dims[0], bestDim.Name) {
			sl.Notes = append(sl.Notes, fmt.Sprintf("grid %q is by %s; this sheet is by %s only — put the metrics on a grid by %s", sl.Grid, strings.Join(dims, " × "), bestDim.Name, bestDim.Name))
		}
	}
	// 5. The reshape and column map the import uses.
	labels := map[string]string{}
	for _, r := range sl.Rows {
		if r.Metric != "" {
			labels[r.Label] = r.Metric
		}
	}
	vm := importpkg.ValueMaps{sl.LabelColumn: labels}
	members := map[string]string{}
	for h, code := range sl.MemberOf {
		if h != code {
			members[h] = code
		}
	}
	if len(members) > 0 {
		vm[bestDim.Name] = members
	}
	sl.Reshape = &importpkg.Reshape{
		HeaderRow: bestRow,
		SkipRows:  []importpkg.RowFilter{{Column: sl.Columns[0], Blank: true}},
		Unpivot:   &importpkg.Unpivot{Columns: sl.Columns, NameColumn: bestDim.Name, ValueColumn: "value"},
		ValueMap:  vm,
	}
	sl.ColumnMap = map[string]string{sl.LabelColumn: "metric", bestDim.Name: bestDim.Name, "value": "value", "*": "ignore"}
	return sl, nil
}

// Unmatched are the rows no metric was found for.
func (s *SheetLoad) Unmatched() []LoadRow {
	var out []LoadRow
	for _, r := range s.Rows {
		if r.Metric == "" {
			out = append(out, r)
		}
	}
	return out
}

// Describe is the reading for the AI: layout, every row's metric, what is
// missing, and the step to propose.
func (s *SheetLoad) Describe(sheet, convName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Sheet %s: header row %d; labels in column %q; %d %s columns (%s … %s); %d data rows.\n",
		sheet, s.HeaderRow, s.LabelColumn, len(s.Columns), s.Dimension, s.Columns[0], s.Columns[len(s.Columns)-1], len(s.Rows))
	counts := map[string]int{}
	for _, r := range s.Rows {
		counts[r.Match]++
	}
	fmt.Fprintf(&b, "Rows matched to metrics: %d exact, %d by label, %d nearest (check them), %d with no metric.\n",
		counts["exact"], counts["label"], counts["nearest"], counts["none"])
	for _, r := range s.Rows {
		switch r.Match {
		case "nearest":
			fmt.Fprintf(&b, "- row %d %q → %s (nearest: confirm it is this row's metric)\n", r.Row, r.Label, r.Metric)
		case "none":
			fmt.Fprintf(&b, "- row %d %q → NO METRIC (suggested name %s)\n", r.Row, r.Label, r.Suggested)
		}
	}
	for _, n := range s.Notes {
		fmt.Fprintf(&b, "NOTE: %s\n", n)
	}
	if un := s.Unmatched(); len(un) > 0 {
		fmt.Fprintf(&b, "The import is all or nothing: create the %d missing metric(s) first (create_metric, input, on grid %q), then call load_sheet again.\n", len(un), s.Grid)
		return b.String()
	}
	params := map[string]any{"file": convName, "target_type": "grid"}
	if s.GridID != "" {
		params["target_id"] = s.GridID
	}
	if s.Percent {
		params["percent_fractions"] = true
	}
	fmt.Fprintf(&b, "Saved as the converted file %q (the sheet read with this mapping). Propose ONE step — never retype these values:\n", convName)
	fmt.Fprintf(&b, "  import_file_data %s\n", mustJSON(params))
	if s.Percent {
		b.WriteString("(percent_fractions: the sheet holds percentages as fractions; the server multiplies only the Percentage metrics' values by 100.)\n")
	}
	return b.String()
}

func mustJSON(v any) string {
	out, _ := jsonMarshalNoEscape(v)
	return out
}

func jsonMarshalNoEscape(v any) (string, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}
