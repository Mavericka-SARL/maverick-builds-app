// Package formula provides an Excel-compatible formula parser and evaluator
// for use in Metric formulas and Form field formulas.
package formula

import "strings"

// Parse parses a formula string into an AST node.
// The leading "=" and legacy {name} references are normalised automatically.
func Parse(text string) (Node, error) {
	return parse(text)
}

// Eval evaluates a formula string with the given variable map.
// Variable names are matched case-insensitively.
// Returns a Value; callers should check Value.IsError() before using the result.
func Eval(text string, vars map[string]Value) (Value, error) {
	node, err := parse(text)
	if err != nil {
		return ErrorVal(ErrValue), err
	}
	ctx := &EvalContext{Vars: upperKeys(vars)}
	return ctx.eval(node), nil
}

// EvalWithContext evaluates a formula using the provided EvalContext.
// Variable keys in ctx.Vars are matched case-insensitively.
func EvalWithContext(text string, ctx *EvalContext) (Value, error) {
	node, err := parse(text)
	if err != nil {
		return ErrorVal(ErrValue), err
	}
	if ctx != nil && ctx.Vars != nil {
		ctx = &EvalContext{Vars: upperKeys(ctx.Vars), Funcs: ctx.Funcs}
	}
	return ctx.eval(node), nil
}

// EvalNumber is a convenience wrapper that returns a float64 result.
// It preserves backward compatibility with the old arithmetic-only evaluator.
// If the result is not a number or is an error, it returns 0 and an error.
func EvalNumber(text string, vars map[string]float64) (float64, error) {
	valMap := make(map[string]Value, len(vars))
	for k, v := range vars {
		valMap[k] = NumberVal(v)
	}
	result, err := Eval(text, valMap)
	if err != nil {
		return 0, err
	}
	if result.IsError() {
		return 0, result.Err()
	}
	n, ok := result.Number()
	if !ok {
		return 0, errValue("formula did not produce a number")
	}
	return n, nil
}

// ExtractRefs returns the list of identifiers referenced in the formula.
func ExtractRefs(text string) ([]string, error) {
	return ExtractIdents(text)
}

// Normalise returns the formula with the leading "=" stripped and whitespace trimmed.
func Normalise(text string) string {
	return strings.TrimPrefix(strings.TrimSpace(text), "=")
}

func upperKeys(m map[string]Value) map[string]Value {
	if m == nil {
		return nil
	}
	out := make(map[string]Value, len(m))
	for k, v := range m {
		out[strings.ToUpper(k)] = v
	}
	return out
}
