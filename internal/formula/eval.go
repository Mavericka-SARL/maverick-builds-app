package formula

import (
	"fmt"
	"math"
	"strings"
)

// CustomFunc is a function that receives unevaluated args for lazy evaluation,
// or pre-evaluated values — callers decide by convention.
type CustomFunc func(ctx *EvalContext, args []Node) Value

// EvalContext holds variable bindings and optional custom/override functions.
type EvalContext struct {
	// Vars maps upper-cased identifier names to Values.
	Vars map[string]Value
	// Funcs overrides or extends built-in functions (keys should be UPPER-CASE).
	Funcs map[string]CustomFunc
}

func (ctx *EvalContext) lookup(name string) (Value, bool) {
	if ctx == nil || ctx.Vars == nil {
		return BlankVal(), false
	}
	v, ok := ctx.Vars[strings.ToUpper(name)]
	return v, ok
}

// eval evaluates a node within a context.
func (ctx *EvalContext) eval(node Node) Value {
	switch n := node.(type) {
	case *NumberLit:
		return NumberVal(n.Val)
	case *StringLit:
		return StringVal(n.Val)
	case *BoolLit:
		return BoolVal(n.Val)
	case *Ident:
		if v, ok := ctx.lookup(n.Name); ok {
			return v
		}
		return ErrorVal(errName(n.Name))

	case *UnaryExpr:
		v := ctx.eval(n.Expr)
		if v.IsError() {
			return v
		}
		if n.Op == "-" {
			num, ok := v.Number()
			if !ok {
				return ErrorVal(ErrValue)
			}
			return NumberVal(-num)
		}
		return v

	case *BinaryExpr:
		return ctx.evalBinary(n)

	case *CallExpr:
		return ctx.evalCall(n)
	}
	return ErrorVal(errValue(fmt.Sprintf("unknown node type %T", node)))
}

func (ctx *EvalContext) evalBinary(n *BinaryExpr) Value {
	// Comparison operators
	switch n.Op {
	case "=", "<>", "<", "<=", ">", ">=":
		left := ctx.eval(n.Left)
		right := ctx.eval(n.Right)
		if left.IsError() {
			return left
		}
		if right.IsError() {
			return right
		}
		cmp, ferr := compareValues(left, right)
		if ferr != nil {
			return ErrorVal(ferr)
		}
		var result bool
		switch n.Op {
		case "=":
			result = cmp == 0
		case "<>":
			result = cmp != 0
		case "<":
			result = cmp < 0
		case "<=":
			result = cmp <= 0
		case ">":
			result = cmp > 0
		case ">=":
			result = cmp >= 0
		}
		return BoolVal(result)

	case "&":
		left := ctx.eval(n.Left)
		right := ctx.eval(n.Right)
		if left.IsError() {
			return left
		}
		if right.IsError() {
			return right
		}
		return StringVal(left.String() + right.String())
	}

	// Arithmetic
	left := ctx.eval(n.Left)
	right := ctx.eval(n.Right)
	if left.IsError() {
		return left
	}
	if right.IsError() {
		return right
	}
	lnum, lok := left.Number()
	rnum, rok := right.Number()
	if !lok || !rok {
		return ErrorVal(ErrValue)
	}
	switch n.Op {
	case "+":
		return NumberVal(lnum + rnum)
	case "-":
		return NumberVal(lnum - rnum)
	case "*":
		return NumberVal(lnum * rnum)
	case "/":
		if rnum == 0 {
			return ErrorVal(ErrDiv0)
		}
		return NumberVal(lnum / rnum)
	case "^":
		result := math.Pow(lnum, rnum)
		if math.IsNaN(result) || math.IsInf(result, 0) {
			return ErrorVal(ErrNum)
		}
		return NumberVal(result)
	}
	return ErrorVal(errValue("unknown operator " + n.Op))
}

func (ctx *EvalContext) evalCall(n *CallExpr) Value {
	// Check custom functions first
	if ctx != nil && ctx.Funcs != nil {
		if fn, ok := ctx.Funcs[n.Name]; ok {
			return fn(ctx, n.Args)
		}
	}
	// Built-in functions
	if fn, ok := builtins[n.Name]; ok {
		return fn(ctx, n.Args)
	}
	return ErrorVal(&FormulaError{Code: "#NAME?", Message: fmt.Sprintf("Unknown function: %s", n.Name)})
}

// evalArgs evaluates all args eagerly, short-circuiting on first error.
func (ctx *EvalContext) evalArgs(args []Node) ([]Value, Value) {
	vals := make([]Value, len(args))
	for i, a := range args {
		v := ctx.eval(a)
		if v.IsError() {
			return nil, v
		}
		vals[i] = v
	}
	return vals, Value{}
}

func requireArgCount(name string, args []Node, min, max int) *FormulaError {
	n := len(args)
	if n < min || (max >= 0 && n > max) {
		return errValue(fmt.Sprintf("%s: wrong number of arguments (%d)", name, n))
	}
	return nil
}
