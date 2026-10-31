package metricformula

import (
	"testing"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// A number formula reads a text metric only as a comparison operand or in
// ISBLANK; anywhere else the text would read as 0.
func TestTextComparedOnly(t *testing.T) {
	for text, want := range map[string]bool{
		`IF(chk_status = "OK", 1, 0)`:                true,
		`IF("OK" <> chk_status, 0, 1)`:               true,
		`IF(ISBLANK(chk_status), 1, 0)`:              true,
		`IF(AND(chk_status = "OK", cash > 0), 1, 0)`: true,
		`chk_status + 1`:                             false,
		`LEN(chk_status)`:                            false,
		`IF(chk_status, 1, 0)`:                       false,
		`-chk_status`:                                false,
	} {
		node, err := formula.Parse(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if got := textComparedOnly(node, "chk_status"); got != want {
			t.Errorf("textComparedOnly(%s) = %v, want %v", text, got, want)
		}
	}
}
