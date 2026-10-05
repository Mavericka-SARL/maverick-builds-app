package importpkg

// File-level helpers shared by every caller that imports a whole attached
// file rather than a CSV body: picking the parser by extension, choosing an
// XLSX sheet by name, and applying a saved column_map (the Import Wizard's
// "file column → model field" record) before ResolveRows or the dimension
// importer see the header.

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/xuri/excelize/v2"
)

// IsTabularFile reports whether a filename is a spreadsheet the import
// pipeline can read.
func IsTabularFile(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".csv", ".xlsx", ".xlsm":
		return true
	}
	return false
}

// ParseTabularFile parses a .csv or .xlsx/.xlsm file into the RawRow shape.
// sheet picks an XLSX worksheet by name (empty = the first); it must be empty
// for a CSV. XLSX cells are read as their stored values, not as displayed:
// a value formatted "#,##0.00" arrives as 1234.5, not "1,234.50", which
// strconv.ParseFloat would reject.
func ParseTabularFile(filename string, data []byte, sheet string) (header []string, rows []RawRow, err error) {
	return ReadShaped(filename, data, sheet, nil)
}

// XLSXSheetNames lists a workbook's worksheets in order.
func XLSXSheetNames(data []byte) ([]string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open xlsx: %w", err)
	}
	defer func() { _ = f.Close() }()
	return f.GetSheetList(), nil
}

func parseXLSXSheet(data []byte, sheet string, rawValues bool) (header []string, rows []RawRow, err error) {
	grid, err := readXLSXGrid(data, sheet, rawValues)
	if err != nil {
		return nil, nil, err
	}
	return ShapeGrid(grid, nil)
}

// ColumnIgnored is the column_map target that drops a file column. An empty
// target means the same.
const ColumnIgnored = "ignore"

// ColumnOthers is the column_map key for every column the map does not name:
// {"*": "ignore"} drops them. Without it a sheet laid out for people — a key
// column, a note, a total beside the columns imported — needed every one of
// them listed as "ignore", and an import naming only the columns it loads
// failed on the first one left out ("Month #" matches no metric).
const ColumnOthers = "*"

// ApplyColumnMap renames a file's columns to the model fields a column_map
// names, in place on rows, and returns the new header. The vocabulary is the
// Import Wizard's: a metric or dimension name; "metric" + "value" for a long
// file (one row per metric value — "metric" holds metric names or ids);
// code / label / parent_code / property:<name> / period_start / period_end
// for a dimension; "ignore" or "" to drop the column. A column the map does
// not mention keeps its own header, or takes the "*" entry's target (only
// "ignore" — ColumnOthers). "metric" is spelled "metric_id" for
// ResolveRows, whose legacy metric_id+value pair accepts names as well as
// ids. Two columns mapped to the same field is an error, not a silent
// last-one-wins.
func ApplyColumnMap(header []string, rows []RawRow, columnMap map[string]string) ([]string, error) {
	if len(columnMap) == 0 {
		return header, nil
	}
	others, hasOthers := columnMap[ColumnOthers]
	if hasOthers && strings.TrimSpace(others) != "" && !strings.EqualFold(strings.TrimSpace(others), ColumnIgnored) {
		return nil, fmt.Errorf(`column_map "*" stands for every column the map does not name and can only be %q`, ColumnIgnored)
	}
	lookup := func(col string) (string, bool) {
		if v, ok := columnMap[col]; ok {
			return v, true
		}
		trimmed := strings.TrimSpace(col)
		if v, ok := columnMap[trimmed]; ok {
			return v, true
		}
		for k, v := range columnMap {
			if strings.EqualFold(strings.TrimSpace(k), trimmed) {
				return v, true
			}
		}
		if hasOthers {
			return ColumnIgnored, true
		}
		return "", false
	}

	out := make([]string, 0, len(header))
	renames := map[string]string{}
	var dropped []string
	usedBy := map[string]string{}
	for _, col := range header {
		target, mapped := lookup(col)
		if !mapped {
			target = col
		}
		target = strings.TrimSpace(target)
		if mapped && (target == "" || strings.EqualFold(target, ColumnIgnored)) {
			dropped = append(dropped, col)
			continue
		}
		switch strings.ToLower(target) {
		case "metric", "metric_name", "metric_id":
			target = "metric_id"
		}
		key := strings.ToLower(target)
		if prev, dup := usedBy[key]; dup && strings.TrimSpace(col) != "" {
			return nil, fmt.Errorf("columns %q and %q both map to %q — map one of them to %q", prev, col, target, ColumnIgnored)
		}
		if strings.TrimSpace(col) != "" {
			usedBy[key] = col
		}
		if target != col {
			renames[col] = target
		}
		out = append(out, target)
	}
	for i := range rows {
		cells := rows[i].Cells
		for _, col := range dropped {
			delete(cells, col)
		}
		if len(renames) == 0 {
			continue
		}
		next := make(map[string]string, len(cells))
		for col, v := range cells {
			if to, ok := renames[col]; ok {
				col = to
			}
			next[col] = v
		}
		rows[i].Cells = next
	}
	return out, nil
}

// UnmatchedColumnMapKeys lists the column_map keys that match no file
// column — usually a typo — so a caller can refuse rather than import with
// part of the mapping silently unused.
func UnmatchedColumnMapKeys(header []string, columnMap map[string]string) []string {
	present := map[string]bool{}
	for _, col := range header {
		present[strings.ToLower(strings.TrimSpace(col))] = true
	}
	var missing []string
	for k := range columnMap {
		if k == ColumnOthers {
			continue
		}
		if !present[strings.ToLower(strings.TrimSpace(k))] {
			missing = append(missing, k)
		}
	}
	return missing
}
