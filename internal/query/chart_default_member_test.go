package query

import "testing"

// The member a restricted viewer's hidden context default is replaced with
// must be the one the client's selector starts on (defaultLeafCode in
// web/src/consoles/dashboardLayout.ts): the first leaf of the member tree
// walked depth-first, siblings in the order members come.
func TestDefaultVisibleCodeIsTheTreesFirstLeaf(t *testing.T) {
	for _, tc := range []struct {
		name    string
		members []dimMember
		want    string
	}{
		{
			// Every sort_order 0: the API sends code order, which is not
			// depth-first. The flat list's first leaf is NYC; the tree's
			// (ALL › EU › PARIS) is PARIS.
			name: "code order is not depth-first",
			members: []dimMember{
				{Code: "ALL"},
				{Code: "EU", ParentCode: "ALL"},
				{Code: "NYC", ParentCode: "US"},
				{Code: "PARIS", ParentCode: "EU"},
				{Code: "US", ParentCode: "ALL"},
			},
			want: "PARIS",
		},
		{
			// Siblings keep their order, not code order.
			name: "sibling order is kept",
			members: []dimMember{
				{Code: "ALL"},
				{Code: "US", ParentCode: "ALL"},
				{Code: "EU", ParentCode: "ALL"},
				{Code: "PARIS", ParentCode: "EU"},
				{Code: "NYC", ParentCode: "US"},
			},
			want: "NYC",
		},
		{
			// A member whose parent the viewer cannot see is a root.
			name: "hidden parent makes a root",
			members: []dimMember{
				{Code: "B", ParentCode: "HIDDEN"},
				{Code: "A", ParentCode: "HIDDEN"},
			},
			want: "B",
		},
		{
			name:    "a lone rollup is its own leaf",
			members: []dimMember{{Code: "ALL"}},
			want:    "ALL",
		},
		{
			// No root at all: the first member, as the client falls back.
			name: "cycle falls back to the first member",
			members: []dimMember{
				{Code: "X", ParentCode: "Y"},
				{Code: "Y", ParentCode: "X"},
			},
			want: "X",
		},
		{name: "nothing visible", members: nil, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultVisibleCode(tc.members); got != tc.want {
				t.Errorf("defaultVisibleCode = %q, want %q", got, tc.want)
			}
		})
	}
}
