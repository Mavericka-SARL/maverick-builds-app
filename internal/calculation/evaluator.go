package calculation

import (
	"github.com/mavericks-engine/mavericks/internal/formula"
)

// Evaluate resolves all metric references in a formula and returns a float64.
// Supports {name} and plain name references, full Excel-style function set,
// arithmetic, comparisons, and logic operators.
// This is the legacy entry point; new code should use formula.EvalNumber directly.
func Evaluate(expr string, values map[string]float64) (float64, error) {
	return formula.EvalNumber(expr, values)
}

// EvaluateWithDims is like Evaluate but also binds dimension member values
// (string-typed) into the context so formulas like =IF(department="SALES",…)
// can compare against the current partition's dimension members.
func EvaluateWithDims(expr string, values map[string]float64, dimMembers map[string]string) (float64, error) {
	vars := make(map[string]formula.Value, len(values)+len(dimMembers))
	for k, v := range values {
		vars[k] = formula.NumberVal(v)
	}
	for k, v := range dimMembers {
		vars[k] = formula.StringVal(v)
	}
	result, err := formula.EvalWithContext(expr, &formula.EvalContext{Vars: vars})
	if err != nil {
		return 0, err
	}
	if result.IsError() {
		return 0, result.Err()
	}
	n, ok := result.Number()
	if !ok {
		return 0, formula.ErrValue
	}
	return n, nil
}
