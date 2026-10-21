package importpkg

import (
	"reflect"
	"testing"
)

// TestCSVBlankLinesAreRows: a sheet saved as CSV keeps its blank lines as
// rows, so header_row names the same row in the CSV as in the workbook,
// and a quoted cell over two lines is still one row.
func TestCSVBlankLinesAreRows(t *testing.T) {
	csv := "GL import\n\nPaste below\n\naccount,amount\n4000,\"10\"\n\n4100,\"multi\nline\"\n4200,30"
	grid, err := readCSVGrid([]byte(csv), ',')
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"GL import"}, {}, {"Paste below"}, {}, {"account", "amount"}, {"4000", "10"}, {}, {"4100", "multi\nline"}, {"4200", "30"}}
	if !reflect.DeepEqual(grid, want) {
		t.Fatalf("grid = %q\nwant  %q", grid, want)
	}
	header, rows, err := ShapeCSV([]byte(csv), &Reshape{HeaderRow: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(header, []string{"account", "amount"}) || len(rows) != 3 || rows[2].Cells["account"] != "4200" || rows[2].RowNumber != 4 {
		t.Fatalf("header %q, rows %+v", header, rows)
	}
	// No header row named: the first row that is not blank.
	header, rows, err = ShapeCSV([]byte("\n\naccount,amount\n4000,1\r\n4100,2\r\n"), nil)
	if err != nil || !reflect.DeepEqual(header, []string{"account", "amount"}) || len(rows) != 2 {
		t.Fatalf("leading blank lines: header %q rows %+v err %v", header, rows, err)
	}
}
