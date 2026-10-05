package aiassistant

import (
	"strconv"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// The AI Developer reads an attached workbook as the model it describes:
// stored values with row numbers, the layout, the formulas compressed by
// block, the drop-down lists and the highlights. Values alone (what it read
// before) gave it no formula and only the drop-down options in use.
func TestWorkbookExtractionReadsFormulasListsAndLayout(t *testing.T) {
	f := excelize.NewFile()
	sh := "Sheet1"
	_ = f.SetSheetName(sh, "Plan")
	sh = "Plan"
	set := func(cell string, v any) { _ = f.SetCellValue(sh, cell, v) }
	setF := func(cell, formula string) { _ = f.SetCellFormula(sh, cell, formula) }
	set("A1", "Region")
	set("B1", "Jan")
	set("C1", "Feb")
	set("D1", "Total")
	set("E1", "Growth")
	set("F1", "Status")
	for i, r := range []string{"North", "South", "East"} {
		row := i + 2
		set("A"+strconv.Itoa(row), r)
		set("B"+strconv.Itoa(row), 13.36+float64(i))
		set("C"+strconv.Itoa(row), 14.07)
		// D: a total per row; E: each month column read in turn from a
		// fixed key column (the month columns merge side by side below).
		setF("D"+strconv.Itoa(row), "SUM(B"+strconv.Itoa(row)+":C"+strconv.Itoa(row)+")")
		setF("E"+strconv.Itoa(row), "B"+strconv.Itoa(row)+"/$B$2")
		set("F"+strconv.Itoa(row), "Draft")
	}
	set("A6", "Total")
	setF("B6", "SUM(B2:B4)")
	setF("C6", "SUM(C2:C4)")
	// Two side-by-side columns each reading their own column of another
	// block, with a fixed key column (A) — one line, not two.
	set("H1", "Jan LY")
	set("I1", "Feb LY")
	for _, row := range []string{"2", "3"} {
		setF("H"+row, "SUMIFS($B$2:$B$4,$A$2:$A$4,A"+row+")")
		setF("I"+row, "SUMIFS($C$2:$C$4,$A$2:$A$4,A"+row+")")
	}
	dv := excelize.NewDataValidation(true)
	dv.Sqref = "F2:F4"
	_ = dv.SetDropList([]string{"Draft", "Committed", "Cancelled"})
	_ = f.AddDataValidation(sh, dv)
	var buf strings.Builder
	if err := f.Write(&writerAdapter{&buf}); err != nil {
		t.Fatal(err)
	}

	text, _, truncated, err := ExtractDocumentText("plan.xlsx", []byte(buf.String()))
	if err != nil || truncated {
		t.Fatalf("extract: %v (truncated %v)", err, truncated)
	}
	for _, want := range []string{
		"2\tNorth\t13.36",                                          // stored value with its row number, not "13.4"
		"D2:D4 [Total] = SUM(B2:C2)",                               // a column of one relative formula, under its header
		"B6:C6 [Jan … Feb | Total] = SUM(B2:B4)",                   // a total row across two columns, with its row label
		"H2:I3 [Jan LY … Feb LY] = SUMIFS($B$2:$B$4,$A$2:$A$4,A2)", // month columns reading their own month
		"F2:F4: one of Draft,Committed,Cancelled",                  // every option, not only those in use
		"B [Jan]: input 2-4, formula 6",                            // the layout: what each column's rows hold
	} {
		if !strings.Contains(text, want) {
			t.Errorf("extraction lacks %q:\n%s", want, text)
		}
	}
}

type writerAdapter struct{ sb *strings.Builder }

func (w *writerAdapter) Write(p []byte) (int, error) { return w.sb.Write(p) }
