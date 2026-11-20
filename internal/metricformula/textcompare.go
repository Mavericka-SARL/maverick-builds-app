package metricformula

import (
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// textInArithmetic reports whether name — a text metric — is an operand of
// arithmetic in node (+, -, *, /, ^, a unary minus) or a condition by itself
// (IF(chk_status, 1, 0)): there its text can only be #VALUE! or a wrong
// truth. A number formula reads a text metric everywhere else at the cell:
// compared (chk_status = "OK", the restaurant model's count of accepted
// months, 2026-10-31), joined with & ("W" & week_code), as a function's
// argument (LEN, LEFT, VALUE, TEXT) or a LOOKUP member (LOOKUP(drv, week,
// week_code)) — the 13-week cash model repeated TEXT(start, "yyyy-mm-dd") in
// every LOOKUP while the code was a metric of its own (2026-11-20).
func textInArithmetic(node formula.Node, name string) bool {
	found := false
	direct := func(n formula.Node) bool {
		id, ok := n.(*formula.Ident)
		return ok && strings.EqualFold(id.Name, name)
	}
	var walk func(n formula.Node)
	walk = func(n formula.Node) {
		switch v := n.(type) {
		case *formula.BinaryExpr:
			switch v.Op {
			case "+", "-", "*", "/", "^":
				if direct(v.Left) || direct(v.Right) {
					found = true
				}
			}
			walk(v.Left)
			walk(v.Right)
		case *formula.UnaryExpr:
			if direct(v.Expr) {
				found = true
			}
			walk(v.Expr)
		case *formula.CallExpr:
			// IF / IFS take a condition: the text alone there is no truth.
			if (strings.EqualFold(v.Name, "IF") || strings.EqualFold(v.Name, "IFS")) && len(v.Args) > 0 && direct(v.Args[0]) {
				found = true
			}
			for _, a := range v.Args {
				walk(a)
			}
		}
	}
	walk(node)
	return found
}
