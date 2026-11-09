package formula

import "testing"

// TestRenameDimension: a dimension's rename reaches every place a formula
// names it — bare, in braces, as a dim.property prefix, as LOOKUP's and a
// criteria range's dimension and PARENT's argument — and nothing else: a
// string, a function of that name and a property spelled like the dimension
// stay byte-for-byte.
func TestRenameDimension(t *testing.T) {
	cases := []struct {
		name, in, old, new, want string
		changed                  bool
	}{
		{"property prefix", `=revenue * region.factor`, "region", "territory", `=revenue * territory.factor`, true},
		{"bare comparison", `IF(region = "EMEA", 1, 0)`, "region", "territory", `IF(territory = "EMEA", 1, 0)`, true},
		{"braced bare name", `{region} & "x"`, "region", "territory", `territory & "x"`, true},
		{"LOOKUP dimension", `LOOKUP(revenue, region, "EMEA")`, "region", "territory", `LOOKUP(revenue, territory, "EMEA")`, true},
		{"criteria range, any case", `SUMIFS(revenue, Region, "EMEA")`, "region", "territory", `SUMIFS(revenue, territory, "EMEA")`, true},
		{"PARENT", `IF(PARENT(region) = "World", 1, 0)`, "region", "territory", `IF(PARENT(territory) = "World", 1, 0)`, true},
		{"strings, functions and a property named like it untouched",
			`"region" & x.region & region(1)`, "region", "territory", `"region" & x.region & region(1)`, false},
		{"braced dimension with a property", `{Cost Center}.p + 1`, "Cost Center", "Cost Centre", `{Cost Centre}.p + 1`, true},
		{"a plain name becoming a spaced one is braced", `region.factor * region`, "region", "Sales Region", `{Sales Region}.factor * {Sales Region}`, true},
		{"a braced name becoming a plain one", `{Cost Center}.p * {cost center}`, "Cost Center", "cc", `cc.p * cc`, true},
		{"non-ASCII before the token", `IF(x = "Zürich", region.factor, 0)`, "region", "territory", `IF(x = "Zürich", territory.factor, 0)`, true},
		{"nothing to do", `revenue - cogs`, "region", "territory", `revenue - cogs`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := RenameDimension(c.in, c.old, c.new)
			if got != c.want || changed != c.changed {
				t.Fatalf("RenameDimension(%q, %q, %q) = %q, %v; want %q, %v", c.in, c.old, c.new, got, changed, c.want, c.changed)
			}
		})
	}
}

// TestRenameMemberCode: a member's new code reaches the literals that name
// it — exactly where the member-delete guard (NamesMember) sees them — and
// no other literal: a LOOKUP member (exact), an =, "=" or "<>" criterion
// on the dimension's code range, an = or <> comparison with the bare
// dimension or PARENT(dim), a SWITCH match value and, on the time
// dimension, a TIMESUM bound. Without the "<>" case SUMIFS(x, region,
// "<>EMEA") would silently start counting the renamed member.
func TestRenameMemberCode(t *testing.T) {
	cases := []struct {
		name, in, dim, old, new, want string
		timeCodes, changed            bool
	}{
		{"LOOKUP member", `LOOKUP(revenue, region, "EMEA")`, "region", "EMEA", "EUR", `LOOKUP(revenue, region, "EUR")`, false, true},
		{"LOOKUP matches exactly", `LOOKUP(revenue, region, "emea")`, "region", "EMEA", "EUR", `LOOKUP(revenue, region, "emea")`, false, false},
		{"comparison", `IF(region = "EMEA", 1, 0)`, "region", "EMEA", "EUR", `IF(region = "EUR", 1, 0)`, false, true},
		{"comparison either side, any case", `IF("emea" <> Region, 1, 0)`, "region", "EMEA", "EUR", `IF("EUR" <> Region, 1, 0)`, false, true},
		{"PARENT comparison", `IF(PARENT(region) = "EMEA", 1, 0)`, "region", "EMEA", "EUR", `IF(PARENT(region) = "EUR", 1, 0)`, false, true},
		{"criteria keep their operator",
			`SUMIFS(revenue, region, "EMEA") + SUMIFS(revenue, region, "=EMEA") + SUMIFS(revenue, region, "<>EMEA")`, "region", "EMEA", "EUR",
			`SUMIFS(revenue, region, "EUR") + SUMIFS(revenue, region, "=EUR") + SUMIFS(revenue, region, "<>EUR")`, false, true},
		{"patterns, orderings and property ranges untouched",
			`SUMIFS(revenue, region, "EM*") + SUMIFS(revenue, region, ">EMEA") + SUMIFS(revenue, region.segment, "EMEA")`, "region", "EMEA", "EUR",
			`SUMIFS(revenue, region, "EM*") + SUMIFS(revenue, region, ">EMEA") + SUMIFS(revenue, region.segment, "EMEA")`, false, false},
		{"SWITCH match value, not its default", `SWITCH(region, "EMEA", 1, "US", 2, "EMEA")`, "region", "EMEA", "EUR",
			`SWITCH(region, "EUR", 1, "US", 2, "EMEA")`, false, true},
		{"another dimension untouched", `LOOKUP(revenue, product, "EMEA") + IF(product = "EMEA", 1, 0)`, "region", "EMEA", "EUR",
			`LOOKUP(revenue, product, "EMEA") + IF(product = "EMEA", 1, 0)`, false, false},
		{"a string not compared with the dimension untouched", `IF(x = "EMEA", 1, 0) & "EMEA" & region`, "region", "EMEA", "EUR",
			`IF(x = "EMEA", 1, 0) & "EMEA" & region`, false, false},
		{"leading space and =", `  = LOOKUP(revenue, region, "EMEA")`, "region", "EMEA", "EUR", `  = LOOKUP(revenue, region, "EUR")`, false, true},
		{"non-ASCII and a doubled quote before", `IF(x = "Zürich ""A""", LOOKUP(revenue, region, "EMEA"), 0)`, "region", "EMEA", "EUR",
			`IF(x = "Zürich ""A""", LOOKUP(revenue, region, "EUR"), 0)`, false, true},
		{"a number code", `IF(region = 100, SUMIFS(revenue, region, 100), 0)`, "region", "100", "R100",
			`IF(region = "R100", SUMIFS(revenue, region, "R100"), 0)`, false, true},
		{"a new code with a quote", `LOOKUP(revenue, region, "EMEA")`, "region", "EMEA", `E"U`, `LOOKUP(revenue, region, "E""U")`, false, true},
		{"a criterion escapes a new code's wildcards", `SUMIFS(revenue, region, "EMEA") + LOOKUP(revenue, region, "EMEA")`, "region", "EMEA", "EU*",
			`SUMIFS(revenue, region, "EU~*") + LOOKUP(revenue, region, "EU*")`, false, true},
		{"TIMESUM bound on the time dimension", `TIMESUM(revenue, "2026-01", "2026-03")`, "month", "2026-01", "2026-M01",
			`TIMESUM(revenue, "2026-M01", "2026-03")`, true, true},
		{"TIMESUM bound elsewhere untouched", `TIMESUM(revenue, "2026-01", "2026-03")`, "month", "2026-01", "2026-M01",
			`TIMESUM(revenue, "2026-01", "2026-03")`, false, false},
		{"a formula that does not parse", `LOOKUP(revenue, region, "EMEA"`, "region", "EMEA", "EUR", `LOOKUP(revenue, region, "EMEA"`, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := RenameMemberCode(c.in, c.dim, c.old, c.new, c.timeCodes)
			if got != c.want || changed != c.changed {
				t.Fatalf("RenameMemberCode(%q, %s, %q→%q) = %q, %v; want %q, %v", c.in, c.dim, c.old, c.new, got, changed, c.want, c.changed)
			}
			// What the rename leaves, the delete guard no longer sees under
			// the old code (save a code the rename does not cover).
			if c.changed && NamesMember(got, c.dim, c.old, c.timeCodes) {
				t.Errorf("after the rename %q still names %s", got, c.old)
			}
		})
	}
}
