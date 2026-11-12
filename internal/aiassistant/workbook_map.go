package aiassistant

// The workbook map (migration 179): which metric each row of an attached
// workbook became and how it matched when last compared. compare_with_sheet
// writes it; the read tool workbook_map, the turn's brief and the server's
// check after a confirmed plan read it; a new session (or a new draft) reads
// the map its model already has. Recommendation R1 of the High-Tech build
// (2026-11-11): the AI spent most of its calls re-listing metrics and
// re-reading sheets to learn what a row had become.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// MapRow is one row of an attached workbook and the metric it became.
type MapRow struct {
	File        string            `json:"file"`
	Sheet       string            `json:"sheet"`
	Row         int               `json:"row"`
	Label       string            `json:"label,omitempty"`
	Metric      string            `json:"metric"`
	Members     map[string]string `json:"members,omitempty"`
	FirstColumn string            `json:"first_column"`
	Matched     int               `json:"matched"`
	Compared    int               `json:"compared"`
	// Revision is where the row was last compared: this revision, or the
	// one the map was carried from.
	Revision string `json:"revision_id"`
}

// Matches reports whether every compared period matched.
func (r MapRow) Matches() bool { return r.Compared > 0 && r.Matched == r.Compared }

// membersKey is a stable text of members ("" for none).
func membersKey(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, strings.ToLower(k))
	}
	sort.Strings(keys)
	low := map[string]string{}
	for k, v := range m {
		low[strings.ToLower(k)] = v
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + low[k]
	}
	return strings.Join(parts, ";")
}

// RecordWorkbookRow keeps what one comparison found (best-effort: the
// comparison's answer never depends on it).
func RecordWorkbookRow(ctx context.Context, q dbx.DB, modelID, revID string, r MapRow) {
	if modelID == "" || revID == "" || r.Row <= 0 || strings.TrimSpace(r.Metric) == "" {
		return
	}
	members, _ := json.Marshal(r.Members)
	if r.Members == nil {
		members = []byte("{}")
	}
	_, _ = q.Exec(ctx, `
		INSERT INTO ai_assistant.workbook_map (model_id, revision_id, file_name, sheet, row_number, members_key, members, metric_name, first_column, label, matched, compared, updated_at)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, now())
		ON CONFLICT (revision_id, file_name, sheet, row_number, members_key) DO UPDATE
		  SET metric_name = EXCLUDED.metric_name, members = EXCLUDED.members, first_column = EXCLUDED.first_column,
		      label = CASE WHEN EXCLUDED.label <> '' THEN EXCLUDED.label ELSE ai_assistant.workbook_map.label END,
		      matched = EXCLUDED.matched, compared = EXCLUDED.compared, updated_at = now()`,
		modelID, revID, r.File, r.Sheet, r.Row, membersKey(r.Members), string(members), r.Metric, strings.ToUpper(r.FirstColumn), r.Label, r.Matched, r.Compared)
}

// LoadWorkbookMap is the model's map for files (all files when none are
// named): this revision's rows, and for a row it never compared, the latest
// row from another revision of the model — a new draft starts from its
// source's map. Ordered by file, sheet and row.
func LoadWorkbookMap(ctx context.Context, q dbx.DB, modelID, revID string, files []string) ([]MapRow, error) {
	if files == nil {
		files = []string{}
	}
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ON (lower(file_name), lower(sheet), row_number, members_key)
		       file_name, sheet, row_number, label, metric_name, members, first_column, matched, compared, revision_id::text
		FROM ai_assistant.workbook_map
		WHERE model_id = $1::uuid AND (cardinality($3::text[]) = 0 OR lower(file_name) = ANY($3))
		ORDER BY lower(file_name), lower(sheet), row_number, members_key, (revision_id = NULLIF($2, '')::uuid) IS TRUE DESC, updated_at DESC`,
		modelID, revID, lowerAll(files))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MapRow, error) {
		var m MapRow
		var members []byte
		err := r.Scan(&m.File, &m.Sheet, &m.Row, &m.Label, &m.Metric, &members, &m.FirstColumn, &m.Matched, &m.Compared, &m.Revision)
		_ = json.Unmarshal(members, &m.Members)
		return m, err
	})
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(strings.TrimSpace(s))
	}
	return out
}

// SheetCoverage is one sheet's part of the map: rows mapped, matching,
// differing, and the rows holding numbers that no metric was compared with.
type SheetCoverage struct {
	File, Sheet          string
	Mapped, Match        int
	Differ, NotMapped    []int
	NotMappedLabels      map[int]string
	FirstColumn          string
	unmappedRowsComputed bool
}

// Coverage groups rows by file and sheet and, given the workbooks (file name
// → bytes), finds each mapped sheet's rows with a label and two numbers or
// more from its first compared column that no metric was compared with.
func Coverage(rows []MapRow, workbooks map[string][]byte) []SheetCoverage {
	type key struct{ file, sheet string }
	byKey := map[key]*SheetCoverage{}
	var order []key
	firstCols := map[key]map[string]int{}
	mappedRows := map[key]map[int]bool{}
	for _, r := range rows {
		k := key{strings.ToLower(r.File), strings.ToLower(r.Sheet)}
		c := byKey[k]
		if c == nil {
			c = &SheetCoverage{File: r.File, Sheet: r.Sheet, NotMappedLabels: map[int]string{}}
			byKey[k] = c
			order = append(order, k)
			firstCols[k] = map[string]int{}
			mappedRows[k] = map[int]bool{}
		}
		if !mappedRows[k][r.Row] {
			c.Mapped++
		}
		mappedRows[k][r.Row] = true
		firstCols[k][r.FirstColumn]++
		if r.Matches() {
			c.Match++
		} else if r.Compared > 0 {
			c.Differ = append(c.Differ, r.Row)
		}
	}
	out := make([]SheetCoverage, 0, len(order))
	for _, k := range order {
		c := byKey[k]
		best, n := "", 0
		for col, cnt := range firstCols[k] {
			if cnt > n || (cnt == n && col < best) {
				best, n = col, cnt
			}
		}
		c.FirstColumn = best
		for name, data := range workbooks {
			if !strings.EqualFold(name, c.File) {
				continue
			}
			c.NotMapped, c.NotMappedLabels = unmappedRows(data, c.Sheet, best, mappedRows[k])
			c.unmappedRowsComputed = true
		}
		sort.Ints(c.Differ)
		out = append(out, *c)
	}
	return out
}

// unmappedRows reads sheet of a workbook: the rows with a text label left of
// firstCol and two numbers or more from it on that mapped does not hold.
func unmappedRows(data []byte, sheet, firstCol string, mapped map[int]bool) ([]int, map[int]string) {
	labels := map[int]string{}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return nil, labels
	}
	defer func() { _ = f.Close() }()
	col, err := excelize.ColumnNameToNumber(firstCol)
	if err != nil || strings.TrimSpace(sheet) == "" {
		return nil, labels
	}
	grid, err := f.GetRows(sheet)
	if err != nil {
		return nil, labels
	}
	var out []int
	for i, cells := range grid {
		row := i + 1
		if mapped[row] {
			continue
		}
		label := ""
		for c := 0; c < col-1 && c < len(cells); c++ {
			v := strings.TrimSpace(cells[c])
			if _, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", ""), 64); v != "" && err != nil {
				label = v
				break
			}
		}
		if label == "" {
			continue
		}
		var nums []float64
		for c := col - 1; c < len(cells); c++ {
			if v, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(cells[c]), ",", ""), 64); err == nil && strings.TrimSpace(cells[c]) != "" {
				nums = append(nums, v)
			}
		}
		if len(nums) >= 2 && !isCounter(nums) {
			out = append(out, row)
			labels[row] = label
		}
	}
	return out, labels
}

// RenderCoverage is the map's summary for a turn's brief: one line a sheet.
func RenderCoverage(cov []SheetCoverage) string {
	if len(cov) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### Workbook map (workbook_map lists it row by row; compare_with_sheet keeps it)\n")
	for _, c := range cov {
		fmt.Fprintf(&b, "- %s / %s: %d row(s) mapped, %d match every period", c.File, c.Sheet, c.Mapped, c.Match)
		if len(c.Differ) > 0 {
			fmt.Fprintf(&b, ", %d differ (rows %s)", len(c.Differ), intList(c.Differ, 15))
		}
		if c.unmappedRowsComputed {
			if len(c.NotMapped) > 0 {
				fmt.Fprintf(&b, "; %d row(s) with numbers not mapped yet (rows %s)", len(c.NotMapped), intList(c.NotMapped, 15))
			} else {
				b.WriteString("; every row with numbers is mapped")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func intList(ns []int, limit int) string {
	parts := make([]string, 0, min(len(ns), limit)+1)
	for i, n := range ns {
		if i == limit {
			parts = append(parts, fmt.Sprintf("… %d more", len(ns)-limit))
			break
		}
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ", ")
}

// RenderWorkbookMap is the read tool's answer: each mapped row, then the
// rows with numbers no metric was compared with.
func RenderWorkbookMap(rows []MapRow, cov []SheetCoverage, sheet string) string {
	if len(rows) == 0 {
		return "The workbook map is empty: compare_with_sheet a row with the metric it became, and the map keeps it for every later turn and session."
	}
	var b strings.Builder
	cur := ""
	for _, r := range rows {
		if sheet != "" && !strings.EqualFold(r.Sheet, sheet) {
			continue
		}
		if k := r.File + " / " + r.Sheet; k != cur {
			cur = k
			fmt.Fprintf(&b, "## %s\n", k)
		}
		state := "not compared"
		switch {
		case r.Matches():
			state = fmt.Sprintf("matches (%d periods)", r.Compared)
		case r.Compared > 0:
			state = fmt.Sprintf("%d of %d periods match", r.Matched, r.Compared)
		}
		target := r.Metric
		if mk := membersKey(r.Members); mk != "" {
			target += " [" + mk + "]"
		}
		label := r.Label
		if label != "" {
			label = " " + strconv.Quote(label)
		}
		fmt.Fprintf(&b, "row %d%s → %s (from column %s): %s\n", r.Row, label, target, r.FirstColumn, state)
	}
	for _, c := range cov {
		if sheet != "" && !strings.EqualFold(c.Sheet, sheet) || len(c.NotMapped) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s / %s — rows with numbers not mapped yet\n", c.File, c.Sheet)
		for _, row := range c.NotMapped {
			fmt.Fprintf(&b, "row %d %q\n", row, c.NotMappedLabels[row])
		}
	}
	return b.String()
}

// mapCellRef finds a cell reference in a formula: an optional sheet ('Sheet
// name'! or Sheet!), then $col$row.
var mapCellRef = regexp.MustCompile(`((?:'[^']+'|[A-Za-z_][A-Za-z0-9_.]*)!)?(\$?)([A-Z]{1,3})(\$?)([0-9]+)`)

// formulaLine is a line of read_attached_sheet's formulas: the cell or
// range, an optional [header], and the formula.
var formulaLine = regexp.MustCompile(`^([A-Z]{1,3}[0-9]+)(?::[A-Z]{1,3}[0-9]+)?(?: \[[^\]]*\])? = (.*)$`)

// AnnotateFormulas adds under each formula of a read_attached_sheet answer
// its reading in the model's names, where the workbook map knows the rows it
// reads (recommendation R2 of the High-Tech build, 2026-11-11): a reference
// to a mapped row in the cell's own column is that metric; one to the
// column before is LAG(metric, 1) (n columns before: LAG(metric, n)); a
// fixed column ($C9) is the metric. Other references stay as they are. The
// AI then checks a translation instead of working one out — the part mini
// models got wrong most.
func AnnotateFormulas(text, sheet string, rows []MapRow) string {
	byRow := map[string]string{} // lower(sheet) + "\x00" + row → metric
	for _, r := range rows {
		k := strings.ToLower(strings.TrimSpace(r.Sheet)) + "\x00" + strconv.Itoa(r.Row)
		if _, dup := byRow[k]; !dup {
			byRow[k] = r.Metric
		}
	}
	if len(byRow) == 0 {
		return text
	}
	var out strings.Builder
	current := sheet
	inFormulas := false
	for _, line := range strings.SplitAfter(text, "\n") {
		out.WriteString(line)
		trim := strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(trim, "### Formulas on "):
			inFormulas = true
			current = strings.TrimSpace(strings.SplitN(strings.TrimPrefix(trim, "### Formulas on "), " (", 2)[0])
			continue
		case strings.HasPrefix(trim, "#"):
			inFormulas = false
			continue
		}
		if !inFormulas {
			continue
		}
		m := formulaLine.FindStringSubmatch(trim)
		if m == nil {
			continue
		}
		col0, row0, err := excelize.CellNameToCoordinates(m[1])
		if err != nil {
			continue
		}
		if translated, ok := translateFormula(m[2], current, col0, row0, byRow); ok {
			out.WriteString("   ≈ in the model's names: " + translated + "\n")
		}
	}
	return out.String()
}

// translateFormula rewrites the references of formula (in sheet, at column
// col0) that the map knows; ok when at least one was.
func translateFormula(formula, sheet string, col0, row0 int, byRow map[string]string) (string, bool) {
	changed := false
	quoted := false
	var out strings.Builder
	last := 0
	locs := mapCellRef.FindAllStringSubmatchIndex(formula, -1)
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		// Inside a quoted text, part of a range (A1:B2), part of a name or a
		// function (LOG10(…)): left as it is.
		quoted = strings.Count(formula[:start], `"`)%2 == 1
		if quoted || (start > 0 && (isIdentChar(formula[start-1]) || formula[start-1] == ':')) ||
			(end < len(formula) && (isIdentChar(formula[end]) || formula[end] == ':' || formula[end] == '(')) {
			continue
		}
		refSheet := sheet
		if loc[2] >= 0 {
			refSheet = strings.Trim(strings.TrimSuffix(formula[loc[2]:loc[3]], "!"), "'")
		}
		colAbs := loc[4] < loc[5]
		colName := formula[loc[6]:loc[7]]
		row, _ := strconv.Atoi(formula[loc[10]:loc[11]])
		metric, ok := byRow[strings.ToLower(strings.TrimSpace(refSheet))+"\x00"+strconv.Itoa(row)]
		if !ok {
			continue
		}
		col, err := excelize.ColumnNameToNumber(colName)
		if err != nil {
			continue
		}
		repl := ""
		switch {
		case colAbs || col == col0:
			repl = metric
		case col < col0:
			repl = fmt.Sprintf("LAG(%s, %d)", metric, col0-col)
		default:
			continue
		}
		out.WriteString(formula[last:start])
		out.WriteString(repl)
		last = end
		changed = true
	}
	if !changed {
		return "", false
	}
	out.WriteString(formula[last:])
	return out.String(), true
}

func isIdentChar(c byte) bool {
	return c == '_' || c == '.' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// isCounter reports a row of whole numbers each one more than the last —
// 1, 2, 3, … or, read from its second column on, 2, 3, 4, … — a sheet's
// month counter, which a formula reads as the model month number, not a row
// a metric becomes. Two numbers count only as 1, 2.
func isCounter(nums []float64) bool {
	if len(nums) < 6 && !(len(nums) >= 2 && nums[0] == 1) {
		return false
	}
	for i, v := range nums {
		if v != nums[0]+float64(i) || v != float64(int64(v)) {
			return false
		}
	}
	return true
}
