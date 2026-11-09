package formula

import "testing"

// LOOKUP of a source that has no value at the overridden coordinates by its
// own rules (DimEvalContext.NoValue: time summary none over several periods,
// aggregation none above its members) is blank, as the *VALUE functions
// are; a member with nothing recorded still reads 0.
func TestLookupBlankWhereTheSourceHasNoValueByItsRules(t *testing.T) {
	m := dimModel()
	cell := map[string]string{"region": "DE", "product": "A100", "currency": "EUR"}
	nothing := `LOOKUP(sales, region, "FR", product, "A200")` // Resolve: (0, found=false)

	ctx := m.ctx(cell)
	var asked string
	ctx.Dim.NoValue = func(metric string, overrides map[string]string) bool {
		asked = metric + ":" + overrides["region"]
		return true
	}
	if v := evalIn(t, ctx, nothing); !v.IsBlank() {
		t.Errorf("%s with NoValue true = %v, want blank", nothing, v)
	}
	if asked != "sales:FR" {
		t.Errorf("NoValue asked about %q, want sales at FR", asked)
	}
	if v := evalIn(t, ctx, "IF(ISBLANK("+nothing+"), 1, 2)"); !isNum(v, 1) {
		t.Errorf("ISBLANK of it = %v, want TRUE (1)", v)
	}
	// A recorded value is never asked about.
	if v := evalIn(t, ctx, `LOOKUP(sales, region, "US")`); !isNum(v, 100) {
		t.Errorf("a recorded value = %v, want 100", v)
	}

	ctx = m.ctx(cell)
	ctx.Dim.NoValue = func(string, map[string]string) bool { return false }
	if v := evalIn(t, ctx, nothing); !isNum(v, 0) {
		t.Errorf("nothing recorded, NoValue false = %v, want 0", v)
	}
	if v := evalIn(t, m.ctx(cell), nothing); !isNum(v, 0) {
		t.Errorf("nothing recorded, no NoValue = %v, want 0", v)
	}
}

func isNum(v Value, want float64) bool {
	n, ok := v.Number()
	return ok && !v.IsBlank() && n == want
}
