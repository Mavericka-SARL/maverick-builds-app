package rollup

import "testing"

// TestMemberIndexFollowsMembers: the index answers as the scans it replaced
// did, and a Dimension given another member list is indexed afresh.
func TestMemberIndexFollowsMembers(t *testing.T) {
	d := &Dimension{ID: "r", Members: []Member{{Code: "W"}, {Code: "EU", ParentCode: "W"}, {Code: "DE", ParentCode: "EU"}, {Code: "FR", ParentCode: "EU"}, {Code: "DE", ParentCode: "X"}}}
	if c := childrenOf(d, "EU"); len(c) != 2 || c[0].Code != "DE" || c[1].Code != "FR" {
		t.Fatalf("children of EU: %v", c)
	}
	if m := findMember(d, "DE"); m == nil || m.ParentCode != "EU" {
		t.Fatalf("first DE: %v", m)
	}
	if childrenOf(d, "") != nil || findMember(d, "NOPE") != nil {
		t.Fatal("no parent code and an unknown code find nothing")
	}
	_ = append(childrenOf(d, "EU"), Member{Code: "IT"})
	if c := childrenOf(d, "EU"); len(c) != 2 {
		t.Fatalf("a caller's append changed the index: %v", c)
	}
	d.Members = append(d.Members[:4:4], Member{Code: "IT", ParentCode: "EU"})
	if c := childrenOf(d, "EU"); len(c) != 3 || c[2].Code != "IT" {
		t.Fatalf("after a member was added: %v", c)
	}
	d.Members = []Member{{Code: "EU"}, {Code: "ES", ParentCode: "EU"}}
	if c := childrenOf(d, "EU"); len(c) != 1 || c[0].Code != "ES" {
		t.Fatalf("after the members were replaced: %v", c)
	}
}
