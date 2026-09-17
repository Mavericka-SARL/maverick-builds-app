package model

import (
	"fmt"
	"regexp"
	"strings"
)

// metricRefPattern matches {metric_name} references inside a formula string.
var metricRefPattern = regexp.MustCompile(`\{([^}]+)\}`)

// ExtractFormulaRefs returns the metric names referenced in a formula string.
// Formula syntax: arithmetic expressions referencing other metrics as {metric_name}.
// Example: "({revenue} - {cogs}) / {revenue}"
func ExtractFormulaRefs(formula string) []string {
	matches := metricRefPattern.FindAllStringSubmatch(formula, -1)
	seen := make(map[string]bool)
	var refs []string
	for _, m := range matches {
		name := strings.TrimSpace(m[1])
		if !seen[name] {
			seen[name] = true
			refs = append(refs, name)
		}
	}
	return refs
}

// ResolveRefIDs maps formula metric-name references to their IDs using the names map.
// Returns an error if any referenced name doesn't exist in the model.
func ResolveRefIDs(refs []string, nameToID map[string]string) ([]string, error) {
	var ids []string
	for _, name := range refs {
		id, ok := nameToID[name]
		if !ok {
			return nil, fmt.Errorf("metric %q referenced in formula not found in model", name)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

type cycleState int

const (
	unvisited cycleState = iota
	inStack
	done
)

// DetectCycles runs DFS on the dependency graph and returns a list of cycle descriptions.
// graph: metricID → []dependsOnMetricID (directed: A depends on B means edge A→B)
// names: metricID → metric name (for human-readable error messages)
func DetectCycles(graph map[string][]string, names map[string]string) []string {
	state := make(map[string]cycleState, len(graph))
	path := make([]string, 0, 8)
	var cycles []string

	var dfs func(node string)
	dfs = func(node string) {
		if state[node] == done {
			return
		}
		if state[node] == inStack {
			// Found cycle — find where it starts in path
			cycleStart := -1
			for i, n := range path {
				if n == node {
					cycleStart = i
					break
				}
			}
			if cycleStart >= 0 {
				loop := path[cycleStart:]
				nameLoop := make([]string, len(loop))
				for i, id := range loop {
					nameLoop[i] = names[id]
				}
				cycles = append(cycles, strings.Join(nameLoop, " → ")+" → "+names[node])
			}
			return
		}

		state[node] = inStack
		path = append(path, node)

		for _, dep := range graph[node] {
			dfs(dep)
		}

		path = path[:len(path)-1]
		state[node] = done
	}

	// Ensure all nodes are visited, even those with no outbound edges
	allNodes := make(map[string]bool)
	for n, deps := range graph {
		allNodes[n] = true
		for _, d := range deps {
			allNodes[d] = true
		}
	}
	for node := range allNodes {
		if state[node] == unvisited {
			dfs(node)
		}
	}

	return cycles
}

// TopologicalOrder returns metrics in dependency-first order (leaves first).
// Returns an error if the graph contains cycles (caller should run DetectCycles first).
func TopologicalOrder(graph map[string][]string, allIDs []string) ([]string, error) {
	state := make(map[string]cycleState, len(allIDs))
	var order []string

	var visit func(n string) error
	visit = func(n string) error {
		switch state[n] {
		case done:
			return nil
		case inStack:
			return fmt.Errorf("cycle detected at %s", n)
		case unvisited: // proceed to visit
		}
		state[n] = inStack
		for _, dep := range graph[n] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[n] = done
		order = append(order, n)
		return nil
	}

	for _, id := range allIDs {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return order, nil
}
