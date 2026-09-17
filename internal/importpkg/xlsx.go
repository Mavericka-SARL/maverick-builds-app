package importpkg

// Native .xlsx parsing for fact import, sharing the exact same RawRow shape
// (and therefore the exact same ResolveRows validation) as CSV — so a
// spreadsheet upload isn't a lesser, differently-validated path: it goes
// through the same name-based column mapping, leaf-member check, and
// non-negative check as everything else. excelize is already a direct
// dependency (used by internal/aiassistant for document parsing).

import (
	"bytes"
	"encoding/csv"
	"fmt"

	"github.com/xuri/excelize/v2"
)

// ParseCSVRows reads header + data rows from CSV text.
func ParseCSVRows(data []byte) (header []string, rows []RawRow, err error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.TrimLeadingSpace = true
	header, err = r.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("read header: %w", err)
	}
	rowNum := 0
	for {
		record, rerr := r.Read()
		if rerr != nil {
			break
		}
		rowNum++
		rows = append(rows, recordToRawRow(header, record, rowNum))
	}
	return header, rows, nil
}

// ParseXLSXRows reads header + data rows from the first sheet of a native
// Excel workbook. Blank trailing rows (common in exported/templated sheets)
// are skipped rather than staged as empty.
func ParseXLSXRows(data []byte) (header []string, rows []RawRow, err error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("open xlsx: %w", err)
	}
	defer func() { _ = f.Close() }()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, nil, fmt.Errorf("workbook has no sheets")
	}
	allRows, err := f.GetRows(sheets[0])
	if err != nil {
		return nil, nil, fmt.Errorf("read sheet %q: %w", sheets[0], err)
	}
	if len(allRows) == 0 {
		return nil, nil, fmt.Errorf("sheet %q has no header row", sheets[0])
	}
	header = allRows[0]
	rowNum := 0
	for _, record := range allRows[1:] {
		rowNum++
		if isBlankRecord(record) {
			continue
		}
		rows = append(rows, recordToRawRow(header, record, rowNum))
	}
	return header, rows, nil
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
