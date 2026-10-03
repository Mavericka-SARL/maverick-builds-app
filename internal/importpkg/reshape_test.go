package importpkg

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// peopleWorkbook is a sheet laid out for people, as finance teams send them:
// a title and a blank line above the header, the region written once per
// group, months across the top, subtotal rows, an amount "in thousands" in
// accounting notation.
func peopleWorkbook(t *testing.T) []byte {
	t.Helper()
	f := excelize.NewFile()
	rows := [][]any{
		{"Opex plan 2026 (EUR thousands)"},
		{},
		{"Region", "Account", "Jan", "Feb", "Mar", "Note"},
		{"EMEA", "Travel", 10, 12.5, "", "x"},
		{"", "Rent", "(5)", 5, 5},
		{"", "Total EMEA", 5, 17.5, 5},
		{"APAC", "Travel", 3, "", 4},
		{"Total", "", 8, 17.5, 9},
	}
	for i, r := range rows {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		if err := f.SetSheetRow("Sheet1", cell, &r); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReshapeAPeopleShapedWorkbook(t *testing.T) {
	r := &Reshape{
		HeaderRow: 3,
		FillDown:  []string{"region"},
		SkipRows:  []RowFilter{{Column: "Account", Contains: "total"}, {Column: "Region", Equals: "Total"}},
		Unpivot:   &Unpivot{From: "Jan", To: "Mar", NameColumn: "Month", ValueColumn: "Opex"},
		Constants: map[string]string{"Scenario": "Budget"},
		ValueMap:  map[string]map[string]string{"Month": {"jan": "2026-01", "Feb": "2026-02", "MAR": "2026-03"}},
		Scale:     map[string]float64{"Opex": 1000},
	}
	header, rows, err := ReadShaped("plan.xlsx", peopleWorkbook(t), "", r)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Region", "Account", "Note", "Month", "Opex", "Scenario"}; !reflect.DeepEqual(header, want) {
		t.Fatalf("header = %v, want %v", header, want)
	}
	type row struct {
		num                                    int
		region, account, month, opex, scenario string
	}
	var got []row
	for _, r := range rows {
		got = append(got, row{r.RowNumber, r.Cells["Region"], r.Cells["Account"], r.Cells["Month"], r.Cells["Opex"], r.Cells["Scenario"]})
	}
	want := []row{
		// Blank cells make no row (EMEA Travel has no March, APAC no February).
		{1, "EMEA", "Travel", "2026-01", "10000", "Budget"},
		{1, "EMEA", "Travel", "2026-02", "12500", "Budget"},
		{2, "EMEA", "Rent", "2026-01", "-5000", "Budget"},
		{2, "EMEA", "Rent", "2026-02", "5000", "Budget"},
		{2, "EMEA", "Rent", "2026-03", "5000", "Budget"},
		{4, "APAC", "Travel", "2026-01", "3000", "Budget"},
		{4, "APAC", "Travel", "2026-03", "4000", "Budget"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows =\n%v\nwant\n%v", got, want)
	}
}

func TestReshapeEuropeanCSV(t *testing.T) {
	csv := "Konto;Betrag;Anteil\nReise;1.234,5;12,5%\nMiete;-200;\n"
	header, rows, err := ShapeCSV([]byte(csv), &Reshape{Delimiter: ";", DecimalComma: true, NumberColumns: []string{"Betrag", "Anteil"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(header, []string{"Konto", "Betrag", "Anteil"}) || len(rows) != 2 {
		t.Fatalf("header %v, %d rows", header, len(rows))
	}
	if rows[0].Cells["Betrag"] != "1234.5" || rows[0].Cells["Anteil"] != "0.125" || rows[1].Cells["Betrag"] != "-200" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestParseHumanNumber(t *testing.T) {
	for _, c := range []struct {
		in    string
		comma bool
		want  float64
		ok    bool
	}{
		{"1,234.50", false, 1234.5, true},
		{"1 234,50", true, 1234.5, true},
		{"1.234.567,8", true, 1234567.8, true},
		{"(123)", false, -123, true},
		{"(1,234.50)", false, -1234.5, true},
		{"12%", false, 0.12, true},
		{"$ 1,000", false, 1000, true},
		{"€1.000", true, 1000, true},
		{"+7", false, 7, true},
		{"1,5", false, 0, false}, // never guessed to be 15
		{"1.5", true, 0, false},  // never guessed to be 15
		{"12,34,567", false, 0, false},
		{"abc", false, 0, false},
		{"", false, 0, false},
	} {
		got, ok := ParseHumanNumber(c.in, c.comma)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseHumanNumber(%q, %v) = %v, %v; want %v, %v", c.in, c.comma, got, ok, c.want, c.ok)
		}
	}
}

func TestReshapeErrorsNameWhatIsWrong(t *testing.T) {
	data := peopleWorkbook(t)
	for _, c := range []struct {
		r    *Reshape
		want string
	}{
		{&Reshape{HeaderRow: 3, FillDown: []string{"Area"}}, `names column "Area"`},
		{&Reshape{HeaderRow: 3, FillDown: []string{"Area"}}, "its columns are: Region, Account, Jan"},
		{&Reshape{HeaderRow: 40}, "the sheet has 8 row(s)"},
		{&Reshape{HeaderRow: 3, Unpivot: &Unpivot{From: "Mar", To: "Jan", NameColumn: "M", ValueColumn: "V"}}, "comes after"},
		{&Reshape{HeaderRow: 3, Unpivot: &Unpivot{Columns: []string{"Jan"}, NameColumn: "Region", ValueColumn: "V"}}, `already has a column "Region"`},
		{&Reshape{HeaderRow: 3, Constants: map[string]string{"account": "x"}}, "use value_map"},
		{&Reshape{SkipRows: []RowFilter{{Column: "Region"}}}, "exactly one of equals, contains or blank"},
		{&Reshape{Unpivot: &Unpivot{Columns: []string{"Jan"}, From: "Jan", To: "Feb", NameColumn: "M", ValueColumn: "V"}}, "either columns"},
		{&Reshape{Scale: map[string]float64{"Jan": 0}}, "non-zero"},
		{&Reshape{Delimiter: ";"}, "applies to CSV files only"},
		{&Reshape{Delimiter: "#"}, "reshape.delimiter must be"},
	} {
		if _, _, err := ReadShaped("plan.xlsx", data, "", c.r); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("reshape %+v: error %v, want it to contain %q", c.r, err, c.want)
		}
	}
}

// A row longer than the header — a stray trailing comma — used to end the
// file there without a word: every row after it was dropped.
func TestCSVRowsAfterARaggedRowAreRead(t *testing.T) {
	header, rows, err := ParseCSVRows([]byte("Region,Revenue\nEMEA,10\nAPAC,20,\nAMER,30\n,\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(header) != 2 || len(rows) != 3 || rows[2].Cells["Region"] != "AMER" || rows[2].RowNumber != 3 {
		t.Fatalf("header %v, rows %+v", header, rows)
	}
}
