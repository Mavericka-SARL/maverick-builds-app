package aiassistant

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestAnnotateFormulasInModelNames(t *testing.T) {
	rows := []MapRow{
		{Sheet: "SaaS", Row: 9, Metric: "saas_closing_subscribers"},
		{Sheet: "SaaS", Row: 11, Metric: "saas_new_subscribers"},
		{Sheet: "Sites Logistics", Row: 9, Metric: "sites_capacity"},
		{Sheet: "Assumptions", Row: 5, Metric: "price_per_unit"},
	}
	text := "## Sheet: SaaS\n1\tx\n### Formulas on SaaS (a range shares the formula of its first cell)\n" +
		"D10:AL10 [Feb-27 … Dec-29 | Opening] = C9\n" +
		"C12:AL12 [Jan-27 … Dec-29 | Revenue] = C11*Assumptions!$C5+'Sites Logistics'!C9+LOG10(C7)+SUM(C1:C3)&\"C9\"\n" +
		"C13 = C7*2\n" +
		"### Lists on SaaS\nD10 = C9\n"
	out := AnnotateFormulas(text, "SaaS", rows)
	for _, want := range []string{
		"D10:AL10 [Feb-27 … Dec-29 | Opening] = C9\n   ≈ in the model's names: LAG(saas_closing_subscribers, 1)\n",
		"   ≈ in the model's names: saas_new_subscribers*price_per_unit+sites_capacity+LOG10(C7)+SUM(C1:C3)&\"C9\"\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("annotation lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "C13 = C7*2\n   ≈") || strings.Contains(out, "### Lists on SaaS\nD10 = C9\n   ≈") {
		t.Errorf("an unmapped formula or a line outside the formulas was annotated:\n%s", out)
	}
}

func TestCoverageFindsRowsNotMapped(t *testing.T) {
	f := excelize.NewFile()
	_ = f.SetSheetName("Sheet1", "PnL")
	for i, r := range [][]any{{"Line", "Unit", "Jan", "Feb"}, {"Revenue", "EUR", 100, 200}, {"Costs", "EUR", 50, 60}, {"Note", "", "x", ""}, {"EBITDA", "EUR", 50, 140}, {"Model month #", "", 1, 2}} {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		_ = f.SetSheetRow("PnL", cell, &r)
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	rows := []MapRow{
		{File: "m.xlsx", Sheet: "PnL", Row: 2, Metric: "revenue", FirstColumn: "C", Matched: 2, Compared: 2},
		{File: "m.xlsx", Sheet: "PnL", Row: 5, Metric: "ebitda", FirstColumn: "C", Matched: 1, Compared: 2},
	}
	cov := Coverage(rows, map[string][]byte{"m.xlsx": buf.Bytes()})
	if len(cov) != 1 || cov[0].Mapped != 2 || cov[0].Match != 1 || len(cov[0].Differ) != 1 || cov[0].Differ[0] != 5 {
		t.Fatalf("coverage = %+v", cov)
	}
	if len(cov[0].NotMapped) != 1 || cov[0].NotMapped[0] != 3 || cov[0].NotMappedLabels[3] != "Costs" {
		t.Fatalf("not mapped = %v %v", cov[0].NotMapped, cov[0].NotMappedLabels)
	}
	if s := RenderCoverage(cov); !strings.Contains(s, "m.xlsx / PnL: 2 row(s) mapped, 1 match every period, 1 differ (rows 5); 1 row(s) with numbers not mapped yet (rows 3)") {
		t.Fatalf("brief:\n%s", s)
	}
}
