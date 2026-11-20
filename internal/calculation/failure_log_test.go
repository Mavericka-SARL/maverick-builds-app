package calculation

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// A partition failing the same way on every recalculation is logged as an
// error once, then at debug, and again as an error when its failure changes
// or after it calculated (2026-11-20: ~5,000 lines in two minutes).
func TestRepeatedFailureIsLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	s := &Scheduler{log: zerolog.New(&buf).Level(zerolog.InfoLevel)}
	errorLines := func() int { return strings.Count(buf.String(), `"level":"error"`) }
	pk := "test-partition-repeated-failure"
	fail := func(n int, first string) {
		s.logFailure(pk, "cf_equity", errors.New(
			"metric cf_equity: "+string(rune('0'+n))+"/39 combos failed to evaluate for metric cf_equity (first error: "+first+")"))
	}
	fail(4, "#N/A: LOOKUP: week has no member \"1899-12-30\"")
	fail(5, "#N/A: LOOKUP: week has no member \"1899-12-30\"")
	fail(4, "#N/A: LOOKUP: week has no member \"1899-12-30\"")
	if got := errorLines(); got != 1 {
		t.Fatalf("the same failure three times: %d error lines, want 1\n%s", got, buf.String())
	}
	fail(4, "#DIV/0!: Division by zero")
	if got := errorLines(); got != 2 {
		t.Fatalf("a changed failure: %d error lines, want 2", got)
	}
	forgetFailure(pk)
	fail(4, "#DIV/0!: Division by zero")
	if got := errorLines(); got != 3 {
		t.Fatalf("failing again after a clean pass: %d error lines, want 3", got)
	}
}
