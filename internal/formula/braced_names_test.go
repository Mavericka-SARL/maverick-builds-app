package formula

import (
	"strings"
	"testing"
)

// A dimension or metric whose name is not an identifier is written in
// braces, and a property of such a dimension follows the closing brace —
// reported live: the AI Developer could not reference "Setup Item" or a
// property of "Cost Center" at all.
func TestBracedNamesAndProperties(t *testing.T) {
	an, err := Analyze(`SUMIFS(amount, {Cost Center}.p_and_l_line, "Revenue") + IF({Setup Item} = "X", {Gross margin}, 0)`)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range an.PropertyRefs {
		if p.Dim == "Cost Center" && p.Property == "p_and_l_line" {
			found = true
		}
	}
	if !found {
		t.Errorf("property refs = %+v, want Cost Center.p_and_l_line", an.PropertyRefs)
	}
	var names []string
	for _, r := range an.References {
		names = append(names, r.Name)
	}
	for _, want := range []string{"amount", "Setup Item", "Gross margin"} {
		if !strings.Contains(strings.Join(names, "|"), want) {
			t.Errorf("references %v lack %q", names, want)
		}
	}

	out, changed := RenameProperty(`{Cost Center}.p_and_l_line & cc.p_and_l_line & {Cost Center}.other`, "cost center", "P_AND_L_LINE", "pl_line")
	if !changed || out != `{Cost Center}.pl_line & cc.p_and_l_line & {Cost Center}.other` {
		t.Errorf("rename = %q (%v)", out, changed)
	}
	if out, _ := RenameProperty(`region.factor * 2`, "region", "factor", "weight"); out != `region.weight * 2` {
		t.Errorf("plain rename = %q", out)
	}

	for _, c := range []struct{ src, want string }{
		{`{Cost Center}.p.x`, "exactly one dot"},
		{`{Cost Center}.2026`, "must start with a letter"},
		{`{Cost Center`, "no closing brace"},
	} {
		if _, err := Analyze(c.src); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Analyze(%q) = %v, want %q", c.src, err, c.want)
		}
	}

	for in, want := range map[string]string{"revenue": "revenue", "Setup Item": "{Setup Item}", "P&L": "{P&L}", "2026_plan": "{2026_plan}", "_x1": "_x1"} {
		if got := QuoteName(in); got != want {
			t.Errorf("QuoteName(%q) = %q, want %q", in, got, want)
		}
	}
}
