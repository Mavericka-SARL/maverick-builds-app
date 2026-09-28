package metricformula

import (
	"errors"
	"strings"
	"testing"
)

func edge(to string, min, max int) Edge { return Edge{To: to, MinTimeOffset: min, MaxTimeOffset: max} }

func gr(edges map[string][]Edge) Graph { return Graph{Edges: edges} }

func TestPlanAcyclicGraphIsDependencyOrdered(t *testing.T) {
	g := gr(map[string][]Edge{"c": {edge("b", 0, 0)}, "b": {edge("a", -1, -1)}, "a": nil})
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
	g := gr(map[string][]Edge{
		"opening": {edge("closing", -1, -1)},
		"closing": {edge("opening", 0, 0), edge("flow", 0, 0)},
		"flow":    nil,
	})
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
	g := gr(map[string][]Edge{"a": {edge("b", 1, 1)}, "b": {edge("a", 0, 0)}})
	comps, err := Plan(g, []string{"a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !comps[0].Recurrence || comps[0].Direction != DirectionBackward {
		t.Errorf("want backward recurrence, got %+v", comps[0])
	}
}

func TestPlanSelfReference(t *testing.T) {
	if _, err := Plan(gr(map[string][]Edge{"a": {edge("a", -1, -1)}}), []string{"a"}, nil); err != nil {
		t.Errorf("strictly-past self reference must be legal: %v", err)
	}
	for name, g := range map[string]map[string][]Edge{
		"self at zero":         {"a": {edge("a", 0, 0)}},
		"self decumulate":      {"a": {edge("a", -1, 0)}},
		"self unbounded":       {"a": {Edge{To: "a", UnboundedPast: true}}},
		"zero cycle":           {"a": {edge("b", 0, 0)}, "b": {edge("a", 0, 0)}},
		"mixed direction":      {"a": {edge("b", -1, -1)}, "b": {edge("a", 1, 1)}},
		"unbounded in cycle":   {"a": {edge("b", -1, -1)}, "b": {Edge{To: "a", UnboundedPast: true}}},
		"window includes zero": {"a": {edge("b", -2, 0)}, "b": {edge("a", 0, 0)}},
	} {
		_, err := Plan(gr(g), []string{"a"}, map[string]string{"a": "A", "b": "B"})
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

// Contract C4: a metric calling LOOKUP or a conditional aggregation may not
// belong to a recurrence, whatever it reads — even a sourceless COUNTIFS
// with no edge at all — and the message names the function.
func TestPlanRefusesDimensionalCallInRecurrence(t *testing.T) {
	names := map[string]string{"opening": "opening_cash", "closing": "closing_cash", "flow": "flow"}
	for fn, formulaText := range map[string]string{
		"LOOKUP":   `LOOKUP(flow, region, "EMEA")`,
		"COUNTIFS": `COUNTIFS(region, "E*") + opening`,
		"SUMIFS":   `SUMIFS(flow, region.segment, "SMB")`,
	} {
		g := NewGraph()
		g.SetMetric("opening", []Edge{edge("closing", -1, -1)}, "LAG(closing, 1, 0)")
		g.SetMetric("closing", []Edge{edge("opening", 0, 0), edge("flow", 0, 0)}, formulaText)
		g.SetMetric("flow", nil, "")
		_, err := Plan(g, []string{"opening"}, names)
		var te *TemporalError
		if !errors.As(err, &te) {
			t.Fatalf("%s: want TemporalError, got %v", fn, err)
		}
		if !strings.Contains(err.Error(), fn) || !strings.Contains(err.Error(), "closing_cash") {
			t.Errorf("%s: message should name the function and the metric: %v", fn, err)
		}
		if strings.Contains(err.Error(), "PREVIOUS") {
			t.Errorf("%s: message should not talk about PREVIOUS/LAG: %v", fn, err)
		}
	}
	// Outside a recurrence the same formula plans normally.
	g := NewGraph()
	g.SetMetric("x", []Edge{edge("flow", 0, 0)}, `LOOKUP(flow, region, "EMEA")`)
	g.SetMetric("flow", nil, "")
	if _, err := Plan(g, []string{"x"}, nil); err != nil {
		t.Errorf("LOOKUP outside a recurrence must plan: %v", err)
	}
	// Plain property references are cell-local and allowed in a recurrence.
	g = NewGraph()
	g.SetMetric("opening", []Edge{edge("closing", -1, -1)}, "LAG(closing, 1, 0) * region.factor")
	g.SetMetric("closing", []Edge{edge("opening", 0, 0)}, "opening + 1")
	if _, err := Plan(g, []string{"opening"}, nil); err != nil {
		t.Errorf("dim.property inside a recurrence must plan: %v", err)
	}
}
