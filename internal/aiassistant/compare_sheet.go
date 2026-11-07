package aiassistant

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/calculation"
)

// compare_with_sheet (2026-11-07): the assistant said "complete" when a
// third of a sheet's rows did not match it, and only an outside comparison
// of values showed which. The tool reads one row of an attached sheet across
// the time dimension's periods and the metric the row became, and names
// every period where they differ.

const compareWithSheetSchema = `{"type":"object","properties":{
	"file":{"type":"string","description":"The attached file's name"},
	"sheet":{"type":"string","description":"Worksheet name; omit for a CSV file"},
	"row":{"type":"integer","description":"The row (1-based) holding the line, as read_attached_sheet numbers it"},
	"first_column":{"type":"string","description":"The column letter of the row's first period value (e.g. \"D\")"},
	"metric":{"type":"string","description":"The model metric the row became"},
	"members":{"type":"object","description":"A member code for each other dimension of the metric, by dimension name (e.g. {\"Scenario\": \"BASE\"}); the time dimension's periods run along the row"},
	"rows":{"type":"array","description":"Several rows of the sheet at once instead of row/metric/members: [{\"row\", \"metric\", \"members\"?, \"first_column\"?}], each taking the call's first_column and members where it gives none. Check a whole sheet in one call.","items":{"type":"object","properties":{"row":{"type":"integer"},"metric":{"type":"string"},"members":{"type":"object"},"first_column":{"type":"string"}},"required":["row","metric"]}},
	"tolerance":{"type":"number","description":"Largest difference counted as equal, as a fraction of the sheet's value; default 0.0005"}
},"required":["file","first_column"]}`

// compareRow is one row compare_with_sheet checks.
type compareRow struct {
	Row         int               `json:"row"`
	Metric      string            `json:"metric"`
	Members     map[string]string `json:"members"`
	FirstColumn string            `json:"first_column"`
}

// compareWithSheet is compare_with_sheet: one row, or several ("rows").
func (e *ToolExecutor) compareWithSheet(ctx context.Context, raw json.RawMessage) (string, error) {
	if e.hooks.AttachedFile == nil {
		return "", fmt.Errorf("comparing with an attached file is not available here")
	}
	var p struct {
		File        string            `json:"file"`
		Sheet       string            `json:"sheet"`
		Row         int               `json:"row"`
		FirstColumn string            `json:"first_column"`
		Metric      string            `json:"metric"`
		Members     map[string]string `json:"members"`
		Rows        []compareRow      `json:"rows"`
		Tolerance   float64           `json:"tolerance"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", fmt.Errorf("invalid params: %w", err)
	}
	if p.Tolerance <= 0 {
		p.Tolerance = 0.0005
	}
	rows := p.Rows
	if len(rows) == 0 {
		if p.Row == 0 || strings.TrimSpace(p.Metric) == "" {
			return "", fmt.Errorf("give row and metric, or rows")
		}
		rows = []compareRow{{Row: p.Row, Metric: p.Metric, Members: p.Members}}
	}
	if len(rows) > maxCompareRows {
		return "", fmt.Errorf("at most %d rows in one call", maxCompareRows)
	}
	name, data, err := e.hooks.AttachedFile(ctx, p.File)
	if err != nil {
		return "", err
	}
	where := fmt.Sprintf("sheet %q", strings.TrimSpace(p.Sheet))
	if strings.TrimSpace(p.Sheet) == "" {
		where = fmt.Sprintf("file %q", name)
	}
	var b strings.Builder
	matchedRows := 0
	for i, r := range rows {
		if r.FirstColumn == "" {
			r.FirstColumn = p.FirstColumn
		}
		if r.Members == nil {
			r.Members = p.Members
		}
		if i > 0 {
			b.WriteString("\n")
		}
		line, ok, err := e.compareOne(ctx, name, data, p.Sheet, where, r, p.Tolerance, len(rows) == 1)
		if err != nil {
			fmt.Fprintf(&b, "%s against %s row %d: not compared — %v", r.Metric, where, r.Row, err)
			continue
		}
		if ok {
			matchedRows++
		}
		b.WriteString(line)
	}
	if len(rows) > 1 {
		fmt.Fprintf(&b, "\n%d of %d rows match every period.", matchedRows, len(rows))
	}
	return b.String(), nil
}

// maxCompareRows caps one compare_with_sheet call.
const maxCompareRows = 80

// compareOne compares row r of the sheet with its metric: the line it
// reports, and whether every period matched. full lists up to 24 differing
// periods; otherwise 3, so a whole sheet stays short.
func (e *ToolExecutor) compareOne(ctx context.Context, name string, data []byte, sheet, where string, r compareRow, tolerance float64, full bool) (string, bool, error) {
	col, err := excelize.ColumnNameToNumber(strings.ToUpper(strings.TrimSpace(r.FirstColumn)))
	if err != nil {
		return "", false, fmt.Errorf("first_column %q is not a column letter", r.FirstColumn)
	}
	cells, err := sheetRow(name, data, sheet, r.Row)
	if err != nil {
		return "", false, err
	}

	store := calculation.NewStore(e.pool)
	metrics, err := store.LoadModelMetrics(ctx, e.modelID, e.revID)
	if err != nil {
		return "", false, err
	}
	var m *calculation.MetricDef
	for _, md := range metrics {
		if strings.EqualFold(md.Name, strings.TrimSpace(r.Metric)) {
			m = md
		}
	}
	if m == nil {
		return "", false, fmt.Errorf("no metric named %q in this revision", r.Metric)
	}
	dimIDs, err := store.LoadMetricDimensionIDs(ctx, e.modelID, e.revID)
	if err != nil {
		return "", false, err
	}
	dims, err := store.LoadAllDimensions(ctx, e.modelID, e.revID)
	if err != nil {
		return "", false, err
	}
	dimNames, err := store.LoadDimIDToName(ctx, e.modelID, e.revID)
	if err != nil {
		return "", false, err
	}
	combo := map[string]string{}
	timeID := ""
	for _, id := range dimIDs[m.ID] {
		d := dims[id]
		if d == nil {
			continue
		}
		if d.IsTime {
			timeID = id
			continue
		}
		code := ""
		for k, v := range r.Members {
			if strings.EqualFold(k, dimNames[id]) {
				code = v
			}
		}
		if code == "" {
			return "", false, fmt.Errorf("members needs a code for %s (a dimension of %s)", dimNames[id], m.Name)
		}
		combo[id] = code
	}
	if timeID == "" {
		return "", false, fmt.Errorf("%s has no time dimension to run along the row", m.Name)
	}
	type period struct {
		code string
		idx  int
	}
	var periods []period
	for _, mm := range dims[timeID].Members {
		if mm.IsLeafPeriod() {
			periods = append(periods, period{mm.Code, mm.TimeIndex})
		}
	}
	sort.Slice(periods, func(i, j int) bool { return periods[i].idx < periods[j].idx })

	values, err := store.LoadCalcValueMap(ctx, e.modelID, e.revID, m.ID)
	if m.IsInput {
		values, err = store.LoadInputValueMap(ctx, e.modelID, e.revID, m.ID)
	}
	if err != nil {
		return "", false, err
	}
	var format string
	_ = e.pool.QueryRow(ctx, `SELECT COALESCE(format, '') FROM model.metric_def WHERE id = $1::uuid`, m.ID).Scan(&format)
	percent := format == "percentage"

	limit := 3
	if full {
		limit = 24
	}
	var diffs []string
	compared, matched := 0, 0
	for i, per := range periods {
		ci := col - 1 + i
		if ci >= len(cells) || strings.TrimSpace(cells[ci]) == "" {
			continue
		}
		want, perr := strconv.ParseFloat(strings.TrimSpace(cells[ci]), 64)
		if perr != nil {
			continue
		}
		at := map[string]string{timeID: per.code}
		for k, v := range combo {
			at[k] = v
		}
		key, _ := json.Marshal(at)
		got, ok := values[string(key)]
		compared++
		equal := func(a, b float64) bool { return math.Abs(a-b) <= tolerance*math.Max(1, math.Abs(b)) }
		if ok && (equal(got, want) || (percent && equal(got, want*100))) {
			matched++
			continue
		}
		model := "no value"
		if ok {
			model = strconv.FormatFloat(got, 'f', -1, 64)
		}
		if len(diffs) < limit {
			diffs = append(diffs, fmt.Sprintf("%s: sheet %s, model %s", per.code, strconv.FormatFloat(want, 'f', -1, 64), model))
		}
	}
	if compared == 0 {
		return fmt.Sprintf("%s against %s row %d: no number from column %s on — check the row and first_column", m.Name, where, r.Row, r.FirstColumn), false, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s against %s row %d: %d of %d periods match", m.Name, where, r.Row, matched, compared)
	if percent {
		b.WriteString(" (a percentage: the sheet's fraction counts as equal to the metric's percent)")
	}
	if len(diffs) > 0 {
		sep := "\n- "
		if !full {
			sep = "; "
		}
		b.WriteString(". Different:" + strings.TrimPrefix(sep, ";") + strings.Join(diffs, sep))
		if compared-matched > len(diffs) {
			fmt.Fprintf(&b, "%sand %d more", sep, compared-matched-len(diffs))
		}
	}
	return b.String(), matched == compared, nil
}

// sheetRow is one row of an attached workbook's sheet or of a CSV file, its
// values as stored.
func sheetRow(filename string, data []byte, sheet string, row int) ([]string, error) {
	if row <= 0 {
		return nil, fmt.Errorf("row is 1-based")
	}
	if strings.HasSuffix(strings.ToLower(filename), ".csv") {
		recs, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
		if err != nil {
			return nil, fmt.Errorf("csv parse failed: %w", err)
		}
		if row > len(recs) {
			return nil, fmt.Errorf("%s has %d rows", filename, len(recs))
		}
		return recs[row-1], nil
	}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("xlsx parse failed: %w", err)
	}
	defer func() { _ = f.Close() }()
	for _, s := range readSheets(f) {
		if strings.EqualFold(s.name, strings.TrimSpace(sheet)) {
			if row > len(s.rows) {
				return nil, fmt.Errorf("sheet %s has %d rows", s.name, len(s.rows))
			}
			return s.rows[row-1], nil
		}
	}
	return nil, fmt.Errorf("no sheet named %q", sheet)
}
