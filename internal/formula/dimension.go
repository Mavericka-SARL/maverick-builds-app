package formula

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Dimensional references (contract C1-C3): dim.property, PARENT, LOOKUP
// and the conditional aggregations SUMIFS/AVERAGEIFS/MINIFS/MAXIFS/
// COUNTIFS/SUMIF/AVERAGEIF/COUNTIF. Like TimeEvalContext, the formula
// package defines the contract and owns the semantics (argument roles,
// criteria matching, found-skipping, memoisation); the calculation package
// supplies member metadata and metric reads through DimEvalContext.
//
// See FORMULA_CALCULATION_INSTRUCTIONS.md, "Implementation contract".

// DimMember is one leaf member of a dimension offered for criteria
// iteration: its code and its declared properties, already typed by
// TypedPropertyValue. Properties should be keyed by UPPER-CASE property
// name (lookups fall back to a case-insensitive scan); a declared property
// the member has no value for may be omitted or blank — both read as blank.
type DimMember struct {
	Code       string
	Properties map[string]Value
}

// DimEvalContext is the member-metadata and dimensional-read context of the
// cell being evaluated. Dimension names are passed exactly as written in
// the formula and must be matched case-insensitively by the implementer;
// an unknown dimension or property is reported as an error, never as a
// blank.
//
// It describes ONE cell. An evaluation at another period (TimeEvalContext
// .EvalAt) must be given a DimEvalContext rebuilt for the shifted combo,
// sharing Memo.
type DimEvalContext struct {
	// Current returns the code of the cell's member of dim, and whether
	// dim is pinned in the cell at all.
	Current func(dim string) (code string, pinned bool, err *FormulaError)
	// Property returns the typed value (TypedPropertyValue) of prop on the
	// cell's member of dim: blank when dim is not pinned or the member has
	// no value, the member's OWN value at a parent (never inherited), and
	// an error Value for an unknown dimension or undeclared property.
	Property func(dim, prop string) Value
	// Parent returns the parent code of the cell's member of dim; "" at a
	// root or when dim is not pinned.
	Parent func(dim string) (code string, err *FormulaError)
	// Member reports whether code is a member of dim (at any level) and
	// returns the stored code to use in overrides.
	Member func(dim, code string) (canonical string, ok bool, err *FormulaError)
	// Leaves returns dim's leaf members in the dimension's own order (for a
	// time dimension, its dated leaf periods), from UNFILTERED metadata.
	Leaves func(dim string) ([]DimMember, *FormulaError)
	// Resolve reads a bare source metric at the cell's coordinates with
	// each override (dimension name -> member code) applied. The
	// implementer normalises the combo onto the source's dimensions (C2,
	// rollup.NormalizeCombo) and resolves it with rollup.ResolveTime.
	// found is false when no value is recorded (value is then 0). When
	// NormalizeCombo refuses the overrides (rollup.ErrConflictingOverrides:
	// two overridden dimensions select members of the same own dimension of
	// the source) the implementer returns CodeConflictingDimensions, never
	// a value.
	Resolve func(metric string, overrides map[string]string) (value float64, found bool, err *FormulaError)
	// CoordKey returns a stable key of the cell's coordinates, including
	// the time period, with the given dimensions left out.
	CoordKey func(excludeDims []string) string
	// Memo caches conditional-aggregation results for one recalculation
	// pass. The same map must be shared by every context of that pass,
	// time-shifted children included. nil disables memoisation.
	Memo map[string]Value
}

// Error identifiers for dimensional references (contract C10). Kept as the
// Code of a FormulaError or AnalysisError.
const (
	CodeDimContextRequired   = "DIM_CONTEXT_REQUIRED"
	CodeSourceMustBeMetric   = "SOURCE_MUST_BE_METRIC"
	CodeDimensionArgRequired = "DIMENSION_ARGUMENT_REQUIRED"
	CodeUnknownProperty      = "UNKNOWN_PROPERTY"
	CodeUnknownMember        = "UNKNOWN_MEMBER"
	CodeDimensionNotOnSource = "DIMENSION_NOT_ON_SOURCE"
	// CodeConflictingDimensions: a LOOKUP or conditional aggregation
	// overrides two dimensions related to the same dimension of its source
	// (employees and region on a source keyed by employees), an
	// intersection one read cannot express. Refused, never approximated.
	CodeConflictingDimensions = "CONFLICTING_DIMENSION_ARGUMENTS"
)

const (
	codeNA                       = "#N/A"
	propertyTypeNumber           = "number"
	propertyTypeDate             = "date"
	dimensionalFuncParent        = "PARENT"
	dimensionalFuncLookup        = "LOOKUP"
	conditionalCountIfs          = "COUNTIFS"
	conditionalCountIf           = "COUNTIF"
	conditionalSumIf             = "SUMIF"
	conditionalAverageIf         = "AVERAGEIF"
	conditionalSumIfs            = "SUMIFS"
	conditionalAverageIfs        = "AVERAGEIFS"
	conditionalMinIfs            = "MINIFS"
	conditionalMaxIfs            = "MAXIFS"
	isoDateLayout                = "2006-01-02"
	maxConditionalTuples         = 1_000_000 // a guard against a runaway cartesian product
	memoKeySeparator             = "\x1f"
	memoValueSeparator           = "\x1e"
	dimPropertyDisplaySeparator  = "."
	dimensionalFunctionSeparator = ", "
)

// DimensionalFunctionNames lists the functions that read OTHER cells along
// a dimension (LOOKUP and the conditional aggregations). They are refused
// in a recurrence (C4) and served from persisted rows on scoped reads (C6).
// PARENT is not among them: like dim.property it is cell-local.
var DimensionalFunctionNames = []string{
	dimensionalFuncLookup,
	conditionalSumIfs, conditionalAverageIfs, conditionalMinIfs, conditionalMaxIfs, conditionalCountIfs,
	conditionalSumIf, conditionalAverageIf, conditionalCountIf,
}

// IsDimensionalFunction reports whether name (any case) is one of
// DimensionalFunctionNames.
func IsDimensionalFunction(name string) bool {
	u := strings.ToUpper(name)
	for _, n := range DimensionalFunctionNames {
		if n == u {
			return true
		}
	}
	return false
}

func init() {
	builtins[dimensionalFuncParent] = fnPARENT
	builtins[dimensionalFuncLookup] = fnLOOKUP
	for _, name := range []string{conditionalSumIfs, conditionalAverageIfs, conditionalMinIfs, conditionalMaxIfs, conditionalCountIfs, conditionalSumIf, conditionalAverageIf, conditionalCountIf} {
		fn := name
		builtins[fn] = func(ctx *EvalContext, args []Node) Value { return conditional(ctx, fn, args) }
	}
}

// TypedPropertyValue converts a stored (always string) member property
// value to a formula Value by the property's declared data_type — the one
// conversion every evaluator uses (contract C1):
//   - an empty or all-space value is blank, whatever the type;
//   - "number": a number, or #VALUE! when it does not parse;
//   - "date": an ISO yyyy-mm-dd date (UTC) as a date serial, or #VALUE!;
//   - "text" or any other type: the string, unchanged.
func TypedPropertyValue(raw, dataType string) Value {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return BlankVal()
	}
	switch strings.ToLower(strings.TrimSpace(dataType)) {
	case propertyTypeNumber:
		n, ok := parseCriteriaNumber(trimmed)
		if !ok {
			return ErrorVal(errValue(fmt.Sprintf("%q is not a number", raw)))
		}
		return NumberVal(n)
	case propertyTypeDate:
		d, err := time.Parse(isoDateLayout, trimmed)
		if err != nil {
			return ErrorVal(errValue(fmt.Sprintf("%q is not a yyyy-mm-dd date", raw)))
		}
		return NumberVal(timeToSerial(d))
	}
	return StringVal(raw)
}

func dimCtx(ctx *EvalContext, fn string) (*DimEvalContext, *FormulaError) {
	if ctx == nil || ctx.Dim == nil {
		return nil, &FormulaError{Code: CodeDimContextRequired,
			Message: fmt.Sprintf("%s needs dimension member data, which this evaluation does not have", fn)}
	}
	return ctx.Dim, nil
}

func dimCallbackMissing(fn, what string) *FormulaError {
	return &FormulaError{Code: CodeDimContextRequired,
		Message: fmt.Sprintf("%s needs %s, which this evaluation does not provide", fn, what)}
}

func (ctx *EvalContext) evalDimProperty(n *DimProperty) Value {
	name := n.Dim + dimPropertyDisplaySeparator + n.Property
	d, ferr := dimCtx(ctx, name)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	if d.Property == nil {
		return ErrorVal(dimCallbackMissing(name, "member properties"))
	}
	return d.Property(n.Dim, n.Property)
}

// dimensionArg returns the dimension named by a bare identifier argument.
func dimensionArg(node Node, fn string) (string, *FormulaError) {
	id, ok := node.(*Ident)
	if !ok {
		return "", &FormulaError{Code: CodeDimensionArgRequired,
			Message: fmt.Sprintf("%s: expected a dimension name, got %s", fn, describeNode(node))}
	}
	return id.Name, nil
}

// sourceArg returns the metric named by a bare identifier source argument.
func sourceArg(node Node, fn string) (string, *FormulaError) {
	id, ok := node.(*Ident)
	if !ok {
		return "", &FormulaError{Code: CodeSourceMustBeMetric,
			Message: fmt.Sprintf("%s: the source must be a metric name, got %s", fn, describeNode(node))}
	}
	return id.Name, nil
}

func describeNode(node Node) string {
	switch n := node.(type) {
	case *DimProperty:
		return "the property reference " + n.Dim + dimPropertyDisplaySeparator + n.Property
	case *CallExpr:
		return "a call to " + n.Name
	case *NumberLit, *StringLit, *BoolLit:
		return "a literal"
	}
	return "an expression"
}

// fnPARENT implements PARENT(dim): the parent code of the cell's member of
// dim, blank at a root or when dim is not pinned.
func fnPARENT(ctx *EvalContext, args []Node) Value {
	const fn = dimensionalFuncParent
	if ferr := requireArgCount(fn, args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	dim, ferr := dimensionArg(args[0], fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	d, ferr := dimCtx(ctx, fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	if d.Parent == nil {
		return ErrorVal(dimCallbackMissing(fn, "the member hierarchy"))
	}
	code, ferr := d.Parent(dim)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	if code == "" {
		return BlankVal()
	}
	return StringVal(code)
}

// memberCode converts an evaluated member argument to a member code: a
// number without an exponent (negative zero is "0"), a string as is. ok is
// false for a blank.
func memberCode(v Value) (string, bool) {
	switch v.Kind() {
	case KindBlank:
		return "", false
	case KindNumber:
		n, _ := v.Number()
		return numberText(n), true
	default:
	}
	s := v.String()
	return s, s != ""
}

func errNotAvailable(fn, dim, code string) *FormulaError {
	if code == "" {
		return MemberNotAvailable(fmt.Sprintf("%s: the %s member is blank", fn, dim))
	}
	return MemberNotAvailable(fmt.Sprintf("%s: %s has no member %q", fn, dim, code))
}

// MemberNotAvailable is the #N/A of a blank member, or of a member (or
// period) its dimension does not have (contract C2). It is a configuration
// problem at the cell, never an absence of data: an evaluator deciding
// whether a failed cell merely "has no data" must not count it as such
// (IsMemberNotAvailable).
func MemberNotAvailable(message string) *FormulaError {
	return &FormulaError{Code: codeNA, Message: message, memberNotAvailable: true}
}

// IsMemberNotAvailable reports whether err is (or wraps) a
// MemberNotAvailable error.
func IsMemberNotAvailable(err error) bool {
	var fe *FormulaError
	return errors.As(err, &fe) && fe.memberNotAvailable
}

// fnLOOKUP implements LOOKUP(source, dim1, member1 [, dim2, member2 ...]):
// the source at the cell's coordinates with each dimN replaced by memberN.
// A member that does not exist, or a blank one, is #N/A.
func fnLOOKUP(ctx *EvalContext, args []Node) Value {
	const fn = dimensionalFuncLookup
	if len(args) < 3 || len(args)%2 == 0 {
		return ErrorVal(errValue(fmt.Sprintf("%s: expected a source followed by dimension, member pairs (%d arguments)", fn, len(args))))
	}
	source, ferr := sourceArg(args[0], fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	dims := make([]string, 0, len(args)/2)
	for i := 1; i < len(args); i += 2 {
		dim, ferr := dimensionArg(args[i], fn)
		if ferr != nil {
			return ErrorVal(ferr)
		}
		for _, seen := range dims {
			if strings.EqualFold(seen, dim) {
				return ErrorVal(errValue(fmt.Sprintf("%s: dimension %s appears more than once", fn, dim)))
			}
		}
		dims = append(dims, dim)
	}
	d, ferr := dimCtx(ctx, fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	if d.Member == nil || d.Resolve == nil {
		return ErrorVal(dimCallbackMissing(fn, "dimensional reads"))
	}
	overrides := make(map[string]string, len(dims))
	for i, dim := range dims {
		v := ctx.eval(args[2+2*i])
		if v.IsError() {
			return v
		}
		code, ok := memberCode(v)
		if !ok {
			return ErrorVal(errNotAvailable(fn, dim, ""))
		}
		canonical, exists, ferr := d.Member(dim, code)
		if ferr != nil {
			return ErrorVal(ferr)
		}
		if !exists {
			return ErrorVal(errNotAvailable(fn, dim, code))
		}
		overrides[dim] = canonical
	}
	val, _, ferr := d.Resolve(source, overrides)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	return NumberVal(val)
}

// ── Conditional aggregation ─────────────────────────────────────────────────

// criteriaRange is one range argument: a dimension, optionally narrowed to
// one of its properties.
type criteriaRange struct {
	dim, prop string
}

func rangeArg(node Node, fn string) (criteriaRange, *FormulaError) {
	switch n := node.(type) {
	case *Ident:
		return criteriaRange{dim: n.Name}, nil
	case *DimProperty:
		return criteriaRange{dim: n.Dim, prop: n.Property}, nil
	}
	return criteriaRange{}, &FormulaError{Code: CodeDimensionArgRequired,
		Message: fmt.Sprintf("%s: a criteria range must be a dimension or dimension.property, got %s", fn, describeNode(node))}
}

// conditionalShape splits a conditional call's arguments into its source
// (nil for the COUNT forms) and its (range, criterion) pairs, in Excel's
// argument order for the single-criterion forms.
func conditionalShape(fn string, args []Node) (source Node, pairs [][2]Node, ferr *FormulaError) {
	pairsFrom := func(rest []Node) [][2]Node {
		out := make([][2]Node, 0, len(rest)/2)
		for i := 0; i+1 < len(rest); i += 2 {
			out = append(out, [2]Node{rest[i], rest[i+1]})
		}
		return out
	}
	switch fn {
	case conditionalCountIf:
		if len(args) != 2 {
			return nil, nil, errValue(fmt.Sprintf("%s: expected (range, criterion), got %d arguments", fn, len(args)))
		}
		return nil, pairsFrom(args), nil
	case conditionalSumIf, conditionalAverageIf:
		if len(args) != 3 {
			return nil, nil, errValue(fmt.Sprintf("%s: expected (range, criterion, source), got %d arguments", fn, len(args)))
		}
		return args[2], pairsFrom(args[:2]), nil
	case conditionalCountIfs:
		if len(args) < 2 || len(args)%2 != 0 {
			return nil, nil, errValue(fmt.Sprintf("%s: expected range, criterion pairs, got %d arguments", fn, len(args)))
		}
		return nil, pairsFrom(args), nil
	}
	if len(args) < 3 || len(args)%2 == 0 {
		return nil, nil, errValue(fmt.Sprintf("%s: expected a source followed by range, criterion pairs, got %d arguments", fn, len(args)))
	}
	return args[0], pairsFrom(args[1:]), nil
}

type evaluatedCriterion struct {
	rng  criteriaRange
	crit criterion
	raw  Value
}

// conditional implements the *IFS/*IF family (contract C3).
func conditional(ctx *EvalContext, fn string, args []Node) Value {
	sourceNode, pairs, ferr := conditionalShape(fn, args)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	source := ""
	if sourceNode != nil {
		if source, ferr = sourceArg(sourceNode, fn); ferr != nil {
			return ErrorVal(ferr)
		}
	}
	ranges := make([]criteriaRange, len(pairs))
	for i, p := range pairs {
		if ranges[i], ferr = rangeArg(p[0], fn); ferr != nil {
			return ErrorVal(ferr)
		}
	}
	d, ferr := dimCtx(ctx, fn)
	if ferr != nil {
		return ErrorVal(ferr)
	}
	if d.Leaves == nil || (source != "" && d.Resolve == nil) {
		return ErrorVal(dimCallbackMissing(fn, "dimensional reads"))
	}

	crits := make([]evaluatedCriterion, len(pairs))
	var dimOrder []string // distinct range dimensions, first-seen order
	for i, p := range pairs {
		v := ctx.eval(p[1])
		if v.IsError() {
			return v
		}
		crits[i] = evaluatedCriterion{rng: ranges[i], crit: parseCriterion(v), raw: v}
		if !containsFold(dimOrder, ranges[i].dim) {
			dimOrder = append(dimOrder, ranges[i].dim)
		}
	}

	memoKey := ""
	if d.Memo != nil && d.CoordKey != nil {
		var sb strings.Builder
		sb.WriteString(fn)
		sb.WriteString(memoKeySeparator)
		for _, a := range args {
			sb.WriteString(nodeKey(a))
			sb.WriteString(memoKeySeparator)
		}
		for _, c := range crits {
			sb.WriteString(valueKey(c.raw))
			sb.WriteString(memoValueSeparator)
		}
		sb.WriteString(memoKeySeparator)
		sb.WriteString(d.CoordKey(dimOrder))
		memoKey = sb.String()
		if v, ok := d.Memo[memoKey]; ok {
			return v
		}
	}
	v := conditionalCompute(d, fn, source, dimOrder, crits)
	if memoKey != "" {
		d.Memo[memoKey] = v
	}
	return v
}

func conditionalCompute(d *DimEvalContext, fn, source string, dimOrder []string, crits []evaluatedCriterion) Value {
	// Every range dimension's leaves are fetched before any filtering, so
	// an unknown dimension is an error whatever the argument order — never
	// hidden behind an earlier range that matched nothing.
	leavesOf := make([][]DimMember, len(dimOrder))
	for i, dim := range dimOrder {
		leaves, ferr := d.Leaves(dim)
		if ferr != nil {
			return ErrorVal(ferr)
		}
		leavesOf[i] = leaves
	}
	// Filter each dimension's leaves by every criterion on it, then take
	// the cartesian product of the survivors. A candidate that is an error
	// (an unparsable typed property) is not a failure of the whole call:
	// criterion.matches treats it like a value of another kind, so the
	// result never depends on the order of the criteria.
	matches := make([][]string, len(dimOrder))
	tuples := 1
	for i, dim := range dimOrder {
		for _, m := range leavesOf[i] {
			keep := true
			for _, c := range crits {
				if !strings.EqualFold(c.rng.dim, dim) {
					continue
				}
				candidate := StringVal(m.Code)
				if c.rng.prop != "" {
					candidate = memberProperty(m, c.rng.prop)
				}
				if !c.crit.matches(candidate) {
					keep = false
					break
				}
			}
			if keep {
				matches[i] = append(matches[i], m.Code)
			}
		}
		tuples *= len(matches[i])
		if tuples == 0 {
			break
		}
		if tuples > maxConditionalTuples {
			return ErrorVal(&FormulaError{Code: ErrNum.Code,
				Message: fmt.Sprintf("%s: the criteria match more than %d member combinations", fn, maxConditionalTuples)})
		}
	}

	if fn == conditionalCountIf || fn == conditionalCountIfs {
		return NumberVal(float64(tuples))
	}
	var sum, lo, hi float64
	found := 0
	if tuples > 0 {
		idx := make([]int, len(dimOrder))
		overrides := make(map[string]string, len(dimOrder))
		for {
			for i, dim := range dimOrder {
				overrides[dim] = matches[i][idx[i]]
			}
			v, ok, ferr := d.Resolve(source, overrides)
			if ferr != nil {
				return ErrorVal(ferr)
			}
			if ok {
				if found == 0 || v < lo {
					lo = v
				}
				if found == 0 || v > hi {
					hi = v
				}
				found++
			}
			sum += v
			// Advance the odometer.
			k := len(idx) - 1
			for ; k >= 0; k-- {
				idx[k]++
				if idx[k] < len(matches[k]) {
					break
				}
				idx[k] = 0
			}
			if k < 0 {
				break
			}
		}
	}
	switch fn {
	case conditionalAverageIfs, conditionalAverageIf:
		if found == 0 {
			return ErrorVal(&FormulaError{Code: ErrDiv0.Code, Message: fmt.Sprintf("%s: no matching member has a recorded value", fn)})
		}
		// A missing intersection is skipped: it resolves to 0 (the Resolve
		// contract), so sum is already the sum of the recorded values.
		return NumberVal(sum / float64(found))
	case conditionalMinIfs:
		if found == 0 {
			return NumberVal(0)
		}
		return NumberVal(lo)
	case conditionalMaxIfs:
		if found == 0 {
			return NumberVal(0)
		}
		return NumberVal(hi)
	}
	return NumberVal(sum)
}

func memberProperty(m DimMember, prop string) Value {
	if v, ok := m.Properties[strings.ToUpper(prop)]; ok {
		return v
	}
	for k, v := range m.Properties {
		if strings.EqualFold(k, prop) {
			return v
		}
	}
	return BlankVal()
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// valueKey encodes a Value for a memo key.
func valueKey(v Value) string {
	switch v.Kind() {
	case KindNumber:
		n, _ := v.Number()
		return "n" + strconv.FormatFloat(n, 'g', -1, 64)
	case KindString:
		return "s" + v.String()
	case KindBool:
		return "b" + v.String()
	case KindBlank:
		return "_"
	default:
		return "e" + v.String()
	}
}

// nodeKey renders a node canonically: equal keys mean the same expression,
// whichever parse produced it, so a memo keyed on it survives re-parsing
// and can never confuse two ASTs that happened to share an address.
func nodeKey(node Node) string {
	switch n := node.(type) {
	case nil:
		return ""
	case *NumberLit:
		return strconv.FormatFloat(n.Val, 'g', -1, 64)
	case *StringLit:
		return strconv.Quote(n.Val)
	case *BoolLit:
		if n.Val {
			return "TRUE"
		}
		return "FALSE"
	case *Ident:
		return "{" + strings.ToUpper(n.Name) + "}"
	case *DimProperty:
		return "{" + strings.ToUpper(n.Dim) + "}.{" + strings.ToUpper(n.Property) + "}"
	case *UnaryExpr:
		return "(" + n.Op + nodeKey(n.Expr) + ")"
	case *BinaryExpr:
		return "(" + nodeKey(n.Left) + n.Op + nodeKey(n.Right) + ")"
	case *CallExpr:
		parts := make([]string, len(n.Args))
		for i, a := range n.Args {
			parts[i] = nodeKey(a)
		}
		return n.Name + "(" + strings.Join(parts, dimensionalFunctionSeparator) + ")"
	}
	return fmt.Sprintf("%T", node)
}
