package dataexport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

const geoID, periodID = "dim-geo", "dim-period"
const revenueID, marginID = "m-revenue", "m-margin"

func fixtureGrid() Grid {
	return Grid{
		Name: "Sales",
		Dimensions: []Dimension{
			{ID: geoID, Name: "geography", Members: []Member{
				{Code: "WORLD", Label: "World"},
				{Code: "AMER", Label: "Americas", ParentCode: "WORLD"},
				{Code: "CA", Label: "Canada", ParentCode: "AMER"},
				{Code: "US", Label: "United States", ParentCode: "AMER"},
				{Code: "EMEA", Label: "EMEA", ParentCode: "WORLD"},
				{Code: "UK", Label: "United Kingdom", ParentCode: "EMEA"},
			}},
			{ID: periodID, Name: "period", Members: []Member{
				{Code: "FY26", Label: "FY 2026"},
				{Code: "Q1", Label: "Quarter 1", ParentCode: "FY26"},
				{Code: "Q2", Label: "Quarter 2", ParentCode: "FY26"},
			}},
		},
		Metrics: []Metric{
			{ID: revenueID, Name: "revenue", Label: "Revenue", DimensionIDs: []string{geoID, periodID}},
			{ID: marginID, Name: "margin_pct", Label: "Margin Pct", DimensionIDs: []string{geoID, periodID}},
		},
	}
}

func fixtureSnapshot() Snapshot {
	return Snapshot{Grid: fixtureGrid(), Cells: map[string]float64{
		revenueID + ":CA:Q1": 100,
		revenueID + ":CA:Q2": 200,
		revenueID + ":US:Q1": 50.125,
		marginID + ":CA:Q1":  0.25,
		// Rollup rows /api/grid serves for a calculated metric: never a
		// leaf-level export row.
		marginID + ":AMER:Q1": 0.3,
		marginID + ":CA:FY26": 0.27,
	}}
}

func render(t *testing.T, spec Spec) (string, *Table) {
	t.Helper()
	tbl, err := Render(spec, fixtureSnapshot())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, tbl); err != nil {
		t.Fatalf("write: %v", err)
	}
	return buf.String(), tbl
}

func TestWideCSVIsLeafLevelInDisplayOrder(t *testing.T) {
	got, _ := render(t, Spec{})
	want := "geography,period,revenue,margin_pct\n" +
		"CA,Q1,100,0.25\n" +
		"CA,Q2,200,\n" +
		"US,Q1,50.125,\n"
	if got != want {
		t.Errorf("wide csv:\n%s\nwant:\n%s", got, want)
	}
}

func TestLongJSONWithLabels(t *testing.T) {
	got, _ := render(t, Spec{Format: "json", Layout: "long", MemberDisplay: "label", MetricDisplay: "label", Metrics: []string{"revenue"},
		Filters: map[string][]string{"geography": {"CA"}}})
	var rows []map[string]any
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("json: %v\n%s", err, got)
	}
	if len(rows) != 2 || rows[0]["geography"] != "Canada" || rows[0]["period"] != "Quarter 1" ||
		rows[0]["metric"] != "Revenue" || rows[0]["value"] != 100.0 || rows[1]["value"] != 200.0 {
		t.Errorf("long json rows = %v", rows)
	}
	// Keys keep column order.
	if !strings.Contains(got, `{"geography": "Canada", "period": "Quarter 1", "metric": "Revenue", "value": 100}`) {
		t.Errorf("json key order:\n%s", got)
	}
}

func TestPivotWithParentFilter(t *testing.T) {
	// AMER stands for CA and US; UK is left out.
	got, _ := render(t, Spec{Layout: "pivot", PivotDimension: "period", Metrics: []string{"revenue"},
		Filters: map[string][]string{"geography": {"AMER"}}})
	want := "geography,Q1,Q2\nCA,100,200\nUS,50.125,\n"
	if got != want {
		t.Errorf("pivot:\n%s\nwant:\n%s", got, want)
	}
	// Two metrics add a metric column.
	got, _ = render(t, Spec{Layout: "pivot", PivotDimension: "period", MemberDisplay: "label",
		Filters: map[string][]string{"geography": {"CA"}}})
	want = "geography,metric,Quarter 1,Quarter 2\nCanada,revenue,100,200\nCanada,margin_pct,0.25,\n"
	if got != want {
		t.Errorf("pivot, two metrics:\n%s\nwant:\n%s", got, want)
	}
}

func TestPinnedDimensionIsNotAColumn(t *testing.T) {
	got, _ := render(t, Spec{Dimensions: []string{"geography"}, Filters: map[string][]string{"period": {"Q1"}},
		ColumnNames: map[string]string{"geography": "Country", "revenue": "Revenue Q1"}})
	want := "Country,Revenue Q1,margin_pct\nCA,100,0.25\nUS,50.125,\n"
	if got != want {
		t.Errorf("pinned:\n%s\nwant:\n%s", got, want)
	}
}

func TestCSVNumberFormatting(t *testing.T) {
	two := 2
	got, _ := render(t, Spec{Delimiter: ";", DecimalSeparator: ",", Decimals: &two, Metrics: []string{"revenue"},
		MemberDisplay: "code_and_label", Filters: map[string][]string{"geography": {"US"}}})
	want := "geography;geography label;period;period label;revenue\nUS;United States;Q1;Quarter 1;50,13\n"
	if got != want {
		t.Errorf("formatted csv:\n%s\nwant:\n%s", got, want)
	}
	no := false
	got, _ = render(t, Spec{Delimiter: "tab", IncludeHeader: &no, Metrics: []string{"revenue"}, Filters: map[string][]string{"geography": {"US"}}})
	if got != "US\tQ1\t50.125\n" {
		t.Errorf("headerless tab csv = %q", got)
	}
}

func TestIncludeEmptyRowsIsTheFullLeafProduct(t *testing.T) {
	_, tbl := render(t, Spec{IncludeEmptyRows: true})
	if len(tbl.Rows) != 6 { // CA, US, UK × Q1, Q2
		t.Errorf("rows = %d, want 6", len(tbl.Rows))
	}
}

func TestHiddenMetricAndMemberRenderAbsent(t *testing.T) {
	snap := fixtureSnapshot()
	// The downloader cannot see margin_pct, nor US.
	snap.Metrics = snap.Metrics[:1]
	snap.Dimensions[0].Members = []Member{
		{Code: "WORLD"}, {Code: "AMER", ParentCode: "WORLD"}, {Code: "CA", ParentCode: "AMER"},
	}
	delete(snap.Cells, revenueID+":US:Q1")
	tbl, err := Render(Spec{Metrics: []string{"revenue", "margin_pct"}, Filters: map[string][]string{"geography": {"US", "CA"}}}, snap)
	if err != nil {
		t.Fatalf("a spec naming hidden things must still render: %v", err)
	}
	if strings.Join(tbl.Header, ",") != "geography,period,revenue" || len(tbl.Rows) != 2 {
		t.Errorf("header %v rows %v", tbl.Header, tbl.TextRows(0))
	}
	// Pinned to a member the user cannot see: empty, not an error.
	tbl, err = Render(Spec{Dimensions: []string{"period"}, Filters: map[string][]string{"geography": {"US"}}}, snap)
	if err != nil || len(tbl.Rows) != 0 {
		t.Errorf("pinned to a hidden member: rows %v err %v", tbl.TextRows(0), err)
	}
}

func TestMemberCodesContainingTheSeparator(t *testing.T) {
	snap := fixtureSnapshot()
	snap.Dimensions[0].Members = append(snap.Dimensions[0].Members, Member{Code: "UK:LON", ParentCode: "EMEA"})
	snap.Dimensions[0].Members[5] = Member{Code: "UK:MAN", ParentCode: "EMEA"}
	snap.Cells = map[string]float64{revenueID + ":UK:LON:Q2": 7}
	tbl, err := Render(Spec{Metrics: []string{"revenue"}}, snap)
	if err != nil || len(tbl.Rows) != 1 || tbl.Rows[0][0].Text != "UK:LON" || tbl.Rows[0][1].Text != "Q2" {
		t.Errorf("rows %v err %v", tbl.TextRows(0), err)
	}
}

func TestXLSXKeepsNumbersNumeric(t *testing.T) {
	one := 1
	got, _ := render(t, Spec{Format: "xlsx", SheetName: "Revenue", Decimals: &one, Metrics: []string{"revenue"}})
	f, err := excelize.OpenReader(strings.NewReader(got))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	if sheets := f.GetSheetList(); len(sheets) != 1 || sheets[0] != "Revenue" {
		t.Fatalf("sheets = %v", sheets)
	}
	// A numeric cell carries no type attribute (or "n"); text would be a
	// shared or inline string.
	typ, err := f.GetCellType("Revenue", "C4")
	if err != nil || (typ != excelize.CellTypeNumber && typ != excelize.CellTypeUnset) {
		t.Errorf("C4 type = %v (err %v), want a number", typ, err)
	}
	if typ, _ := f.GetCellType("Revenue", "A2"); typ != excelize.CellTypeSharedString && typ != excelize.CellTypeInlineString {
		t.Errorf("A2 (a member code) type = %v, want a string — the number check above must be able to fail", typ)
	}
	if v, _ := f.GetCellValue("Revenue", "C4", excelize.Options{RawCellValue: true}); v != "50.1" {
		t.Errorf("C4 = %q, want 50.1 (rounded to 1 decimal)", v)
	}
	if v, _ := f.GetCellValue("Revenue", "A1"); v != "geography" {
		t.Errorf("A1 = %q", v)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	g := fixtureGrid()
	if p := Validate(Spec{}, g); len(p) != 0 {
		t.Fatalf("the default spec must be valid: %v", p)
	}
	cases := []struct {
		name string
		spec Spec
		want string
	}{
		{"unknown metric", Spec{Metrics: []string{"revenu"}}, `metric "revenu" is not in grid "Sales" (its metrics: revenue, margin_pct)`},
		{"aggregate", Spec{Dimensions: []string{"geography"}}, `dimension "period" is not a column`},
		{"pinned to a parent", Spec{Dimensions: []string{"geography"}, Filters: map[string][]string{"period": {"FY26"}}}, `dimension "period" is not a column`},
		{"pivot without dimension", Spec{Layout: "pivot"}, "needs pivot_dimension"},
		{"pivot dimension listed", Spec{Layout: "pivot", PivotDimension: "period", Dimensions: []string{"geography", "period"}}, "is the pivot dimension"},
		{"comma decimal and delimiter", Spec{DecimalSeparator: ","}, "needs another delimiter"},
		{"unknown filter member", Spec{Filters: map[string][]string{"geography": {"FR"}}}, `member(s) FR`},
		{"unknown column name", Spec{ColumnNames: map[string]string{"revnue": "Revenue"}}, `column_names key "revnue"`},
		{"colliding renames", Spec{ColumnNames: map[string]string{"revenue": "X", "margin_pct": "x"}}, `both be named`},
		{"bad format", Spec{Format: "pdf"}, "format must be"},
	}
	for _, c := range cases {
		problems := Validate(c.spec, g)
		if !strings.Contains(strings.Join(problems, "; "), c.want) {
			t.Errorf("%s: problems %v, want one containing %q", c.name, problems, c.want)
		}
	}
}

func TestFileNameAndDescribe(t *testing.T) {
	if got := FileName(Spec{Format: "xlsx"}, "Sales export / EU"); got != "Sales_export__EU.xlsx" {
		t.Errorf("file name = %q", got)
	}
	if got := FileName(Spec{FileName: "../../etc"}, "x"); got != "etc.csv" {
		t.Errorf("file name = %q", got)
	}
	if d := Describe(Spec{Layout: "pivot", PivotDimension: "period", Delimiter: ";"}); !strings.Contains(d, "pivot on period") || !strings.Contains(d, `delimiter ";"`) {
		t.Errorf("describe = %q", d)
	}
}

// A text metric's cells (migration 111) are written as their notes, in the
// wide and the long layout, never as the 0 the fact row holds.
func TestTextMetricWritesItsNotes(t *testing.T) {
	const noteID = "m-note"
	snap := fixtureSnapshot()
	snap.Metrics = append(snap.Metrics, Metric{ID: noteID, Name: "comment", Label: "Comment", DimensionIDs: []string{geoID, periodID}, Text: true})
	snap.Cells[noteID+":CA:Q1"] = 0
	snap.Texts = map[string]string{noteID + ":CA:Q1": "Retailer confirmed"}
	for _, layout := range []string{LayoutWide, LayoutLong} {
		tbl, err := Render(Spec{Layout: layout}, snap)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		var buf bytes.Buffer
		if err := Write(&buf, tbl); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), "Retailer confirmed") {
			t.Errorf("%s export lacks the note:\n%s", layout, buf.String())
		}
	}
}
