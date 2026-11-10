package metricformula

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mavericks-engine/mavericks/internal/rollup"
	"github.com/mavericks-engine/mavericks/internal/timedim"
)

// Total rules (migration 174, rollup/totalrules.go): how a dimension's
// members total — sum (they add up, each metric by its own rule), first,
// last, or none — and a metric's own rule over one of its dimensions.

// CodeInvalidTotalRule refuses a total rule the engine could not apply.
const CodeInvalidTotalRule = "INVALID_TOTAL_RULE"

// CheckDimensionTotalRule refuses rule for a dimension of dimensionType: one
// of rollup.TotalRules ("" keeps it), and sum for a time dimension, whose
// totals are each metric's time summary.
func CheckDimensionTotalRule(rule, dimensionType string) error {
	if !rollup.ValidTotalRule(rule) {
		return invalidCode(CodeInvalidTotalRule, "agg_rule is how the dimension's members total: one of %s, not %q",
			strings.Join(rollup.TotalRules, ", "), rule)
	}
	if dimensionType == timedim.TypeTime && rule != "" && rule != rollup.TotalSum {
		return invalidCode(CodeInvalidTotalRule, "a time dimension totals by each metric's time summary: its agg_rule is %q", rollup.TotalSum)
	}
	return nil
}

// CheckMetricDimensionRules validates a metric's own total rules — {"<the
// dimension's id or name>": rule} — against its revision, and returns them
// by dimension id. raw empty or null is none. A time dimension takes no
// rule: its totals are the metric's time summary.
func CheckMetricDimensionRules(ctx context.Context, q Querier, modelID, revisionID string, raw json.RawMessage) (map[string]string, error) {
	out := map[string]string{}
	if t := strings.TrimSpace(string(raw)); t == "" || t == "null" {
		return out, nil
	}
	var in map[string]string
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, invalidCode(CodeInvalidTotalRule, `dimension_rules is {"<dimension>": "sum" | "first" | "last" | "none"}: %v`, err)
	}
	for ref, rule := range in {
		rule = strings.ToLower(strings.TrimSpace(rule))
		if rule == "" || !rollup.ValidTotalRule(rule) {
			return nil, invalidCode(CodeInvalidTotalRule, "dimension_rules %q: one of %s, not %q", ref, strings.Join(rollup.TotalRules, ", "), rule)
		}
		var id, kind string
		err := q.QueryRow(ctx, `
			SELECT id::text, dimension_type FROM model.dimension_def
			WHERE model_id = $1::uuid AND (revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid OR revision_id IS NULL)
			  AND (id::text = $3 OR lower(name) = lower($3))
			ORDER BY (id::text = $3) DESC, (name = $3) DESC LIMIT 1`, modelID, revisionID, strings.TrimSpace(ref)).Scan(&id, &kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, invalidCode(CodeInvalidTotalRule, "dimension_rules names %q, which is no dimension of this revision", ref)
		} else if err != nil {
			return nil, fmt.Errorf("dimension_rules: %w", err)
		}
		if kind == timedim.TypeTime {
			return nil, invalidCode(CodeInvalidTotalRule, "dimension_rules %q: a time dimension totals by the metric's time_summary", ref)
		}
		out[id] = rule
	}
	return out, nil
}

// SetMetricDimensionRules replaces a metric's own total rules with rules
// (dimension id → rule; empty removes them).
func SetMetricDimensionRules(ctx context.Context, q interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}, metricID string, rules map[string]string) error {
	if _, err := q.Exec(ctx, `DELETE FROM model.metric_dimension_rule WHERE metric_id = $1::uuid`, metricID); err != nil {
		return fmt.Errorf("set dimension rules: %w", err)
	}
	if len(rules) == 0 {
		return nil
	}
	ids := make([]string, 0, len(rules))
	vals := make([]string, 0, len(rules))
	for id, r := range rules {
		ids, vals = append(ids, id), append(vals, r)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO model.metric_dimension_rule (metric_id, dimension_id, rule)
		SELECT $1::uuid, d::uuid, r FROM unnest($2::text[], $3::text[]) AS x(d, r)`, metricID, ids, vals); err != nil {
		return fmt.Errorf("set dimension rules: %w", err)
	}
	return nil
}
