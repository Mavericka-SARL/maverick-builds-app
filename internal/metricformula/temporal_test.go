package metricformula

import (
	"errors"
	"strings"
	"testing"
)

func edge(to string, min, max int) Edge { return Edge{To: to, MinTimeOffset: min, MaxTimeOffset: max} }

func TestPlanAcyclicGraphIsDependencyOrdered(t *testing.T) {
	g := Graph{"c": {edge("b", 0, 0)}, "b": {edge("a", -1, -1)}, "a": nil}
	comps, err := Plan(g, []string{"c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range comps {
		if c.Recurrence {
			t.Errorf("no recurrence expected: %+v", c)
		}
		order = append(order, c.Members...)
	}
	if strings.Join(order, ",") != "a,b,c" {
		t.Errorf("order %v, want a,b,c", order)
	}
}

func TestPlanOpeningClosingBalanceIsForwardRecurrence(t *testing.T) {
	g := Graph{
		"opening": {edge("closing", -1, -1)},
		"closing": {edge("opening", 0, 0), edge("flow", 0, 0)},
		"flow":    nil,
	}
	comps, err := Plan(g, []string{"opening", "closing"}, map[string]string{"opening": "opening_cash", "closing": "closing_cash"})
	if err != nil {
		t.Fatal(err)
	}
	var rec *Component
	for i := range comps {
		if comps[i].Recurrence {
			rec = &comps[i]
		}
	}
	if rec == nil || rec.Direction != DirectionForward {
		t.Fatalf("want a forward recurrence, got %+v", comps)
	}
	// Within a period, opening (read by closing at offset 0) comes first.
	if strings.Join(rec.Members, ",") != "opening,closing" {
		t.Errorf("zero-offset order %v, want opening,closing", rec.Members)
	}
}

func TestPlanFutureRecurrenceIsBackward(t *testing.T) {
	g := Graph{"a": {edge("b", 1, 1)}, "b": {edge("a", 0, 0)}}
	comps, err := Plan(g, []string{"a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !comps[0].Recurrence || comps[0].Direction != DirectionBackward {
		t.Errorf("want backward recurrence, got %+v", comps[0])
	}
}

func TestPlanSelfReference(t *testing.T) {
	if _, err := Plan(Graph{"a": {edge("a", -1, -1)}}, []string{"a"}, nil); err != nil {
		t.Errorf("strictly-past self reference must be legal: %v", err)
	}
	for name, g := range map[string]Graph{
		"self at zero":         {"a": {edge("a", 0, 0)}},
		"self decumulate":      {"a": {edge("a", -1, 0)}},
		"self unbounded":       {"a": {Edge{To: "a", UnboundedPast: true}}},
		"zero cycle":           {"a": {edge("b", 0, 0)}, "b": {edge("a", 0, 0)}},
		"mixed direction":      {"a": {edge("b", -1, -1)}, "b": {edge("a", 1, 1)}},
		"unbounded in cycle":   {"a": {edge("b", -1, -1)}, "b": {Edge{To: "a", UnboundedPast: true}}},
		"window includes zero": {"a": {edge("b", -2, 0)}, "b": {edge("a", 0, 0)}},
	} {
		_, err := Plan(g, []string{"a"}, map[string]string{"a": "A", "b": "B"})
		var te *TemporalError
		if !errors.As(err, &te) {
			t.Errorf("%s: want TemporalError, got %v", name, err)
			continue
		}
		if !strings.Contains(err.Error(), "TEMPORAL_CYCLE_NOT_CAUSAL") || !strings.Contains(err.Error(), "A") {
			t.Errorf("%s: message should carry the code and metric names: %v", name, err)
		}
	}
}
