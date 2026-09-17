package query

import "testing"

// A rollup member is the sum of everything beside it, so plotting "All
// Regions" next to Moscow and Saint-Petersburg gives one bar roughly twice the
// height of the others and flattens the comparison the chart exists to show.
// hide_rollup_members drops those from the plotted axis.
func TestLeafMembersOnlyDropsRollups(t *testing.T) {
	members := []dimMember{
		{Code: "ALL", Label: "All Regions"},
		{Code: "MSK", Label: "Moscow", ParentCode: "ALL"},
		{Code: "SPB", Label: "Saint-Petersburg", ParentCode: "ALL"},
	}
	got := leafMembersOnly(members)
	if len(got) != 2 || got[0].Code != "MSK" || got[1].Code != "SPB" {
		t.Fatalf("got %v, want the two leaves", got)
	}
}

// Parenthood is judged against the members actually being plotted, not the
// dimension as a whole. A caller who can see only a rollup — because access
// rules hid its children — must keep it: it is the only data they have, and
// treating it as a removable duplicate would empty the chart.
func TestLeafMembersOnlyKeepsARollupWhoseChildrenAreNotPresent(t *testing.T) {
	got := leafMembersOnly([]dimMember{{Code: "ALL", Label: "All Regions"}})
	if len(got) != 1 || got[0].Code != "ALL" {
		t.Fatalf("got %v, want the rollup kept when nothing rolls up into it", got)
	}
}

// A deeper hierarchy: only the true leaves survive, not merely the root.
func TestLeafMembersOnlyHandlesMultipleLevels(t *testing.T) {
	members := []dimMember{
		{Code: "ALL"},
		{Code: "EU", ParentCode: "ALL"},
		{Code: "FR", ParentCode: "EU"},
		{Code: "DE", ParentCode: "EU"},
		{Code: "US", ParentCode: "ALL"},
	}
	got := leafMembersOnly(members)
	want := map[string]bool{"FR": true, "DE": true, "US": true}
	if len(got) != 3 {
		t.Fatalf("got %d members, want 3", len(got))
	}
	for _, m := range got {
		if !want[m.Code] {
			t.Errorf("%s is a rollup and should have been dropped", m.Code)
		}
	}
}
