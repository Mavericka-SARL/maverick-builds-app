package readset

import (
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// The formula walk: every read one formula makes, each with the time
// window it is read at and the dimension arguments that move it off the
// cell's members. It mirrors formula.Analyze's window composition
// (analyze.go walkTimeCall) — TestUsesMatchAnalyze holds the two together —
// but keeps each read separate instead of merging them per name, because a
// LOOKUP of x and a plain x read different members, and a TIMESUM with a
// literal range reads ABSOLUTE periods, which the relative window of
// formula.ReferenceUse cannot express.

// window is the range of periods a read covers relative to the cell's
// period, or — abs — relative to a literal TIMESUM range.
type window struct {
	abs              bool
	absStart, absEnd string // abs: the range's start and end period codes
	min, max         int
	unbPast          bool
	unbFuture        bool
}

// trivial reports whether w is the cell's own period.
func (w window) trivial() bool {
	return !w.abs && w.min == 0 && w.max == 0 && !w.unbPast && !w.unbFuture
}

func (w window) shift(k int) window {
	w.min += k
	w.max += k
	return w
}

// widen composes an inner window [a, b] with w: every position w covers
// reads positions a..b around it.
func (w window) widen(a, b int, unbPast, unbFuture bool) window {
	w.min += a
	w.max += b
	w.unbPast = w.unbPast || unbPast
	w.unbFuture = w.unbFuture || unbFuture
	return w
}

// dimArg is one dimension a dimensional call moves its read along: a LOOKUP
// member, or every criterion of a *IFS/*IF range dimension.
type dimArg struct {
	dim      string // as written
	member   *formula.DimensionalArg
	criteria []formula.DimensionalArg
}

// use is one read a formula makes.
type use struct {
	// source is the name read, as written: a metric, or — for a bare
	// identifier — possibly a dimension, which reads nothing. "" for the
	// member-only read of COUNTIFS/COUNTIF.
	source string
	win    window
	// args are the dimensions a LOOKUP or conditional aggregation reads the
	// source along, first-seen order.
	args []dimArg
}

// uses returns every read of a parsed formula.
func uses(node formula.Node) ([]use, error) {
	var out []use
	if err := walk(node, window{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func walk(node formula.Node, w window, out *[]use) error {
	switch n := node.(type) {
	case nil, *formula.NumberLit, *formula.StringLit, *formula.BoolLit, *formula.DimProperty:
		return nil
	case *formula.Ident:
		*out = append(*out, use{source: n.Name, win: w})
		return nil
	case *formula.UnaryExpr:
		return walk(n.Expr, w, out)
	case *formula.BinaryExpr:
		if err := walk(n.Left, w, out); err != nil {
			return err
		}
		return walk(n.Right, w, out)
	case *formula.CallExpr:
		switch {
		case formula.IsTimeFunction(n.Name):
			return walkTime(n, w, out)
		case strings.EqualFold(n.Name, "PARENT"):
			return nil // cell-local: reads the cell's own member only
		case formula.IsDimensionalFunction(n.Name):
			return walkDimensional(n, w, out)
		}
		return walkAll(n.Args, w, out)
	}
	return &unsupportedNode{node: node}
}

type unsupportedNode struct{ node formula.Node }

func (e *unsupportedNode) Error() string { return "readset: unsupported formula node" }

func walkAll(args []formula.Node, w window, out *[]use) error {
	for _, a := range args {
		if err := walk(a, w, out); err != nil {
			return err
		}
	}
	return nil
}

// walkTime applies each time function's argument roles exactly as
// formula.Analyze does. A shape Analyze would refuse (a wrong argument
// count, a bad keyword) reads every argument at an unbounded window: it
// never computed, so any answer is conservative.
func walkTime(n *formula.CallExpr, w window, out *[]use) error {
	all := w.widen(0, 0, true, true)
	arg := func(i int) formula.Node {
		if i < len(n.Args) {
			return n.Args[i]
		}
		return nil
	}
	switch strings.ToUpper(n.Name) {
	case "PREVIOUS":
		return walk(arg(0), w.shift(-1), out)
	case "NEXT":
		return walk(arg(0), w.shift(+1), out)
	case "LAG", "LEAD", "OFFSET":
		if len(n.Args) < 3 || !formula.IsOffsetLiteral(n.Args[1]) {
			// A dynamic offset reads the source at any period (C5).
			if err := walk(arg(0), all, out); err != nil {
				return err
			}
			return walk(arg(1), w, out)
		}
		k, ferr := formula.IntegerLiteral(n.Args[1], n.Name)
		if ferr != nil {
			return walkAll(n.Args[:1], all, out)
		}
		if strings.EqualFold(n.Name, "LAG") {
			k = -k
		}
		if err := walk(n.Args[0], w.shift(k), out); err != nil {
			return err
		}
		return walk(n.Args[2], w, out)
	case "MOVINGSUM":
		var src window
		switch len(n.Args) {
		case 0:
			return nil
		case 1:
			src = all
		case 2:
			start, ferr := formula.IntegerLiteral(n.Args[1], n.Name)
			if ferr != nil {
				src = all
			} else {
				src = w.widen(start, start, false, true)
			}
		default:
			start, e1 := formula.IntegerLiteral(n.Args[1], n.Name)
			end, e2 := formula.IntegerLiteral(n.Args[2], n.Name)
			if e1 != nil || e2 != nil {
				src = all
			} else {
				if start > end {
					start, end = end, start
				}
				src = w.widen(start, end, false, false)
			}
		}
		return walk(n.Args[0], src, out)
	case "CUMULATE":
		past := w.widen(0, 0, true, false)
		if err := walk(arg(0), past, out); err != nil {
			return err
		}
		return walk(arg(1), past, out)
	case "DECUMULATE":
		return walk(arg(0), w.widen(-1, 0, false, false), out)
	case "MONTHTODATE", "QUARTERTODATE", "HALFYEARTODATE", "YEARTODATE":
		return walk(arg(0), w.widen(0, 0, true, false), out)
	case "MONTHVALUE", "QUARTERVALUE", "HALFYEARVALUE", "YEARVALUE":
		// The source over the containing year/half/quarter/month, which
		// reaches both ways: unbounded, as Analyze records it.
		return walk(arg(0), all, out)
	case "TIMESUM":
		inner := all
		if len(n.Args) >= 3 {
			if an, err := formula.AnalyzeNode(n); err == nil && len(an.TimeSums) > 0 && an.TimeSums[0].Ranged {
				start, ok1 := an.TimeSums[0].Start.LiteralCode()
				end, ok2 := an.TimeSums[0].End.LiteralCode()
				if ok1 && ok2 {
					inner = window{abs: true, absStart: start, absEnd: end}
				}
			}
			if err := walk(n.Args[1], w, out); err != nil {
				return err
			}
			if err := walk(n.Args[2], w, out); err != nil {
				return err
			}
		}
		return walk(arg(0), inner, out)
	case "START", "END":
		return nil
	}
	return walkAll(n.Args, all, out)
}

// walkDimensional records a LOOKUP or conditional aggregation: its source
// read along its dimension arguments, then the reads its member and
// criterion expressions make at the cell.
func walkDimensional(n *formula.CallExpr, w window, out *[]use) error {
	an, err := formula.AnalyzeNode(n)
	if err != nil || len(an.DimensionalCalls) == 0 {
		// Refused at save; nothing was computed from it. Its arguments are
		// still walked so a read inside them is never missed.
		return walkAll(n.Args, w.widen(0, 0, true, true), out)
	}
	call := an.DimensionalCalls[0] // pre-order: this call before nested ones
	u := use{source: call.Source, win: w}
	argFor := func(dim string) *dimArg {
		for i := range u.args {
			if strings.EqualFold(u.args[i].dim, dim) {
				return &u.args[i]
			}
		}
		u.args = append(u.args, dimArg{dim: dim})
		return &u.args[len(u.args)-1]
	}
	for i := range call.Members {
		m := call.Members[i]
		argFor(m.Dim).member = &m
	}
	for _, c := range call.Criteria {
		a := argFor(c.Dim)
		a.criteria = append(a.criteria, c)
	}
	*out = append(*out, u)
	for _, m := range call.Members {
		if err := walk(m.Expr, w, out); err != nil {
			return err
		}
	}
	for _, c := range call.Criteria {
		if err := walk(c.Expr, w, out); err != nil {
			return err
		}
	}
	return nil
}
