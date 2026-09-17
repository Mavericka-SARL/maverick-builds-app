package writeguard

// MemberEdge is one dimension_member's structural link to its parent, for
// bulk in-memory cascade computation over an already-loaded member universe
// (grid()/chart.go load every member of every dimension in one query anyway
// — walking AncestorChain per member there would be an avoidable N+1 DB
// round trip; ExpandHidden below does the same walk in memory instead).
// ParentID is "" for a root member (no parent_member_id). DimID is the
// member's dimension and is OPTIONAL: callers that provide it (the display
// filters) additionally get upward closure — a parent whose same-dimension
// children are all hidden becomes hidden itself; callers that leave it ""
// (the write paths, and any edge set built without dimension info) keep the
// downward-only cascade unchanged.
type MemberEdge struct {
	ID       string
	ParentID string
	DimID    string
}

// ExpandHidden returns every member ID that is hidden for a user — either
// directly (dimRules[id] == "hidden") or because an ancestor, walked via
// ParentID (crosses dimensions in one hop exactly like AncestorChain, e.g.
// an employee's parent is directly its cost-center member), is hidden.
// Depth-capped at 20, matching AncestorChain. Does not cascade "read" — a
// read-restricted ancestor does not make its descendants read-only, only an
// explicit direct rule on a member does; this matches the only pattern
// actually used today (hidden-only cost-center scoping) and keeps the
// cascade's blast radius limited to what's actually needed.
func ExpandHidden(edges []MemberEdge, dimRules map[string]string) map[string]bool {
	parent := make(map[string]string, len(edges))
	for _, e := range edges {
		parent[e.ID] = e.ParentID
	}

	memo := make(map[string]bool, len(edges))
	var hidden func(id string, depth int) bool
	hidden = func(id string, depth int) bool {
		if id == "" || depth > 20 {
			return false
		}
		if v, ok := memo[id]; ok {
			return v
		}
		memo[id] = false // break cycles defensively; a well-formed hierarchy never revisits
		result := dimRules[id] == "hidden"
		if !result {
			if p, ok := parent[id]; ok && p != "" {
				result = hidden(p, depth+1)
			}
		}
		memo[id] = result
		return result
	}

	out := make(map[string]bool)
	for id := range parent {
		if hidden(id, 0) {
			out[id] = true
		}
	}

	// Upward closure: a parent whose children are ALL hidden is itself
	// hidden — otherwise leaf-only rules ("hide UK and DE") leave the parent
	// (EMEA) standing as an empty husk in every grid, chart and export,
	// leaking structure the rules meant to remove. Two deliberate limits:
	// only children in the SAME dimension count (a cost-center whose
	// cross-dimension employee children are all hidden may still carry its
	// own directly-entered data — over-hiding it would be a new leak the
	// other way), and edges without DimID never participate, which is what
	// keeps the write paths' behavior byte-for-byte identical. Iterated to a
	// fixpoint so a fully-hidden subtree collapses through every level,
	// bounded by the same depth cap as the downward walk.
	childrenByParent := make(map[string][]MemberEdge)
	dimOf := make(map[string]string, len(edges))
	for _, e := range edges {
		dimOf[e.ID] = e.DimID
	}
	for _, e := range edges {
		if e.ParentID != "" && e.DimID != "" && e.DimID == dimOf[e.ParentID] {
			childrenByParent[e.ParentID] = append(childrenByParent[e.ParentID], e)
		}
	}
	for range [20]struct{}{} {
		changed := false
		for parentID, children := range childrenByParent {
			if out[parentID] || dimRules[parentID] == "read" {
				continue
			}
			allHidden := true
			for _, c := range children {
				if !out[c.ID] {
					allHidden = false
					break
				}
			}
			if allHidden {
				out[parentID] = true
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	return out
}
