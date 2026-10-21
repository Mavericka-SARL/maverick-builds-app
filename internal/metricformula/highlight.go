package metricformula

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Highlight rules (migration 111): how a metric's cells are tinted, as a
// spreadsheet's conditional formatting does — "the variance % in red when
// its absolute value is over the threshold". Each rule compares a value at
// the cell with a constant or with another metric, and the first rule that
// holds gives the cell its tone. Grids evaluate them on every cell they show
// (leaves and totals alike); metrics are named as formulas name them, so a
// revision copy or a model export carries rules unchanged.

// HighlightRule is one rule.
type HighlightRule struct {
	// Metric tests another metric's value at the same cell instead of the
	// cell's own (highlight a target when its status is "Review").
	Metric string `json:"metric,omitempty"`
	// Abs compares the absolute value.
	Abs bool `json:"abs,omitempty"`
	// Op: > >= < <= = <> between not_between blank not_blank.
	Op string `json:"op"`
	// Value is the constant compared with: a number, or for = and <> a
	// text (a pick-list member's code, a note).
	Value any `json:"value,omitempty"`
	// Value2 is between's upper bound.
	Value2 *float64 `json:"value2,omitempty"`
	// Than compares with another metric's value at the cell (its total
	// when it is not on the grid: a threshold setting) instead of Value.
	Than string `json:"than,omitempty"`
	// Tone: negative | warning | positive | info.
	Tone string `json:"tone"`
	// Members limits the rule to the cells of these members (codes) — of
	// Dimension when it is named, else of any dimension. A variance is a
	// member (a calculated VAR column beside Plan and Actual), and a rule
	// "> 0 is red" on a cost line used to colour the costs themselves; with
	// "members": ["VAR"] it colours the variance only (2026-10-20, IBCS).
	Members   []string `json:"members,omitempty"`
	Dimension string   `json:"dimension,omitempty"`
}

// HighlightOps and HighlightTones are what a rule may say.
var (
	HighlightOps   = []string{">", ">=", "<", "<=", "=", "<>", "between", "not_between", "blank", "not_blank"}
	HighlightTones = []string{"negative", "warning", "positive", "info"}
)

// maxHighlightRules bounds one metric's rules.
const maxHighlightRules = 10

// CodeInvalidHighlightRule refuses a rule the grid could not apply.
const CodeInvalidHighlightRule = "INVALID_HIGHLIGHT_RULE"

// CheckHighlightRules validates a metric's rules against its revision and
// returns them normalised (metric names as the revision spells them), as
// JSON to store. raw empty or null is no rules.
func CheckHighlightRules(ctx context.Context, q Querier, modelID, revisionID, metricName string, raw json.RawMessage) (json.RawMessage, error) {
	if t := strings.TrimSpace(string(raw)); t == "" || t == "null" {
		return json.RawMessage("[]"), nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var rules []HighlightRule
	if err := dec.Decode(&rules); err != nil {
		return nil, invalidCode(CodeInvalidHighlightRule,
			`highlight_rules is a list of {"op", "value" | "than", "abs", "metric", "tone", "members", "dimension"}: %v`, err)
	}
	if len(rules) > maxHighlightRules {
		return nil, invalidCode(CodeInvalidHighlightRule, "a metric has at most %d highlight rules (this has %d)", maxHighlightRules, len(rules))
	}
	formatOf := func(name string) (string, string, error) {
		var canonical, format string
		err := q.QueryRow(ctx, `
			SELECT name, COALESCE(format,'number') FROM model.metric_def
			WHERE model_id=$1::uuid AND lower(name)=lower($2)
			  AND (revision_id IS NOT DISTINCT FROM NULLIF($3,'')::uuid OR revision_id IS NULL)
			ORDER BY (name = $2) DESC LIMIT 1`, modelID, name, revisionID).Scan(&canonical, &format)
		if err != nil {
			return "", "", invalidCode(CodeInvalidHighlightRule, "highlight rule names %q, which is no metric of this revision", name)
		}
		return canonical, format, nil
	}
	for i := range rules {
		r := &rules[i]
		at := fmt.Sprintf("highlight rule %d", i+1)
		if !contains(HighlightOps, r.Op) {
			return nil, invalidCode(CodeInvalidHighlightRule, "%s: op %q is not one of %s", at, r.Op, strings.Join(HighlightOps, ", "))
		}
		if r.Tone == "" {
			r.Tone = "warning"
		}
		if !contains(HighlightTones, r.Tone) {
			return nil, invalidCode(CodeInvalidHighlightRule, "%s: tone %q is not one of %s", at, r.Tone, strings.Join(HighlightTones, ", "))
		}
		subjectFormat := ""
		if r.Metric != "" && !strings.EqualFold(r.Metric, metricName) {
			name, format, err := formatOf(r.Metric)
			if err != nil {
				return nil, err
			}
			r.Metric, subjectFormat = name, format
		} else {
			r.Metric = ""
		}
		if r.Than != "" {
			name, format, err := formatOf(r.Than)
			if err != nil {
				return nil, err
			}
			if format == FormatText || format == FormatPicklist {
				return nil, invalidCode(CodeInvalidHighlightRule, "%s: than names %s, a %s metric — compare with a number", at, name, format)
			}
			r.Than = name
		}
		_, isNumber := r.Value.(float64)
		_, isText := r.Value.(string)
		switch r.Op {
		case "blank", "not_blank":
			if r.Value != nil || r.Than != "" || r.Value2 != nil {
				return nil, invalidCode(CodeInvalidHighlightRule, "%s: %s compares with nothing (no value or than)", at, r.Op)
			}
		case "between", "not_between":
			if !isNumber || r.Value2 == nil || r.Than != "" {
				return nil, invalidCode(CodeInvalidHighlightRule, `%s: %s needs a number "value" and "value2"`, at, r.Op)
			}
		case "=", "<>":
			if r.Than == "" && !isNumber && !isText {
				return nil, invalidCode(CodeInvalidHighlightRule, `%s: %s needs a "value" (a number or a text) or "than" (a metric)`, at, r.Op)
			}
		default:
			if r.Than == "" && !isNumber {
				return nil, invalidCode(CodeInvalidHighlightRule, `%s: %s needs a number "value" or "than" (a metric)`, at, r.Op)
			}
		}
		if r.Than != "" && r.Value != nil {
			return nil, invalidCode(CodeInvalidHighlightRule, `%s: give "value" or "than", not both`, at)
		}
		if r.Abs && (subjectFormat == FormatText || subjectFormat == FormatPicklist || isText) {
			return nil, invalidCode(CodeInvalidHighlightRule, "%s: abs is for numbers", at)
		}
		if err := checkRuleMembers(ctx, q, modelID, revisionID, r, at); err != nil {
			return nil, err
		}
	}
	out, err := json.Marshal(rules)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// checkRuleMembers resolves a rule's dimension by name (its canonical
// spelling kept) and checks each of its members is a code of it — of some
// dimension of the revision when it names none, calculated members
// included.
func checkRuleMembers(ctx context.Context, q Querier, modelID, revisionID string, r *HighlightRule, at string) error {
	if r.Dimension != "" && len(r.Members) == 0 {
		return invalidCode(CodeInvalidHighlightRule, `%s: "dimension" goes with "members", the member codes the rule is for`, at)
	}
	if len(r.Members) == 0 {
		return nil
	}
	if r.Dimension != "" {
		var name string
		if err := q.QueryRow(ctx, `
			SELECT name FROM model.dimension_def
			WHERE model_id=$1::uuid AND lower(name)=lower($2) AND (revision_id IS NOT DISTINCT FROM NULLIF($3,'')::uuid OR revision_id IS NULL)
			ORDER BY (name = $2) DESC LIMIT 1`, modelID, r.Dimension, revisionID).Scan(&name); err != nil {
			return invalidCode(CodeInvalidHighlightRule, "%s: %q is no dimension of this revision", at, r.Dimension)
		}
		r.Dimension = name
	}
	for _, code := range r.Members {
		var found bool
		if err := q.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id
			WHERE d.model_id=$1::uuid AND (d.revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid OR d.revision_id IS NULL)
			  AND m.code = $3 AND ($4 = '' OR d.name = $4))`, modelID, revisionID, code, r.Dimension).Scan(&found); err != nil {
			return err
		}
		if !found {
			where := "any dimension"
			if r.Dimension != "" {
				where = r.Dimension
			}
			return invalidCode(CodeInvalidHighlightRule, "%s: %q is no member of %s", at, code, where)
		}
	}
	return nil
}
