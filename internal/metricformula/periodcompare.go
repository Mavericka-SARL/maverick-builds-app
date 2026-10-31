package metricformula

import (
	"strconv"
	"strings"

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
			if dim, crit := periodCriterion(v, rd); dim != "" {
				return invalid("%s(…, %s, %s) compares %q's period codes (text such as 2026-01) with a number — %s is a date or a value, never a code — so it matches nothing and reads 0. "+
					"A plain reference already reads the cell's own period; another period is LOOKUP(x, %s, \"2026-01\") or a time function (PREVIOUS, LAG, CUMULATE); "+
					"a range of periods compares the code with text: %s, \">=2026-07\"", strings.ToUpper(v.Name), dim, criterionText(crit), dim, criterionText(crit), dim, dim)
			}
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

// periodCriterion finds, in a conditional aggregation (SUMIFS, COUNTIF, …),
// a time dimension given as a criteria range with a number for its
// criterion: SUMIFS(x, month, START()) tests the period code against a
// date and is 0 everywhere (asked for three times in the restaurant
// rebuild, 2026-10-31). It returns the dimension and the criterion.
func periodCriterion(call *formula.CallExpr, rd *revisionDims) (string, formula.Node) {
	var first int // index of the first criteria range
	switch strings.ToUpper(call.Name) {
	case "SUMIFS", "AVERAGEIFS", "MINIFS", "MAXIFS":
		first = 1
	case "COUNTIFS", "COUNTIF", "SUMIF", "AVERAGEIF":
		first = 0
	default:
		return "", nil
	}
	for i := first; i+1 < len(call.Args); i += 2 {
		if dim := timeDimIdent(call.Args[i], rd); dim != "" && (dateCall(call.Args[i+1]) || numberExpr(call.Args[i+1])) {
			return dim, call.Args[i+1]
		}
		if strings.HasSuffix(strings.ToUpper(call.Name), "IF") {
			break // SUMIF/COUNTIF/AVERAGEIF: one range and one criterion
		}
	}
	return "", nil
}

// criterionText names a criterion in a message: START(), a metric's name,
// a number.
func criterionText(n formula.Node) string {
	switch v := n.(type) {
	case *formula.CallExpr:
		return strings.ToUpper(v.Name) + "(…)"
	case *formula.Ident:
		return v.Name
	case *formula.NumberLit:
		return strconv.FormatFloat(v.Val, 'f', -1, 64)
	}
	return "the criterion"
}

// numberExpr: a number literal or arithmetic — a criterion that is
// certainly no code. A metric's name is not one: a pick-list of months
// reads as the member's code.
func numberExpr(n formula.Node) bool {
	if _, ok := n.(*formula.Ident); ok {
		return false
	}
	return numericOperand(n, nil)
}

// dateCall: a call whose result is a date serial (START(), END(), DATE(…)).
func dateCall(n formula.Node) bool {
	c, ok := n.(*formula.CallExpr)
	if !ok {
		return false
	}
	switch strings.ToUpper(c.Name) {
	case "START", "END", "DATE", "TODAY", "EDATE", "EOMONTH":
		return true
	}
	return false
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
