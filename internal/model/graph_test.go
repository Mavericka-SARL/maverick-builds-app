package model

import (
	"testing"
)

func TestExtractFormulaRefs(t *testing.T) {
	tests := []struct {
		formula string
		want    []string
	}{
		{"{revenue} - {cogs}", []string{"revenue", "cogs"}},
		{"({revenue} - {cogs}) / {revenue}", []string{"revenue", "cogs"}}, // deduped
		{"{headcount} * {salary_per_head}", []string{"headcount", "salary_per_head"}},
		{"42", nil},
		{"", nil},
	}
	for _, tt := range tests {
		got := ExtractFormulaRefs(tt.formula)
		if len(got) != len(tt.want) {
			t.Errorf("ExtractFormulaRefs(%q) = %v, want %v", tt.formula, got, tt.want)
			continue
		}
		for i, g := range got {
			if g != tt.want[i] {
				t.Errorf("ExtractFormulaRefs(%q)[%d] = %q, want %q", tt.formula, i, g, tt.want[i])
			}
		}
	}
}

func TestDetectCycles_NoCycle(t *testing.T) {
	// gross_profit = revenue - cogs
	// net_profit = gross_profit - opex
	graph := map[string][]string{
		"gross_profit": {"revenue", "cogs"},
		"net_profit":   {"gross_profit", "opex"},
	}
	names := map[string]string{
		"gross_profit": "gross_profit",
		"net_profit":   "net_profit",
		"revenue":      "revenue",
		"cogs":         "cogs",
		"opex":         "opex",
	}
	cycles := DetectCycles(graph, names)
	if len(cycles) != 0 {
		t.Errorf("expected no cycles, got %v", cycles)
	}
}

func TestDetectCycles_DirectCycle(t *testing.T) {
	// A → B → A
	graph := map[string][]string{
		"A": {"B"},
		"B": {"A"},
	}
	names := map[string]string{"A": "metric_a", "B": "metric_b"}
	cycles := DetectCycles(graph, names)
	if len(cycles) == 0 {
		t.Error("expected cycle to be detected, got none")
	}
}

func TestDetectCycles_IndirectCycle(t *testing.T) {
	// A → B → C → A
	graph := map[string][]string{
		"A": {"B"},
		"B": {"C"},
		"C": {"A"},
	}
	names := map[string]string{"A": "a", "B": "b", "C": "c"}
	cycles := DetectCycles(graph, names)
	if len(cycles) == 0 {
		t.Error("expected cycle to be detected, got none")
	}
}

func TestDetectCycles_SelfLoop(t *testing.T) {
	graph := map[string][]string{
		"A": {"A"},
	}
	names := map[string]string{"A": "self_ref"}
	cycles := DetectCycles(graph, names)
	if len(cycles) == 0 {
		t.Error("expected self-loop cycle, got none")
	}
}

func TestTopologicalOrder(t *testing.T) {
	// revenue (leaf), cogs (leaf), gross = revenue - cogs, net = gross - opex, opex (leaf)
	graph := map[string][]string{
		"gross": {"revenue", "cogs"},
		"net":   {"gross", "opex"},
	}
	all := []string{"revenue", "cogs", "opex", "gross", "net"}
	order, err := TopologicalOrder(graph, all)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Verify: gross must come after revenue and cogs; net must come after gross
	pos := make(map[string]int, len(order))
	for i, id := range order {
		pos[id] = i
	}
	if pos["gross"] < pos["revenue"] || pos["gross"] < pos["cogs"] {
		t.Errorf("gross must come after revenue and cogs in order %v", order)
	}
	if pos["net"] < pos["gross"] {
		t.Errorf("net must come after gross in order %v", order)
	}
}

func TestResolveRefIDs_MissingRef(t *testing.T) {
	nameToID := map[string]string{"revenue": "id-1", "cogs": "id-2"}
	_, err := ResolveRefIDs([]string{"revenue", "missing_metric"}, nameToID)
	if err == nil {
		t.Error("expected error for missing metric reference, got nil")
	}
}
