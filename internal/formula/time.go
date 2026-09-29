package formula

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Time-series functions (PREVIOUS, LAG, MOVINGSUM, CUMULATE, ...) need what a
// scalar Vars map cannot give them: the current period, the ordered period
// set, and a way to evaluate an expression at ANOTHER period with every
// non-time coordinate held fixed. TimeEvalContext is that contract. The
// formula package defines it; the calculation package fulfils EvalAt.
//
// See TIME_SERIES_FUNCTIONS_IMPLEMENTATION.md §5-6.

// TimePeriod is one member of a time dimension in chronological order.
type TimePeriod struct {
	Code       string
	Index      int
	Start, End time.Time
}

// TimeEvalContext is the current time coordinate of an evaluation.
type TimeEvalContext struct {
	DimensionID          string
	Granularity          string
	FiscalYearStartMonth int
	// Position is the index into Periods of the period being evaluated.
	Position int
	Periods  []TimePeriod
	// EvalAt evaluates node at Periods[position] with all non-time
	// coordinates unchanged. The callee should build its child context with
	// Child so nesting depth carries across the call boundary, and — when
	// the evaluation has an EvalContext.Dim — set the child's Dim rebuilt
	// for the shifted combo (the same cell at Periods[position]), sharing
	// the parent's Dim.Memo.
	EvalAt func(node Node, position int) Value
	// Summarize reduces the bare metric's values at the given leaf
	// positions (ascending) by the metric's own time_summary (sum, average,
	// min, max, first, last), holding every non-time coordinate of the
	// cell. Positions with no recorded value are skipped. found is false
	// when the metric's time_summary is none or no position has a value;
	// the result is then blank. Used by YEARVALUE and its siblings; nil
	// makes them fail with TIME_CONTEXT_REQUIRED.
	Summarize func(metric string, positions []int) (value float64, found bool, err *FormulaError)
	// Span maps a period code — a leaf period or an aggregate (H1, FY26) —
	// to the first and last leaf positions it covers; ok is false for an
	// unknown code or an aggregate with no dated leaf. Used by TIMESUM;
	// nil makes a ranged TIMESUM fail with TIME_CONTEXT_REQUIRED.
	Span func(code string) (first, last int, ok bool)

	depth int
}

// maxTimeDepth bounds nested EvalAt calls. A causal recurrence resolved
// through cached results never nests deeply; anything approaching this is a
// cycle that would otherwise overflow the Go stack.
const maxTimeDepth = 256

// Child returns a copy of t positioned at position, one nesting level deeper.
// EvalAt implementations use it to build the context for the shifted
// evaluation so the recursion guard sees the true depth.
func (t *TimeEvalContext) Child(position int) *TimeEvalContext {
	c := *t
	c.Position = position
	c.depth = t.depth + 1
	return &c
}

// InRange reports whether position addresses a configured period.
func (t *TimeEvalContext) InRange(position int) bool {
	return position >= 0 && position < len(t.Periods)
}

// Error identifiers for time functions (spec §9). Kept as the Code of a
// FormulaError so a cell, a validation message, and a log line all name the
// same thing.
const (
	CodeTimeContextRequired = "TIME_CONTEXT_REQUIRED"
	// CodeTimeOffsetNotInteger: a time offset that is not a whole number —
	// a decimal literal (refused at save), or a dynamic LAG/LEAD/OFFSET
	// offset that does not evaluate to one (the cell's error at run time).
	CodeTimeOffsetNotInteger = "TIME_OFFSET_NOT_INTEGER"
	// CodeMovingWindowNotLiteral: a MOVINGSUM start or end that is not an
	// integer literal. The window must be static so its read range is
	// knowable at save.
	CodeMovingWindowNotLiteral = "MOVING_WINDOW_NOT_LITERAL"
	CodeTemporalCycleNotCausal = "TEMPORAL_CYCLE_NOT_CAUSAL"
	CodeTimeDimensionMismatch  = "TIME_DIMENSION_MISMATCH"
	CodeInvalidTimeMember      = "INVALID_TIME_MEMBER"
	CodeTimeDimensionRequired  = "TIME_DIMENSION_REQUIRED"
	CodeMultipleTimeDimensions = "MULTIPLE_TIME_DIMENSIONS"
	strictnessNonStrict        = "NONSTRICT"
	strictnessSemiStrict       = "SEMISTRICT"
	strictnessStrict           = "STRICT"
	movingMethodSum            = "SUM"
	movingMethodAverage        = "AVERAGE"
	movingMethodMin            = "MIN"
	movingMethodMax            = "MAX"
)

// TimeFunctionNames lists the time-series functions: every function that
// needs a TimeEvalContext. Names not in this list or among the builtins
// (POST, SPREAD, PROFILE, WEEKVALUE) stay unknown to the evaluator and fail
// validation as such — never a placeholder returning 0.
var TimeFunctionNames = []string{
	"PREVIOUS", "NEXT", "LAG", "LEAD", "OFFSET", "MOVINGSUM",
	"CUMULATE", "DECUMULATE", "MONTHTODATE", "QUARTERTODATE", "HALFYEARTODATE", "YEARTODATE",
	"MONTHVALUE", "QUARTERVALUE", "HALFYEARVALUE", "YEARVALUE", "TIMESUM", "START", "END",
}

// IsTimeFunction reports whether name (any case) is a time function.
func IsTimeFunction(name string) bool {
	u := strings.ToUpper(name)
	for _, n := range TimeFunctionNames {
		if n == u {
			return true
		}
	}
	return false
}

func init() {
	builtins["PREVIOUS"] = fnPREVIOUS
	builtins["NEXT"] = fnNEXT
	builtins["LAG"] = fnLAG
	builtins["LEAD"] = fnLEAD
	builtins["OFFSET"] = fnOFFSET
	builtins["MOVINGSUM"] = fnMOVINGSUM
	builtins["CUMULATE"] = fnCUMULATE
	builtins["DECUMULATE"] = fnDECUMULATE
	builtins["MONTHTODATE"] = fnMONTHTODATE
	builtins["QUARTERTODATE"] = fnQUARTERTODATE
	builtins["YEARTODATE"] = fnYEARTODATE
	builtins["HALFYEARTODATE"] = fnHALFYEARTODATE
	builtins["MONTHVALUE"] = fnMONTHVALUE
	builtins["QUARTERVALUE"] = fnQUARTERVALUE
	builtins["HALFYEARVALUE"] = fnHALFYEARVALUE
	builtins["YEARVALUE"] = fnYEARVALUE
	builtins["TIMESUM"] = fnTIMESUM
	builtins["START"] = fnSTART
	builtins["END"] = fnEND
}

// EvalNode evaluates one AST node in ctx. It is the one sanctioned way for
// another package (the calculation scheduler's EvalAt) to evaluate a
// sub-expression without duplicating the evaluator.
func EvalNode(ctx *EvalContext, node Node) Value {
	return ctx.safeEval(node)
}

func timeCtx(ctx *EvalContext, fn string) (*TimeEvalContext, *FormulaError) {
	if ctx == nil || ctx.Time == nil || ctx.Time.EvalAt == nil {
		return nil, &FormulaError{Code: CodeTimeContextRequired,
			Message: fmt.Sprintf("%s needs a time dimension: the metric is not dimensioned by a time dimension", fn)}
	}
	if ctx.Time.depth > maxTimeDepth {
		return nil, &FormulaError{Code: CodeTemporalCycleNotCausal,
			Message: fmt.Sprintf("%s: time evaluation nested %d levels deep — a non-causal cycle", fn, ctx.Time.depth)}
	}
	return ctx.Time, nil
}

// at evaluates node at position through the caller-supplied EvalAt. Out of
// range positions are the caller's responsibility to check first.
func (t *TimeEvalContext) at(node Node, position int) Value {
	return t.EvalAt(node, position)
}

// IsOffsetLiteral reports whether node is a literal offset: a number
// literal, optionally negated. LAG/LEAD/OFFSET check a literal statically
// (IntegerLiteral) and evaluate anything else at run time.
func IsOffsetLiteral(node Node) bool {
	if u, ok := node.(*UnaryExpr); ok && u.Op == "-" {
		node = u.Expr
	}
	_, ok := node.(*NumberLit)
	return ok
}

// IntegerLiteral extracts an integer offset argument that must be an
// integer literal (optionally negated), so dependency ranges and
// recurrence direction are statically knowable. MOVINGSUM windows always
// are; LAG/LEAD/OFFSET use it for a literal offset only. A non-literal is
// MOVING_WINDOW_NOT_LITERAL (only a MOVINGSUM window reaches that: callers
// route a dynamic LAG/LEAD/OFFSET offset elsewhere first); a decimal
// literal is TIME_OFFSET_NOT_INTEGER.
func IntegerLiteral(node Node, fn string) (int, *FormulaError) {
	sign := 1.0
	if u, ok := node.(*UnaryExpr); ok && u.Op == "-" {
		sign = -1
		node = u.Expr
	}
	lit, ok := node.(*NumberLit)
	if !ok {
		return 0, &FormulaError{Code: CodeMovingWindowNotLiteral,
			Message: fmt.Sprintf("%s: the window must be an integer literal (a metric, cell reference or expression is not supported)", fn)}
	}
	if lit.Val != math.Trunc(lit.Val) {
		return 0, &FormulaError{Code: CodeTimeOffsetNotInteger,
			Message: fmt.Sprintf("%s: the offset must be a whole number, got %v", fn, lit.Val)}
	}
	return int(sign * lit.Val), nil
}

// keywordArg reads a bare identifier argument that must be one of allowed.
func keywordArg(node Node, fn string, allowed ...string) (string, *FormulaError) {
	id, ok := node.(*Ident)
	if !ok {
		return "", errValue(fmt.Sprintf("%s: expected one of %s", fn, strings.Join(allowed, ", ")))
	}
	u := strings.ToUpper(id.Name)
	for _, a := range allowed {
		if a == u {
			return u, nil
		}
	}
	return "", errValue(fmt.Sprintf("%s: unknown keyword %s (expected one of %s)", fn, id.Name, strings.Join(allowed, ", ")))
}

// ── Position functions ───────────────────────────────────────────────────────

func fnPREVIOUS(ctx *EvalContext, args []Node) Value { return shiftOrZero(ctx, "PREVIOUS", args, -1) }
func fnNEXT(ctx *EvalContext, args []Node) Value     { return shiftOrZero(ctx, "NEXT", args, +1) }

func shiftOrZero(ctx *EvalContext, fn string, args []Node, delta int) Value {
	if ferr := requireArgCount(fn, args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	t, ferr := timeCtx(ctx, fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	target := t.Position + delta
	if !t.InRange(target) {
		return NumberVal(0)
	}
	return t.at(args[0], target)
}

func fnLAG(ctx *EvalContext, args []Node) Value    { return lagLead(ctx, "LAG", args, -1) }
func fnLEAD(ctx *EvalContext, args []Node) Value   { return lagLead(ctx, "LEAD", args, +1) }
func fnOFFSET(ctx *EvalContext, args []Node) Value { return lagLead(ctx, "OFFSET", args, +1) }

// lagLead implements LAG/LEAD/OFFSET(value, n, substitute[, strictness]).
// dir is the sign applied to n: LAG looks back (-n), LEAD/OFFSET forward
// (+n). OFFSET takes no strictness keyword (it is non-strict LEAD).
func lagLead(ctx *EvalContext, fn string, args []Node, dir int) Value {
	maxArgs := 4
	if fn == "OFFSET" {
		maxArgs = 3
	}
	if ferr := requireArgCount(fn, args, 3, maxArgs); ferr != nil {
		return ErrorVal(ferr)
	}
	t, ferr := timeCtx(ctx, fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	n, ferr := offsetArg(ctx, args[1], fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	mode := strictnessNonStrict
	if len(args) == 4 {
		mode, ferr = keywordArg(args[3], fn, strictnessNonStrict, strictnessSemiStrict, strictnessStrict)
		if ferr != nil {
			return ErrorVal(ferr)
		}
	}
	// The substitute is always evaluated at the CURRENT period.
	substitute := func() Value { return ctx.eval(args[2]) }
	switch mode {
	case strictnessSemiStrict:
		if n < 0 {
			return substitute()
		}
	case strictnessStrict:
		if n <= 0 {
			return substitute()
		}
	}
	target := t.Position + dir*n
	if !t.InRange(target) {
		return substitute()
	}
	return t.at(args[0], target)
}

// maxDynamicOffset bounds a dynamic offset before it is converted to int:
// anything larger is simply out of range.
const maxDynamicOffset = 1 << 30

// offsetArg returns the offset of LAG/LEAD/OFFSET. A literal must be a
// whole number (a static error otherwise, also reported by Analyze); any
// other expression is evaluated at the current period and must yield a
// whole number, or the cell gets TIME_OFFSET_NOT_INTEGER.
func offsetArg(ctx *EvalContext, node Node, fn string) (int, *FormulaError) {
	if IsOffsetLiteral(node) {
		return IntegerLiteral(node, fn)
	}
	v := ctx.eval(node)
	if v.IsError() {
		return 0, v.Err()
	}
	f, ok := v.Number()
	if !ok || v.Kind() == KindString || f != math.Trunc(f) || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, &FormulaError{Code: CodeTimeOffsetNotInteger,
			Message: fmt.Sprintf("%s: the offset must evaluate to a whole number, got %s", fn, describeValue(v))}
	}
	if f > maxDynamicOffset {
		f = maxDynamicOffset
	}
	if f < -maxDynamicOffset {
		f = -maxDynamicOffset
	}
	return int(f), nil
}

func describeValue(v Value) string {
	switch v.Kind() {
	case KindString:
		return fmt.Sprintf("the text %q", v.String())
	case KindBlank:
		return "a blank"
	default:
		return v.String()
	}
}

// ── Moving range ─────────────────────────────────────────────────────────────

// fnMOVINGSUM implements MOVINGSUM(source[, start[, end[, method]]]) with
// the modern two-argument semantics: one argument spans every configured
// period, two run from t+start to the last period, three or four span the
// inclusive window t+start..t+end. Periods outside the configured range are
// ignored; an empty effective window is numeric zero for every method.
func fnMOVINGSUM(ctx *EvalContext, args []Node) Value {
	const fn = "MOVINGSUM"
	if ferr := requireArgCount(fn, args, 1, 4); ferr != nil {
		return ErrorVal(ferr)
	}
	t, ferr := timeCtx(ctx, fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	last := len(t.Periods) - 1
	from, to := 0, last
	if len(args) >= 2 {
		start, ferr := IntegerLiteral(args[1], fn)
		if ferr != nil {
			return ErrorVal(ferr)
		}
		from = t.Position + start
	}
	if len(args) >= 3 {
		end, ferr := IntegerLiteral(args[2], fn)
		if ferr != nil {
			return ErrorVal(ferr)
		}
		to = t.Position + end
	}
	method := movingMethodSum
	if len(args) == 4 {
		method, ferr = keywordArg(args[3], fn, movingMethodSum, movingMethodAverage, movingMethodMin, movingMethodMax)
		if ferr != nil {
			return ErrorVal(ferr)
		}
	}
	return reduceWindow(t, args[0], from, to, method)
}

// reduceWindow reduces source over the inclusive position window from..to
// by method, ignoring positions outside the configured range. An empty
// effective window is numeric zero for every method.
func reduceWindow(t *TimeEvalContext, source Node, from, to int, method string) Value {
	last := len(t.Periods) - 1
	if from > to {
		return NumberVal(0)
	}
	if from < 0 {
		from = 0
	}
	if to > last {
		to = last
	}
	var sum, lo, hi float64
	count := 0
	for i := from; i <= to; i++ {
		v := t.at(source, i)
		if v.IsError() {
			return v
		}
		n, ok := v.Number()
		if !ok {
			return ErrorVal(ErrValue)
		}
		if count == 0 || n < lo {
			lo = n
		}
		if count == 0 || n > hi {
			hi = n
		}
		sum += n
		count++
	}
	if count == 0 {
		return NumberVal(0)
	}
	switch method {
	case movingMethodAverage:
		return NumberVal(sum / float64(count))
	case movingMethodMin:
		return NumberVal(lo)
	case movingMethodMax:
		return NumberVal(hi)
	}
	return NumberVal(sum)
}

// ── Cumulative and difference ────────────────────────────────────────────────

// fnCUMULATE implements CUMULATE(source[, reset]): the inclusive running sum
// from the first configured period. A true reset at a period starts a new
// run whose first value is that period's own source value.
func fnCUMULATE(ctx *EvalContext, args []Node) Value {
	const fn = "CUMULATE"
	if ferr := requireArgCount(fn, args, 1, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	t, ferr := timeCtx(ctx, fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	var acc float64
	for i := 0; i <= t.Position && i < len(t.Periods); i++ {
		if len(args) == 2 {
			r := t.at(args[1], i)
			if r.IsError() {
				return r
			}
			if r.Bool() {
				acc = 0
			}
		}
		v := t.at(args[0], i)
		if v.IsError() {
			return v
		}
		n, ok := v.Number()
		if !ok {
			return ErrorVal(ErrValue)
		}
		acc += n
	}
	return NumberVal(acc)
}

// fnDECUMULATE returns source[t] - source[t-1]; in the first period the
// value before the range is numeric zero, so it returns source[t].
func fnDECUMULATE(ctx *EvalContext, args []Node) Value {
	const fn = "DECUMULATE"
	if ferr := requireArgCount(fn, args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	t, ferr := timeCtx(ctx, fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	cur := t.at(args[0], t.Position)
	if cur.IsError() {
		return cur
	}
	c, ok := cur.Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	if t.Position == 0 {
		return NumberVal(c)
	}
	prev := t.at(args[0], t.Position-1)
	if prev.IsError() {
		return prev
	}
	p, ok := prev.Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	return NumberVal(c - p)
}

// ── Period-to-date ───────────────────────────────────────────────────────────

func fnMONTHTODATE(ctx *EvalContext, args []Node) Value {
	return periodToDate(ctx, "MONTHTODATE", args, levelMonth)
}
func fnQUARTERTODATE(ctx *EvalContext, args []Node) Value {
	return periodToDate(ctx, "QUARTERTODATE", args, levelQuarter)
}
func fnHALFYEARTODATE(ctx *EvalContext, args []Node) Value {
	return periodToDate(ctx, "HALFYEARTODATE", args, levelHalfYear)
}
func fnYEARTODATE(ctx *EvalContext, args []Node) Value {
	return periodToDate(ctx, "YEARTODATE", args, levelYear)
}

type intervalLevel int

// The levels mirror timedim.IntervalLevel; keep the two in sync.
const (
	levelMonth intervalLevel = iota
	levelQuarter
	levelYear
	levelHalfYear
)

// granularityFits mirrors timedim.GranularityFitsLevel (spec §5.4) without
// importing it: the formula package stays free of the model packages.
func granularityFits(level intervalLevel, gran string) bool {
	switch level {
	case levelMonth:
		return gran == "day"
	case levelQuarter:
		return gran == "day" || gran == "week" || gran == "month"
	case levelHalfYear:
		return gran == "day" || gran == "week" || gran == "month" || gran == "quarter"
	case levelYear:
		return gran == "day" || gran == "week" || gran == "month" || gran == "quarter" || gran == "half_year"
	}
	return false
}

// intervalKey names the calendar interval containing d at level. The fiscal
// year is named by the calendar year it starts in.
func intervalKey(level intervalLevel, fiscalStart int, d time.Time) string {
	if fiscalStart < 1 {
		fiscalStart = 1
	}
	offset := ((int(d.Month()) - fiscalStart) + 12) % 12
	fy := d.Year()
	if int(d.Month()) < fiscalStart {
		fy--
	}
	switch level {
	case levelMonth:
		return fmt.Sprintf("%04d-%02d", d.Year(), int(d.Month()))
	case levelQuarter:
		return fmt.Sprintf("FY%04d-Q%d", fy, offset/3+1)
	case levelHalfYear:
		return fmt.Sprintf("FY%04d-H%d", fy, offset/6+1)
	default:
		return fmt.Sprintf("FY%04d", fy)
	}
}

// periodToDate sums source from the first period of the containing
// interval through the current one. Boundaries come from the periods'
// dates, the granularity and the fiscal year start — never from labels.
func periodToDate(ctx *EvalContext, fn string, args []Node, level intervalLevel) Value {
	if ferr := requireArgCount(fn, args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	t, ferr := timeCtx(ctx, fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	if !granularityFits(level, t.Granularity) {
		return ErrorVal(&FormulaError{Code: CodeInvalidTimeMember,
			Message: fmt.Sprintf("%s: a %s-granularity time dimension is too coarse for this function", fn, t.Granularity)})
	}
	if !t.InRange(t.Position) {
		return ErrorVal(&FormulaError{Code: CodeTimeContextRequired, Message: fn + ": no current period"})
	}
	cur := t.Periods[t.Position]
	key := intervalKey(level, t.FiscalYearStartMonth, cur.Start)
	if intervalKey(level, t.FiscalYearStartMonth, cur.End) != key {
		return ErrorVal(&FormulaError{Code: CodeInvalidTimeMember,
			Message: fmt.Sprintf("%s: period %s crosses an interval boundary", fn, cur.Code)})
	}
	var acc float64
	for i := t.Position; i >= 0; i-- {
		p := t.Periods[i]
		ks := intervalKey(level, t.FiscalYearStartMonth, p.Start)
		if ks != key {
			break
		}
		if intervalKey(level, t.FiscalYearStartMonth, p.End) != ks {
			return ErrorVal(&FormulaError{Code: CodeInvalidTimeMember,
				Message: fmt.Sprintf("%s: period %s crosses an interval boundary", fn, p.Code)})
		}
		v := t.at(args[0], i)
		if v.IsError() {
			return v
		}
		n, ok := v.Number()
		if !ok {
			return ErrorVal(ErrValue)
		}
		acc += n
	}
	return NumberVal(acc)
}

// ── Value over the containing interval ───────────────────────────────────────

func fnMONTHVALUE(ctx *EvalContext, args []Node) Value {
	return intervalValue(ctx, "MONTHVALUE", args, levelMonth)
}
func fnQUARTERVALUE(ctx *EvalContext, args []Node) Value {
	return intervalValue(ctx, "QUARTERVALUE", args, levelQuarter)
}
func fnHALFYEARVALUE(ctx *EvalContext, args []Node) Value {
	return intervalValue(ctx, "HALFYEARVALUE", args, levelHalfYear)
}
func fnYEARVALUE(ctx *EvalContext, args []Node) Value {
	return intervalValue(ctx, "YEARVALUE", args, levelYear)
}

// intervalValue implements YEARVALUE(m) and its siblings: the bare metric m
// over every leaf period sharing the current period's month, quarter, half
// or fiscal year, reduced by m's own time_summary (TimeEvalContext
// .Summarize). Bucketing is the *TODATE functions' date intervalKey, with
// the same granularity rule. A period belongs to the interval when both its
// start and its end fall inside it. The current period crossing a boundary
// is invalid (as for *TODATE); a NEIGHBOUR that crosses one only ends the
// bucket — on a weekly axis the week straddling a quarter end belongs to
// neither quarter, and the weeks wholly inside the quarter still resolve.
func intervalValue(ctx *EvalContext, fn string, args []Node, level intervalLevel) Value {
	if ferr := requireArgCount(fn, args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	source, ferr := sourceArg(args[0], fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	t, ferr := timeCtx(ctx, fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	if t.Summarize == nil {
		return ErrorVal(&FormulaError{Code: CodeTimeContextRequired,
			Message: fn + " needs the metric's time summary, which this evaluation does not provide"})
	}
	if !granularityFits(level, t.Granularity) {
		return ErrorVal(&FormulaError{Code: CodeInvalidTimeMember,
			Message: fmt.Sprintf("%s: a %s-granularity time dimension is too coarse for this function", fn, t.Granularity)})
	}
	if !t.InRange(t.Position) {
		return ErrorVal(&FormulaError{Code: CodeTimeContextRequired, Message: fn + ": no current period"})
	}
	cur := t.Periods[t.Position]
	key := intervalKey(level, t.FiscalYearStartMonth, cur.Start)
	if intervalKey(level, t.FiscalYearStartMonth, cur.End) != key {
		return ErrorVal(&FormulaError{Code: CodeInvalidTimeMember,
			Message: fmt.Sprintf("%s: period %s crosses an interval boundary", fn, cur.Code)})
	}
	inInterval := func(i int) bool {
		p := t.Periods[i]
		return intervalKey(level, t.FiscalYearStartMonth, p.Start) == key &&
			intervalKey(level, t.FiscalYearStartMonth, p.End) == key
	}
	lo, hi := t.Position, t.Position
	for lo > 0 && inInterval(lo-1) {
		lo--
	}
	for hi < len(t.Periods)-1 && inInterval(hi+1) {
		hi++
	}
	positions := make([]int, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		positions = append(positions, i)
	}
	v, found, ferr := t.Summarize(source, positions)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	if !found {
		return BlankVal()
	}
	return NumberVal(v)
}

// ── TIMESUM ──────────────────────────────────────────────────────────────────

// fnTIMESUM implements TIMESUM(x [, start, end [, method]]): x reduced over
// the leaf periods from start to end inclusive (an aggregate start means
// its first leaf, an aggregate end its last), or over every leaf period
// with one argument. An empty range (start after end) is MOVINGSUM's empty
// window, 0; an unknown or blank period code is #N/A.
func fnTIMESUM(ctx *EvalContext, args []Node) Value {
	const fn = "TIMESUM"
	if n := len(args); n != 1 && n != 3 && n != 4 {
		return ErrorVal(errValue(fmt.Sprintf("%s: expected 1, 3 or 4 arguments (x [, start, end [, method]]), got %d", fn, n)))
	}
	t, ferr := timeCtx(ctx, fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	from, to := 0, len(t.Periods)-1
	if len(args) >= 3 {
		if t.Span == nil {
			return ErrorVal(&FormulaError{Code: CodeTimeContextRequired,
				Message: fn + " needs the time hierarchy, which this evaluation does not provide"})
		}
		first, _, fv := timeSpanArg(ctx, t, args[1], fn)
		if fv.IsError() {
			return fv
		}
		_, last, lv := timeSpanArg(ctx, t, args[2], fn)
		if lv.IsError() {
			return lv
		}
		from, to = first, last
	}
	method := movingMethodSum
	if len(args) == 4 {
		method, ferr = keywordArg(args[3], fn, movingMethodSum, movingMethodAverage, movingMethodMin, movingMethodMax)
		if ferr != nil {
			return ErrorVal(ferr)
		}
	}
	return reduceWindow(t, args[0], from, to, method)
}

// timeSpanArg evaluates a period-code argument at the current period and
// maps it to its leaf-position span.
func timeSpanArg(ctx *EvalContext, t *TimeEvalContext, node Node, fn string) (int, int, Value) {
	v := ctx.eval(node)
	if v.IsError() {
		return 0, 0, v
	}
	code, ok := memberCode(v)
	if !ok {
		return 0, 0, ErrorVal(MemberNotAvailable(fn + ": the period is blank"))
	}
	first, last, ok := t.Span(code)
	if !ok {
		return 0, 0, ErrorVal(MemberNotAvailable(fmt.Sprintf("%s: the time dimension has no period %q", fn, code)))
	}
	return first, last, Value{}
}

// ── Period dates ─────────────────────────────────────────────────────────────

func fnSTART(ctx *EvalContext, args []Node) Value { return periodDate(ctx, "START", args, true) }
func fnEND(ctx *EvalContext, args []Node) Value   { return periodDate(ctx, "END", args, false) }

// periodDate returns the current leaf period's first (START) or last (END,
// inclusive) day as a date serial.
func periodDate(ctx *EvalContext, fn string, args []Node, start bool) Value {
	if ferr := requireArgCount(fn, args, 0, 0); ferr != nil {
		return ErrorVal(ferr)
	}
	t, ferr := timeCtx(ctx, fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	if !t.InRange(t.Position) {
		return ErrorVal(&FormulaError{Code: CodeTimeContextRequired, Message: fn + ": no current period"})
	}
	p := t.Periods[t.Position]
	d := p.End
	if start {
		d = p.Start
	}
	if d.IsZero() {
		return ErrorVal(&FormulaError{Code: CodeInvalidTimeMember, Message: fmt.Sprintf("%s: period %s has no dates", fn, p.Code)})
	}
	d = d.UTC()
	return NumberVal(timeToSerial(time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)))
}
