package importpkg

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// buildWorkbook writes a two-sheet workbook whose "Data" sheet holds a
// number displayed with a thousands separator — the shape a finance
// spreadsheet nearly always has.
func buildWorkbook(t *testing.T) []byte {
	t.Helper()
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	if _, err := f.NewSheet("Data"); err != nil {
		t.Fatal(err)
	}
	_ = f.SetSheetRow("Sheet1", "A1", &[]any{"ignored"})
	_ = f.SetSheetRow("Data", "A1", &[]any{"Country", "Revenue USD", "Notes"})
	_ = f.SetSheetRow("Data", "A2", &[]any{"CA", 1234.5, "first"})
	_ = f.SetSheetRow("Data", "A3", &[]any{"US", 1000000, ""})
	style, err := f.NewStyle(&excelize.Style{NumFmt: 4}) // #,##0.00
	if err != nil {
		t.Fatal(err)
	}
	_ = f.SetCellStyle("Data", "B2", "B3", style)
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseTabularFileReadsStoredValuesAndPicksSheet(t *testing.T) {
	data := buildWorkbook(t)

	header, rows, err := ParseTabularFile("plan.xlsx", data, "data")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Join(header, "|") != "Country|Revenue USD|Notes" {
		t.Fatalf("header = %v", header)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Displayed as "1,234.50"; ParseFloat needs the stored value.
	if got := rows[0].Cells["Revenue USD"]; got != "1234.5" {
		t.Errorf("CA revenue = %q, want the stored value 1234.5", got)
	}
	if got := rows[1].Cells["Revenue USD"]; got != "1000000" {
		t.Errorf("US revenue = %q, want 1000000", got)
	}

	// No sheet = the first one.
	header, _, err = ParseTabularFile("plan.xlsx", data, "")
	if err != nil || len(header) != 1 || header[0] != "ignored" {
		t.Errorf("default sheet header = %v, err %v", header, err)
	}
	if _, _, err := ParseTabularFile("plan.xlsx", data, "Budget"); err == nil || !strings.Contains(err.Error(), "Sheet1, Data") {
		t.Errorf("unknown sheet error = %v, want it to list the sheets", err)
	}
	if _, _, err := ParseTabularFile("plan.csv", []byte("a\n1\n"), "Data"); err == nil {
		t.Error("a sheet name on a CSV must be refused")
	}
	if _, _, err := ParseTabularFile("plan.pdf", nil, ""); err == nil {
		t.Error("a non-spreadsheet must be refused")
	}
	// A CSV with a byte-order mark keeps its first header clean.
	header, _, err = ParseTabularFile("plan.csv", []byte("\xef\xbb\xbfCountry,Revenue\nCA,1\n"), "")
	if err != nil || header[0] != "Country" {
		t.Errorf("BOM csv header = %q, err %v", header, err)
	}
}

func TestApplyColumnMap(t *testing.T) {
	rows := func() []RawRow {
		return []RawRow{{RowNumber: 1, Cells: map[string]string{"Country": "CA", "Account": "Revenue", "Amount": "5", "Notes": "x"}}}
	}

	r := rows()
	header, err := ApplyColumnMap([]string{"Country", "Account", "Amount", "Notes"}, r, map[string]string{
		"country": "geography", // keys match case-insensitively
		"Account": "metric",    // the wizard's long-format field
		"Amount":  "value",
		"Notes":   "ignore",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(header, "|") != "geography|metric_id|value" {
		t.Errorf("header = %v", header)
	}
	want := map[string]string{"geography": "CA", "metric_id": "Revenue", "value": "5"}
	for k, v := range want {
		if r[0].Cells[k] != v {
			t.Errorf("cell %s = %q, want %q (cells %v)", k, r[0].Cells[k], v, r[0].Cells)
		}
	}
	if _, kept := r[0].Cells["Notes"]; kept {
		t.Error("an ignored column must be dropped from the rows")
	}

	// Unmapped columns keep their header (the saved-sheet behaviour).
	r = rows()
	header, err = ApplyColumnMap([]string{"Country", "Amount"}, r, map[string]string{"Amount": "revenue"})
	if err != nil || strings.Join(header, "|") != "Country|revenue" || r[0].Cells["revenue"] != "5" {
		t.Errorf("partial map: header %v cells %v err %v", header, r[0].Cells, err)
	}

	// Two columns onto one field is refused.
	if _, err := ApplyColumnMap([]string{"Country", "Account"}, rows(), map[string]string{"Country": "geography", "Account": "Geography"}); err == nil {
		t.Error("two columns mapped to one field must be refused")
	}

	if missing := UnmatchedColumnMapKeys([]string{"Country"}, map[string]string{"country": "geography", "Cuntry": "x"}); len(missing) != 1 || missing[0] != "Cuntry" {
		t.Errorf("unmatched keys = %v, want [Cuntry]", missing)
	}
}
