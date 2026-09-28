package formula

import (
	"fmt"
	"testing"
)

// The #N/A of a blank or unknown member is recognisable (an evaluator must
// never count it as "no data"); a generic #N/A (IFS with no match) is not.
func TestIsMemberNotAvailable(t *testing.T) {
	d := &DimEvalContext{
		Member: func(_, code string) (string, bool, *FormulaError) { return code, code == "EUR", nil },
		Resolve: func(string, map[string]string) (float64, bool, *FormulaError) {
			return 1, true, nil
		},
	}
	eval := func(expr string) Value {
		node, err := Parse(expr)
		if err != nil {
			t.Fatalf("parse %s: %v", expr, err)
		}
		return EvalNode(&EvalContext{Vars: map[string]Value{"X": NumberVal(2)}, Dim: d}, node)
	}
	for expr, want := range map[string]bool{
		`LOOKUP(fx, currency, "GBP")`:     true,
		`X * LOOKUP(fx, currency, "")`:    true, // propagated through arithmetic
		`IFS(X > 5, 1)`:                   false,
		`LOOKUP(fx, currency, "EUR") / 0`: false,
	} {
		v := eval(expr)
		if !v.IsError() {
			t.Fatalf("%s: want an error, got %v", expr, v)
		}
		if got := IsMemberNotAvailable(v.Err()); got != want {
			t.Errorf("IsMemberNotAvailable(%s = %v) = %v, want %v", expr, v.Err(), got, want)
		}
	}
	if !IsMemberNotAvailable(fmt.Errorf("wrapped: %w", MemberNotAvailable("x"))) {
		t.Error("a wrapped MemberNotAvailable must be recognised")
	}
	if IsMemberNotAvailable(nil) || IsMemberNotAvailable(ErrNA) {
		t.Error("nil and the generic #N/A are not member errors")
	}
}
