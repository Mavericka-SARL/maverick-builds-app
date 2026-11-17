package query

import (
	"context"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
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
		out[name] = r.valueAt(ctx, d, point, dims, cc)
	}
	return out, nil
}

// MetricValuesAtPoints reads one metric, by name, at many points of a
// revision with the rules of MetricValuesAt — a dimension a point leaves
// out is read at its total, a parent member or an aggregate period (FY,
// a quarter) at the engine's own total there — loading the revision once.
// The AI Developer's sheet check compares every cell of a sheet block with
// it, a whole sheet in one read. A name that is no metric is an error.
func (r *ChartResolver) MetricValuesAtPoints(ctx context.Context, modelID, revisionID, name string, points []map[string]string) ([]formula.Value, error) {
	cc, err := r.loadChartCalc(ctx, modelID, revisionID, nil)
	if err != nil {
		return nil, err
	}
	dims, err := r.loadAllDimensions(ctx, modelID, revisionID, nil)
	if err != nil {
		return nil, err
	}
	var d *fullMetricDef
	for _, def := range cc.defs {
		if strings.EqualFold(def.Name, strings.TrimSpace(name)) {
			d = def
		}
	}
	if d == nil {
		return nil, fmt.Errorf("no metric named %q in this revision", name)
	}
	out := make([]formula.Value, len(points))
	for i, p := range points {
		out[i] = r.valueAt(ctx, d, p, dims, cc)
	}
	return out, nil
}

// valueAt is one metric at one point: a number, blank when nothing is
// recorded or computed there, #VALUE! when it cannot be computed.
func (r *ChartResolver) valueAt(ctx context.Context, d *fullMetricDef, point map[string]string, dims map[string]*rollup.Dimension, cc *chartCalc) formula.Value {
	v, has, err := r.rawAt(ctx, d, point, dims, cc)
	switch {
	case err != nil:
		return formula.ErrorVal(&formula.FormulaError{Code: formula.ErrValue.Code, Message: fmt.Sprintf("%s cannot be read here: %v", d.Name, err)})
	case !has:
		return formula.BlankVal()
	}
	if codec, isPicklist := cc.meta.Picklist(d.Name); isPicklist {
		if code, known := codec.Code(v); known {
			return formula.StringVal(code)
		} else if v == 0 {
			return formula.BlankVal()
		}
	}
	return formula.NumberVal(v)
}

// rawAt is one metric's stored number at one point (a pick-list's member
// index, not its code), and whether anything is recorded or computed there.
func (r *ChartResolver) rawAt(ctx context.Context, d *fullMetricDef, point map[string]string, dims map[string]*rollup.Dimension, cc *chartCalc) (float64, bool, error) {
	if d.IsInput {
		return resolveInput(ctx, dims, d, point, cc.fetchInput)
	}
	return r.evalCalcMetricVisited(ctx, d.ID, point, dims, cc, map[string]bool{})
}

// EvalAt evaluates node at one point of a revision (dimension ID → member
// code; a dimension left out is at its total) with vars bound by the caller
// — the metrics it names, read with MetricValuesAt, and anything else — and
// with the dimensional reads of the formula language: LOOKUP and the
// *IFS/*IF family read metrics at other members through the revision's own
// values, as MetricValuesAt reads them, and dim.property and PARENT answer
// for the point's members. A workflow condition is evaluated through it:
// LOOKUP(gate_pass, gate, "G1") = 1 used to pass Validate and then stall
// the run, "LOOKUP needs dimension member data, which this evaluation does
// not have" (2026-11-16).
func (r *ChartResolver) EvalAt(ctx context.Context, modelID, revisionID string, node formula.Node, vars map[string]formula.Value, point map[string]string) (formula.Value, error) {
	cc, err := r.loadChartCalc(ctx, modelID, revisionID, nil)
	if err != nil {
		return formula.BlankVal(), err
	}
	dims, err := r.loadAllDimensions(ctx, modelID, revisionID, nil)
	if err != nil {
		return formula.BlankVal(), err
	}
	byName := make(map[string]*fullMetricDef, len(cc.defs))
	for _, d := range cc.defs {
		byName[strings.ToLower(d.Name)] = d
	}
	dc := cc.meta.CellContext(point)
	dc.Resolve = func(metric string, overrides map[string]string) (float64, bool, *formula.FormulaError) {
		d, ok := byName[strings.ToLower(metric)]
		if !ok {
			return 0, false, &formula.FormulaError{Code: formula.CodeSourceMustBeMetric, Message: fmt.Sprintf("%s is no metric of this revision", metric)}
		}
		at := make(map[string]string, len(point)+len(overrides))
		for k, v := range point {
			at[k] = v
		}
		for dim, code := range overrides {
			id, ferr := cc.meta.DimensionID(dim, point)
			if ferr != nil {
				return 0, false, ferr
			}
			at[id] = code
		}
		v, has, err := r.rawAt(ctx, d, at, dims, cc)
		if err != nil {
			return 0, false, &formula.FormulaError{Code: formula.ErrValue.Code, Message: fmt.Sprintf("%s cannot be read there: %v", d.Name, err)}
		}
		return v, has, nil
	}
	return formula.EvalNode(&formula.EvalContext{Vars: vars, Dim: dc}, node), nil
}
