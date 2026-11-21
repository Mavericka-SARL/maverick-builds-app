package formula

import (
	"math"
	"strings"
)

// Statistical, logarithmic and exponential functions (2026-11-21), under the
// spreadsheet's names so a workbook's formulas carry over: an inventory
// workbook's safety stock is CEILING(NORM.S.INV(service) × σ, 1), and without
// NORM.S.INV, LN or EXP its model needed a thirteen-coefficient
// approximation. Excel's legacy names (NORMSINV, STDEV, …) are accepted too.
//
// The dotted names (NORM.S.INV, STDEV.S) are read as function names only
// directly before "(" (lexer.readIdent, DottedFunction); anywhere else a
// dotted name stays a dimension.property reference.

// dottedFunctions are the built-ins whose names contain dots.
var dottedFunctions = map[string]bool{}

// DottedFunction reports whether name (any case) is a built-in whose name has
// a dot: a dotted name before "(" calls it instead of reading a property.
func DottedFunction(name string) bool { return dottedFunctions[strings.ToUpper(name)] }

func init() {
	for name, fn := range map[string]CustomFunc{
		"LN":    fnLN,
		"EXP":   fnEXP,
		"LOG":   fnLOG,
		"LOG10": fnLOG10,

		"NORM.S.INV":  fnNormSInv,
		"NORMSINV":    fnNormSInv,
		"NORM.INV":    fnNormInv,
		"NORMINV":     fnNormInv,
		"NORM.S.DIST": fnNormSDist,
		"NORMSDIST":   fnNormSDistLegacy,
		"NORM.DIST":   fnNormDist,
		"NORMDIST":    fnNormDist,

		"STDEV.S": stdevFn("STDEV.S", true, true),
		"STDEV":   stdevFn("STDEV", true, true),
		"STDEV.P": stdevFn("STDEV.P", false, true),
		"STDEVP":  stdevFn("STDEVP", false, true),
		"VAR.S":   stdevFn("VAR.S", true, false),
		"VAR":     stdevFn("VAR", true, false),
		"VAR.P":   stdevFn("VAR.P", false, false),
		"VARP":    stdevFn("VARP", false, false),
	} {
		builtins[name] = fn
		if strings.Contains(name, ".") {
			dottedFunctions[name] = true
		}
	}
	for name, a := range map[string][2]int{
		"LN": {1, 1}, "EXP": {1, 1}, "LOG": {1, 2}, "LOG10": {1, 1},
		"NORM.S.INV": {1, 1}, "NORMSINV": {1, 1}, "NORM.INV": {3, 3}, "NORMINV": {3, 3},
		"NORM.S.DIST": {2, 2}, "NORMSDIST": {1, 1}, "NORM.DIST": {4, 4}, "NORMDIST": {4, 4},
		"STDEV.S": {1, -1}, "STDEV": {1, -1}, "STDEV.P": {1, -1}, "STDEVP": {1, -1},
		"VAR.S": {1, -1}, "VAR": {1, -1}, "VAR.P": {1, -1}, "VARP": {1, -1},
	} {
		arity[name] = a
	}
}

// numberArgs evaluates args as numbers, as the other math functions do: an
// error passes through, a blank is 0, TRUE/FALSE 1/0, a text #VALUE!.
func numberArgs(ctx *EvalContext, name string, args []Node, min, max int) ([]float64, Value) {
	if ferr := requireArgCount(name, args, min, max); ferr != nil {
		return nil, ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return nil, errv
	}
	out := make([]float64, len(vals))
	for i, v := range vals {
		n, ok := v.Number()
		if !ok {
			return nil, ErrorVal(ErrValue)
		}
		out[i] = n
	}
	return out, Value{}
}

// finite is a result, or #NUM! when it overflowed or is not a real number.
func finite(x float64) Value {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return ErrorVal(ErrNum)
	}
	return NumberVal(x)
}

func fnLN(ctx *EvalContext, args []Node) Value {
	x, errv := numberArgs(ctx, "LN", args, 1, 1)
	if errv.IsError() {
		return errv
	}
	if x[0] <= 0 {
		return ErrorVal(ErrNum)
	}
	return finite(math.Log(x[0]))
}

func fnEXP(ctx *EvalContext, args []Node) Value {
	x, errv := numberArgs(ctx, "EXP", args, 1, 1)
	if errv.IsError() {
		return errv
	}
	return finite(math.Exp(x[0]))
}

// fnLOG is LOG(x [, base]), base 10 by default; base 1 divides by ln 1 = 0.
func fnLOG(ctx *EvalContext, args []Node) Value {
	x, errv := numberArgs(ctx, "LOG", args, 1, 2)
	if errv.IsError() {
		return errv
	}
	base := 10.0
	if len(x) == 2 {
		base = x[1]
	}
	if x[0] <= 0 || base <= 0 {
		return ErrorVal(ErrNum)
	}
	if base == 1 {
		return ErrorVal(ErrDiv0)
	}
	if base == 10 {
		return finite(math.Log10(x[0]))
	}
	return finite(math.Log(x[0]) / math.Log(base))
}

func fnLOG10(ctx *EvalContext, args []Node) Value {
	x, errv := numberArgs(ctx, "LOG10", args, 1, 1)
	if errv.IsError() {
		return errv
	}
	if x[0] <= 0 {
		return ErrorVal(ErrNum)
	}
	return finite(math.Log10(x[0]))
}

// normSInv is the standard normal quantile: the z with Φ(z) = p, 0 < p < 1,
// from the inverse complementary error function on the smaller tail so a p
// near 1 keeps its precision.
func normSInv(p float64) (float64, bool) {
	if !(p > 0 && p < 1) {
		return 0, false
	}
	var z float64
	if p < 0.5 {
		z = -math.Sqrt2 * math.Erfcinv(2*p)
	} else {
		z = math.Sqrt2 * math.Erfcinv(2*(1-p))
	}
	// One Halley step on Φ(z) − p: Erfcinv alone drifts to ~1e-9 far in the
	// tails (p = 1e-9); the step brings z to the last digit.
	var e float64
	if p < 0.5 {
		e = normDist(z, true) - p
	} else {
		e = (1 - p) - 0.5*math.Erfc(z/math.Sqrt2) // Φ(z) − p on the upper tail, without cancellation
	}
	u := e * math.Sqrt(2*math.Pi) * math.Exp(z*z/2)
	return z - u/(1+z*u/2), true
}

func fnNormSInv(ctx *EvalContext, args []Node) Value {
	x, errv := numberArgs(ctx, "NORM.S.INV", args, 1, 1)
	if errv.IsError() {
		return errv
	}
	z, ok := normSInv(x[0])
	if !ok {
		return ErrorVal(ErrNum)
	}
	return finite(z)
}

// fnNormInv is NORM.INV(p, mean, standard_dev).
func fnNormInv(ctx *EvalContext, args []Node) Value {
	x, errv := numberArgs(ctx, "NORM.INV", args, 3, 3)
	if errv.IsError() {
		return errv
	}
	z, ok := normSInv(x[0])
	if !ok || x[2] <= 0 {
		return ErrorVal(ErrNum)
	}
	return finite(x[1] + x[2]*z)
}

// normDist is the normal distribution's cumulative probability or density
// at z, standardised.
func normDist(z float64, cumulative bool) float64 {
	if cumulative {
		return 0.5 * math.Erfc(-z/math.Sqrt2)
	}
	return math.Exp(-z*z/2) / math.Sqrt(2*math.Pi)
}

// fnNormSDist is NORM.S.DIST(z, cumulative).
func fnNormSDist(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("NORM.S.DIST", args, 2, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	z, ok := vals[0].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	return finite(normDist(z, vals[1].Bool()))
}

// fnNormSDistLegacy is NORMSDIST(z): always cumulative.
func fnNormSDistLegacy(ctx *EvalContext, args []Node) Value {
	x, errv := numberArgs(ctx, "NORMSDIST", args, 1, 1)
	if errv.IsError() {
		return errv
	}
	return finite(normDist(x[0], true))
}

// fnNormDist is NORM.DIST(x, mean, standard_dev, cumulative); the density is
// per unit of x.
func fnNormDist(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("NORM.DIST", args, 4, 4); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	var n [3]float64
	for i := 0; i < 3; i++ {
		v, ok := vals[i].Number()
		if !ok {
			return ErrorVal(ErrValue)
		}
		n[i] = v
	}
	if n[2] <= 0 {
		return ErrorVal(ErrNum)
	}
	z := (n[0] - n[1]) / n[2]
	if vals[3].Bool() {
		return finite(normDist(z, true))
	}
	return finite(normDist(z, false) / n[2])
}

// varianceOf is the variance of vals about their mean, two passes for
// precision: sample (n − 1) or population (n). ok is false when there are too
// few values (a sample needs two).
func varianceOf(vals []float64, sample bool) (float64, bool) {
	n := len(vals)
	if n == 0 || (sample && n < 2) {
		return 0, false
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(n)
	var ss float64
	for _, v := range vals {
		d := v - mean
		ss += d * d
	}
	if sample {
		return ss / float64(n-1), true
	}
	return ss / float64(n), true
}

// stdevFn makes STDEV.S / STDEV.P / VAR.S / VAR.P over the arguments' numbers
// (blanks skipped, as AVERAGE skips them); too few values is #DIV/0!.
func stdevFn(name string, sample, root bool) CustomFunc {
	return func(ctx *EvalContext, args []Node) Value {
		if ferr := requireArgCount(name, args, 1, -1); ferr != nil {
			return ErrorVal(ferr)
		}
		nums, errv := collectNumbers(ctx, args)
		if errv.IsError() {
			return errv
		}
		v, ok := varianceOf(nums, sample)
		if !ok {
			return ErrorVal(ErrDiv0)
		}
		if root {
			return finite(math.Sqrt(v))
		}
		return finite(v)
	}
}
