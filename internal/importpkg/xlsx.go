package importpkg

// Native .xlsx parsing for fact import, sharing the exact same RawRow shape
// (and therefore the exact same ResolveRows validation) as CSV — so a
// spreadsheet upload isn't a lesser, differently-validated path: it goes
// through the same name-based column mapping, leaf-member check and number
// check as everything else. excelize is already a direct
// dependency (used by internal/aiassistant for document parsing).

// ParseCSVRows reads header + data rows from CSV text. Rows of a different
// length are read (missing cells are blank); a malformed file is an error
// naming its line. Until 2026-10 the first such row silently ended the
// file, so everything after it was never imported.
func ParseCSVRows(data []byte) (header []string, rows []RawRow, err error) {
	grid, err := readCSVGrid(data, ',')
	if err != nil {
		return nil, nil, err
	}
	return ShapeGrid(grid, nil)
}

// ParseXLSXRows reads header + data rows from the first sheet of a native
// Excel workbook. Blank trailing rows (common in exported/templated sheets)
// are skipped rather than staged as empty. Cells are read as DISPLAYED (see
// docs/OBSERVATIONS.md: a "#,##0" value arrives as "1,234" and fails the
// number check); ParseTabularFile reads stored values instead.
func ParseXLSXRows(data []byte) (header []string, rows []RawRow, err error) {
	return parseXLSXSheet(data, "", false)
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
