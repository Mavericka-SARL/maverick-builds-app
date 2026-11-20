package gateway

import (
	"fmt"
	"testing"
)

// A grid read carries the members of the dimensions it can need and leaves
// out those of a large unrelated one (2026-11-20: 5,000 records' members in
// every grid read).
func TestLeanAllDimensions(t *testing.T) {
	members := func(n int) []gridDimMember {
		out := make([]gridDimMember, n)
		for i := range out {
			out[i] = gridDimMember{Code: fmt.Sprintf("M%d", i)}
		}
		return out
	}
	ptr := func(s string) *string { return &s }
	all := []gridDimension{
		{ID: "week", DimensionType: "time", Members: members(300)},
		{ID: "scenario", DimensionType: "standard", Members: members(9)},
		{ID: "invoice", DimensionType: "standard", Members: members(2000)},
		{ID: "region", DimensionType: "standard", Members: members(250)},
		{ID: "territory", DimensionType: "standard", Members: members(400), ParentDimensionID: ptr("region")},
		{ID: "segment", DimensionType: "standard", Members: members(260), SourceDimensionID: ptr("territory")},
		{ID: "payment", DimensionType: "standard", Members: members(3000)},
	}
	grid := []gridDimension{all[1], all[4]} // scenario × territory
	metrics := []metricRow{{DimensionIDs: []string{"scenario", "territory"}}, {DimensionIDs: []string{"payment"}}}
	got := map[string]gridDimension{}
	for _, d := range leanAllDimensions(all, grid, metrics) {
		got[d.ID] = d
	}
	for id, want := range map[string]int{"week": 300, "scenario": 9, "territory": 400, "region": 250, "segment": 260, "payment": 3000} {
		if d := got[id]; d.MembersOmitted || len(d.Members) != want {
			t.Errorf("%s: %d members (omitted %v), want all %d", id, len(d.Members), d.MembersOmitted, want)
		}
	}
	if d := got["invoice"]; !d.MembersOmitted || len(d.Members) != 0 || d.MemberCount != 2000 {
		t.Errorf("invoice: omitted %v, %d members, count %d; want omitted, none, 2000", d.MembersOmitted, len(d.Members), d.MemberCount)
	}
	if len(all[2].Members) != 2000 {
		t.Error("leanAllDimensions changed the dimensions it was given")
	}
}
