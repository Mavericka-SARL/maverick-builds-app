package formula

import "strings"

// ExtractCalls returns every function name called in the formula, in source
// order, de-duplicated case-insensitively. Counterpart to ExtractIdents,
// which deliberately skips call names and returns only value references.
func ExtractCalls(text string) ([]string, error) {
	node, err := parse(text)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var results []string
	walkCalls(node, seen, &results)
	return results, nil
}

func walkCalls(node Node, seen map[string]bool, out *[]string) {
	if node == nil {
		return
	}
	switch n := node.(type) {
	case *UnaryExpr:
		walkCalls(n.Expr, seen, out)
	case *BinaryExpr:
		walkCalls(n.Left, seen, out)
		walkCalls(n.Right, seen, out)
	case *CallExpr:
		key := strings.ToUpper(n.Name)
		if !seen[key] {
			seen[key] = true
			*out = append(*out, n.Name)
		}
		for _, a := range n.Args {
			walkCalls(a, seen, out)
		}
	}
}

// IsBuiltin reports whether name resolves to a built-in function. Matching is
// case-insensitive, mirroring how evalCall looks names up. Callers that
// install custom functions via EvalContext.Funcs must check those separately —
// this covers the built-in set only.
func IsBuiltin(name string) bool {
	_, ok := builtins[strings.ToUpper(name)]
	return ok
}

// BuiltinNames returns every built-in function name, for error messages that
// want to suggest what IS available.
func BuiltinNames() []string {
	out := make([]string, 0, len(builtins))
	for name := range builtins {
		out = append(out, name)
	}
	return out
}
