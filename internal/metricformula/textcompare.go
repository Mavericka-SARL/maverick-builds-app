package metricformula

import (
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// textComparedOnly reports whether every reference to name in node is an
// operand of a comparison (chk_status = "OK", stage <> "Lost") or the
// argument of ISBLANK: the one way a number formula reads a text metric.
// The comparison's result is a number (TRUE 1, FALSE 0), so a count of
// accepted months is SUM(IF(chk_status = "OK", 1, 0)) over the text the
// workbook shows — before, it needed a numeric twin of the status
// (restaurant model, 2026-10-31). Anywhere else (arithmetic, a function
// argument, LOOKUP) the text would read as 0.
func textComparedOnly(node formula.Node, name string) bool {
	ok := true
	var walk func(n formula.Node, allowed bool)
	walk = func(n formula.Node, allowed bool) {
		switch v := n.(type) {
		case *formula.Ident:
			if strings.EqualFold(v.Name, name) && !allowed {
				ok = false
			}
		case *formula.BinaryExpr:
			cmp := false
			switch v.Op {
			case "=", "<>", "<", "<=", ">", ">=":
				cmp = true
			}
			walk(v.Left, cmp)
			walk(v.Right, cmp)
		case *formula.UnaryExpr:
			walk(v.Expr, false)
		case *formula.CallExpr:
			isBlank := strings.EqualFold(v.Name, "ISBLANK")
			for _, a := range v.Args {
				walk(a, isBlank)
			}
		}
	}
	walk(node, false)
	return ok
}
