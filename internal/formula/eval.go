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
	// Time is the current time coordinate, set by a caller evaluating a
	// time-dimensioned metric. nil for scalar evaluation: time functions then
	// return TIME_CONTEXT_REQUIRED rather than a silent zero.
	Time *TimeEvalContext
	// Dim is the member-metadata and dimensional-read context of the cell
	// (contract C1-C3): dim.property, PARENT, LOOKUP and the *IFS/*IF
	// family read through it. nil for an evaluator that has none: those
	// functions then return DIM_CONTEXT_REQUIRED rather than a silent zero.
	// A time-shifted child evaluation (TimeEvalContext.EvalAt) builds its
	// own EvalContext and must set Dim again, rebuilt for the SHIFTED
	// combo, sharing the parent's Dim.Memo.
	Dim *DimEvalContext
}

func (ctx *EvalContext) lookup(name string) (Value, bool) {
	if ctx == nil || ctx.Vars == nil {
		return BlankVal(), false
	}
	v, ok := ctx.Vars[strings.ToUpper(name)]
	return v, ok
}

// unboundDimension resolves a bare identifier that no variable binds but
// the cell's Dim context knows as a DIMENSION: blank when the dimension is
// not pinned in the cell (a total, or a rollup/slice row that leaves it
// open) — as dim.property and PARENT read there — and the member code when
// it is pinned but the caller did not bind it. ok is false without a Dim
// context or for a name that is not a dimension: that stays #NAME?.
// Variables are looked up first, so where a metric shares a dimension's
// name the metric is read only where the dimension is UNPINNED: every
// evaluator binds a pinned dimension's member code into Vars after the
// metric values, so at a pinned cell the bare name is the member code.
func (ctx *EvalContext) unboundDimension(name string) (Value, bool) {
	if ctx == nil || ctx.Dim == nil || ctx.Dim.Current == nil {
		return BlankVal(), false
	}
	code, pinned, err := ctx.Dim.Current(name)
	if err != nil {
		return BlankVal(), false
	}
	if !pinned {
		return BlankVal(), true
	}
	return StringVal(code), true
}

// safeEval is eval behind the package's entry points (Eval,
// EvalWithContext, EvalNode): a panic inside a function becomes the cell's
// #VALUE! error instead of unwinding into the caller. Recalculation runs in
// background goroutines, where an unrecovered panic stops the whole process
// for every tenant.
func (ctx *EvalContext) safeEval(node Node) (v Value) {
	defer func() {
		if r := recover(); r != nil {
			v = ErrorVal(errValue(fmt.Sprintf("internal error while evaluating the formula: %v", r)))
		}
	}()
	return ctx.eval(node)
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
		if v, ok := ctx.unboundDimension(n.Name); ok {
			return v
		}
		return ErrorVal(errName(n.Name))

	case *DimProperty:
		return ctx.evalDimProperty(n)

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
