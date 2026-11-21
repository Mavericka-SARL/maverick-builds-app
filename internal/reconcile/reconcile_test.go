package reconcile

import (
	"bytes"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestMapValidate(t *testing.T) {
	ok := Map{Rules: []Rule{{Name: "r", Sheet: "S", Rows: &Span{From: 2, To: 3}, MetricsByColumn: map[string]string{"B": "x"},
		Members: map[string]Binding{"item": {Column: "A"}}}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid map refused: %v", err)
	}
	zero := 0
	for name, m := range map[string]Map{
		"no rules":           {},
		"no sheet":           {Rules: []Rule{{Rows: &Span{From: 1, To: 1}, Metric: "x", ColumnList: []string{"A"}}}},
		"no rows":            {Rules: []Rule{{Sheet: "S", Metric: "x", ColumnList: []string{"A"}}}},
		"two metric kinds":   {Rules: []Rule{{Sheet: "S", Rows: &Span{From: 1, To: 1}, Metric: "x", MetricsByColumn: map[string]string{"A": "y"}}}},
		"rows vs metrics":    {Rules: []Rule{{Sheet: "S", Rows: &Span{From: 1, To: 2}, MetricsByRow: []string{"x"}, ColumnList: []string{"A"}}}},
		"two bindings":       {Rules: []Rule{{Sheet: "S", Rows: &Span{From: 1, To: 1}, Metric: "x", ColumnList: []string{"A"}, Members: map[string]Binding{"d": {Case: true, Column: "A"}}}}},
		"block_row no block": {Rules: []Rule{{Sheet: "S", Rows: &Span{From: 1, To: 1}, Metric: "x", ColumnList: []string{"A"}, Members: map[string]Binding{"d": {Column: "A", BlockRow: &zero}}}}},
		"bad op":             {Rules: []Rule{{Sheet: "S", Rows: &Span{From: 1, To: 1}, Metric: "x", ColumnList: []string{"A"}, OnlyWhen: []Condition{{Column: "A", Op: "~"}}}}},
		"bad column":         {Rules: []Rule{{Sheet: "S", Rows: &Span{From: 1, To: 1}, Metric: "x", ColumnList: []string{"1A"}}}},
	} {
		if err := m.Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := ValidateCases([]Case{{Name: "a"}, {Name: "A"}}); err == nil {
		t.Error("a case listed twice must be refused")
	}
	if err := ValidateCases([]Case{{Name: "a", Edits: []Edit{{Range: "S!A1:A2"}}}}); err == nil {
		t.Error("a range edit needs copy_from")
	}
}

func TestISODatesAreSerials(t *testing.T) {
	for in, want := range map[string]string{
		"2026-11-16T00:00:00.000Z": "46342",
		"2026-11-16T12:00:00Z":     "46342.5",
		"2026-11-16T06:00":         "46342.25",
	} {
		if got := isoSerial(in); got != want || !isoDateTime.MatchString(in) {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	if isoDateTime.MatchString("2026-11-16") {
		t.Error("a bare date text stays text (a member code may look like one)")
	}
}

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, op, b string
		want     bool
	}{
		{"46349", ">=", "46342", true}, {"46335", ">=", "46342", false}, {"10", "=", "10.0", true},
		{"DC", "=", "dc", true}, {"DC", "<>", "Store", true}, {"2", "<", "10", true}, {"b", ">", "a", true},
	} {
		if got := compare(c.a, c.op, c.b); got != c.want {
			t.Errorf("%s %s %s = %v", c.a, c.op, c.b, got)
		}
	}
}

func TestApplyEdits(t *testing.T) {
	x := excelize.NewFile()
	_ = x.SetSheetName("Sheet1", "S")
	_ = x.SetCellValue("S", "A1", 1)
	_ = x.SetCellValue("S", "A2", 2)
	_ = x.SetCellFormula("S", "B2", "A2*10")
	var buf bytes.Buffer
	_ = x.Write(&buf)
	_, err := applyEdits(buf.Bytes(), []Edit{{Cell: "S!A1", Value: "7"}, {Range: "S!C1:C2", CopyFrom: "S!A2:B2"}})
	if err == nil {
		t.Fatal("ranges of different shapes must be refused")
	}
	out, err := applyEdits(buf.Bytes(), []Edit{{Cell: "S!A1", Value: "7"}, {Range: "S!C1:D1", CopyFrom: "S!A2:B2"}})
	if err != nil {
		t.Fatal(err)
	}
	y, err := excelize.OpenReader(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := y.GetCellValue("S", "A1"); v != "7" {
		t.Errorf("A1 = %q, want 7 (a number)", v)
	}
	if v, _ := y.GetCellValue("S", "C1"); v != "2" {
		t.Errorf("C1 = %q, want the copied value 2", v)
	}
	if f, _ := y.GetCellFormula("S", "D1"); !strings.EqualFold(f, "A2*10") {
		t.Errorf("D1 formula %q, want the copied formula", f)
	}
	if _, err := applyEdits(buf.Bytes(), []Edit{{Cell: "A1", Value: 1}}); err == nil {
		t.Error("an edit must name its sheet")
	}
}

func TestRecalculateNeedsLibreOffice(t *testing.T) {
	if _, err := recalculate(t.Context(), "", nil); err != ErrNoRecalculation {
		t.Errorf("no LibreOffice configured: %v", err)
	}
}
