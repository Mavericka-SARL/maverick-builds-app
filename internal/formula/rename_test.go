package formula

import "testing"

func TestRenameProperty(t *testing.T) {
	cases := []struct {
		name, in, want string
		changed        bool
	}{
		{"value", `=revenue * region.fact`, `=revenue * region.factor`, true},
		{"case-insensitive match keeps the dimension as written", `Region.FACT + 1`, `Region.factor + 1`, true},
		{"every occurrence", `SUMIFS(revenue, region.fact, region.fact)`, `SUMIFS(revenue, region.factor, region.factor)`, true},
		{"LOOKUP member", `LOOKUP(fx, currency, region.fact)`, `LOOKUP(fx, currency, region.factor)`, true},
		{"string literal untouched", `IF(x = "region.fact", region.fact, 0)`, `IF(x = "region.fact", region.factor, 0)`, true},
		{"brace metric name untouched", `{region.fact} + region.fact`, `{region.fact} + region.factor`, true},
		{"other dimension untouched", `country.fact + region.fact`, `country.fact + region.factor`, true},
		{"other property untouched", `region.factory + region.fac`, `region.factory + region.fac`, false},
		{"non-ASCII before the token", `IF(name = "Zürich", region.fact, 0)`, `IF(name = "Zürich", region.factor, 0)`, true},
		{"nothing to do", `revenue - cogs`, `revenue - cogs`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := RenameProperty(c.in, "region", "fact", "factor")
			if got != c.want || changed != c.changed {
				t.Fatalf("RenameProperty(%q) = %q, %v; want %q, %v", c.in, got, changed, c.want, c.changed)
			}
		})
	}
}

func TestRenameIdent(t *testing.T) {
	for in, want := range map[string]string{
		"{RF} - LY":                "{FCST 2026} - LY",
		"IF(rf = 0, 0, {LY} / RF)": "IF({FCST 2026} = 0, 0, {LY} / {FCST 2026})",
		`RF("x") + "RF" + RFX`:     `RF("x") + "RF" + RFX`,
		"ABS(RF)":                  "ABS({FCST 2026})",
	} {
		if got, _ := RenameIdent(in, "RF", "FCST 2026"); got != want {
			t.Errorf("RenameIdent(%q) = %q, want %q", in, got, want)
		}
	}
}
