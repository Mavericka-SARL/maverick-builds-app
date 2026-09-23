package metricformula

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// Temporal dependency graph analysis, shared by save-time validation and
// the calculation scheduler so both agree on what a legal recurrence is.
//
// An Anaplan-style opening/closing balance is a cross-metric cycle that is
// broken by time:
//
//	opening_cash = LAG(closing_cash, 1, 100)
//	closing_cash = opening_cash + net_cash_flow
//
// The rule (spec §8.2): inside each strongly connected component of the
// metric graph, the zero-offset edges must be acyclic, and every other
// internal edge must point the same way — all into the past (then periods
// run earliest to latest) or all into the future (latest to earliest). An
// unbounded edge (CUMULATE, MOVINGSUM without a window) inside a component
// is never causal.

// Edge is one dependency with the union of the time offsets it is read at.
type Edge struct {
	To              string
	MinTimeOffset   int
	MaxTimeOffset   int
	UnboundedPast   bool
	UnboundedFuture bool
}

// Graph maps a metric ID to its outgoing dependency edges.
type Graph map[string][]Edge

// Direction is the period order a recurrence component is evaluated in.
type Direction int

const (
	// DirectionNone: an ordinary (acyclic) metric.
	DirectionNone Direction = iota
	// DirectionForward: edges point to the past; periods run earliest → latest.
	DirectionForward
	// DirectionBackward: edges point to the future; periods run latest → earliest.
	DirectionBackward
)

// Component is one unit of scheduling: a single metric, or a recurrence
// whose members must be evaluated together period by period.
type Component struct {
	// Members in zero-offset topological order (dependencies first).
	Members   []string
	Direction Direction
	// Recurrence is true when the component is a genuine cycle (more than
	// one member, or a self-referencing member).
	Recurrence bool
}

// TemporalError reports a non-causal component.
type TemporalError struct {
	Members []string
	Detail  string
}

func (e *TemporalError) Error() string {
	return fmt.Sprintf("%s: %s", formula.CodeTemporalCycleNotCausal, e.Detail)
}

// Plan validates every component reachable from ids and returns the
// components in dependency order (a component appears after every component
// it depends on). names is used only for messages and may be nil.
func Plan(g Graph, ids []string, names map[string]string) ([]Component, error) {
	// Reachable closure from ids, so the plan covers prerequisites.
	nodes := map[string]bool{}
	var stack []string
	for _, id := range ids {
		if !nodes[id] {
			nodes[id] = true
			stack = append(stack, id)
		}
	}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, e := range g[n] {
			if !nodes[e.To] {
				nodes[e.To] = true
				stack = append(stack, e.To)
			}
		}
	}
	sccs := tarjan(g, nodes)
	// tarjan emits components in reverse topological order of the
	// condensation (a component is emitted after all components it depends
	// on), which is exactly dependencies-first.
	out := make([]Component, 0, len(sccs))
	for _, scc := range sccs {
		c, err := validateComponent(g, scc, names)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// ValidateGraph checks every component of the graph and returns the first
// non-causal one as a TemporalError.
func ValidateGraph(g Graph, names map[string]string) error {
	ids := make([]string, 0, len(g))
	for id := range g {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	_, err := Plan(g, ids, names)
	return err
}

func label(names map[string]string, id string) string {
	if n, ok := names[id]; ok && n != "" {
		return n
	}
	return id
}

func validateComponent(g Graph, scc []string, names map[string]string) (Component, error) {
	in := map[string]bool{}
	for _, id := range scc {
		in[id] = true
	}
	selfLoop := false
	for _, id := range scc {
		for _, e := range g[id] {
			if e.To == id {
				selfLoop = true
			}
		}
	}
	if len(scc) == 1 && !selfLoop {
		return Component{Members: scc}, nil
	}

	// Decompose each internal edge into its zero / past / future parts.
	zero := map[string][]string{}
	var past, future []string
	var offending []string
	for _, id := range scc {
		for _, e := range g[id] {
			if !in[e.To] {
				continue
			}
			desc := fmt.Sprintf("%s → %s [%d, %d]", label(names, id), label(names, e.To), e.MinTimeOffset, e.MaxTimeOffset)
			if e.UnboundedPast || e.UnboundedFuture {
				return Component{}, &TemporalError{Members: scc, Detail: fmt.Sprintf(
					"%s reads %s over an unbounded time range inside a dependency cycle; a recurrence must use a fixed offset such as PREVIOUS or LAG",
					label(names, id), label(names, e.To))}
			}
			if e.MinTimeOffset <= 0 && e.MaxTimeOffset >= 0 {
				zero[id] = append(zero[id], e.To)
			}
			if e.MinTimeOffset < 0 {
				past = append(past, desc)
			}
			if e.MaxTimeOffset > 0 {
				future = append(future, desc)
			}
			offending = append(offending, desc)
		}
	}
	if len(past) > 0 && len(future) > 0 {
		return Component{}, &TemporalError{Members: scc, Detail: fmt.Sprintf(
			"dependency cycle mixes past and future references, so no period order can resolve it: %s",
			strings.Join(offending, "; "))}
	}
	order, err := zeroTopo(zero, scc)
	if err != nil {
		return Component{}, &TemporalError{Members: scc, Detail: fmt.Sprintf(
			"dependency cycle is not broken by time (%s); every cycle needs a PREVIOUS/LAG (or NEXT/LEAD) step: %s",
			err, strings.Join(offending, "; "))}
	}
	dir := DirectionForward
	if len(future) > 0 {
		dir = DirectionBackward
	}
	return Component{Members: order, Direction: dir, Recurrence: true}, nil
}

// zeroTopo orders scc members so every zero-offset dependency precedes its
// dependent, failing on a zero-offset cycle.
func zeroTopo(zero map[string][]string, scc []string) ([]string, error) {
	const (
		unvisited = iota
		inStack
		done
	)
	state := map[string]int{}
	var order []string
	var visit func(n string) error
	visit = func(n string) error {
		switch state[n] {
		case done:
			return nil
		case inStack:
			return fmt.Errorf("same-period cycle through %s", n)
		}
		state[n] = inStack
		deps := append([]string(nil), zero[n]...)
		sort.Strings(deps)
		for _, d := range deps {
			if err := visit(d); err != nil {
				return err
			}
		}
		state[n] = done
		order = append(order, n)
		return nil
	}
	sorted := append([]string(nil), scc...)
	sort.Strings(sorted)
	for _, n := range sorted {
		if err := visit(n); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// tarjan returns the strongly connected components of g restricted to
// nodes, in reverse topological order of the condensation.
func tarjan(g Graph, nodes map[string]bool) [][]string {
	index := 0
	indices := map[string]int{}
	lowlink := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var out [][]string

	var strong func(v string)
	strong = func(v string) {
		indices[v] = index
		lowlink[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, e := range g[v] {
			w := e.To
			if !nodes[w] {
				continue
			}
			if _, seen := indices[w]; !seen {
				strong(w)
				if lowlink[w] < lowlink[v] {
					lowlink[v] = lowlink[w]
				}
			} else if onStack[w] && indices[w] < lowlink[v] {
				lowlink[v] = indices[w]
			}
		}
		if lowlink[v] == indices[v] {
			var scc []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == v {
					break
				}
			}
			out = append(out, scc)
		}
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, seen := indices[id]; !seen {
			strong(id)
		}
	}
	return out
}
