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
		for _, a := range n.Args {
			walkIdents(a, seen, out)
		}
	}
}
