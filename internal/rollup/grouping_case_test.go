package rollup

import (
	"slices"
	"testing"
)

// TestPropertyGroupingKeyCase: a grouping by the declared property "area"
// counts a member whose value is stored under "Area" — formulas read
// dim.property case-insensitively, and so does the grouping.
func TestPropertyGroupingKeyCase(t *testing.T) {
	dims := map[string]*Dimension{
		"emp": {ID: "emp", Members: []Member{
			{Code: "E1", Properties: map[string]string{"area": "North"}},
			{Code: "E2", Properties: map[string]string{"Area": "North"}},
			{Code: "E3", Properties: map[string]string{"AREA": "South"}},
		}},
		"area": {ID: "area", SourceDimensionID: "emp", SourceProperty: "area",
			Members: []Member{{Code: "North"}, {Code: "South"}}},
	}
	got := relate(dims, "emp", map[string]string{"area": "North"})
	slices.Sort(got)
	if !slices.Equal(got, []string{"E1", "E2"}) {
		t.Errorf("North groups %v, want [E1 E2]", got)
	}
	if v := PropertyValue(map[string]string{"Area": "x"}, "area"); v != "x" {
		t.Errorf("PropertyValue = %q, want x", v)
	}
}
