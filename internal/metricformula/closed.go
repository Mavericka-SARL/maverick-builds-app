package metricformula

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ClosedRule is one condition under which an input metric's cell takes no
// input (migration 147): the calculated metric Metric, read at the same cell,
// compared with Value. A cell where any of the metric's rules holds is
// closed — the CPG rolling forecast closes its planning % where actual_month
// (1 in the months up to Actual Through Month) = 1, and its actuals where it
// is 0. A blank value counts as 0.
type ClosedRule struct {
	Metric string  `json:"metric"`
	Op     string  `json:"op"`
	Value  float64 `json:"value"`
}

// ClosedOps are the comparisons a closed rule may make.
var ClosedOps = []string{"=", "<>", ">", ">=", "<", "<="}

// CodeInvalidClosedRule refuses a closed rule the engine could not apply.
const CodeInvalidClosedRule = "INVALID_CLOSED_RULE"

const maxClosedRules = 5

// Holds reports whether v satisfies the rule.
func (r ClosedRule) Holds(v float64) bool {
	switch r.Op {
	case "=":
		return v == r.Value
	case "<>":
		return v != r.Value
	case ">":
		return v > r.Value
	case ">=":
		return v >= r.Value
	case "<":
		return v < r.Value
	case "<=":
		return v <= r.Value
	}
	return false
}

// CheckClosedRules validates an input metric's closed rules against its
// revision and returns them normalised (metric names as the revision spells
// them), as JSON to store. raw empty or null is no rules. metricID is the
// metric being edited ("" while it is created): each rule's metric must be a
// calculated metric, and on the same grid as it once both are on one, so the
// grid has the values its cells are drawn from.
func CheckClosedRules(ctx context.Context, q Querier, modelID, revisionID, metricID string, raw json.RawMessage) (json.RawMessage, error) {
	if t := strings.TrimSpace(string(raw)); t == "" || t == "null" {
		return json.RawMessage("[]"), nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var rules []ClosedRule
	if err := dec.Decode(&rules); err != nil {
		return nil, invalidCode(CodeInvalidClosedRule, `closed_when is a list of {"metric", "op", "value"}: %v`, err)
	}
	if len(rules) > maxClosedRules {
		return nil, invalidCode(CodeInvalidClosedRule, "a metric has at most %d closed rules (this has %d)", maxClosedRules, len(rules))
	}
	for i := range rules {
		r := &rules[i]
		at := fmt.Sprintf("closed rule %d", i+1)
		if !contains(ClosedOps, r.Op) {
			return nil, invalidCode(CodeInvalidClosedRule, "%s: op %q is not one of %s", at, r.Op, strings.Join(ClosedOps, ", "))
		}
		var id, name string
		var isInput bool
		if err := q.QueryRow(ctx, `
			SELECT id::text, name, is_input FROM model.metric_def
			WHERE model_id=$1::uuid AND lower(name)=lower($2)
			  AND (revision_id IS NOT DISTINCT FROM NULLIF($3,'')::uuid OR revision_id IS NULL)
			ORDER BY (name = $2) DESC LIMIT 1`, modelID, strings.TrimSpace(r.Metric), revisionID).Scan(&id, &name, &isInput); err != nil {
			return nil, invalidCode(CodeInvalidClosedRule, "%s names %q, which is no metric of this revision", at, r.Metric)
		}
		if isInput {
			return nil, invalidCode(CodeInvalidClosedRule, "%s: %s is an input; name a calculated metric (a formula such as IF(MONTH(START()) <= actual_through_month, 1, 0))", at, name)
		}
		if metricID != "" {
			var apart bool
			if err := q.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM model.grid_metric a JOIN model.grid_metric b ON b.metric_id = $2::uuid
				               WHERE a.metric_id = $1::uuid AND a.grid_id <> b.grid_id)`, metricID, id).Scan(&apart); err == nil && apart {
				return nil, invalidCode(CodeInvalidClosedRule, "%s: %s is on another grid; a closed rule reads a metric of the same grid", at, name)
			}
		}
		r.Metric = name
	}
	out, err := json.Marshal(rules)
	if err != nil {
		return nil, err
	}
	return out, nil
}
