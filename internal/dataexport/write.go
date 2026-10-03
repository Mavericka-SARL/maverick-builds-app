package dataexport

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// ContentType is the HTTP content type of a format's file.
func ContentType(format string) string {
	switch format {
	case FormatXLSX:
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case FormatJSON:
		return "application/json"
	default:
		return "text/csv; charset=utf-8"
	}
}

// FileName is the download's file name: the spec's file_name, else the
// fallback (the export's name), made safe for a Content-Disposition header,
// plus the format's extension.
func FileName(s Spec, fallback string) string {
	s = s.Normalized()
	base := strings.TrimSpace(s.FileName)
	if base == "" {
		base = fallback
	}
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		case r == ' ':
			return '_'
		}
		return -1
	}, base)
	base = strings.Trim(base, "._")
	if base == "" {
		base = "export"
	}
	return base + "." + s.Format
}

// NewTable is a table a caller already has rather than one rendered from a
// grid (an AI Assistant conversion), written in format — "csv" or "xlsx" —
// with every default a spec would have.
func NewTable(header []string, rows [][]Value, format string) *Table {
	return &Table{Header: header, DefaultHeader: header, Rows: rows, spec: Spec{Format: format}.Normalized()}
}

// Write writes a rendered table in its spec's format.
func Write(w io.Writer, t *Table) error {
	switch t.spec.Format {
	case FormatXLSX:
		return writeXLSX(w, t)
	case FormatJSON:
		return writeJSON(w, t)
	default:
		return writeCSV(w, t)
	}
}

func writeCSV(w io.Writer, t *Table) error {
	cw := csv.NewWriter(w)
	cw.Comma = rune(t.spec.Delimiter[0])
	if *t.spec.IncludeHeader {
		if err := cw.Write(t.Header); err != nil {
			return err
		}
	}
	for _, row := range t.TextRows(0) {
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func writeXLSX(w io.Writer, t *Table) error {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	sheet := t.spec.SheetName
	if err := f.SetSheetName("Sheet1", sheet); err != nil {
		return err
	}
	sw, err := f.NewStreamWriter(sheet)
	if err != nil {
		return err
	}
	numStyle := 0
	if t.spec.Decimals != nil {
		format := "0"
		if *t.spec.Decimals > 0 {
			format += "." + strings.Repeat("0", *t.spec.Decimals)
		}
		if numStyle, err = f.NewStyle(&excelize.Style{CustomNumFmt: &format}); err != nil {
			return err
		}
	}
	rowNum := 1
	if *t.spec.IncludeHeader {
		header := make([]any, len(t.Header))
		for i, h := range t.Header {
			header[i] = h
		}
		if err := sw.SetRow("A1", header); err != nil {
			return err
		}
		rowNum++
	}
	for _, row := range t.Rows {
		cells := make([]any, len(row))
		for i, v := range row {
			switch {
			case v.IsNum:
				cells[i] = excelize.Cell{StyleID: numStyle, Value: v.Num}
			case v.Text != "":
				cells[i] = v.Text
			default:
				cells[i] = nil
			}
		}
		ref, err := excelize.CoordinatesToCellName(1, rowNum)
		if err != nil {
			return err
		}
		if err := sw.SetRow(ref, cells); err != nil {
			return err
		}
		rowNum++
	}
	if err := sw.Flush(); err != nil {
		return err
	}
	return f.Write(w)
}

// writeJSON writes an array of objects keyed by the header, in column
// order (encoding/json would sort a map's keys). Numbers stay numbers —
// rounded to the spec's decimals when set — and an empty cell is null.
func writeJSON(w io.Writer, t *Table) error {
	bw := bufio.NewWriter(w)
	keys := make([][]byte, len(t.Header))
	for i, h := range t.Header {
		k, err := json.Marshal(h)
		if err != nil {
			return err
		}
		keys[i] = k
	}
	_, _ = bw.WriteString("[")
	for r, row := range t.Rows {
		if r > 0 {
			_, _ = bw.WriteString(",")
		}
		_, _ = bw.WriteString("\n  {")
		for i, v := range row {
			if i > 0 {
				_, _ = bw.WriteString(", ")
			}
			_, _ = bw.Write(keys[i])
			_, _ = bw.WriteString(": ")
			switch {
			case v.IsNum:
				if t.spec.Decimals != nil {
					_, _ = bw.WriteString(strconv.FormatFloat(v.Num, 'f', *t.spec.Decimals, 64))
				} else {
					_, _ = bw.WriteString(strconv.FormatFloat(v.Num, 'f', -1, 64))
				}
			case v.Text != "":
				b, err := json.Marshal(v.Text)
				if err != nil {
					return err
				}
				_, _ = bw.Write(b)
			default:
				_, _ = bw.WriteString("null")
			}
		}
		_, _ = bw.WriteString("}")
	}
	if len(t.Rows) > 0 {
		_, _ = bw.WriteString("\n")
	}
	_, _ = bw.WriteString("]\n")
	return bw.Flush()
}

// Describe is a one-line summary of a spec, for lists and the assistant.
func Describe(s Spec) string {
	s = s.Normalized()
	parts := []string{strings.ToUpper(s.Format), s.Layout}
	if s.Layout == LayoutPivot && s.PivotDimension != "" {
		parts[1] = "pivot on " + s.PivotDimension
	}
	if s.Format == FormatCSV && (s.Delimiter != "," || s.DecimalSeparator != ".") {
		d := s.Delimiter
		if d == "\t" {
			d = "tab"
		}
		parts = append(parts, fmt.Sprintf("delimiter %q, decimal %q", d, s.DecimalSeparator))
	}
	if len(s.Metrics) > 0 {
		parts = append(parts, "metrics "+strings.Join(s.Metrics, ", "))
	} else {
		parts = append(parts, "all metrics")
	}
	if s.MemberDisplay != DisplayCode {
		parts = append(parts, "members as "+s.MemberDisplay)
	}
	if len(s.Filters) > 0 {
		var fs []string
		for k, v := range s.Filters {
			fs = append(fs, k+"="+strings.Join(v, "|"))
		}
		sort.Strings(fs)
		parts = append(parts, "filters "+strings.Join(fs, "; "))
	}
	if s.Decimals != nil {
		parts = append(parts, fmt.Sprintf("%d decimals", *s.Decimals))
	}
	return strings.Join(parts, " · ")
}
