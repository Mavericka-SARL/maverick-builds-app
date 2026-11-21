package reconcile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// MaxWorkbookBytes bounds a stored workbook.
const MaxWorkbookBytes = 25 << 20

// workbook is a workbook's stored values, by sheet (any case), row, column
// (both 0-based): numbers and dates as written ("46342" for a date), texts
// as they are, errors as their code (#N/A).
type workbook struct {
	sheets map[string][][]string
	names  []string
}

// openWorkbook reads every sheet's values (the cached results of formulas).
func openWorkbook(data []byte) (*workbook, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("the workbook cannot be read: %w", err)
	}
	defer func() { _ = f.Close() }()
	wb := &workbook{sheets: map[string][][]string{}}
	for _, name := range f.GetSheetList() {
		rows, err := f.GetRows(name, excelize.Options{RawCellValue: true})
		if err != nil {
			return nil, fmt.Errorf("sheet %s: %w", name, err)
		}
		// A date a workbook stores as text (an ISO cell, t="d") is its
		// serial number, as every other date: compared, tested and bound
		// alike.
		for _, row := range rows {
			for i, v := range row {
				if isoDateTime.MatchString(v) {
					row[i] = isoSerial(v)
				}
			}
		}
		wb.sheets[strings.ToLower(name)] = rows
		wb.names = append(wb.names, name)
	}
	return wb, nil
}

var isoDateTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d+)?)?Z?$`)

// isoSerial is an ISO date-time's serial number (days since 1899-12-30, the
// time as a fraction); the text itself when it does not parse.
func isoSerial(v string) string {
	for _, layout := range []string{"2006-01-02T15:04:05.000Z", "2006-01-02T15:04:05Z", "2006-01-02T15:04:05", "2006-01-02T15:04Z", "2006-01-02T15:04"} {
		if t, err := time.Parse(layout, v); err == nil {
			days := t.Sub(time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)).Hours() / 24
			return strconv.FormatFloat(days, 'f', -1, 64)
		}
	}
	return v
}

// SheetNames lists a workbook's sheets.
func SheetNames(data []byte) ([]string, error) {
	wb, err := openWorkbook(data)
	if err != nil {
		return nil, err
	}
	return wb.names, nil
}

func (wb *workbook) has(sheet string) bool {
	_, ok := wb.sheets[strings.ToLower(strings.TrimSpace(sheet))]
	return ok
}

// value is the cell's value; col and row 1-based.
func (wb *workbook) value(sheet string, col, row int) string {
	rows := wb.sheets[strings.ToLower(strings.TrimSpace(sheet))]
	if row < 1 || row > len(rows) || col < 1 || col > len(rows[row-1]) {
		return ""
	}
	return strings.TrimSpace(rows[row-1][col-1])
}

// ref reads "A1" on sheet, or "Other!A1".
func (wb *workbook) ref(sheet, ref string) (string, error) {
	s, cell := splitRef(sheet, ref)
	col, row, err := excelize.CellNameToCoordinates(cell)
	if err != nil {
		return "", fmt.Errorf("cell %q: %w", ref, err)
	}
	if !wb.has(s) {
		return "", fmt.Errorf("the workbook has no sheet %q", s)
	}
	return wb.value(s, col, row), nil
}

// splitRef splits "Sheet!A1" (quotes allowed) into sheet and cell; a bare
// "A1" is on sheet.
func splitRef(sheet, ref string) (string, string) {
	ref = strings.TrimSpace(ref)
	if i := strings.LastIndex(ref, "!"); i >= 0 {
		return strings.Trim(ref[:i], "'"), ref[i+1:]
	}
	return sheet, ref
}

// applyEdits writes a case's input edits into a copy of the workbook: a
// value into a cell, or a range copied from another (a formula as written,
// else its value).
func applyEdits(data []byte, edits []Edit) ([]byte, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("the workbook cannot be read: %w", err)
	}
	defer func() { _ = f.Close() }()
	for _, e := range edits {
		if e.Cell != "" {
			sheet, cell := splitRef("", e.Cell)
			if sheet == "" {
				return nil, fmt.Errorf("edit %s: name the sheet (Sheet!A1)", e.Cell)
			}
			if err := f.SetCellValue(sheet, cell, editValue(e.Value)); err != nil {
				return nil, fmt.Errorf("edit %s: %w", e.Cell, err)
			}
			continue
		}
		dst, dw, err := rangeCells(e.Range)
		if err != nil {
			return nil, err
		}
		src, sw, err := rangeCells(e.CopyFrom)
		if err != nil {
			return nil, err
		}
		if len(dst) != len(src) || dw != sw {
			return nil, fmt.Errorf("edit %s ← %s: the ranges differ in shape", e.Range, e.CopyFrom)
		}
		for i := range dst {
			formula, err := f.GetCellFormula(src[i][0], src[i][1])
			if err != nil {
				return nil, fmt.Errorf("edit %s: %w", e.CopyFrom, err)
			}
			if formula != "" {
				if err := f.SetCellFormula(dst[i][0], dst[i][1], formula); err != nil {
					return nil, err
				}
				continue
			}
			v, err := f.GetCellValue(src[i][0], src[i][1], excelize.Options{RawCellValue: true})
			if err != nil {
				return nil, err
			}
			if err := f.SetCellValue(dst[i][0], dst[i][1], editValue(v)); err != nil {
				return nil, err
			}
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// editValue is a value as a cell takes it: a number written as text is a
// number.
func editValue(v any) any {
	switch x := v.(type) {
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return n
		}
		return x
	case nil:
		return ""
	}
	return v
}

// rangeCells lists "Sheet!F45:BE45" as (sheet, cell) pairs, row by row,
// with the range's width.
func rangeCells(r string) ([][2]string, int, error) {
	sheet, span := splitRef("", r)
	if sheet == "" {
		return nil, 0, fmt.Errorf("range %s: name the sheet (Sheet!A1:B2)", r)
	}
	a, b, ok := strings.Cut(span, ":")
	if !ok {
		b = a
	}
	c1, r1, err := excelize.CellNameToCoordinates(a)
	if err != nil {
		return nil, 0, fmt.Errorf("range %s: %w", r, err)
	}
	c2, r2, err := excelize.CellNameToCoordinates(b)
	if err != nil {
		return nil, 0, fmt.Errorf("range %s: %w", r, err)
	}
	var out [][2]string
	for row := min(r1, r2); row <= max(r1, r2); row++ {
		for col := min(c1, c2); col <= max(c1, c2); col++ {
			name, _ := excelize.CoordinatesToCellName(col, row)
			out = append(out, [2]string{sheet, name})
		}
	}
	return out, max(c1, c2) - min(c1, c2) + 1, nil
}

// ErrNoRecalculation: a case changes inputs, and no LibreOffice is
// configured to recalculate the workbook.
var ErrNoRecalculation = errors.New("this case changes the workbook's inputs, and recalculating a workbook needs LibreOffice: the server's RECONCILE_SOFFICE names its soffice program (not set here)")

// RecalcTimeout bounds one recalculation.
var RecalcTimeout = 3 * time.Minute

// recalculate has LibreOffice (soffice, headless, in a profile of its own
// that recalculates every formula on load) calculate the workbook and save
// it again.
func recalculate(ctx context.Context, soffice string, data []byte) ([]byte, error) {
	if strings.TrimSpace(soffice) == "" {
		return nil, ErrNoRecalculation
	}
	dir, err := os.MkdirTemp("", "reconcile-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	profile := filepath.Join(dir, "profile")
	if err := os.MkdirAll(filepath.Join(profile, "user"), 0o700); err != nil {
		return nil, err
	}
	// Always recalculate an xlsx on load: the stored results are the saved
	// case's, not this one's.
	xcu := `<?xml version="1.0" encoding="UTF-8"?>
<oor:items xmlns:oor="http://openoffice.org/2001/registry" xmlns:xs="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
<item oor:path="/org.openoffice.Office.Calc/Formula/Load"><prop oor:name="OOXMLRecalcMode" oor:op="fuse"><value>0</value></prop></item>
<item oor:path="/org.openoffice.Office.Calc/Formula/Load"><prop oor:name="ODFRecalcMode" oor:op="fuse"><value>0</value></prop></item>
</oor:items>
`
	if err := os.WriteFile(filepath.Join(profile, "user", "registrymodifications.xcu"), []byte(xcu), 0o600); err != nil {
		return nil, err
	}
	in := filepath.Join(dir, "case.xlsx")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		return nil, err
	}
	out := filepath.Join(dir, "out")
	cctx, cancel := context.WithTimeout(ctx, RecalcTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, soffice, "-env:UserInstallation=file://"+filepath.ToSlash(profile),
		"--headless", "--norestore", "--nolockcheck", "--nodefault", "--calc",
		"--convert-to", "xlsx:Calc MS Excel 2007 XML", "--outdir", out, in)
	cmd.Env = append(os.Environ(), "HOME="+dir)
	if msg, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("LibreOffice could not recalculate the workbook: %v %s", err, strings.TrimSpace(string(msg)))
	}
	got, err := os.ReadFile(filepath.Join(out, "case.xlsx"))
	if err != nil {
		return nil, fmt.Errorf("LibreOffice saved no workbook: %w", err)
	}
	return got, nil
}
