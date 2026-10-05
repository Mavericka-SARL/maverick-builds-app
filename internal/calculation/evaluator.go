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
	return EvaluateWithDimContext(expr, values, dimMembers, nil)
}

// EvaluateWithDimContext is EvaluateWithDims with the cell's member-metadata
// context attached, so dim.property and PARENT evaluate exactly as in the
// scheduler. Build dim with NewDimMetadata(...).CellContext(combo) over
// UNFILTERED dimensions. LOOKUP and the conditional aggregations fail with
// DIM_CONTEXT_REQUIRED through such a context: their metrics are served from
// persisted rows (contract C6), never re-evaluated here. A nil dim behaves
// like EvaluateWithDims. A blank result is ErrBlankResult: no value there.
func EvaluateWithDimContext(expr string, values map[string]float64, dimMembers map[string]string, dim *formula.DimEvalContext) (float64, error) {
	return EvaluateMetricWithDimContext(nil, "", expr, values, dimMembers, dim)
}

// EvaluateMetricWithDimContext is EvaluateWithDimContext for the formula of
// metric: when meta describes it as a pick-list, the member code its formula
// gives is returned as the key its cell stores (DimMetadata.EncodeResult).
// A nil meta evaluates exactly as EvaluateWithDimContext.
func EvaluateMetricWithDimContext(meta *DimMetadata, metric, expr string, values map[string]float64, dimMembers map[string]string, dim *formula.DimEvalContext) (float64, error) {
	vars := make(map[string]formula.Value, len(values)+len(dimMembers))
	for k, v := range values {
		vars[k] = formula.NumberVal(v)
	}
	for k, v := range dimMembers {
		vars[k] = formula.StringVal(v)
	}
	result, err := formula.EvalWithContext(expr, &formula.EvalContext{Vars: vars, Dim: dim})
	if err != nil {
		return 0, err
	}
	if metric != "" {
		result = meta.EncodeResult(metric, result)
	}
	if result.IsError() {
		return 0, result.Err()
	}
	if result.IsBlank() {
		// No value (a bare unset property, PARENT of a root): the caller
		// leaves the cell out, as the scheduler persists no row — never a
		// 0 that would enter a total or an average.
		return 0, ErrBlankResult
	}
	n, ok := result.Number()
	if !ok {
		return 0, formula.ErrValue
	}
	return n, nil
}
