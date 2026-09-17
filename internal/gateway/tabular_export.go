package gateway

// Shared CSV/XLSX response writers for tabular data export (grid cell data,
// form records). Mirrors the column-header conventions importpkg.ResolveRows
// already expects on the way back in — dimension/metric names as headers —
// so a grid export round-trips through /api/import/upload unmodified.

import (
	"encoding/csv"
	"fmt"
	"net/http"

	"github.com/xuri/excelize/v2"
)

// writeTabularResponse serializes header+records as CSV or XLSX (format must
// be "csv" or "xlsx") and writes it as a file-download response.
func writeTabularResponse(w http.ResponseWriter, format, filename string, header []string, records [][]string) error {
	switch format {
	case "xlsx":
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename+".xlsx"))
		return writeXLSX(w, header, records)
	default:
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename+".csv"))
		return writeCSV(w, header, records)
	}
}

func writeCSV(w http.ResponseWriter, header []string, records [][]string) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, rec := range records {
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func writeXLSX(w http.ResponseWriter, header []string, records [][]string) error {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	const sheet = "Sheet1"

	headerRow := make([]interface{}, len(header))
	for i, h := range header {
		headerRow[i] = h
	}
	if err := f.SetSheetRow(sheet, "A1", &headerRow); err != nil {
		return err
	}
	for i, rec := range records {
		row := make([]interface{}, len(rec))
		for j, v := range rec {
			row[j] = v
		}
		cell, err := excelize.CoordinatesToCellName(1, i+2)
		if err != nil {
			return err
		}
		if err := f.SetSheetRow(sheet, cell, &row); err != nil {
			return err
		}
	}
	return f.Write(w)
}
