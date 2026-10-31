package recordpost

import (
	"encoding/json"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// A record posts its number, and its date as the serial a date-format cell
// stores; other text posts nothing.
func TestMappingValueReadsNumbersAndDates(t *testing.T) {
	open, _ := formula.DateSerial("2026-03-01")
	for _, c := range []struct {
		v    any
		want float64
		ok   bool
	}{
		{12.5, 12.5, true},
		{json.Number("7"), 7, true},
		{"2026-03-01", open, true},
		{"abc", 0, false},
		{nil, 0, false},
	} {
		got, ok := formMapping{sourceField: "f"}.value(map[string]any{"f": c.v})
		if ok != c.ok || got != c.want {
			t.Errorf("value(%v) = %v, %v; want %v, %v", c.v, got, ok, c.want, c.ok)
		}
	}
}
