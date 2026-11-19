package importpkg

import "testing"

// The Integrated FP&A workbook's Actuals sheet: row labels in C under an
// empty C5, a unit column D, month-ends from I5 across. The label column is
// named by its letter and can be mapped; an empty spacer column stays
// unnamed.
func TestUnheadedColumnsAreNamedByTheirLetter(t *testing.T) {
	grid := [][]string{
		{"", "", "Actuals"},
		{},
		{"", "", "", "", "", "2026-01-31", "2026-02-28"},
		{"", "", "Product A volume", "units", "", "10474.8", "11233.2"},
		{"", "", "Product A revenue", "EUR k", "", "881.5", "945.3", "", "note past the header"},
	}
	header, rows, err := ShapeGrid(grid, &Reshape{HeaderRow: 3, Unpivot: &Unpivot{From: "2026-01-31", To: "2026-02-28",
		NameColumn: "Month", ValueColumn: "Value"}})
	if err != nil {
		t.Fatal(err)
	}
	// Column I has a note past the header row's last cell: not a column, as before.
	want := map[string]bool{"Column C": true, "Column D": true, "Month": true, "Value": true}
	for _, h := range header {
		if h != "" && !want[h] {
			t.Errorf("unexpected column %q in %q", h, header)
		}
		delete(want, h)
	}
	if len(want) > 0 {
		t.Errorf("missing columns %v in %q", want, header)
	}
	if len(rows) != 4 || rows[0].Cells["Column C"] != "Product A volume" || rows[0].Cells["Month"] != "2026-01-31" ||
		rows[3].Cells["Value"] != "945.3" {
		t.Fatalf("rows %+v", rows)
	}
	for i, w := range map[int]string{0: "A", 25: "Z", 26: "AA", 43: "AR", 701: "ZZ", 702: "AAA"} {
		if got := columnLetters(i); got != w {
			t.Errorf("columnLetters(%d) = %q, want %q", i, got, w)
		}
	}
}

func TestUnheadedColumnNames(t *testing.T) {
	for name, want := range map[string]bool{"Column C": true, "Column AR": true, "Column": false, "Column c": false,
		"Column 3": false, "Columns C": false, "Column ABCD": false} {
		if got := IsUnheadedColumnName(name); got != want {
			t.Errorf("IsUnheadedColumnName(%q) = %v, want %v", name, got, want)
		}
	}
}
