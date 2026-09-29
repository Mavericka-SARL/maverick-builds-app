package formula

import "strings"

// ExtractIdents returns all identifier names referenced in a formula.
// Used for dependency graph extraction.
func ExtractIdents(text string) ([]string, error) {
	node, err := parse(text)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var results []string
	walkIdents(node, seen, &results)
	return results, nil
}

func walkIdents(node Node, seen map[string]bool, out *[]string) {
	if node == nil {
		return
	}
	switch n := node.(type) {
	case *Ident:
		key := strings.ToUpper(n.Name)
		if !seen[key] {
			seen[key] = true
			*out = append(*out, n.Name)
		}
	case *UnaryExpr:
		walkIdents(n.Expr, seen, out)
	case *BinaryExpr:
		walkIdents(n.Left, seen, out)
		walkIdents(n.Right, seen, out)
	case *CallExpr:
		for i, a := range n.Args {
			if isKeywordArg(n, i) {
				continue
			}
			walkIdents(a, seen, out)
		}
	}
}

// isKeywordArg reports whether argument i of call is a keyword position —
// LAG/LEAD's STRICT | SEMISTRICT | NONSTRICT, MOVINGSUM's and TIMESUM's
// SUM | AVERAGE | MIN | MAX — whose bare word is not a reference (Analyze
// treats these positions the same way).
func isKeywordArg(call *CallExpr, i int) bool {
	switch call.Name {
	case "LAG", "LEAD", "MOVINGSUM", "TIMESUM":
		return i == 3
	}
	return false
}
