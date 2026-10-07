package query

import (
	"context"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// MetricValuesAt reads metrics, by name (ignoring case, as formulas name
// them), at one point of a revision: point pins dimensions (dimension ID →
// member code), and a dimension it leaves out is read at its total — the
// chart's own rules (rollup.ResolveTime for inputs, evalCalcMetricVisited
// for calculated metrics), unrestricted by any viewer's access. A pick-list
// reads as its member's code, a missing value as blank, a value that
// cannot be computed as #VALUE!. A name that is no metric is left out of
// the result. A workflow condition reads the model through it ("the
// company variance is over the threshold → another round").
func (r *ChartResolver) MetricValuesAt(ctx context.Context, modelID, revisionID string, names []string, point map[string]string) (map[string]formula.Value, error) {
	cc, err := r.loadChartCalc(ctx, modelID, revisionID, nil)
	if err != nil {
		return nil, err
	}
	dims, err := r.loadAllDimensions(ctx, modelID, revisionID, nil)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*fullMetricDef, len(cc.defs))
	for _, d := range cc.defs {
		byName[strings.ToLower(d.Name)] = d
	}
	out := make(map[string]formula.Value, len(names))
	for _, name := range names {
		d, ok := byName[strings.ToLower(name)]
		if !ok {
			continue
		}
		var v float64
		var has bool
		if d.IsInput {
			v, has, err = resolveInput(ctx, dims, d, point, cc.fetchInput)
		} else {
			v, has, err = r.evalCalcMetricVisited(ctx, d.ID, point, dims, cc, map[string]bool{})
		}
		switch {
		case err != nil:
			out[name] = formula.ErrorVal(&formula.FormulaError{Code: formula.ErrValue.Code, Message: fmt.Sprintf("%s cannot be read here: %v", d.Name, err)})
		case !has:
			out[name] = formula.BlankVal()
		default:
			out[name] = formula.NumberVal(v)
			if codec, isPicklist := cc.meta.Picklist(d.Name); isPicklist {
				if code, known := codec.Code(v); known {
					out[name] = formula.StringVal(code)
				} else if v == 0 {
					out[name] = formula.BlankVal()
				}
			}
		}
	}
	return out, nil
}
