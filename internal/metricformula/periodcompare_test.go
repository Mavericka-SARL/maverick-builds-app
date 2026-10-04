package metricformula

import (
	"strings"
	"testing"
)

// The AI Developer wrote IF(month <= actual_through_month, ...) twice: the
// bare time dimension is the period's code (text), so the comparison with a
// number saved cleanly and never meant what it said. It is refused with the
// MONTH(START()) idiom; comparing the code with text stays a real comparison.
func TestPeriodCodeComparedWithANumberIsRefused(t *testing.T) {
	f := setupDimFixture(t)
	for _, text := range []string{
		"IF(month <= other, revenue, 0)",
		"IF(month > 9, revenue, 0)",
		"IF(9 >= month, revenue, 0)",
		"IF(month = 3, revenue, 0)",
		"IF(month <= other + 1, revenue, 0)",
	} {
		_, err := f.validate(t, "", "probe", text)
		if err == nil || !strings.Contains(err.Error(), "MONTH(START())") {
			t.Errorf("%s: err = %v, want the MONTH(START()) refusal", text, err)
		}
	}
	for _, text := range []string{
		"IF(MONTH(START()) <= other, revenue, 0)",
		`IF(month >= "2026-07", revenue, 0)`,
		`IF(month = "2026-01", revenue, 0)`,
		"IF(region = \"EMEA\", revenue, 0)",
		"IF(other > 9, revenue, 0)",
	} {
		if _, err := f.validate(t, "", "probe", text); err != nil {
			t.Errorf("%s: refused: %v", text, err)
		}
	}
}

// Any format string used to be stored; "percent" then showed as a number.
func TestValidMetricFormat(t *testing.T) {
	for _, f := range []string{"", "number", "percentage", "currency", "boolean", "text"} {
		if err := ValidMetricFormat(f); err != nil {
			t.Errorf("%q refused: %v", f, err)
		}
	}
	if err := ValidMetricFormat("percent"); err == nil || !strings.Contains(err.Error(), `did you mean "percentage"`) {
		t.Errorf("percent: %v, want a refusal pointing at percentage", err)
	}
	if err := ValidMetricFormat("money"); err == nil {
		t.Error("money accepted")
	}
}

func TestSingleQuotedTextIsExplained(t *testing.T) {
	f := setupDimFixture(t)
	_, err := f.validate(t, "", "probe", `LOOKUP(revenue, region, 'EMEA')`)
	if err == nil || !strings.Contains(err.Error(), `write "EMEA", not 'EMEA'`) {
		t.Errorf("err = %v, want the double-quotes hint", err)
	}
}
