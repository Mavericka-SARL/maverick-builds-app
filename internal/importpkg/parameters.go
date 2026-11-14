package importpkg

import (
	"fmt"
	"strings"
	"time"
)

// RunParameter is a value a saved file loader asks for at each run (its
// config's "parameters"): the column it fills on every row, its label where
// it is asked, its type ("date" or "text") and whether a run needs it. A
// trial balance file says nothing of its period; the Group Controller gives
// it at upload instead of a developer editing the loader's fixed values
// every month (2026-11-13).
type RunParameter struct {
	Column   string `json:"column"`
	Label    string `json:"label,omitempty"`
	Type     string `json:"type,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// WithParameters is reshape with the run's values of params added as fixed
// values (a copy: the saved shape stays as it is). A parameter given no
// value keeps the shape's own fixed value for its column, if any; an error
// says which required value is missing or which date is not one.
func WithParameters(reshape *Reshape, params []RunParameter, values map[string]string) (*Reshape, error) {
	if len(params) == 0 {
		return reshape, nil
	}
	out := Reshape{}
	if reshape != nil {
		out = *reshape
	}
	constants := map[string]string{}
	for k, v := range out.Constants {
		constants[k] = v
	}
	for _, p := range params {
		col := strings.TrimSpace(p.Column)
		if col == "" {
			continue
		}
		label := p.Label
		if label == "" {
			label = col
		}
		v := strings.TrimSpace(values[col])
		if v == "" {
			if p.Required {
				return nil, fmt.Errorf("this load asks for %s: give it with the file (\"parameters\": {%q: …})", label, col)
			}
			continue
		}
		if p.Type == "date" {
			if _, err := time.Parse("2006-01-02", v); err != nil {
				return nil, fmt.Errorf("%s is a date (YYYY-MM-DD), not %q", label, v)
			}
		}
		constants[col] = v
	}
	out.Constants = constants
	return &out, nil
}

// CheckParameters is params trimmed and checked: each names a column, once,
// of type text or date.
func CheckParameters(params []RunParameter) ([]RunParameter, error) {
	out := make([]RunParameter, 0, len(params))
	seen := map[string]bool{}
	for _, p := range params {
		p.Column, p.Label, p.Type = strings.TrimSpace(p.Column), strings.TrimSpace(p.Label), strings.ToLower(strings.TrimSpace(p.Type))
		if p.Column == "" {
			return nil, fmt.Errorf("parameters: each names the column it fills (\"column\")")
		}
		if seen[p.Column] {
			return nil, fmt.Errorf("parameters: %q is asked twice", p.Column)
		}
		seen[p.Column] = true
		if p.Type != "" && p.Type != "text" && p.Type != "date" {
			return nil, fmt.Errorf("parameters: %q has type %q; a parameter is text or date", p.Column, p.Type)
		}
		out = append(out, p)
	}
	return out, nil
}
