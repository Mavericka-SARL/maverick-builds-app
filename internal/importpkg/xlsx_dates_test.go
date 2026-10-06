package importpkg

import (
	"bytes"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

// A date cell is read as its ISO date, not the serial the workbook stores;
// numbers keep their stored values whatever their format.
func TestXLSXDateCellsReadAsISODates(t *testing.T) {
	f := excelize.NewFile()
	sheet := f.GetSheetName(0)
	iso := "yyyy-mm-dd"
	red := "#,##0.0;[Red](#,##0.0);-"
	dateStyle, _ := f.NewStyle(&excelize.Style{CustomNumFmt: &iso})
	builtinDate, _ := f.NewStyle(&excelize.Style{NumFmt: 22}) // m/d/yy h:mm
	redStyle, _ := f.NewStyle(&excelize.Style{CustomNumFmt: &red})
	rows := [][]any{{"Employee ID", "Hire Date", "Joined", "Salary", "Plain"},
		{"E001", time.Date(2014, 11, 24, 0, 0, 0, 0, time.UTC), time.Date(2020, 1, 2, 9, 30, 0, 0, time.UTC), 78.2, 41967}}
	for r, row := range rows {
		for c, v := range row {
			ref, _ := excelize.CoordinatesToCellName(c+1, r+1)
			_ = f.SetCellValue(sheet, ref, v)
		}
	}
	_ = f.SetCellStyle(sheet, "B2", "B2", dateStyle)
	_ = f.SetCellStyle(sheet, "C2", "C2", builtinDate)
	_ = f.SetCellStyle(sheet, "D2", "D2", redStyle)
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	_, got, err := ParseTabularFile("master.xlsx", buf.Bytes(), "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Hire Date": "2014-11-24", "Joined": "2020-01-02T09:30:00", "Salary": "78.2", "Plain": "41967"}
	for col, w := range want {
		if g := got[0].Cells[col]; g != w {
			t.Errorf("%s: got %q, want %q", col, g, w)
		}
	}
	// Into a number, the ISO date is the serial the workbook stores.
	if v, ok := dateSerial("2014-11-24"); !ok || v != 41967 {
		t.Errorf("dateSerial(2014-11-24) = %v, %v; want 41967", v, ok)
	}
	if v, ok := dateSerial("2020-01-02T09:30:00"); !ok || v != 43832.395833333 {
		t.Errorf("dateSerial(2020-01-02T09:30) = %v, %v", v, ok)
	}
	if _, ok := dateSerial("Jan"); ok {
		t.Error("dateSerial read text that is no date")
	}
}

func TestIsDateFormat(t *testing.T) {
	for _, c := range []struct {
		id     int
		custom string
		want   bool
	}{
		{14, "", true}, {22, "", true}, {20, "", false}, {0, "General", false}, {0, "0.0%", false},
		{0, "#,##0.0;[Red](#,##0.0);-", false}, {0, `[$-409]mmmm d, yyyy`, true}, {0, "mmm-yy", true},
		{0, `"Day "0`, false}, {0, `0\d`, false}, {0, "h:mm:ss", false}, {0, "dd/mm/yyyy hh:mm", true},
	} {
		if got := isDateFormat(c.id, c.custom); got != c.want {
			t.Errorf("isDateFormat(%d, %q) = %v, want %v", c.id, c.custom, got, c.want)
		}
	}
}

// The upload reads a plain number cell as stored ("1,234.50" was refused by
// the number check) and keeps a date's and a percentage's displayed text.
func TestParseXLSXRowsReadsStoredNumbers(t *testing.T) {
	f := excelize.NewFile()
	sheet := f.GetSheetName(0)
	_ = f.SetSheetRow(sheet, "A1", &[]any{"period", "revenue", "share", "code"})
	_ = f.SetSheetRow(sheet, "A2", &[]any{46037, 1234.5, 0.25, 1001})
	style := func(numFmt int) int {
		id, err := f.NewStyle(&excelize.Style{NumFmt: numFmt})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	_ = f.SetCellStyle(sheet, "A2", "A2", style(14)) // m/d/yy
	_ = f.SetCellStyle(sheet, "B2", "B2", style(4))  // #,##0.00
	_ = f.SetCellStyle(sheet, "C2", "C2", style(10)) // 0.00%
	_ = f.SetCellStyle(sheet, "D2", "D2", style(3))  // #,##0
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	_, rows, err := ParseXLSXRows(buf.Bytes())
	if err != nil || len(rows) != 1 {
		t.Fatalf("parse: %v, %d rows", err, len(rows))
	}
	cells := rows[0].Cells
	if cells["revenue"] != "1234.5" {
		t.Errorf("revenue = %q, want the stored 1234.5", cells["revenue"])
	}
	if cells["code"] != "1001" {
		t.Errorf("code = %q, want the stored 1001", cells["code"])
	}
	if cells["share"] != "25.00%" {
		t.Errorf("share = %q, want its displayed 25.00%%", cells["share"])
	}
	if p := cells["period"]; p == "46037" || p == "2026-01-15" {
		t.Errorf("period = %q, want its displayed text", p)
	}
}
