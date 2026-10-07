package formula

import (
	"fmt"
	"strings"
)

// Semantic analysis of a formula for dependency extraction. ExtractIdents
// and ExtractCalls know nothing about argument roles: they would report the
// AVERAGE in MOVINGSUM(x, -2, 0, AVERAGE) as a metric reference and could not
// say that PREVIOUS(LAG(x, 2, 0)) reads x three periods back. Analyze walks
// the AST once with a time window that composes through nested time
// functions, and reports for every referenced name the range of source
// periods it is read at, relative to the result period.
//
// The dependency windows below drive time-series scheduling and recurrence validation.

// ReferenceUse is one referenced identifier with the union of every time
// offset it is read at. Direct references are [0, 0].
type ReferenceUse struct {
	Name            string
	MinTimeOffset   int
	MaxTimeOffset   int
	UnboundedPast   bool
	UnboundedFuture bool
	// Dimensional marks a reference read as the SOURCE of LOOKUP, of the
	// *IFS/*IF family, or of the *VALUE family: it is read at members other
	// than the cell's along OverriddenDims (LOOKUP dimensions and criteria
	// range dimensions, as written; empty for *VALUE, whose reads run along
	// the time dimension and are already in the window). Analyze cannot
	// tell which dimension is time: a consumer that can must treat the
	// dependency as unbounded past and future when OverriddenDims contains
	// the time dimension. IsDirect ignores this flag, so a self-reference
	// check must test it separately.
	Dimensional    bool
	OverriddenDims []string
}

// PropertyRef is one dim.property reference.
type PropertyRef struct {
	Dim      string
	Property string
}

// ArgKind classifies a member, criterion or period-code argument for the
// static read set (contract C7).
type ArgKind int

const (
	// ArgLiteral is built only from literals and pure functions of them;
	// Value holds its constant value.
	ArgLiteral ArgKind = iota
	// ArgMemberLocal reads only literals, bare names, dim.property and
	// PARENT: it is member-local when every entry of Names is a dimension
	// (Analyze cannot tell a bare dimension from a metric — a name that is
	// a metric makes the argument value-dependent).
	ArgMemberLocal
	// ArgValueDependent calls a time or dimensional function, TODAY, or
	// anything else whose value is not member metadata.
	ArgValueDependent
)

// DimensionalArg is one member (LOOKUP), criterion (*IFS/*IF) or period
// code (TIMESUM) argument.
type DimensionalArg struct {
	// Dim is the dimension it applies to, as written ("" for TIMESUM).
	Dim string
	// Property is the criteria range's property for a dim.property range;
	// "" otherwise.
	Property string
	Kind     ArgKind
	// Value is the constant when Kind == ArgLiteral.
	Value Value
	// Names are the bare names, dim.property dimensions and PARENT
	// dimensions the expression reads, first-seen order.
	Names []string
	Expr  Node
}

// LiteralCode returns a literal argument's member or period code (a number
// converts without an exponent); ok is false for a non-literal, a blank or
// an error value.
func (a DimensionalArg) LiteralCode() (string, bool) {
	if a.Kind != ArgLiteral || a.Value.IsError() {
		return "", false
	}
	return memberCode(a.Value)
}

// DimensionalCall is one LOOKUP or *IFS/*IF call.
type DimensionalCall struct {
	// Func is the upper-case function name (one of DimensionalFunctionNames).
	Func string
	// Source is the source metric as written; "" for COUNTIFS/COUNTIF.
	Source string
	// Dims are the distinct LOOKUP or criteria-range dimensions, as
	// written, first-seen order.
	Dims []string
	// Members are LOOKUP's (dimension, member) pairs in argument order.
	Members []DimensionalArg
	// Criteria are the (range, criterion) pairs in argument order.
	Criteria []DimensionalArg
}

// TimeSumCall is one TIMESUM call. Start and End are meaningful only when
// Ranged (three or four arguments).
type TimeSumCall struct {
	Ranged     bool
	Start, End DimensionalArg
}

// IsDirect reports whether the reference is only ever read at the result
// period itself.
func (r ReferenceUse) IsDirect() bool {
	return r.MinTimeOffset == 0 && r.MaxTimeOffset == 0 && !r.UnboundedPast && !r.UnboundedFuture
}

// Analysis is the result of Analyze.
type Analysis struct {
	References     []ReferenceUse
	Calls          []string
	UsesTimeSeries bool
	// ServedFromRows is set by LOOKUP, the *IFS/*IF family, a dynamic
	// LAG/LEAD/OFFSET offset, the *VALUE family and TIMESUM: a scoped read
	// serves the metric from persisted rows and never re-evaluates it
	// (contract C6).
	ServedFromRows bool
	// PropertyRefs are the dim.property references, criteria ranges
	// included, de-duplicated case-insensitively.
	PropertyRefs []PropertyRef
	// DimensionArgs are the dimensions named as arguments — PARENT's,
	// LOOKUP's and the criteria ranges' — de-duplicated
	// case-insensitively. They are never References.
	DimensionArgs []string
	// DimensionalCalls are the LOOKUP and *IFS/*IF calls in source order
	// (an outer call before the calls nested in its arguments).
	DimensionalCalls []DimensionalCall
	// TimeSums are the TIMESUM calls in source order (outer first).
	TimeSums []TimeSumCall
	// RangeNames are the criteria ranges written as a bare name, which are
	// also DimensionArgs, de-duplicated case-insensitively. Analyze cannot
	// tell a dimension from a metric: one that names a metric (and no
	// dimension) is a metric range — SUMIFS(sales, act_region, region) —
	// read at every leaf combination of its dimensions, a Dimensional
	// reference to a consumer that resolves it.
	RangeNames []string
}

// AnalysisError is a semantic rejection with a stable identifier.
type AnalysisError struct {
	Code    string
	Message string
}

func (e *AnalysisError) Error() string { return e.Code + ": " + e.Message }

// window is the range of source offsets the expression currently being
// walked is read at, relative to the result period.
type window struct {
	min, max  int
	unbPast   bool
	unbFuture bool
}

func (w window) shift(k int) window {
	w.min += k
	w.max += k
	return w
}

// widen composes an inner window [a, b] (relative to the current period)
// with the outer window: every outer position p reads inner positions
// p+a..p+b.
func (w window) widen(a, b int, unbPast, unbFuture bool) window {
	out := window{min: w.min + a, max: w.max + b}
	out.unbPast = w.unbPast || unbPast
	out.unbFuture = w.unbFuture || unbFuture
	return out
}

type analyzer struct {
	refs     map[string]*ReferenceUse
	order    []string
	calls    []string
	seenCall map[string]bool
	usesTime bool

	servedFromRows bool
	propRefs       []PropertyRef
	dimArgs        []string
	dimCalls       []DimensionalCall
	timeSums       []TimeSumCall
	rangeNames     []string
}

// Analyze parses and analyzes a formula.
func Analyze(text string) (*Analysis, error) {
	node, err := parse(text)
	if err != nil {
		return nil, err
	}
	return AnalyzeNode(node)
}

// AnalyzeNode analyzes an already-parsed formula.
func AnalyzeNode(node Node) (*Analysis, error) {
	a := &analyzer{refs: map[string]*ReferenceUse{}, seenCall: map[string]bool{}}
	if err := a.walk(node, window{}); err != nil {
		return nil, err
	}
	out := &Analysis{
		Calls:            a.calls,
		UsesTimeSeries:   a.usesTime,
		ServedFromRows:   a.servedFromRows,
		PropertyRefs:     a.propRefs,
		DimensionArgs:    a.dimArgs,
		DimensionalCalls: a.dimCalls,
		TimeSums:         a.timeSums,
		RangeNames:       a.rangeNames,
	}
	for _, key := range a.order {
		out.References = append(out.References, *a.refs[key])
	}
	return out, nil
}

func (a *analyzer) ref(name string, w window) {
	key := strings.ToUpper(name)
	r, ok := a.refs[key]
	if !ok {
		r = &ReferenceUse{Name: name, MinTimeOffset: w.min, MaxTimeOffset: w.max, UnboundedPast: w.unbPast, UnboundedFuture: w.unbFuture}
		a.refs[key] = r
		a.order = append(a.order, key)
		return
	}
	if w.min < r.MinTimeOffset {
		r.MinTimeOffset = w.min
	}
	if w.max > r.MaxTimeOffset {
		r.MaxTimeOffset = w.max
	}
	r.UnboundedPast = r.UnboundedPast || w.unbPast
	r.UnboundedFuture = r.UnboundedFuture || w.unbFuture
}

func (a *analyzer) call(name string) {
	key := strings.ToUpper(name)
	if !a.seenCall[key] {
		a.seenCall[key] = true
		a.calls = append(a.calls, name)
	}
}

func (a *analyzer) walk(node Node, w window) error {
	switch n := node.(type) {
	case nil, *NumberLit, *StringLit, *BoolLit:
		return nil
	case *Ident:
		a.ref(n.Name, w)
		return nil
	case *DimProperty:
		a.propertyRef(n.Dim, n.Property)
		return nil
	case *UnaryExpr:
		return a.walk(n.Expr, w)
	case *BinaryExpr:
		if err := a.walk(n.Left, w); err != nil {
			return err
		}
		return a.walk(n.Right, w)
	case *CallExpr:
		a.call(n.Name)
		if IsTimeFunction(n.Name) {
			a.usesTime = true
			return a.walkTimeCall(n, w)
		}
		if n.Name == dimensionalFuncParent || IsDimensionalFunction(n.Name) {
			return a.walkDimCall(n, w)
		}
		for _, arg := range n.Args {
			if err := a.walk(arg, w); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown node type %T", node)
}

func argCount(n *CallExpr, min, max int) error {
	if len(n.Args) < min || len(n.Args) > max {
		return &AnalysisError{Code: "#VALUE!", Message: fmt.Sprintf("%s: wrong number of arguments (%d)", n.Name, len(n.Args))}
	}
	return nil
}

func literalOffset(node Node, fn string) (int, error) {
	v, ferr := IntegerLiteral(node, fn)
	if ferr != nil {
		return 0, &AnalysisError{Code: ferr.Code, Message: ferr.Message}
	}
	return v, nil
}

func keyword(node Node, fn string, allowed ...string) error {
	if _, ferr := keywordArg(node, fn, allowed...); ferr != nil {
		return &AnalysisError{Code: ferr.Code, Message: ferr.Message}
	}
	return nil
}

// walkTimeCall applies each function's argument roles: the source inherits
// the function's shift or window; substitute and reset expressions stay at
// the current period; keyword arguments are not references.
func (a *analyzer) walkTimeCall(n *CallExpr, w window) error {
	switch n.Name {
	case "PREVIOUS":
		if err := argCount(n, 1, 1); err != nil {
			return err
		}
		return a.walk(n.Args[0], w.shift(-1))
	case "NEXT":
		if err := argCount(n, 1, 1); err != nil {
			return err
		}
		return a.walk(n.Args[0], w.shift(+1))
	case "LAG", "LEAD", "OFFSET":
		maxArgs := 4
		if n.Name == "OFFSET" {
			maxArgs = 3
		}
		if err := argCount(n, 3, maxArgs); err != nil {
			return err
		}
		if !IsOffsetLiteral(n.Args[1]) {
			return a.walkDynamicOffset(n, w)
		}
		k, err := literalOffset(n.Args[1], n.Name)
		if err != nil {
			return err
		}
		if n.Name == "LAG" {
			k = -k
		}
		if len(n.Args) == 4 {
			if err := keyword(n.Args[3], n.Name, strictnessNonStrict, strictnessSemiStrict, strictnessStrict); err != nil {
				return err
			}
		}
		if err := a.walk(n.Args[0], w.shift(k)); err != nil {
			return err
		}
		return a.walk(n.Args[2], w)
	case "MOVINGSUM":
		if err := argCount(n, 1, 4); err != nil {
			return err
		}
		var src window
		switch len(n.Args) {
		case 1:
			src = w.widen(0, 0, true, true)
		case 2:
			start, err := literalOffset(n.Args[1], n.Name)
			if err != nil {
				return err
			}
			src = w.widen(start, start, false, true)
		default:
			start, err := literalOffset(n.Args[1], n.Name)
			if err != nil {
				return err
			}
			end, err := literalOffset(n.Args[2], n.Name)
			if err != nil {
				return err
			}
			if start > end {
				start, end = end, start // an inverted window reads nothing; keep the range sane for the graph
			}
			src = w.widen(start, end, false, false)
			if len(n.Args) == 4 {
				if err := keyword(n.Args[3], n.Name, movingMethodSum, movingMethodAverage, movingMethodMin, movingMethodMax); err != nil {
					return err
				}
			}
		}
		return a.walk(n.Args[0], src)
	case "CUMULATE":
		if err := argCount(n, 1, 2); err != nil {
			return err
		}
		if err := a.walk(n.Args[0], w.widen(0, 0, true, false)); err != nil {
			return err
		}
		if len(n.Args) == 2 {
			// The reset flag decides where each run starts, so it is read at
			// every period of the run, exactly like the source — a reset that
			// depended on a same-cycle metric must be scheduled as a past read.
			return a.walk(n.Args[1], w.widen(0, 0, true, false))
		}
		return nil
	case "DECUMULATE":
		if err := argCount(n, 1, 1); err != nil {
			return err
		}
		return a.walk(n.Args[0], w.widen(-1, 0, false, false))
	case "MONTHTODATE", "QUARTERTODATE", "HALFYEARTODATE", "YEARTODATE":
		if err := argCount(n, 1, 1); err != nil {
			return err
		}
		return a.walk(n.Args[0], w.widen(0, 0, true, false))
	case "MONTHVALUE", "QUARTERVALUE", "HALFYEARVALUE", "YEARVALUE":
		if err := argCount(n, 1, 1); err != nil {
			return err
		}
		source, err := analysisSource(n.Args[0], n.Name)
		if err != nil {
			return err
		}
		a.servedFromRows = true
		a.dimensionalRef(source, w.widen(0, 0, true, true), nil)
		return nil
	case "TIMESUM":
		if k := len(n.Args); k != 1 && k != 3 && k != 4 {
			return &AnalysisError{Code: "#VALUE!", Message: fmt.Sprintf("TIMESUM: expected 1, 3 or 4 arguments (x [, start, end [, method]]), got %d", k)}
		}
		a.servedFromRows = true
		call := TimeSumCall{Ranged: len(n.Args) >= 3}
		slot := len(a.timeSums)
		a.timeSums = append(a.timeSums, call)
		if len(n.Args) == 4 {
			if err := keyword(n.Args[3], n.Name, movingMethodSum, movingMethodAverage, movingMethodMin, movingMethodMax); err != nil {
				return err
			}
		}
		if err := a.walk(n.Args[0], w.widen(0, 0, true, true)); err != nil {
			return err
		}
		if call.Ranged {
			for i, dst := range []*DimensionalArg{&call.Start, &call.End} {
				if err := a.walk(n.Args[1+i], w); err != nil {
					return err
				}
				*dst = classifyArg(n.Args[1+i], "", "")
			}
		}
		a.timeSums[slot] = call
		return nil
	case "START", "END":
		return argCount(n, 0, 0)
	}
	return &AnalysisError{Code: "#NAME?", Message: "unsupported time function " + n.Name}
}

func (a *analyzer) propertyRef(dim, prop string) {
	for _, p := range a.propRefs {
		if strings.EqualFold(p.Dim, dim) && strings.EqualFold(p.Property, prop) {
			return
		}
	}
	a.propRefs = append(a.propRefs, PropertyRef{Dim: dim, Property: prop})
}

func (a *analyzer) dimensionArg(dim string) {
	if !containsFold(a.dimArgs, dim) {
		a.dimArgs = append(a.dimArgs, dim)
	}
}

// dimensionalRef records source as read by a dimensional or *VALUE call at
// window w, overriding dims.
func (a *analyzer) dimensionalRef(source string, w window, dims []string) {
	a.ref(source, w)
	r := a.refs[strings.ToUpper(source)]
	r.Dimensional = true
	for _, d := range dims {
		if !containsFold(r.OverriddenDims, d) {
			r.OverriddenDims = append(r.OverriddenDims, d)
		}
	}
}

func toAnalysisError(ferr *FormulaError) error {
	return &AnalysisError{Code: ferr.Code, Message: ferr.Message}
}

func analysisSource(node Node, fn string) (string, error) {
	source, ferr := sourceArg(node, fn)
	if ferr != nil {
		return "", toAnalysisError(ferr)
	}
	return source, nil
}

// walkDynamicOffset handles LAG/LEAD/OFFSET with a non-literal offset: the
// offset is only known per cell, so the source may be read at any period.
func (a *analyzer) walkDynamicOffset(n *CallExpr, w window) error {
	if len(n.Args) == 4 {
		if err := keyword(n.Args[3], n.Name, strictnessNonStrict, strictnessSemiStrict, strictnessStrict); err != nil {
			return err
		}
	}
	a.servedFromRows = true
	if err := a.walk(n.Args[0], w.widen(0, 0, true, true)); err != nil {
		return err
	}
	if err := a.walk(n.Args[1], w); err != nil {
		return err
	}
	return a.walk(n.Args[2], w)
}

// walkDimCall handles PARENT, LOOKUP and the *IFS/*IF family: dimension
// arguments are recorded as DimensionArgs, never References; the source
// is a Dimensional reference at the current window; member and criterion
// expressions are walked at the current window like any expression.
func (a *analyzer) walkDimCall(n *CallExpr, w window) error {
	switch n.Name {
	case dimensionalFuncParent:
		if err := argCount(n, 1, 1); err != nil {
			return err
		}
		dim, ferr := dimensionArg(n.Args[0], n.Name)
		if ferr != nil {
			return toAnalysisError(ferr)
		}
		a.dimensionArg(dim)
		return nil
	case dimensionalFuncLookup:
		if len(n.Args) < 3 || len(n.Args)%2 == 0 {
			return &AnalysisError{Code: "#VALUE!", Message: fmt.Sprintf("LOOKUP: expected a source followed by dimension, member pairs (%d arguments)", len(n.Args))}
		}
		source, err := analysisSource(n.Args[0], n.Name)
		if err != nil {
			return err
		}
		call := DimensionalCall{Func: n.Name, Source: source}
		for i := 1; i < len(n.Args); i += 2 {
			dim, ferr := dimensionArg(n.Args[i], n.Name)
			if ferr != nil {
				return toAnalysisError(ferr)
			}
			if containsFold(call.Dims, dim) {
				return &AnalysisError{Code: "#VALUE!", Message: fmt.Sprintf("LOOKUP: dimension %s appears more than once", dim)}
			}
			call.Dims = append(call.Dims, dim)
		}
		a.servedFromRows = true
		a.dimensionalRef(source, w, call.Dims)
		slot := len(a.dimCalls)
		a.dimCalls = append(a.dimCalls, DimensionalCall{}) // pre-order: the outer call before nested ones
		for i, dim := range call.Dims {
			a.dimensionArg(dim)
			member := n.Args[2+2*i]
			if err := a.walk(member, w); err != nil {
				return err
			}
			call.Members = append(call.Members, classifyArg(member, dim, ""))
		}
		a.dimCalls[slot] = call
		return nil
	}
	sourceNode, pairs, ferr := conditionalShape(n.Name, n.Args)
	if ferr != nil {
		return toAnalysisError(ferr)
	}
	call := DimensionalCall{Func: n.Name}
	if sourceNode != nil {
		source, err := analysisSource(sourceNode, n.Name)
		if err != nil {
			return err
		}
		call.Source = source
	}
	ranges := make([]criteriaRange, len(pairs))
	for i, p := range pairs {
		if ranges[i], ferr = rangeArg(p[0], n.Name); ferr != nil {
			return toAnalysisError(ferr)
		}
		if !containsFold(call.Dims, ranges[i].dim) {
			call.Dims = append(call.Dims, ranges[i].dim)
		}
	}
	a.servedFromRows = true
	if call.Source != "" {
		a.dimensionalRef(call.Source, w, call.Dims)
	}
	slot := len(a.dimCalls)
	a.dimCalls = append(a.dimCalls, DimensionalCall{})
	for i, p := range pairs {
		a.dimensionArg(ranges[i].dim)
		if ranges[i].prop != "" {
			a.propertyRef(ranges[i].dim, ranges[i].prop)
		} else if !containsFold(a.rangeNames, ranges[i].dim) {
			a.rangeNames = append(a.rangeNames, ranges[i].dim)
		}
		if err := a.walk(p[1], w); err != nil {
			return err
		}
		call.Criteria = append(call.Criteria, classifyArg(p[1], ranges[i].dim, ranges[i].prop))
	}
	a.dimCalls[slot] = call
	return nil
}

// classifyArg classifies a member, criterion or period-code expression.
func classifyArg(node Node, dim, prop string) DimensionalArg {
	arg := DimensionalArg{Dim: dim, Property: prop, Expr: node}
	arg.Kind = classifyNode(node, &arg.Names)
	if arg.Kind == ArgLiteral {
		arg.Value = EvalNode(&EvalContext{}, node)
	}
	return arg
}

func classifyNode(node Node, names *[]string) ArgKind {
	addName := func(n string) {
		if !containsFold(*names, n) {
			*names = append(*names, n)
		}
	}
	switch n := node.(type) {
	case nil, *NumberLit, *StringLit, *BoolLit:
		return ArgLiteral
	case *Ident:
		addName(n.Name)
		return ArgMemberLocal
	case *DimProperty:
		addName(n.Dim)
		return ArgMemberLocal
	case *UnaryExpr:
		return classifyNode(n.Expr, names)
	case *BinaryExpr:
		return maxArgKind(classifyNode(n.Left, names), classifyNode(n.Right, names))
	case *CallExpr:
		if n.Name == dimensionalFuncParent {
			if len(n.Args) == 1 {
				if id, ok := n.Args[0].(*Ident); ok {
					addName(id.Name)
					return ArgMemberLocal
				}
			}
			return ArgValueDependent
		}
		if IsTimeFunction(n.Name) || IsDimensionalFunction(n.Name) || n.Name == "TODAY" || !IsBuiltin(n.Name) {
			return ArgValueDependent
		}
		kind := ArgLiteral
		for _, arg := range n.Args {
			kind = maxArgKind(kind, classifyNode(arg, names))
		}
		return kind
	}
	return ArgValueDependent
}

func maxArgKind(a, b ArgKind) ArgKind {
	if b > a {
		return b
	}
	return a
}
