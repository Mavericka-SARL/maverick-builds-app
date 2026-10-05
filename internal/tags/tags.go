// Package tags holds the one rule for the free-form labels developers put
// on dashboards, grids, dimensions and metrics.
package tags

import "strings"

// Clean returns the tags as the console stores them: each trimmed, lower
// case, with runs of white space turned into a hyphen ("Cost Centre" →
// "cost-centre"), empties and repeats dropped, first occurrence's order kept.
// The result is never nil, so it writes as '{}' rather than NULL.
func Clean(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, t := range in {
		t = strings.ToLower(strings.Join(strings.Fields(t), "-"))
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
