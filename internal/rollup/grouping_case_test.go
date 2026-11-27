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

// TestTwoGroupingsIntersect: a cell pinning a country and a sector — both
// groupings of project — reads the projects of that country AND that sector.
// relate stopped at the first relation, so a country × sector grid showed the
// sector's total in every country (RW·Education 40 instead of nothing,
// 2026-11-27).
func TestTwoGroupingsIntersect(t *testing.T) {
	dims := map[string]*Dimension{
		"project": {ID: "project", Members: []Member{
			{Code: "P1", Properties: map[string]string{"country": "SN", "sector": "HEALTH"}},
			{Code: "P2", Properties: map[string]string{"country": "SN", "sector": "EDU"}},
			{Code: "P3", Properties: map[string]string{"country": "RW", "sector": "HEALTH"}},
		}},
		"country": {ID: "country", SourceDimensionID: "project", SourceProperty: "country",
			Members: []Member{{Code: "SN"}, {Code: "RW"}}},
		"sector": {ID: "sector", SourceDimensionID: "project", SourceProperty: "sector",
			Members: []Member{{Code: "HEALTH"}, {Code: "EDU"}}},
	}
	for _, c := range []struct {
		combo map[string]string
		want  []string
	}{
		{map[string]string{"country": "SN", "sector": "HEALTH"}, []string{"P1"}},
		{map[string]string{"country": "SN", "sector": "EDU"}, []string{"P2"}},
		{map[string]string{"country": "RW", "sector": "HEALTH"}, []string{"P3"}},
		{map[string]string{"country": "RW", "sector": "EDU"}, []string{}},
		{map[string]string{"country": "SN"}, []string{"P1", "P2"}},
		{map[string]string{"sector": "HEALTH"}, []string{"P1", "P3"}},
	} {
		got := relate(dims, "project", c.combo)
		slices.Sort(got)
		if got == nil || !slices.Equal(got, c.want) {
			t.Errorf("relate(project, %v) = %v (nil %v), want %v", c.combo, got, got == nil, c.want)
		}
	}
	if got := relate(dims, "project", map[string]string{}); got != nil {
		t.Errorf("no pinned relation: %v, want nil (every leaf)", got)
	}
}
