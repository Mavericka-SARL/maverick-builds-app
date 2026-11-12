package aiassistant

// analyse_workbook: the whole workbook at once, read by the server — what
// each sheet is (inputs, calculations, a report, an import template, text),
// its layout (header row, label column, period and other value columns), its
// rows marked input or formula, which sheets it reads and is read by, and an
// order to build the sheets in. The AI designs the target model from it
// before planning (owner, 2026-11-12: "analyse the whole model first and
// understand the final picture of the target TO-BE model").

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

var (
	sheetRefRE  = regexp.MustCompile(`'((?:[^']|'')+)'!|([A-Za-z_][A-Za-z0-9_.]*)!`)
	periodHdrRE = regexp.MustCompile(`(?i)^(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*[\s\-/']*\d{2,4}$|^\d{4}-\d{2}(-\d{2})?$|^\d{1,2}/\d{1,2}/\d{2,4}$|^(fy|q[1-4])\s*\d{2,4}$`)
)

type waRow struct {
	row     int
	label   string
	nums    int
	formula int
	text    int
}

type sheetInfo struct {
	name      string
	rows      []waRow
	maxCol    int
	header    int
	headers   []string
	periods   []string
	periodCol [2]int
	reads     map[string]int
	readBy    map[string]bool
	role      string
}

// AnalyseWorkbook describes an .xlsx workbook's sheets (only those named, when
// any are) for designing the model it becomes.
func AnalyseWorkbook(raw []byte, only []string) (string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("read the workbook: %w", err)
	}
	defer func() { _ = f.Close() }()
	names := f.GetSheetList()
	known := map[string]string{}
	for _, n := range names {
		known[strings.ToLower(n)] = n
	}
	want := map[string]bool{}
	for _, s := range only {
		want[strings.ToLower(strings.TrimSpace(s))] = true
	}
	infos := map[string]*sheetInfo{}
	for _, name := range names {
		si := &sheetInfo{name: name, reads: map[string]int{}, readBy: map[string]bool{}}
		infos[name] = si
		rows, err := f.GetRows(name)
		if err != nil {
			continue
		}
		for r, cells := range rows {
			sr := waRow{row: r + 1}
			for c, v := range cells {
				if c+1 > si.maxCol && strings.TrimSpace(v) != "" {
					si.maxCol = c + 1
				}
				axis, _ := excelize.CoordinatesToCellName(c+1, r+1)
				fml, _ := f.GetCellFormula(name, axis)
				v = strings.TrimSpace(v)
				switch {
				case fml != "":
					sr.formula++
					for _, m := range sheetRefRE.FindAllStringSubmatch(fml, -1) {
						ref := m[1]
						if ref == "" {
							ref = m[2]
						}
						ref = strings.ReplaceAll(ref, "''", "'")
						if k, ok := known[strings.ToLower(ref)]; ok && k != name {
							si.reads[k]++
						}
					}
				case v == "":
				case looksNumber(v):
					sr.nums++
				default:
					sr.text++
					if sr.label == "" && c <= 2 {
						sr.label = v
					}
				}
			}
			if sr.label != "" || sr.nums > 0 || sr.formula > 0 {
				si.rows = append(si.rows, sr)
			}
		}
		si.findLayout(rows)
	}
	for _, si := range infos {
		for r := range si.reads {
			infos[r].readBy[si.name] = true
		}
	}
	for _, si := range infos {
		si.role = si.guessRole()
	}
	order, cyclic := buildOrder(names, infos)
	var b strings.Builder
	fmt.Fprintf(&b, "Workbook: %d sheets. Build order (each sheet after the sheets it reads): %s\n", len(names), strings.Join(order, " → "))
	if len(cyclic) > 0 {
		fmt.Fprintf(&b, "Sheets that read each other (a cycle — build them together): %s\n", strings.Join(cyclic, ", "))
	}
	b.WriteString("Rows: I = numbers typed in (inputs), F = formulas, T = text only. Roles are the server's reading; the target model decides.\n")
	b.WriteString("Build structure first: import templates and data tables (actuals) come after the calculations, process and pages, whatever their place in this order; the real data last.\n")
	for _, name := range order {
		if len(want) > 0 && !want[strings.ToLower(name)] {
			continue
		}
		infos[name].render(&b)
	}
	return b.String(), nil
}

func looksNumber(v string) bool {
	v = strings.TrimSpace(strings.NewReplacer(",", "", "%", "", "€", "", "$", "", "(", "-", ")", "").Replace(v))
	_, err := strconv.ParseFloat(v, 64)
	return err == nil
}

// findLayout finds the header row (the last row with three or more text
// cells before the first row holding numbers or formulas under a label) and
// the period columns in it.
func (si *sheetInfo) findLayout(rows [][]string) {
	first := 0
	for _, r := range si.rows {
		if r.label != "" && (r.nums > 0 || r.formula > 0) {
			first = r.row
			break
		}
	}
	// The header: the row above the data with the most period-like cells
	// (dates across), else the last one with three or more cells.
	bestPeriods := 0
	for r := first + 2; r >= 1 && r >= first-8; r-- {
		if r > len(rows) {
			continue
		}
		n, p := 0, 0
		for _, v := range rows[r-1] {
			if v = strings.TrimSpace(v); v != "" {
				n++
				if periodHdrRE.MatchString(v) {
					p++
				}
			}
		}
		if p > bestPeriods {
			bestPeriods, si.header = p, r
		}
		if r < first && n >= 3 && si.header == 0 && bestPeriods == 0 {
			si.header = r
		}
	}
	if si.header == 0 {
		return
	}
	for c, v := range rows[si.header-1] {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if periodHdrRE.MatchString(v) {
			if len(si.periods) == 0 {
				si.periodCol[0] = c + 1
			}
			si.periods = append(si.periods, v)
			si.periodCol[1] = c + 1
		} else {
			si.headers = append(si.headers, v)
		}
	}
}

func (si *sheetInfo) guessRole() string {
	var nums, fml, labels int
	for _, r := range si.rows {
		if r.label != "" {
			labels++
		}
		if r.formula > r.nums {
			fml++
		} else if r.nums > 0 {
			nums++
		}
	}
	switch {
	case len(si.rows) > 50 && labels*5 < len(si.rows):
		return "data table or import template (rows without labels)"
	case nums == 0 && fml == 0 && si.header > 0:
		return "import template (headers, no data)"
	case nums == 0 && fml == 0:
		return "text (guide or notes)"
	case fml == 0:
		return "inputs"
	case nums == 0 && len(si.reads) >= 4:
		return "report (reads many sheets)"
	case nums > fml:
		return "inputs with some formulas"
	default:
		return "calculations"
	}
}

func (si *sheetInfo) render(b *strings.Builder) {
	fmt.Fprintf(b, "\n## %s — %s; %d rows used, %d columns", si.name, si.role, len(si.rows), si.maxCol)
	if si.header > 0 {
		fmt.Fprintf(b, "; header row %d", si.header)
	}
	b.WriteByte('\n')
	if len(si.periods) > 0 {
		c0, _ := excelize.ColumnNumberToName(si.periodCol[0])
		c1, _ := excelize.ColumnNumberToName(si.periodCol[1])
		fmt.Fprintf(b, "Period columns: %d (%s–%s: %s … %s)\n", len(si.periods), c0, c1, si.periods[0], si.periods[len(si.periods)-1])
	}
	if len(si.headers) > 0 {
		h := si.headers
		if len(h) > 12 {
			h = append(append([]string{}, h[:12]...), "…")
		}
		fmt.Fprintf(b, "Other columns: %s\n", strings.Join(h, " | "))
	}
	if len(si.reads) > 0 {
		var rs []string
		for k, n := range si.reads {
			rs = append(rs, fmt.Sprintf("%s (%d)", k, n))
		}
		sort.Strings(rs)
		fmt.Fprintf(b, "Reads: %s\n", strings.Join(rs, ", "))
	}
	if len(si.readBy) > 0 {
		var rs []string
		for k := range si.readBy {
			rs = append(rs, k)
		}
		sort.Strings(rs)
		fmt.Fprintf(b, "Read by: %s\n", strings.Join(rs, ", "))
	}
	const maxRows = 90
	var parts []string
	for i, r := range si.rows {
		if i == maxRows {
			parts = append(parts, fmt.Sprintf("… %d more rows (read_attached_sheet)", len(si.rows)-maxRows))
			break
		}
		kind := "T"
		switch {
		case r.formula > r.nums:
			kind = "F"
		case r.nums > 0:
			kind = "I"
		}
		label := r.label
		if r := []rune(label); len(r) > 60 {
			label = string(r[:60]) + "…"
		}
		parts = append(parts, fmt.Sprintf("%d %s %s", r.row, kind, label))
	}
	if len(parts) > 0 {
		fmt.Fprintf(b, "Rows: %s\n", strings.Join(parts, " | "))
	}
}

// buildOrder sorts the sheets so each comes after the sheets it reads,
// keeping the workbook's order otherwise; sheets left in a cycle follow, in
// the workbook's order.
func buildOrder(names []string, infos map[string]*sheetInfo) (order, cyclic []string) {
	done := map[string]bool{}
	for len(order) < len(names) {
		progressed := false
		for _, n := range names {
			if done[n] {
				continue
			}
			ready := true
			for r := range infos[n].reads {
				if !done[r] {
					ready = false
					break
				}
			}
			if ready {
				done[n] = true
				order = append(order, n)
				progressed = true
			}
		}
		if !progressed {
			// A cycle: take the remaining sheet that waits on the fewest
			// others (the workbook's order on a tie), then go on.
			best, wait := "", 1<<30
			for _, n := range names {
				if done[n] {
					continue
				}
				w := 0
				for r := range infos[n].reads {
					if !done[r] {
						w++
					}
				}
				if w < wait {
					best, wait = n, w
				}
			}
			done[best] = true
			order = append(order, best)
			cyclic = append(cyclic, best)
		}
	}
	return order, cyclic
}
