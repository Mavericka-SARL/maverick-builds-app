package rollup

import (
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// CalculatedMember is a member of a standard dimension whose value, for
// every metric, is computed from the dimension's other members at the same
// coordinate (dimension_member.formula): Variance = {RF} - {LY}. It is not
// one of Dimension.Members, so no aggregation, leaf enumeration or
// calculation pass ever reaches it; the readers that show a cell at it
// compute it with EvalCalculated, at any level of the other dimensions —
// the FY Variance % is computed from the FY RF and FY LY, never added up
// from the months.
type CalculatedMember struct {
	Code    string
	Formula string
}

// MetricFormatFunc is the function a member formula calls for the metric
// it is evaluated for: METRICFORMAT() = "percentage" lets a variance be
// points for a margin and a percentage change for an amount.
const MetricFormatFunc = "METRICFORMAT"

// CalculatedCode finds the calculated member with code (case-insensitively,
// as formula identifiers match).
func (d *Dimension) CalculatedCode(code string) (CalculatedMember, bool) {
	if d == nil {
		return CalculatedMember{}, false
	}
	for _, c := range d.Calculated {
		if strings.EqualFold(c.Code, code) {
			return c, true
		}
	}
	return CalculatedMember{}, false
}

// EvalCalculated evaluates a calculated member's formula for one metric at
// one coordinate. Each identifier is a member code of the same dimension,
// read through value: the metric at the same coordinate with the dimension
// at that member (a calculated one included — the caller evaluates it the
// same way; formulas are checked acyclic when saved). A member with no value
// reads 0, as a blank cell does in a spreadsheet; when none of the members
// the formula reads has a value there is none (ok=false), so an empty row
// stays empty. A formula error (a division by zero) is no value too: the
// cell shows blank rather than failing the read.
func EvalCalculated(text, metricFormat string, value func(code string) (float64, bool, error)) (float64, bool, error) {
	refs, err := formula.ExtractRefs(text)
	if err != nil {
		return 0, false, nil
	}
	vars := make(map[string]formula.Value, len(refs))
	anyValue := false
	for _, ref := range refs {
		v, ok, err := value(ref)
		if err != nil {
			return 0, false, err
		}
		if ok {
			anyValue = true
		}
		vars[ref] = formula.NumberVal(v)
	}
	if len(refs) > 0 && !anyValue {
		return 0, false, nil
	}
	res, err := formula.EvalWithContext(text, &formula.EvalContext{
		Vars: vars,
		Funcs: map[string]formula.CustomFunc{
			MetricFormatFunc: func(*formula.EvalContext, []formula.Node) formula.Value { return formula.StringVal(metricFormat) },
		},
	})
	if err != nil || res.IsError() {
		return 0, false, nil
	}
	n, ok := res.Number()
	return n, ok, nil
}
