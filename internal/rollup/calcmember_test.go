package rollup

import (
	"math"
	"strings"
	"testing"
)

func TestEvalCalculated(t *testing.T) {
	vals := map[string]float64{"RF": 120, "LY": 100}
	get := func(code string) (float64, bool, error) { v, ok := vals[strings.ToUpper(code)]; return v, ok, nil }
	const varPct = `IF(METRICFORMAT() = "percentage", {RF} - {LY}, IF({LY} = 0, 0, ({RF} - {LY}) / ABS({LY}) * 100))`
	for _, tc := range []struct {
		text, format string
		want         float64
		ok           bool
	}{
		{"{RF} - {LY}", "currency", 20, true},
		{"rf - ly", "currency", 20, true}, // codes match case-insensitively
		{varPct, "currency", 20, true},
		{varPct, "percentage", 20, true},
		{"{RF} / {MISSING}", "number", 0, false}, // a division by zero is no value
	} {
		got, ok, err := EvalCalculated(tc.text, tc.format, get)
		if err != nil || ok != tc.ok || (ok && math.Abs(got-tc.want) > 1e-9) {
			t.Errorf("%s (%s) = %v, %v, %v; want %v, %v", tc.text, tc.format, got, ok, err, tc.want, tc.ok)
		}
	}
	vals = map[string]float64{"RF": 120, "LY": 90}
	if got, _, _ := EvalCalculated(varPct, "currency", get); math.Abs(got-100.0/3) > 1e-9 {
		t.Errorf("var %% = %v, want 33.33", got)
	}
	// Nothing to read: no value, so an empty row stays empty.
	empty := func(string) (float64, bool, error) { return 0, false, nil }
	if _, ok, _ := EvalCalculated("{RF} - {LY}", "number", empty); ok {
		t.Error("a formula over members with no value should have none")
	}
}
