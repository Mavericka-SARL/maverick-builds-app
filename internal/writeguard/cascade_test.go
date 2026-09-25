package writeguard

import "testing"

func TestExpandHidden(t *testing.T) {
	// region -> cost_center -> employee, three levels, mirroring the
	// employees/cost_centers/regions shape of the former salary demo.
	edges := []MemberEdge{
		{ID: "region-a", ParentID: ""},
		{ID: "cc-ga", ParentID: "region-a"},
		{ID: "cc-sales", ParentID: "region-a"},
		{ID: "emp-ga-1", ParentID: "cc-ga"},
		{ID: "emp-ga-2", ParentID: "cc-ga"},
		{ID: "emp-sales-1", ParentID: "cc-sales"},
	}

	tests := []struct {
		name        string
		dimRules    map[string]string
		wantHidden  []string
		wantVisible []string
	}{
		{
			name:        "direct hidden, no rules elsewhere",
			dimRules:    map[string]string{"cc-sales": "hidden"},
			wantHidden:  []string{"cc-sales", "emp-sales-1"},
			wantVisible: []string{"region-a", "cc-ga", "emp-ga-1", "emp-ga-2"},
		},
		{
			name:        "hidden via 2-level ancestor chain",
			dimRules:    map[string]string{"region-a": "hidden"},
			wantHidden:  []string{"region-a", "cc-ga", "cc-sales", "emp-ga-1", "emp-ga-2", "emp-sales-1"},
			wantVisible: nil,
		},
		{
			name:        "unrelated sibling stays visible",
			dimRules:    map[string]string{"cc-ga": "hidden"},
			wantHidden:  []string{"cc-ga", "emp-ga-1", "emp-ga-2"},
			wantVisible: []string{"region-a", "cc-sales", "emp-sales-1"},
		},
		{
			name:        "read on an ancestor does not cascade",
			dimRules:    map[string]string{"cc-sales": "read"},
			wantHidden:  nil,
			wantVisible: []string{"region-a", "cc-ga", "cc-sales", "emp-ga-1", "emp-ga-2", "emp-sales-1"},
		},
		{
			name:        "no rules at all",
			dimRules:    map[string]string{},
			wantHidden:  nil,
			wantVisible: []string{"region-a", "cc-ga", "cc-sales", "emp-ga-1", "emp-ga-2", "emp-sales-1"},
		},
		{
			name:        "direct hidden on a leaf does not affect its siblings or ancestors",
			dimRules:    map[string]string{"emp-ga-1": "hidden"},
			wantHidden:  []string{"emp-ga-1"},
			wantVisible: []string{"region-a", "cc-ga", "cc-sales", "emp-ga-2", "emp-sales-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExpandHidden(edges, tt.dimRules)
			for _, id := range tt.wantHidden {
				if !got[id] {
					t.Errorf("ExpandHidden(%v) missing hidden member %q, got %v", tt.dimRules, id, got)
				}
			}
			for _, id := range tt.wantVisible {
				if got[id] {
					t.Errorf("ExpandHidden(%v) incorrectly hid member %q, got %v", tt.dimRules, id, got)
				}
			}
		})
	}
}

// TestExpandHiddenDepthCapDoesNotFalsePositive proves a legitimately deep
// chain (well under the 20-level cap) still resolves correctly all the way
// to its hidden root, and that the cap itself doesn't trip on a normal-depth
// hierarchy.
func TestExpandHiddenDepthCapDoesNotFalsePositive(t *testing.T) {
	const chainLen = 10
	edges := make([]MemberEdge, 0, chainLen)
	prev := ""
	for i := 0; i < chainLen; i++ {
		id := "n" + string(rune('a'+i))
		edges = append(edges, MemberEdge{ID: id, ParentID: prev})
		prev = id
	}
	root := edges[0].ID
	leaf := edges[len(edges)-1].ID

	got := ExpandHidden(edges, map[string]string{root: "hidden"})
	if !got[leaf] {
		t.Errorf("leaf %q at depth %d from hidden root %q should be hidden, got %v", leaf, chainLen-1, root, got)
	}

	gotNone := ExpandHidden(edges, map[string]string{})
	if len(gotNone) != 0 {
		t.Errorf("no rules at all should hide nothing, got %v", gotNone)
	}
}

// Upward closure: a parent whose same-dimension children are all hidden is
// itself hidden — leaf-only rules must not leave the parent standing as an
// empty husk (found live: hiding UK and DE left EMEA visible in every grid
// and chart). Constrained to same-dimension children with a known DimID so
// cross-dimension rollup parents and the DimID-less write paths keep the old
// downward-only behavior.
func TestExpandHiddenCollapsesFullyHiddenParents(t *testing.T) {
	geo := "dim-geo"
	edges := []MemberEdge{
		{ID: "WORLD", ParentID: "", DimID: geo},
		{ID: "EMEA", ParentID: "WORLD", DimID: geo},
		{ID: "AMER", ParentID: "WORLD", DimID: geo},
		{ID: "UK", ParentID: "EMEA", DimID: geo},
		{ID: "DE", ParentID: "EMEA", DimID: geo},
		{ID: "US", ParentID: "AMER", DimID: geo},
		{ID: "CA", ParentID: "AMER", DimID: geo},
	}
	out := ExpandHidden(edges, map[string]string{"UK": "hidden", "DE": "hidden", "US": "hidden"})
	for _, want := range []string{"UK", "DE", "US", "EMEA"} {
		if !out[want] {
			t.Errorf("%s should be hidden (EMEA: all children hidden)", want)
		}
	}
	for _, visible := range []string{"CA", "AMER", "WORLD"} {
		if out[visible] {
			t.Errorf("%s must stay visible (has a visible descendant)", visible)
		}
	}
}

func TestExpandHiddenUpwardClosureIsTransitive(t *testing.T) {
	p := "dim-period"
	edges := []MemberEdge{
		{ID: "FY", ParentID: "", DimID: p},
		{ID: "H1", ParentID: "FY", DimID: p}, {ID: "H2", ParentID: "FY", DimID: p},
		{ID: "Q1", ParentID: "H1", DimID: p}, {ID: "Q2", ParentID: "H1", DimID: p},
		{ID: "Q3", ParentID: "H2", DimID: p}, {ID: "Q4", ParentID: "H2", DimID: p},
	}
	out := ExpandHidden(edges, map[string]string{
		"Q1": "hidden", "Q2": "hidden", "Q3": "hidden", "Q4": "hidden",
	})
	for _, want := range []string{"H1", "H2", "FY"} {
		if !out[want] {
			t.Errorf("%s should collapse: every leaf below it is hidden", want)
		}
	}
}

func TestExpandHiddenUpwardClosureLimits(t *testing.T) {
	// Cross-dimension children (employees under a cost-center) must not
	// hide the parent: it may carry its own directly-entered data.
	edges := []MemberEdge{
		{ID: "CC1", ParentID: "", DimID: "dim-cc"},
		{ID: "E1", ParentID: "CC1", DimID: "dim-emp"},
		{ID: "E2", ParentID: "CC1", DimID: "dim-emp"},
	}
	out := ExpandHidden(edges, map[string]string{"E1": "hidden", "E2": "hidden"})
	if out["CC1"] {
		t.Error("cross-dimension children must not collapse their parent")
	}

	// Without DimID (the write paths) the closure never engages.
	plain := []MemberEdge{
		{ID: "P", ParentID: ""},
		{ID: "C1", ParentID: "P"}, {ID: "C2", ParentID: "P"},
	}
	out = ExpandHidden(plain, map[string]string{"C1": "hidden", "C2": "hidden"})
	if out["P"] {
		t.Error("edges without DimID must keep the downward-only behavior")
	}

	// An explicit "read" rule on the parent is an instruction to show it
	// read-only; the closure must not upgrade it to hidden.
	geo := []MemberEdge{
		{ID: "R", ParentID: "", DimID: "d"},
		{ID: "K1", ParentID: "R", DimID: "d"}, {ID: "K2", ParentID: "R", DimID: "d"},
	}
	out = ExpandHidden(geo, map[string]string{"K1": "hidden", "K2": "hidden", "R": "read"})
	if out["R"] {
		t.Error("an explicit read rule must keep the parent visible")
	}
}
