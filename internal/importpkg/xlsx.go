package importpkg

// Native .xlsx parsing for fact import, sharing the exact same RawRow shape
// (and therefore the exact same ResolveRows validation) as CSV — so a
// spreadsheet upload isn't a lesser, differently-validated path: it goes
// through the same name-based column mapping, leaf-member check and number
// check as everything else. excelize is already a direct
// dependency (used by internal/aiassistant for document parsing).

import (
	"fmt"
	"strings"
)

// ParseCSVRows reads header + data rows from CSV text. Rows of a different
// length are read (missing cells are blank); a malformed file is an error
// naming its line. Until 2026-10 the first such row silently ended the
// file, so everything after it was never imported.
func ParseCSVRows(data []byte) (header []string, rows []RawRow, err error) {
	grid, err := readCSVGrid(data, ',')
	if err != nil {
		return nil, nil, err
	}
	if header, rows, err = ShapeGrid(grid, nil); err != nil {
		return nil, nil, err
	}
	return header, rows, RefuseRepeatedColumns(header)
}

// RefuseRepeatedColumns refuses a header naming one column twice (ignoring
// case and surrounding spaces). A row's cells are kept by column name, so
// the second copy replaced the first without a word: half of such a file
// was dropped. A file posted as it is meets this; one read through a saved
// column map is checked there instead (ApplyColumnMap), where a repeated
// column the map ignores is no conflict.
func RefuseRepeatedColumns(header []string) error {
	seen := make(map[string]bool, len(header))
	for _, h := range header {
		key := strings.ToLower(strings.TrimSpace(h))
		if key == "" {
			continue
		}
		if seen[key] {
			return fmt.Errorf("the file has two columns named %q: a cell can come from one of them only — rename or remove the other", strings.TrimSpace(h))
		}
		seen[key] = true
	}
	return nil
}

// ParseXLSXRows reads header + data rows from the first sheet of a native
// Excel workbook. Blank trailing rows (common in exported/templated sheets)
// are skipped rather than staged as empty. Cells are read as displayed,
// except a plain number cell, which is read as stored ("#,##0.00" shows
// "1,234.50"; the number check takes 1234.5) — see storedNumbers.
// ParseTabularFile reads stored values throughout.
func ParseXLSXRows(data []byte) (header []string, rows []RawRow, err error) {
	if header, rows, err = parseXLSXSheet(data, "", false); err != nil {
		return nil, nil, err
	}
	return header, rows, RefuseRepeatedColumns(header)
}

func recordToRawRow(header, record []string, rowNum int) RawRow {
	cells := make(map[string]string, len(header))
	for i, col := range header {
		if i < len(record) {
			cells[col] = record[i]
		}
	}
	return RawRow{RowNumber: rowNum, Cells: cells}
}

func isBlankRecord(record []string) bool {
	for _, v := range record {
		for _, r := range v {
			if r != ' ' && r != '\t' {
				return false
			}
		}
	}
	return true
}
