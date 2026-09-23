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
// See TIME_SERIES_FUNCTIONS_IMPLEMENTATION.md §7.

// ReferenceUse is one referenced identifier with the union of every time
// offset it is read at. Direct references are [0, 0].
type ReferenceUse struct {
	Name            string
	MinTimeOffset   int
	MaxTimeOffset   int
	UnboundedPast   bool
	UnboundedFuture bool
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
	out := &Analysis{Calls: a.calls, UsesTimeSeries: a.usesTime}
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
	case "MONTHTODATE", "QUARTERTODATE", "YEARTODATE":
		if err := argCount(n, 1, 1); err != nil {
			return err
		}
		return a.walk(n.Args[0], w.widen(0, 0, true, false))
	}
	return &AnalysisError{Code: "#NAME?", Message: "unsupported time function " + n.Name}
}
