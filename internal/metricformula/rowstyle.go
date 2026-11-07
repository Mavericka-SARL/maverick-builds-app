package metricformula

import (
	"fmt"
	"strings"
)

// RowStyleTotal draws a metric's row as a total wherever a grid shows it:
// its label bold, with a rule above (model.metric_def.row_style, migration
// 159). A statement's EBITDA and EBIT read like any other line without it.
const RowStyleTotal = "total"

// RowStyle is style as stored ("" or RowStyleTotal), or an error saying what
// a row style may be.
func RowStyle(style string) (string, error) {
	switch s := strings.ToLower(strings.TrimSpace(style)); s {
	case "", RowStyleTotal:
		return s, nil
	default:
		return "", fmt.Errorf("row_style is %q or empty, not %q", RowStyleTotal, style)
	}
}
