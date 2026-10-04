package metricformula

import (
	"github.com/mavericks-engine/mavericks/internal/formula"
)

// checkPeriodComparisons refuses comparing a time dimension's bare name with
// a number. In a formula the bare name is the period's CODE — text such as
// 2026-01 — so `month <= actual_through_month` compares text with a number
// and is never what was meant; it saved cleanly and computed nothing useful
// (the AI Developer wrote it live, twice). A comparison with text
// (month = "2026-01", month >= "2026-07") is a real one and stays allowed.
func checkPeriodComparisons(node formula.Node, rd *revisionDims) error {
	var walk func(n formula.Node) error
	walk = func(n formula.Node) error {
		switch v := n.(type) {
		case *formula.BinaryExpr:
			switch v.Op {
			case "<", "<=", ">", ">=", "=", "<>":
				for _, pair := range [][2]formula.Node{{v.Left, v.Right}, {v.Right, v.Left}} {
					if dim := timeDimIdent(pair[0], rd); dim != "" && numericOperand(pair[1], rd) {
						return invalid("%q is a time dimension: in a formula its bare name is the period's code (text such as 2026-01), so comparing it with a number never works. "+
							"Read the current period's month number as MONTH(START()) and its year as YEAR(START()) — for example MONTH(START()) <= actual_through_month — "+
							"or compare the code with text: %s >= \"2026-07\"", dim, dim)
					}
				}
			}
			if err := walk(v.Left); err != nil {
				return err
			}
			return walk(v.Right)
		case *formula.UnaryExpr:
			return walk(v.Expr)
		case *formula.CallExpr:
			for _, a := range v.Args {
				if err := walk(a); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(node)
}

// timeDimIdent is the name of the time dimension a bare identifier names,
// or "".
func timeDimIdent(n formula.Node, rd *revisionDims) string {
	id, ok := n.(*formula.Ident)
	if !ok {
		return ""
	}
	if d := rd.lookup(id.Name); d != nil && d.isTime {
		return id.Name
	}
	return ""
}

// numericOperand reports an operand that is certainly a number: a number
// literal, arithmetic, or a name that is not a dimension (a metric).
func numericOperand(n formula.Node, rd *revisionDims) bool {
	switch v := n.(type) {
	case *formula.NumberLit:
		return true
	case *formula.UnaryExpr:
		return v.Op == "-" || v.Op == "+"
	case *formula.BinaryExpr:
		switch v.Op {
		case "+", "-", "*", "/", "^":
			return true
		}
	case *formula.Ident:
		return rd.lookup(v.Name) == nil
	}
	return false
}
