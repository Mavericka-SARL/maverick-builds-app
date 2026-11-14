package aiassistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/query"
	"github.com/mavericks-engine/mavericks/internal/timedim"
)

// check_sheet (2026-11-14): a sheet compared cell by cell as model
// coordinates. compare_with_sheet paired the n-th column after first_column
// with the n-th month of the time dimension and never read the header: a
// correct Actuals sheet "differed" on all 103 rows because its FY total
// column became January of the next year, a Budget sheet starting in 2027
// was read as 2026, and a TOTAL row could not be compared at all. The AI
// then changed correct data and the check ran again.
//
// A SheetMap is the import's own description of a sheet — header row,
// months turned into rows, constants, columns → dimensions and metrics — so
// the map that loads a sheet is the map that checks it. Every cell becomes a
// coordinate (a metric, one member per dimension, a period at any level) and
// is read from the model the way a chart or a workflow condition reads it
// (query.MetricValuesAtPoints): a leaf at its value, a parent member, a
// blank key or an FY column at the engine's own total. Differences are
// grouped by cause, and a mapping problem is kept apart from a value problem.

// SheetMap is one block of a sheet in the model's terms.
type SheetMap struct {
	// Block names the map among a sheet's maps ("main", "FY totals").
	Block     string             `json:"block,omitempty"`
	Reshape   *importpkg.Reshape `json:"reshape,omitempty"`
	ColumnMap map[string]string  `json:"column_map,omitempty"`
	// Source is "load" for the map an executed import loaded the sheet
	// with, "check" for one given to check_sheet.
	Source string `json:"source,omitempty"`
}

// SheetCheck is what one block's check found.
type SheetCheck struct {
	File, Sheet, Block string
	Compared, Matched  int
	// MappingProblems: the map reads the sheet wrongly — fixed in the map,
	// never in the data.
	MappingProblems []string
	// ValueProblems: the model's values differ — the input or the formula.
	ValueProblems []string
	// NotMapped: numbers on the sheet no block reads.
	NotMapped []string
	Notes     []string
	ReadAs    string
	// Overall is said first when nothing matches.
	Overall string
	// TextOnly: the block's value columns hold text (an owner, a status),
	// no number to compare.
	TextOnly  bool
	prev      *SavedSheetMap // the block's last check, when replayed
	skipped   string         // the map loads no values (members, records)
	notMapped notMappedCells
	cells     []*sheetCell
	dims      map[string]*checkDim
	header    []string
}

// OK is a block with every compared cell matching and nothing mis-mapped.
func (c *SheetCheck) OK() bool {
	return c.skipped == "" && c.Compared > 0 && c.Matched == c.Compared && len(c.MappingProblems) == 0
}

// Skipped says why a block has no values to compare ("" when it has).
func (c *SheetCheck) Skipped() string { return c.skipped }

// sheetCell is one compared cell.
type sheetCell struct {
	row, col  int    // 1-based line, 0-based column of the raw sheet (-1 unknown)
	colHeader string // the column's header
	metric    string
	dims      []string          // the metric's dimensions
	point     map[string]string // dimension ID → member code
	members   map[string]string // dimension name → member code, for messages
	want      float64
	percent   bool
	got       float64
	gotOK     bool
	gotErr    string
	match     bool
}

type checkDim struct {
	id, name    string
	isTime      bool
	granularity string
	members     []checkMember
	byCode      map[string]*checkMember
	byLabel     map[string][]*checkMember
	byParent    map[string][]*checkMember
}

type checkMember struct {
	code, label, parent string
	start, end          *time.Time
	leaf, calculated    bool
}

type checkMetric struct {
	id, name, format string
	dims             []string
}

// loadCheckModel reads the revision's dimensions with their members (labels,
// periods, parents) and metrics with their dimensions.
func loadCheckModel(ctx context.Context, e *ToolExecutor) (map[string]*checkDim, map[string]*checkMetric, error) {
	rows, err := e.pool.Query(ctx, `
		SELECT d.id::text, d.name, d.dimension_type = 'time', COALESCE(d.time_granularity,''),
		       m.code, COALESCE(m.label,''), COALESCE(pm.code,''), m.period_start, m.period_end,
		       NOT EXISTS (SELECT 1 FROM model.dimension_member c WHERE c.parent_member_id = m.id AND c.dimension_id = m.dimension_id),
		       COALESCE(btrim(m.formula),'') <> ''
		FROM model.dimension_def d
		JOIN model.dimension_member m ON m.dimension_id = d.id
		LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE d.model_id = $1::uuid AND (d.revision_id = $2::uuid OR d.revision_id IS NULL)
		ORDER BY d.name, m.time_index NULLS LAST, m.sort_order, m.code`, e.modelID, e.revID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	dims := map[string]*checkDim{}
	for rows.Next() {
		var id, name, gran, code, label, parent string
		var isTime, leaf, calc bool
		var start, end *time.Time
		if err := rows.Scan(&id, &name, &isTime, &gran, &code, &label, &parent, &start, &end, &leaf, &calc); err != nil {
			return nil, nil, err
		}
		d := dims[id]
		if d == nil {
			d = &checkDim{id: id, name: name, isTime: isTime, granularity: gran,
				byCode: map[string]*checkMember{}, byLabel: map[string][]*checkMember{}, byParent: map[string][]*checkMember{}}
			dims[id] = d
		}
		d.members = append(d.members, checkMember{code: code, label: label, parent: parent, start: start, end: end, leaf: leaf, calculated: calc})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, d := range dims {
		for i := range d.members {
			m := &d.members[i]
			d.byCode[strings.ToLower(m.code)] = m
			if m.label != "" {
				d.byLabel[strings.ToLower(strings.TrimSpace(m.label))] = append(d.byLabel[strings.ToLower(strings.TrimSpace(m.label))], m)
			}
			d.byParent[m.parent] = append(d.byParent[m.parent], m)
		}
	}

	store := calculation.NewStore(e.pool)
	defs, err := store.LoadModelMetrics(ctx, e.modelID, e.revID)
	if err != nil {
		return nil, nil, err
	}
	dimIDs, err := store.LoadMetricDimensionIDs(ctx, e.modelID, e.revID)
	if err != nil {
		return nil, nil, err
	}
	labels := map[string][2]string{}
	if lr, err := e.pool.Query(ctx, `SELECT id::text, COALESCE(label,''), COALESCE(format,'') FROM model.metric_def
		WHERE model_id = $1::uuid AND (revision_id = $2::uuid OR revision_id IS NULL)`, e.modelID, e.revID); err == nil {
		for lr.Next() {
			var id, label, format string
			if lr.Scan(&id, &label, &format) == nil {
				labels[id] = [2]string{label, format}
			}
		}
		lr.Close()
	}
	metrics := map[string]*checkMetric{}
	for _, d := range defs {
		m := &checkMetric{id: d.ID, name: d.Name, format: labels[d.ID][1], dims: dimIDs[d.ID]}
		metrics[strings.ToLower(d.Name)] = m
		if l := strings.ToLower(strings.TrimSpace(labels[d.ID][0])); l != "" {
			if _, taken := metrics[l]; !taken {
				metrics[l] = m
			}
		}
	}
	return dims, metrics, nil
}

var monthNames = map[string]int{"jan": 1, "january": 1, "feb": 2, "february": 2, "mar": 3, "march": 3, "apr": 4, "april": 4,
	"may": 5, "jun": 6, "june": 6, "jul": 7, "july": 7, "aug": 8, "august": 8, "sep": 9, "sept": 9, "september": 9,
	"oct": 10, "october": 10, "nov": 11, "november": 11, "dec": 12, "december": 12}

// resolve is the member a sheet value names — by code, by its one label,
// on a time dimension by a date in the period or a month name — any level,
// a parent and an aggregate period included. A blank value is the
// dimension's total (ok, ""). Not found: ok false.
func (d *checkDim) resolve(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", true
	}
	if m := d.byCode[strings.ToLower(v)]; m != nil {
		return m.code, true
	}
	if ms := d.byLabel[strings.ToLower(v)]; len(ms) == 1 {
		return ms[0].code, true
	}
	if !d.isTime {
		return "", false
	}
	if day, ok := timedim.DayOf(v); ok {
		var best *checkMember
		for i := range d.members {
			m := &d.members[i]
			if m.leaf && m.start != nil && m.end != nil && !day.Before(m.start.UTC()) && !day.After(m.end.UTC()) {
				if best == nil || m.end.Sub(*m.start) < best.end.Sub(*best.start) {
					best = m
				}
			}
		}
		if best != nil {
			return best.code, true
		}
	}
	if mon, ok := monthNames[strings.ToLower(strings.TrimSuffix(v, "."))]; ok && d.granularity == "month" {
		var hit *checkMember
		n := 0
		for i := range d.members {
			m := &d.members[i]
			if m.leaf && m.start != nil && int(m.start.Month()) == mon {
				hit = m
				n++
			}
		}
		if n == 1 {
			return hit.code, true
		}
	}
	return "", false
}

// leavesUnder is how many leaf members code covers (itself when a leaf).
func (d *checkDim) leavesUnder(code string) map[string]bool {
	out := map[string]bool{}
	var walk func(c string)
	walk = func(c string) {
		kids := d.byParent[c]
		if len(kids) == 0 {
			out[c] = true
			return
		}
		for _, k := range kids {
			walk(k.code)
		}
	}
	walk(code)
	return out
}

// CheckSheet compares one block of a sheet with the model, cell by cell.
// It reads only.
func (e *ToolExecutor) CheckSheet(ctx context.Context, filename string, data []byte, sheet string, m SheetMap) (*SheetCheck, error) {
	res := &SheetCheck{File: filename, Sheet: strings.TrimSpace(sheet), Block: blockName(m.Block)}
	if len(m.ColumnMap) == 0 {
		return nil, fmt.Errorf("the map needs column_map: which column is which dimension or metric (the import's column_map)")
	}
	rs := importpkg.Reshape{}
	if m.Reshape != nil {
		rs = *m.Reshape
	}
	if err := rs.Validate(); err != nil {
		return nil, err
	}
	// The months-to-rows column keeps each cell's header until its source
	// column is known; that column's value_map is applied after.
	var nameMap map[string]string
	nameCol := ""
	if rs.Unpivot != nil {
		nameCol = strings.TrimSpace(rs.Unpivot.NameColumn)
		if len(rs.ValueMap) > 0 {
			vm := importpkg.ValueMaps{}
			for col, mm := range rs.ValueMap {
				if strings.EqualFold(strings.TrimSpace(col), nameCol) {
					nameMap = map[string]string{}
					for from, to := range mm {
						nameMap[strings.ToLower(strings.TrimSpace(from))] = to
					}
					continue
				}
				vm[col] = mm
			}
			rs.ValueMap = vm
		}
	}
	delim, err := rs.Comma()
	if err != nil {
		return nil, err
	}
	grid, err := importpkg.ReadGrid(filename, data, sheet, delim)
	if err != nil {
		return nil, err
	}
	header, rows, _, err := importpkg.ShapeGridStats(grid, &rs)
	if err != nil {
		return nil, err
	}
	hRow := rs.HeaderRow
	if hRow == 0 {
		hRow = 1
		for hRow < len(grid) && blankRecord(grid[hRow-1]) {
			hRow++
		}
	}
	rawHeader := grid[hRow-1]
	colOf := func(h string) int {
		h = strings.TrimSpace(h)
		for i, c := range rawHeader {
			if strings.TrimSpace(c) == h {
				return i
			}
		}
		for i, c := range rawHeader {
			if strings.EqualFold(strings.TrimSpace(c), h) {
				return i
			}
		}
		return -1
	}
	// Where each row's months-to-rows cell came from, then its value_map.
	srcCol := make([]int, len(rows))
	srcHead := make([]string, len(rows))
	for i := range rows {
		srcCol[i] = -1
		if nameCol == "" {
			continue
		}
		for k, v := range rows[i].Cells {
			if strings.EqualFold(strings.TrimSpace(k), nameCol) {
				srcHead[i] = v
				srcCol[i] = colOf(v)
				if to, ok := nameMap[strings.ToLower(strings.TrimSpace(v))]; ok {
					rows[i].Cells[k] = to
				}
			}
		}
	}
	// What each target column was in the file: a row of the columns' own
	// names, mapped the same way.
	probe := []importpkg.RawRow{{Cells: map[string]string{}}}
	for _, h := range header {
		probe[0].Cells[h] = h
	}
	if _, err := importpkg.ApplyColumnMapTo(append([]string(nil), header...), probe, m.ColumnMap, "grid"); err != nil {
		return nil, err
	}
	origOf := probe[0].Cells
	targets, err := importpkg.ApplyColumnMapTo(header, rows, m.ColumnMap, "grid")
	if err != nil {
		return nil, err
	}

	dims, metrics, err := loadCheckModel(ctx, e)
	if err != nil {
		return nil, err
	}
	dimByName := map[string]*checkDim{}
	for _, d := range dims {
		dimByName[strings.ToLower(d.name)] = d
	}
	type valueCol struct {
		target string
		metric *checkMetric
	}
	var dimCols []string
	dimOfCol := map[string]*checkDim{}
	var valueCols []valueCol
	metricCol, longValue := "", ""
	usedCols := map[int]bool{}
	var describe []string
	for _, t := range targets {
		lt := strings.ToLower(strings.TrimSpace(t))
		orig := origOf[t]
		switch {
		case lt == "metric_id":
			metricCol = t
			usedCols[colOf(orig)] = true
			describe = append(describe, fmt.Sprintf("metric ← %q", orig))
		case lt == "value":
			longValue = t
			usedCols[colOf(orig)] = true
		case dimByName[lt] != nil:
			d := dimByName[lt]
			dimCols = append(dimCols, t)
			dimOfCol[t] = d
			if c := colOf(orig); c >= 0 {
				usedCols[c] = true
				describe = append(describe, fmt.Sprintf("%s ← column %q", d.name, orig))
			} else if strings.EqualFold(strings.TrimSpace(orig), nameCol) {
				describe = append(describe, fmt.Sprintf("%s ← the headers of %s", d.name, unpivotSpan(rs, colOf)))
			} else {
				describe = append(describe, fmt.Sprintf("%s = %s", d.name, constantOf(rs, orig)))
			}
		case metrics[lt] != nil:
			valueCols = append(valueCols, valueCol{t, metrics[lt]})
			if c := colOf(orig); c >= 0 {
				usedCols[c] = true
				describe = append(describe, fmt.Sprintf("%s ← column %q", metrics[lt].name, orig))
			} else {
				describe = append(describe, fmt.Sprintf("%s ← the cells of %s", metrics[lt].name, unpivotSpan(rs, colOf)))
			}
		case lt == "code" || lt == "label" || lt == "parent_code" || strings.HasPrefix(lt, "property:") || lt == "period_start" || lt == "period_end":
			res.skipped = "this map loads a dimension's members, not values"
			return res, nil
		default:
			res.MappingProblems = append(res.MappingProblems, fmt.Sprintf("column %q maps to %q, which is no metric or dimension of this revision", orig, t))
		}
	}
	if rs.Unpivot != nil {
		cols := rs.Unpivot.Columns
		from, to := colOf(rs.Unpivot.From), colOf(rs.Unpivot.To)
		if len(cols) == 0 && from >= 0 && to >= from {
			for c := from; c <= to; c++ {
				usedCols[c] = true
			}
		}
		for _, c := range cols {
			usedCols[colOf(c)] = true
		}
	}
	if metricCol != "" && longValue == "" {
		res.MappingProblems = append(res.MappingProblems, `a "metric" column needs its "value" column`)
	}
	if len(valueCols) == 0 && metricCol == "" {
		res.MappingProblems = append(res.MappingProblems, "no column maps to a metric: nothing to compare")
		return res, nil
	}
	res.ReadAs = strings.Join(describe, ", ")

	// Every cell as a coordinate.
	unknown := map[string]map[string]int{} // dimension → value → cells
	leftOut := map[string]map[string]bool{}
	var cells []*sheetCell
	usedRows := map[int]bool{}
	text := 0
	decimalComma := rs.DecimalComma
	addCell := func(i int, metric *checkMetric, raw string, col int, colHeader string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		want, ok := importpkg.ParseHumanNumber(raw, decimalComma)
		if !ok {
			text++
			return
		}
		usedRows[rows[i].RowNumber] = true
		c := &sheetCell{row: rows[i].RowNumber, col: col, colHeader: colHeader, metric: metric.name, want: want, dims: metric.dims,
			percent: metric.format == "percentage", point: map[string]string{}, members: map[string]string{}}
		for _, dimID := range metric.dims {
			d := dims[dimID]
			if d == nil {
				continue
			}
			col := ""
			for _, dc := range dimCols {
				if dimOfCol[dc] == d {
					col = dc
				}
			}
			if col == "" {
				if leaves := 0; true {
					for _, m := range d.members {
						if m.leaf {
							leaves++
						}
					}
					if leaves <= 1 {
						continue // its total is its one member
					}
				}
				if leftOut[metric.name] == nil {
					leftOut[metric.name] = map[string]bool{}
				}
				leftOut[metric.name][d.name] = true
				continue
			}
			code, found := d.resolve(rows[i].Cells[col])
			if !found {
				if unknown[d.name] == nil {
					unknown[d.name] = map[string]int{}
				}
				unknown[d.name][strings.TrimSpace(rows[i].Cells[col])]++
				return
			}
			if code != "" {
				c.point[dimID] = code
				c.members[d.name] = code
			}
		}
		cells = append(cells, c)
	}
	for i := range rows {
		for _, vc := range valueCols {
			col, head := srcCol[i], srcHead[i]
			if nameCol == "" || col < 0 {
				col, head = colOf(origOf[vc.target]), origOf[vc.target]
			}
			addCell(i, vc.metric, rows[i].Cells[vc.target], col, head)
		}
		if metricCol != "" && longValue != "" {
			name := strings.ToLower(strings.TrimSpace(rows[i].Cells[metricCol]))
			if name == "" {
				continue
			}
			mt := metrics[name]
			if mt == nil {
				if unknown["metric"] == nil {
					unknown["metric"] = map[string]int{}
				}
				unknown["metric"][rows[i].Cells[metricCol]]++
				continue
			}
			addCell(i, mt, rows[i].Cells[longValue], srcCol[i], srcHead[i])
		}
	}
	for dim, vals := range unknown {
		var parts []string
		for v, n := range vals {
			parts = append(parts, fmt.Sprintf("%q (%d cells)", v, n))
		}
		sort.Strings(parts)
		if len(parts) > 6 {
			parts = append(parts[:6], fmt.Sprintf("and %d more", len(parts)-6))
		}
		what := "is not a member of " + dim
		if dim == "metric" {
			what = "is no metric of this revision"
		}
		res.MappingProblems = append(res.MappingProblems, fmt.Sprintf("%s %s — map it to a member (value_map) or leave the row out (skip_rows)", strings.Join(parts, ", "), what))
	}
	for metric, ds := range leftOut {
		var names []string
		for n := range ds {
			names = append(names, n)
		}
		sort.Strings(names)
		res.Notes = append(res.Notes, fmt.Sprintf("%s is read at the total of %s: the map names no member of it (a constant, e.g. \"Version\": \"ACTUAL\", pins it)", metric, strings.Join(names, ", ")))
	}

	// The model at every coordinate, one read per metric.
	byMetric := map[string][]*sheetCell{}
	for _, c := range cells {
		byMetric[c.metric] = append(byMetric[c.metric], c)
	}
	resolver := query.NewChartResolver(e.pool)
	for name, cs := range byMetric {
		points := make([]map[string]string, len(cs))
		for i, c := range cs {
			points[i] = c.point
		}
		vals, err := resolver.MetricValuesAtPoints(ctx, e.modelID, e.revID, name, points)
		if err != nil {
			return nil, err
		}
		for i, c := range cs {
			v := vals[i]
			switch {
			case v.IsError():
				c.gotErr = v.Err().Message
			case v.IsBlank():
			default:
				if n, ok := v.Number(); ok {
					c.got, c.gotOK = n, true
				} else {
					c.gotErr = "the model holds " + v.String()
				}
			}
			c.match = cellMatches(c)
		}
	}
	res.Compared = len(cells)
	for _, c := range cells {
		if c.match {
			res.Matched++
		}
	}
	if text > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("%d text cell(s) under value columns were not compared", text))
	}
	if res.Compared == 0 && len(res.MappingProblems) == 0 && text > 0 {
		res.TextOnly = true
		return res, nil
	}
	res.notMapped = notMappedOf(grid, hRow, usedRows, usedCols)
	res.cells, res.dims, res.header = cells, dims, rawHeader
	res.NotMapped = res.notMapped.describe(rawHeader, cells, dims)
	res.groupCauses(cells, filename, data, sheet)
	return res, nil
}

func blankRecord(r []string) bool {
	for _, c := range r {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

func blockName(b string) string {
	if b = strings.TrimSpace(b); b == "" {
		return "main"
	}
	return b
}

func constantOf(rs importpkg.Reshape, col string) string {
	for k, v := range rs.Constants {
		if strings.EqualFold(strings.TrimSpace(k), strings.TrimSpace(col)) {
			return v
		}
	}
	return "column " + strconv.Quote(col)
}

// cellMatches: equal within 0.05% of the sheet's value; a percentage metric
// also matches the sheet's fraction; 0 on the sheet matches nothing in the
// model.
func cellMatches(c *sheetCell) bool {
	if c.gotErr != "" {
		return false
	}
	if !c.gotOK {
		return c.want == 0
	}
	eq := func(a, b float64) bool { return math.Abs(a-b) <= 0.0005*math.Max(1, math.Abs(b)) }
	return eq(c.got, c.want) || (c.percent && eq(c.got, c.want*100))
}

// notMappedCells are numbers of the sheet below its header no block reads.
type notMappedCells struct {
	rows map[int][]int // a row no cell was read from → its number columns
	cols map[int][]int // a column of read rows that was not read → rows
	grid [][]string
}

func notMappedOf(grid [][]string, hRow int, usedRows map[int]bool, usedCols map[int]bool) notMappedCells {
	nm := notMappedCells{rows: map[int][]int{}, cols: map[int][]int{}, grid: grid}
	for r := hRow + 1; r <= len(grid); r++ {
		for c, v := range grid[r-1] {
			if _, ok := importpkg.ParseHumanNumber(strings.TrimSpace(v), false); !ok || strings.TrimSpace(v) == "" {
				continue
			}
			switch {
			case !usedRows[r]:
				nm.rows[r] = append(nm.rows[r], c)
			case !usedCols[c]:
				nm.cols[c] = append(nm.cols[c], r)
			}
		}
	}
	return nm
}

// sheetRowLabel is a sheet row's words: its last text cell in words, not a
// code ("Salaries & wages"), else its last text cell ("TOTAL OPEX").
func sheetRowLabel(grid [][]string, row int) string {
	if row < 1 || row > len(grid) {
		return ""
	}
	label, words := "", ""
	for _, v := range grid[row-1] {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			label = v
			if strings.ContainsAny(v, " abcdefghijklmnopqrstuvwxyz") {
				words = v // words, not a code
			}
		}
	}
	if words != "" {
		label = words
	}
	if r := []rune(label); len(r) > 48 {
		label = string(r[:47]) + "…"
	}
	return label
}

// describe lists what no block reads, a total column named with the period
// that holds the columns before it.
func (nm notMappedCells) describe(header []string, cells []*sheetCell, dims map[string]*checkDim) []string {
	var out []string
	cols := make([]int, 0, len(nm.cols))
	for c := range nm.cols {
		cols = append(cols, c)
	}
	sort.Ints(cols)
	for _, c := range cols {
		h := ""
		if c < len(header) {
			h = strings.TrimSpace(header[c])
		}
		line := fmt.Sprintf("column %s %q (%d numbers)", colLetter(c), h, len(nm.cols[c]))
		if total := periodBefore(c, cells, dims); total != "" {
			line += fmt.Sprintf(" — beside it are the periods of %s: if it is their total, map its header to %s (value_map) and it is compared with the model's own total", total, total)
		}
		out = append(out, line)
	}
	rows := make([]int, 0, len(nm.rows))
	for r := range nm.rows {
		rows = append(rows, r)
	}
	sort.Ints(rows)
	for _, r := range rows {
		out = append(out, fmt.Sprintf("row %d %q (%d numbers)", r, sheetRowLabel(nm.grid, r), len(nm.rows[r])))
	}
	return out
}

// periodBefore is the period whose leaves are exactly the periods of the
// columns just before column c (a run of read columns), or "".
func periodBefore(c int, cells []*sheetCell, dims map[string]*checkDim) string {
	var td *checkDim
	for _, d := range dims {
		if d.isTime {
			if td != nil {
				return "" // more than one time dimension: not guessed
			}
			td = d
		}
	}
	if td == nil {
		return ""
	}
	periodAt := map[int]string{}
	for _, cell := range cells {
		if code, ok := cell.point[td.id]; ok && cell.col >= 0 {
			periodAt[cell.col] = code
		}
	}
	run := map[string]bool{}
	for k := c - 1; k >= 0; k-- {
		p, ok := periodAt[k]
		if !ok {
			break
		}
		run[p] = true
	}
	if len(run) < 2 {
		return ""
	}
	// The smallest member covering exactly the run.
	for i := range td.members {
		m := &td.members[i]
		if m.leaf {
			continue
		}
		under := td.leavesUnder(m.code)
		if len(under) != len(run) {
			continue
		}
		same := true
		for p := range run {
			same = same && under[p]
		}
		if same {
			return m.code
		}
	}
	return ""
}

func colLetter(c int) string {
	name, err := excelize.ColumnNumberToName(c + 1)
	if err != nil {
		return strconv.Itoa(c + 1)
	}
	return name
}

// groupCauses sorts the differing cells by what most likely caused them: a
// whole column, every cell of one member, a scale, a row, single cells.
func (res *SheetCheck) groupCauses(cells []*sheetCell, filename string, data []byte, sheet string) {
	var diff []*sheetCell
	for _, c := range cells {
		if !c.match {
			diff = append(diff, c)
		}
	}
	if len(diff) == 0 {
		return
	}
	// A total that differs only through the differing cells under it is
	// their consequence, not a problem of its own: an FY column, a
	// subtotal, a TOTAL row. Only the cells that differ by themselves are
	// sorted into causes.
	follows := res.followers(diff)
	if len(follows) > 0 {
		var roots []*sheetCell
		for _, c := range diff {
			if !follows[c] {
				roots = append(roots, c)
			}
		}
		diff = roots
		res.Notes = append(res.Notes, fmt.Sprintf("%d total cell(s) — FY columns, subtotals, TOTAL rows — differ only through the differing cells under them, and are not listed", len(follows)))
	}
	taken := map[*sheetCell]bool{}
	for c := range follows {
		taken[c] = true
	}
	pattern := func(cs []*sheetCell) string {
		blank, errs := 0, 0
		ratio, same := 0.0, true
		for i, c := range cs {
			switch {
			case c.gotErr != "":
				errs++
			case !c.gotOK:
				blank++
			case c.want != 0:
				r := c.got / c.want
				if i == 0 || ratio == 0 {
					ratio = r
				} else if math.Abs(r-ratio) > 1e-6*math.Max(1, math.Abs(ratio)) {
					same = false
				}
			default:
				same = false
			}
		}
		switch {
		case errs == len(cs):
			return "the model cannot compute them: " + cs[0].gotErr
		case blank == len(cs):
			return "the model has no value at any of them — the map points at a coordinate the data is not at, or it was never loaded"
		case same && blank == 0 && errs == 0 && ratio != 0:
			for _, k := range []float64{100, 0.01, 1000, 0.001, 1e6, 1e-6, -1} {
				if math.Abs(ratio-k) <= 1e-6*math.Abs(k) {
					return fmt.Sprintf("the model is %g × the sheet — a scale or sign (reshape.scale, the metric's format)", k)
				}
			}
		}
		return ""
	}

	// A whole column: every cell of it differs, while other columns match.
	byCol := map[string][]*sheetCell{}
	okCol := map[string]bool{}
	for _, c := range cells {
		byCol[c.colHeader] = append(byCol[c.colHeader], c)
		if c.match {
			okCol[c.colHeader] = true
		}
	}
	if len(byCol) > 1 {
		heads := make([]string, 0, len(byCol))
		for h := range byCol {
			heads = append(heads, h)
		}
		sort.Strings(heads)
		for _, h := range heads {
			var cs []*sheetCell
			for _, c := range byCol[h] {
				if !taken[c] {
					cs = append(cs, c)
				}
			}
			if okCol[h] || len(cs) < 2 || len(okCol) == 0 {
				continue
			}
			line := fmt.Sprintf("column %q: all %d cells differ, read as %s", h, len(cs), coord(cs[0]))
			if p := pattern(cs); p != "" {
				line += " — " + p
			}
			res.MappingProblems = append(res.MappingProblems, line+" ("+examples(cs, 2)+")")
			for _, c := range cs {
				taken[c] = true
			}
		}
	}
	// Every cell of one member differs, while its other members match.
	type mk struct{ dim, code string }
	byMember := map[mk][]*sheetCell{}
	okMember := map[mk]bool{}
	membersOf := map[string]map[string]bool{}
	for _, c := range cells {
		for dim, code := range c.members {
			k := mk{dim, code}
			byMember[k] = append(byMember[k], c)
			if c.match {
				okMember[k] = true
			}
			if membersOf[dim] == nil {
				membersOf[dim] = map[string]bool{}
			}
			membersOf[dim][code] = true
		}
	}
	// A member is a cause only beside members of its dimension that match:
	// when nothing matches, every member "differs" and says nothing.
	matchingMember := map[string]bool{}
	for k, ok := range okMember {
		if ok {
			matchingMember[k.dim] = true
		}
	}
	keys := make([]mk, 0, len(byMember))
	for k := range byMember {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].dim != keys[j].dim {
			return keys[i].dim < keys[j].dim
		}
		return keys[i].code < keys[j].code
	})
	for _, k := range keys {
		var cs []*sheetCell
		for _, c := range byMember[k] {
			if !taken[c] {
				cs = append(cs, c)
			}
		}
		if okMember[k] || len(cs) < 4 || len(membersOf[k.dim]) < 3 || !matchingMember[k.dim] {
			continue
		}
		// Only a member most rows share is a mapping cause: one row's own
		// member is the row's problem.
		rowsOf := map[int]bool{}
		for _, c := range cs {
			rowsOf[c.row] = true
		}
		if len(rowsOf) < 2 {
			continue
		}
		line := fmt.Sprintf("every cell at %s %s differs (%d cells, rows %s)", k.dim, k.code, len(cs), rowList(rowsOf))
		if p := pattern(cs); p != "" {
			line += " — " + p
		}
		res.MappingProblems = append(res.MappingProblems, line)
		for _, c := range cs {
			taken[c] = true
		}
	}
	// One scale across everything left: a mapping/format cause.
	var rest []*sheetCell
	for _, c := range diff {
		if !taken[c] {
			rest = append(rest, c)
		}
	}
	if len(rest) >= 4 {
		if p := pattern(rest); strings.Contains(p, "×") {
			res.MappingProblems = append(res.MappingProblems, fmt.Sprintf("%d cells: %s (%s)", len(rest), p, examples(rest, 2)))
			return
		}
	}
	if res.Matched == 0 && len(rest) >= 10 {
		p := pattern(rest)
		if p == "" {
			p = "the model's values differ everywhere"
		}
		res.Overall = fmt.Sprintf("Nothing matches (%s). Check the map's constants and periods first — a wrong version, scenario or year moves every cell; "+
			"if they are right, the model does not hold the sheet's values yet: the rows below show where to start", p)
	}
	// Rows: the value problems, each with where the sheet's number comes from.
	formulas := formulaReader(filename, data, sheet)
	byRow := map[int][]*sheetCell{}
	compRow := map[int]int{}
	for _, c := range cells {
		if !follows[c] {
			compRow[c.row]++ // a row's own cells; its totals follow them
		}
	}
	for _, c := range rest {
		byRow[c.row] = append(byRow[c.row], c)
	}
	rowNums := make([]int, 0, len(byRow))
	for r := range byRow {
		rowNums = append(rowNums, r)
	}
	sort.Ints(rowNums)
	kindOf := func(cs []*sheetCell) string {
		if formulas == nil {
			return ""
		}
		f, in := 0, 0
		for _, c := range cs {
			if c.col < 0 {
				continue
			}
			if formulas(c.col, c.row) {
				f++
			} else {
				in++
			}
		}
		switch {
		case f > 0 && in == 0:
			return " — formula cells in the sheet: compare the model's formula with the Excel formula"
		case in > 0 && f == 0:
			return " — input cells in the sheet: check what was loaded there"
		case f > 0:
			return " — input and formula cells"
		}
		return ""
	}
	// Rows alike are said once: each a constant multiple of the model (a
	// factor the model leaves out — a growth rate, a price), or each with
	// no value in the model.
	var ratioRows, blankRows []int
	ratioMin, ratioMax := math.Inf(1), math.Inf(-1)
	for _, r := range rowNums {
		cs := byRow[r]
		if len(cs) < 2 || len(cs) != compRow[r] {
			continue
		}
		blank, ratio, same := 0, 0.0, true
		for i, c := range cs {
			switch {
			case c.gotErr != "":
				same = false
			case !c.gotOK:
				blank++
			case c.got == 0 || c.want == 0:
				same = false
			case i == 0 || ratio == 0:
				ratio = c.want / c.got
			case math.Abs(c.want/c.got-ratio) > 1e-6*math.Abs(ratio):
				same = false
			}
		}
		switch {
		case blank == len(cs):
			blankRows = append(blankRows, r)
		case same && blank == 0 && ratio != 0:
			ratioRows = append(ratioRows, r)
			ratioMin, ratioMax = math.Min(ratioMin, ratio), math.Max(ratioMax, ratio)
		}
	}
	said := map[int]bool{}
	group := func(rows []int, what string) {
		if len(rows) < 3 {
			return
		}
		var cs []*sheetCell
		set := map[int]bool{}
		for _, r := range rows {
			cs = append(cs, byRow[r]...)
			set[r] = true
			said[r] = true
		}
		res.ValueProblems = append(res.ValueProblems, fmt.Sprintf("%d rows (%s): %s%s (row %d: %s)",
			len(rows), rowList(set), what, kindOf(cs), rows[0], examples(byRow[rows[0]], 2)))
	}
	if len(ratioRows) >= 3 {
		span := "×" + fmtNum(ratioMin)
		if ratioMax-ratioMin > 1e-9 {
			span += " to ×" + fmtNum(ratioMax)
		}
		group(ratioRows, fmt.Sprintf("in each row the sheet is a constant multiple of the model (%s) — the model leaves out a factor the sheet applies (a growth rate, a price, a scale)", span))
	}
	group(blankRows, "the model has no value in any of their cells — not loaded or not calculated there")
	for _, r := range rowNums {
		if said[r] {
			continue
		}
		cs := byRow[r]
		res.ValueProblems = append(res.ValueProblems, fmt.Sprintf("row %d %q (%s): %d of %d cells differ%s (%s)",
			r, sheetRowLabel(res.notMapped.grid, r), coordNoTime(cs[0], res.dims), len(cs), compRow[r], kindOf(cs), examples(cs, 3)))
	}
}

// coordNoTime is a cell's coordinate without its period: what its row is.
func coordNoTime(c *sheetCell, dims map[string]*checkDim) string {
	parts := []string{c.metric}
	keys := make([]string, 0, len(c.point))
	for id := range c.point {
		if d := dims[id]; d != nil && !d.isTime {
			keys = append(keys, d.name+" "+c.point[id])
		}
	}
	sort.Strings(keys)
	return strings.Join(append(parts, keys...), ", ")
}

// fmtNum writes a number as people read it: at most four decimals.
func fmtNum(v float64) string {
	return strconv.FormatFloat(math.Round(v*1e4)/1e4, 'f', -1, 64)
}

// coord is a cell's coordinate in words.
func coord(c *sheetCell) string {
	keys := make([]string, 0, len(c.members))
	for k := range c.members {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{c.metric}
	for _, k := range keys {
		parts = append(parts, k+" "+c.members[k])
	}
	return strings.Join(parts, ", ")
}

func examples(cs []*sheetCell, n int) string {
	var parts []string
	for i, c := range cs {
		if i == n {
			parts = append(parts, fmt.Sprintf("and %d more", len(cs)-n))
			break
		}
		model := "no value"
		switch {
		case c.gotErr != "":
			model = c.gotErr
		case c.gotOK:
			model = fmtNum(c.got)
		}
		at := c.colHeader
		if at == "" {
			at = "row " + strconv.Itoa(c.row)
		} else if len(cs) > 1 && cs[0].colHeader == cs[len(cs)-1].colHeader {
			at = "row " + strconv.Itoa(c.row)
		}
		parts = append(parts, fmt.Sprintf("%s: sheet %s, model %s", at, fmtNum(c.want), model))
	}
	return strings.Join(parts, "; ")
}

func rowList(rows map[int]bool) string {
	ns := make([]int, 0, len(rows))
	for r := range rows {
		ns = append(ns, r)
	}
	sort.Ints(ns)
	return intList(ns, 8)
}

// formulaReader reports whether a workbook cell holds a formula; nil for a
// CSV file or a workbook that cannot be opened.
func formulaReader(filename string, data []byte, sheet string) func(col, row int) bool {
	if strings.HasSuffix(strings.ToLower(filename), ".csv") {
		return nil
	}
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	name := ""
	for _, s := range f.GetSheetList() {
		if strings.EqualFold(s, strings.TrimSpace(sheet)) {
			name = s
		}
	}
	if name == "" {
		return nil
	}
	return func(col, row int) bool {
		axis, err := excelize.CoordinatesToCellName(col+1, row)
		if err != nil {
			return false
		}
		fm, err := f.GetCellFormula(name, axis)
		return err == nil && fm != ""
	}
}

// Render is the block's check for the AI and the developer.
func (res *SheetCheck) Render() string {
	var b strings.Builder
	where := fmt.Sprintf("%q", res.Sheet)
	if res.Sheet == "" {
		where = fmt.Sprintf("%q", res.File)
	}
	if res.skipped != "" {
		fmt.Fprintf(&b, "%s block %q: not compared — %s.", where, res.Block, res.skipped)
		return b.String()
	}
	if res.TextOnly {
		fmt.Fprintf(&b, "%s block %q: its columns hold text, not numbers — nothing to compare by value.", where, res.Block)
		return b.String()
	}
	fmt.Fprintf(&b, "%s block %q: %d cell(s) compared — %d match", where, res.Block, res.Compared, res.Matched)
	if d := res.Compared - res.Matched; d > 0 {
		fmt.Fprintf(&b, ", %d differ", d)
	}
	b.WriteString(".")
	if res.ReadAs != "" {
		b.WriteString("\nRead as: " + res.ReadAs + ".")
	}
	list := func(title string, lines []string, limit int) {
		if len(lines) == 0 {
			return
		}
		b.WriteString("\n" + title)
		for i, l := range lines {
			if i == limit {
				fmt.Fprintf(&b, "\n- and %d more", len(lines)-limit)
				break
			}
			b.WriteString("\n- " + l)
		}
	}
	if res.Overall != "" {
		b.WriteString("\n" + res.Overall + ".")
	}
	list("Mapping problems — the map reads the sheet wrongly; fix the map (check_sheet with map), never the model's data:", res.MappingProblems, 8)
	list("Value problems — the model differs from the sheet:", res.ValueProblems, 15)
	list("Not in this block (not compared):", res.NotMapped, 8)
	for _, n := range res.Notes {
		b.WriteString("\nNote: " + n + ".")
	}
	return b.String()
}

// MapJSON is the map as saved.
func (m SheetMap) MapJSON() []byte {
	b, _ := json.Marshal(m)
	return b
}

// CombineCoverage makes the sheet's blocks report together what none of
// them reads: a number is "not in the map" only when every block of its
// sheet leaves it out (a block of FY totals reads the column the month
// block does not). It keeps the list on the first block of each sheet.
func CombineCoverage(checks []*SheetCheck) {
	bySheet := map[string][]*SheetCheck{}
	var order []string
	for _, c := range checks {
		if c.skipped != "" || c.notMapped.grid == nil {
			continue
		}
		k := strings.ToLower(c.File) + "\x00" + strings.ToLower(c.Sheet)
		if bySheet[k] == nil {
			order = append(order, k)
		}
		bySheet[k] = append(bySheet[k], c)
	}
	for _, k := range order {
		cs := bySheet[k]
		if len(cs) < 2 {
			continue
		}
		type rc struct{ r, c int }
		left := func(nm notMappedCells) map[rc]bool {
			out := map[rc]bool{}
			for r, cols := range nm.rows {
				for _, c := range cols {
					out[rc{r, c}] = true
				}
			}
			for c, rows := range nm.cols {
				for _, r := range rows {
					out[rc{r, c}] = true
				}
			}
			return out
		}
		common := left(cs[0].notMapped)
		var cells []*sheetCell
		usedRows := map[int]bool{}
		for _, c := range cs {
			cells = append(cells, c.cells...)
			for _, cell := range c.cells {
				usedRows[cell.row] = true
			}
			if c == cs[0] {
				continue
			}
			other := left(c.notMapped)
			for x := range common {
				if !other[x] {
					delete(common, x)
				}
			}
		}
		nm := notMappedCells{rows: map[int][]int{}, cols: map[int][]int{}, grid: cs[0].notMapped.grid}
		for x := range common {
			if usedRows[x.r] {
				nm.cols[x.c] = append(nm.cols[x.c], x.r)
			} else {
				nm.rows[x.r] = append(nm.rows[x.r], x.c)
			}
		}
		cs[0].NotMapped = nm.describe(cs[0].header, cells, cs[0].dims)
		for _, c := range cs[1:] {
			c.NotMapped = nil
		}
	}
}

// unpivotSpan names the columns an unpivot turns into rows ("columns I–U").
func unpivotSpan(rs importpkg.Reshape, colOf func(string) int) string {
	u := rs.Unpivot
	if u == nil {
		return "the columns turned into rows"
	}
	if len(u.Columns) > 0 {
		var ls []string
		for _, c := range u.Columns {
			if i := colOf(c); i >= 0 {
				ls = append(ls, colLetter(i))
			}
		}
		return "columns " + strings.Join(ls, ", ")
	}
	from, to := colOf(u.From), colOf(u.To)
	if from < 0 || to < 0 {
		return "the columns turned into rows"
	}
	return fmt.Sprintf("columns %s–%s", colLetter(from), colLetter(to))
}

// Metrics are the metrics the block compared.
func (c *SheetCheck) Metrics() []string {
	seen := map[string]bool{}
	var out []string
	for _, cell := range c.cells {
		if !seen[cell.metric] {
			seen[cell.metric] = true
			out = append(out, cell.metric)
		}
	}
	return out
}

// Line is the block in one line when it is in order, else its Render.
func (c *SheetCheck) Line() string {
	if c.OK() && len(c.NotMapped) == 0 {
		where := c.Sheet
		if where == "" {
			where = c.File
		}
		return fmt.Sprintf("%q block %q: %d cells — all match.", where, c.Block, c.Compared)
	}
	return c.Render()
}

// aggregate reports whether a cell stands at a total: a parent member, an
// aggregate period, or a dimension of its metric left out.
func (res *SheetCheck) aggregate(c *sheetCell) bool {
	for _, id := range c.dims {
		d := res.dims[id]
		if d == nil {
			continue
		}
		code, ok := c.point[id]
		if !ok {
			if len(d.leavesUnder("")) > 1 {
				return true
			}
			continue
		}
		if m := d.byCode[strings.ToLower(code)]; m != nil && !m.leaf {
			return true
		}
	}
	return false
}

// followers are the differing total cells whose difference is the sum of
// the differences of the differing leaf cells under them.
func (res *SheetCheck) followers(diff []*sheetCell) map[*sheetCell]bool {
	out := map[*sheetCell]bool{}
	type dc struct{ dim, code string }
	under := map[dc]map[string]bool{}
	covers := func(dim, agg, leaf string) bool {
		if agg == leaf {
			return true
		}
		k := dc{dim, agg}
		set, ok := under[k]
		if !ok {
			if d := res.dims[dim]; d != nil {
				set = d.leavesUnder(agg)
			}
			under[k] = set
		}
		return set[leaf]
	}
	delta := func(c *sheetCell) float64 {
		if c.gotOK {
			return c.got - c.want
		}
		return -c.want
	}
	var leaves, aggs []*sheetCell
	for _, c := range diff {
		if c.gotErr != "" {
			continue
		}
		if res.aggregate(c) {
			aggs = append(aggs, c)
		} else {
			leaves = append(leaves, c)
		}
	}
	for _, a := range aggs {
		sum, n := 0.0, 0
		for _, l := range leaves {
			if l.metric != a.metric {
				continue
			}
			in := true
			for dim, code := range a.point {
				if !covers(dim, code, l.point[dim]) {
					in = false
					break
				}
			}
			if in {
				sum += delta(l)
				n++
			}
		}
		if n > 0 && math.Abs(sum-delta(a)) <= 0.0005*math.Max(1, math.Abs(a.want)) {
			out[a] = true
		}
	}
	return out
}

// Unchanged reports whether a replayed block matches as it did at its last
// check in revID: nothing new to say about it.
func (c *SheetCheck) Unchanged(revID string) bool {
	p := c.prev
	return p != nil && p.CheckedAt != nil && p.Revision == revID && c.OK() &&
		p.Compared == c.Compared && p.Matched == c.Matched && p.MappingProblems == 0
}
