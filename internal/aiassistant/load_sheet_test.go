package aiassistant

import (
	"os"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// The server reads a sheet with scenarios across its columns and labels down
// one column, and writes the import: unpivot, labels → metrics, skip headings.
func TestPlanSheetLoad(t *testing.T) {
	grid := [][]string{
		{"SCENARIO ASSUMPTIONS"},
		{},
		{"Driver", "Group", "Base", "Downside", "Upside", "Active scenario", "Units"},
		{"SaaS & demand"},
		{"SaaS commercial launch month", "SaaS", "5", "7", "4", "5", "month"},
		{"Qualified trial to paid conversion", "SaaS", "0.23", "0.15", "0.3", "0.23", "%"},
		{"Self-serve subscription ARPU", "SaaS", "59", "49", "69", "59", "€/mo"},
		{"Second factory commissioning month", "HW", "22", "26", "18", "22", "month"},
	}
	inv := LoadInventory{
		Dimensions: []LoadDimension{{Name: "scenario", Members: []LoadMember{{Code: "Base"}, {Code: "Downside"}, {Code: "Upside"}}},
			{Name: "month", Time: true, Members: []LoadMember{{Code: "2027-01", Start: "2027-01-01"}, {Code: "2027-02", Start: "2027-02-01"}}}},
		Metrics: []LoadMetric{
			{Name: "saas_commercial_launch_month", Grid: "Assumptions", GridID: "g1"},
			{Name: "trial_to_paid_conversion", Label: "Qualified trial to paid conversion", Format: "percentage", Grid: "Assumptions", GridID: "g1"},
			{Name: "self_serve_subscription_arpu_eur", Grid: "Assumptions", GridID: "g1"},
		},
		GridDims: map[string][]string{"g1": {"scenario"}},
	}
	sl, err := PlanSheetLoad(grid, inv, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if sl.HeaderRow != 3 || sl.LabelColumn != "Driver" || sl.Dimension != "scenario" || strings.Join(sl.Columns, ",") != "Base,Downside,Upside" || sl.GridID != "g1" {
		t.Fatalf("layout %+v", sl)
	}
	got := map[string]string{}
	for _, r := range sl.Rows {
		got[r.Label] = r.Match + ":" + r.Metric
	}
	want := map[string]string{
		"SaaS commercial launch month":       "exact:saas_commercial_launch_month",
		"Qualified trial to paid conversion": "label:trial_to_paid_conversion",
		"Self-serve subscription ARPU":       "nearest:self_serve_subscription_arpu_eur",
		"Second factory commissioning month": "none:",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s → %s, want %s", k, got[k], v)
		}
	}
	if !sl.Percent || len(sl.Unmatched()) != 1 || sl.Unmatched()[0].Suggested != "second_factory_commissioning_month" {
		t.Errorf("percent %v unmatched %+v", sl.Percent, sl.Unmatched())
	}
	if d := sl.Describe("Assumptions", "Assumptions (loaded)"); !strings.Contains(d, "create the 1 missing metric") || strings.Contains(d, "import_file_data") {
		t.Errorf("describe with a missing metric:\n%s", d)
	}
	if sl.Reshape.Unpivot == nil || sl.Reshape.Unpivot.NameColumn != "scenario" || sl.ColumnMap["Driver"] != "metric" || sl.Reshape.ValueMap["Driver"]["Qualified trial to paid conversion"] != "trial_to_paid_conversion" {
		t.Errorf("reshape %+v map %+v", sl.Reshape, sl.ColumnMap)
	}
	// Months across: Jan-27 matches the time member by its start.
	g2 := [][]string{{"Line", "Jan-27", "Feb-27"}, {"Opening cash", "100", "120"}}
	inv.Metrics = append(inv.Metrics, LoadMetric{Name: "opening_cash"})
	if sl, err := PlanSheetLoad(g2, inv, 0, ""); err != nil || sl.Dimension != "month" || sl.MemberOf["Jan-27"] != "2027-01" {
		t.Errorf("months across: %+v %v", sl, err)
	}
}

// On the real workbook: the Assumptions sheet reads as 72 driver rows by scenario.
func TestPlanSheetLoadWorkbook(t *testing.T) {
	p := os.Getenv("WB")
	if p == "" {
		t.Skip("WB not set")
	}
	raw, _ := os.ReadFile(p)
	grid, err := importpkg.ReadGrid("w.xlsx", raw, "Assumptions", ',')
	if err != nil {
		t.Fatal(err)
	}
	inv := LoadInventory{Dimensions: []LoadDimension{{Name: "scenario", Members: []LoadMember{{Code: "base", Label: "Base"}, {Code: "downside", Label: "Downside"}, {Code: "upside", Label: "Upside"}}}}}
	sl, err := PlanSheetLoad(grid, inv, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("header %d label %q cols %v rows %d percent %v first %+v", sl.HeaderRow, sl.LabelColumn, sl.Columns, len(sl.Rows), sl.Percent, sl.Rows[0])
	if sl.HeaderRow != 5 || len(sl.Rows) != 72 {
		t.Errorf("want header 5 and 72 rows")
	}
}
