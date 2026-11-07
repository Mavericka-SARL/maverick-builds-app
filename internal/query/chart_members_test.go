package query

import "testing"

func TestChosenMembersKeepTheirOrder(t *testing.T) {
	all := []dimMember{{Code: "BASE"}, {Code: "DOWN"}, {Code: "UP"}, {Code: "DOWN_VS_BASE"}, {Code: "UP_VS_BASE"}}
	got := chosenMembers(all, []string{"UP_VS_BASE", "DOWN_VS_BASE", "GONE", "UP_VS_BASE"})
	if len(got) != 2 || got[0].Code != "UP_VS_BASE" || got[1].Code != "DOWN_VS_BASE" {
		t.Errorf("chosen %+v, want UP_VS_BASE then DOWN_VS_BASE", got)
	}
}
