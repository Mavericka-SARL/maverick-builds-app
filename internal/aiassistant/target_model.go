package aiassistant

// The target model (owner, 2026-11-12): building from a workbook, the AI
// Developer first analyses the whole workbook and records the TO-BE model —
// dimensions, each sheet's role, grid, dimensions and metrics, the pages,
// roles and workflows — and only then plans and builds, stage by stage,
// always in its context. It lives with the task plan (carried to every new
// session) and is shown in every turn.

import (
	"fmt"
	"strings"
)

// TargetModel is the TO-BE design of a model built from a workbook.
type TargetModel struct {
	Summary    string            `json:"summary"`
	Dimensions []TargetDimension `json:"dimensions"`
	Sheets     []TargetSheet     `json:"sheets"`
	Pages      []string          `json:"pages,omitempty"`
	Roles      []string          `json:"roles,omitempty"`
	Workflows  []string          `json:"workflows,omitempty"`
}

// TargetDimension is one dimension of the target model.
type TargetDimension struct {
	Name    string `json:"name"`
	Members string `json:"members"` // what its members are, or where they come from
}

// TargetSheet is what one workbook sheet becomes, in build order.
type TargetSheet struct {
	Sheet      string   `json:"sheet"`
	Role       string   `json:"role"` // input | calculation | report | actuals | guide | skip
	Grid       string   `json:"grid,omitempty"`
	Dimensions []string `json:"dimensions,omitempty"`
	Rows       string   `json:"rows,omitempty"` // the sheet rows it covers, e.g. "6-77"
	Metrics    []string `json:"metrics,omitempty"`
	Notes      string   `json:"notes,omitempty"`
}

var targetRoles = map[string]bool{"input": true, "calculation": true, "report": true, "actuals": true, "guide": true, "skip": true}

// CheckTarget checks a target the AI wrote; sheets (may be nil) are the
// attached workbooks' sheets: every sheet named must be one of them, and the
// ones the target leaves out are returned so the AI can place them.
func CheckTarget(t *TargetModel, sheets []string) (missing []string, err error) {
	if t == nil || strings.TrimSpace(t.Summary) == "" {
		return nil, fmt.Errorf("set_target_model needs a summary: the model to build, in two or three sentences")
	}
	if len(t.Sheets) == 0 {
		return nil, fmt.Errorf("set_target_model needs sheets: what each workbook sheet becomes, in build order")
	}
	known := map[string]string{}
	for _, s := range sheets {
		known[strings.ToLower(strings.TrimSpace(s))] = s
	}
	seen := map[string]bool{}
	for i := range t.Sheets {
		s := &t.Sheets[i]
		s.Role = strings.ToLower(strings.TrimSpace(s.Role))
		if !targetRoles[s.Role] {
			return nil, fmt.Errorf("set_target_model: sheet %q: role is input, calculation, report, actuals, guide or skip", s.Sheet)
		}
		k := strings.ToLower(strings.TrimSpace(s.Sheet))
		if len(known) > 0 {
			name, ok := known[k]
			if !ok {
				return nil, fmt.Errorf("set_target_model: no attached sheet is named %q — the workbook's sheets: %s", s.Sheet, strings.Join(sheets, ", "))
			}
			s.Sheet = name
		}
		if seen[k] {
			return nil, fmt.Errorf("set_target_model: sheet %q is listed twice", s.Sheet)
		}
		seen[k] = true
		if (s.Role == "input" || s.Role == "calculation" || s.Role == "report" || s.Role == "actuals") && strings.TrimSpace(s.Grid) == "" {
			return nil, fmt.Errorf("set_target_model: sheet %q (%s) needs the grid it becomes", s.Sheet, s.Role)
		}
	}
	for _, s := range sheets {
		if !seen[strings.ToLower(strings.TrimSpace(s))] {
			missing = append(missing, s)
		}
	}
	return missing, nil
}

// BuildSheets are the target's sheets that are built (not guide or skip),
// in build order.
func (t *TargetModel) BuildSheets() []string {
	if t == nil {
		return nil
	}
	var out []string
	for _, s := range t.Sheets {
		if s.Role != "guide" && s.Role != "skip" {
			out = append(out, s.Sheet)
		}
	}
	return out
}

// MetricNames are every metric the target names, lower-cased.
func (t *TargetModel) MetricNames() map[string]bool {
	out := map[string]bool{}
	if t == nil {
		return out
	}
	for _, s := range t.Sheets {
		for _, m := range s.Metrics {
			if m = strings.ToLower(strings.TrimSpace(m)); m != "" {
				out[m] = true
			}
		}
	}
	return out
}

// Render is the whole target (set_target_model's answer).
func (t *TargetModel) Render() string { return t.RenderFor("") }

// RenderFor is the target as a turn reads it: with a current sheet, that
// sheet whole and the others one line each (2026-11-12: the whole target,
// 18k characters, went with every call).
func (t *TargetModel) RenderFor(current string) string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("TARGET MODEL (the TO-BE design you build towards — every plan stays within it; change it with set_target_model when the workbook shows it must):\n")
	fmt.Fprintf(&b, "%s\n", strings.TrimSpace(t.Summary))
	if len(t.Dimensions) > 0 {
		var ds []string
		for _, d := range t.Dimensions {
			ds = append(ds, d.Name+" ("+d.Members+")")
		}
		fmt.Fprintf(&b, "Dimensions: %s\n", strings.Join(ds, "; "))
	}
	b.WriteString("Sheets in build order:\n")
	for _, s := range t.Sheets {
		fmt.Fprintf(&b, "- %s [%s]", s.Sheet, s.Role)
		if s.Grid != "" {
			fmt.Fprintf(&b, " → grid %q", s.Grid)
			if len(s.Dimensions) > 0 {
				fmt.Fprintf(&b, " by %s", strings.Join(s.Dimensions, " × "))
			}
		}
		if s.Rows != "" {
			fmt.Fprintf(&b, ", rows %s", s.Rows)
		}
		if current != "" && !strings.EqualFold(s.Sheet, current) {
			if n := len(s.Metrics); n > 0 {
				fmt.Fprintf(&b, ", %d metrics", n)
			}
			b.WriteByte('\n')
			continue
		}
		if n := len(s.Metrics); n > 0 {
			shown := s.Metrics
			if n > 12 {
				shown = append(append([]string{}, s.Metrics[:12]...), fmt.Sprintf("… %d more", n-12))
			}
			fmt.Fprintf(&b, ", %d metrics: %s", n, strings.Join(shown, ", "))
		}
		if s.Notes != "" {
			fmt.Fprintf(&b, " — %s", s.Notes)
		}
		b.WriteByte('\n')
	}
	for _, l := range []struct {
		name  string
		items []string
	}{{"Pages", t.Pages}, {"Roles", t.Roles}, {"Workflows and triggers", t.Workflows}} {
		if len(l.items) > 0 {
			fmt.Fprintf(&b, "%s: %s\n", l.name, strings.Join(l.items, "; "))
		}
	}
	return b.String()
}
