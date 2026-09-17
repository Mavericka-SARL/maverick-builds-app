package integration

import (
	"fmt"
	"regexp"
	"strings"
)

// Templating is a closed namespace, not an expression language. Exactly the
// documented variables resolve; anything else is a validation error at save
// time and a hard error at run time — never silently left in the payload.
//
//	{{context.application_id}} {{context.model_id}} {{context.revision_id}}
//	{{run.id}} {{run.started_at}}
//	{{page.number}} {{page.cursor}}
//	{{row.<field>}}            (Push only; <field> is a mapped field name)
//
// Variables may appear in query values, header values, request bodies, and
// the URL PATH — never in the scheme or host (enforced by ValidateURL).

var templateVarRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_.]+)\s*\}\}`)

// TemplateContext carries the resolvable values for one substitution pass.
// Nil maps mean "namespace not available here" (e.g. row.* outside Push).
type TemplateContext struct {
	Context map[string]string // application_id, model_id, revision_id
	Run     map[string]string // id, started_at
	Page    map[string]string // number, cursor
	Row     map[string]string // Push per-record fields
}

// ValidateTemplate checks every variable in s against the closed namespace.
// rowFields is the set of mapped field names ("" key set means row.* is not
// permitted in this position at all).
func ValidateTemplate(s string, rowAllowed bool, rowFields map[string]bool) error {
	for _, m := range templateVarRe.FindAllStringSubmatch(s, -1) {
		name := m[1]
		switch name {
		case "context.application_id", "context.model_id", "context.revision_id",
			"run.id", "run.started_at", "page.number", "page.cursor":
			continue
		}
		if f, ok := strings.CutPrefix(name, "row."); ok {
			if !rowAllowed {
				return fmt.Errorf("variable {{row.%s}} is only available in Push templates", f)
			}
			if rowFields != nil && !rowFields[f] {
				return fmt.Errorf("unknown row field %q in template", f)
			}
			continue
		}
		return fmt.Errorf("unknown template variable {{%s}}", name)
	}
	// Reject stray/unbalanced braces that the regex didn't consume — they
	// hide typos that would otherwise ship literally to the remote API.
	stripped := templateVarRe.ReplaceAllString(s, "")
	if strings.Contains(stripped, "{{") || strings.Contains(stripped, "}}") {
		return fmt.Errorf("malformed template braces")
	}
	return nil
}

// Render substitutes every variable; unresolvable variables are errors.
func Render(s string, tc TemplateContext) (string, error) {
	var rerr error
	out := templateVarRe.ReplaceAllStringFunc(s, func(m string) string {
		name := templateVarRe.FindStringSubmatch(m)[1]
		var v string
		var ok bool
		switch {
		case strings.HasPrefix(name, "context."):
			v, ok = tc.Context[strings.TrimPrefix(name, "context.")]
		case strings.HasPrefix(name, "run."):
			v, ok = tc.Run[strings.TrimPrefix(name, "run.")]
		case strings.HasPrefix(name, "page."):
			v, ok = tc.Page[strings.TrimPrefix(name, "page.")]
		case strings.HasPrefix(name, "row."):
			v, ok = tc.Row[strings.TrimPrefix(name, "row.")]
		}
		if !ok && rerr == nil {
			rerr = fmt.Errorf("template variable {{%s}} has no value in this context", name)
		}
		return v
	})
	return out, rerr
}
