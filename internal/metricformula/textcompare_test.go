package metricformula

import (
	"testing"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// A number formula reads a text metric at the cell — compared, joined, as a
// function argument or a LOOKUP member — but not in arithmetic or as a
// condition by itself (2026-11-20).
func TestTextInArithmetic(t *testing.T) {
	for text, want := range map[string]bool{
		`IF(chk_status = "OK", 1, 0)`:                false,
		`IF("OK" <> chk_status, 0, 1)`:               false,
		`IF(ISBLANK(chk_status), 1, 0)`:              false,
		`IF(AND(chk_status = "OK", cash > 0), 1, 0)`: false,
		`LEN(chk_status)`:                            false,
		`LOOKUP(drv, week, chk_status)`:              false,
		`VALUE(chk_status) * 2`:                      false,
		`VALUE(RIGHT(chk_status, 4)) + 1`:            false,
		`IF(LEFT(chk_status, 1) = "W", 1, 0)`:        false,
		`chk_status + 1`:                             true,
		`2 * chk_status`:                             true,
		`-chk_status`:                                true,
		`IF(chk_status, 1, 0)`:                       true,
		`RIGHT(chk_status, 4) + VALUE(chk_status)`:   false, // a function's result, not the text
	} {
		node, err := formula.Parse(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if got := textInArithmetic(node, "chk_status"); got != want {
			t.Errorf("textInArithmetic(%s) = %v, want %v", text, got, want)
		}
	}
}
