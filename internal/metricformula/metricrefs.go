package metricformula

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// CodeMetricInUse refuses deleting a metric while another metric reads it
// (CheckMetricNotInUse); the gateway answers it with 409.
const CodeMetricInUse = "METRIC_IN_USE"

// CheckMetricNotInUse refuses deleting metric metricID while another metric
// of its revision reads it: in its formula (a plain reference, a LOOKUP,
// SUMIFS or *VALUE source, inside any time function) or as the numerator or
// denominator of its Rate total. Allowing it left every cell of those
// metrics failing with #NAME? while they kept serving their last values, and
// a Rate total silently losing an operand. The error names the metrics so
// they can be changed first, as PROPERTY_IN_USE, DIMENSION_IN_USE and
// MEMBER_IN_USE do. A metric's own formula (a time-shifted self-reference)
// does not count.
//
// What else names a metric by text counts too, since 2026-11-09: another
// metric's highlight rule (its "metric" or "than") or closed rule, a
// workflow condition and a data export of the revision. Deleted, the rule
// silently stopped tinting, a closed rule turned every write to its input
// into an error, the condition parked its step for a person and the export
// silently dropped the column.
func CheckMetricNotInUse(ctx context.Context, q Querier, metricID string) error {
	return checkMetricNotInUse(ctx, q, metricID, nil)
}

// CheckMetricsNotInUse refuses deleting the metrics metricIDs together while
// a metric OUTSIDE them reads one: metrics that read each other — a cycle of
// copies, a helper and its only reader — go in one delete. Nine stray
// metrics in a LAG cycle could only be deleted by first patching each
// formula, since each refused while another of them read it (2026-11-11).
func CheckMetricsNotInUse(ctx context.Context, q Querier, metricIDs []string) error {
	together := map[string]bool{}
	if len(metricIDs) > 1 {
		rows, err := q.Query(ctx, `SELECT lower(name) FROM model.metric_def WHERE id::text = ANY($1)`, metricIDs)
		if err != nil {
			return err
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, n := range names {
			together[n] = true
		}
	}
	var refusals []string
	for _, id := range metricIDs {
		if err := checkMetricNotInUse(ctx, q, id, together); err != nil {
			var ve *ValidationError
			if !errors.As(err, &ve) {
				return err
			}
			refusals = append(refusals, strings.TrimSuffix(ve.Message, "; change those first, then delete it"))
		}
	}
	if len(refusals) > 0 {
		return invalidCode(CodeMetricInUse, "%s; change those first, or delete them in the same delete", strings.Join(refusals, "; "))
	}
	return nil
}

// checkMetricNotInUse is CheckMetricNotInUse with the (lower-cased) names
// of metrics deleted together, whose reading does not count.
func checkMetricNotInUse(ctx context.Context, q Querier, metricID string, together map[string]bool) error {
	var modelID, revisionID, name string
	if err := q.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text, ''), name
		FROM model.metric_def WHERE id=$1::uuid
	`, metricID).Scan(&modelID, &revisionID, &name); err != nil {
		return err
	}
	// For a revision-less metric, a revision that owns a metric of the same
	// name (in any case) resolves the name to its own, so its formulas do
	// not read this one ("shadowed"); a Rate operand is by ID either way.
	rows, err := q.Query(ctx, `
		SELECT m.name, COALESCE(m.formula, ''),
		       (m.agg_numerator_metric_id = $2::uuid OR m.agg_denominator_metric_id = $2::uuid) IS TRUE,
		       m.highlight_rules, m.closed_when,
		       ($3 = '' AND EXISTS (
		           SELECT 1 FROM model.metric_def own
		           WHERE own.model_id = m.model_id AND own.revision_id = m.revision_id
		             AND lower(own.name) = lower($4)))
		FROM model.metric_def m
		WHERE m.model_id = $1::uuid AND m.id <> $2::uuid
		  AND ($3 = '' OR m.revision_id = NULLIF($3, '')::uuid)
		ORDER BY m.name
	`, modelID, metricID, revisionID, name)
	if err != nil {
		return err
	}
	type row struct {
		name, text        string
		operand, shadowed bool
		highlight, closed []byte
	}
	metrics, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.name, &x.text, &x.operand, &x.highlight, &x.closed, &x.shadowed)
		return x, err
	})
	if err != nil {
		return err
	}
	var readers []string
	for _, m := range metrics {
		if together[strings.ToLower(m.name)] {
			continue // deleted in the same delete
		}
		switch {
		case !m.shadowed && m.text != "" && FormulaReadsMetric(m.text, name):
			readers = append(readers, m.name)
		case m.operand:
			readers = append(readers, m.name+" (its Rate total)")
		}
		if m.shadowed {
			continue
		}
		// Only the names are read: a rule whose other fields no longer
		// decode still protects what it names.
		var highlight []struct{ Metric, Than string }
		if json.Unmarshal(m.highlight, &highlight) == nil {
			for _, r := range highlight {
				if strings.EqualFold(r.Metric, name) || strings.EqualFold(r.Than, name) {
					readers = append(readers, m.name+" (its highlight rules)")
					break
				}
			}
		}
		var closed []struct{ Metric string }
		if json.Unmarshal(m.closed, &closed) == nil {
			for _, r := range closed {
				if strings.EqualFold(r.Metric, name) {
					readers = append(readers, m.name+" (its closed rule)")
					break
				}
			}
		}
	}
	if revisionID != "" {
		more, err := revisionReadersOfMetric(ctx, q, revisionID, name)
		if err != nil {
			return err
		}
		readers = append(readers, more...)
	}
	if len(readers) > 0 {
		return invalidCode(CodeMetricInUse,
			"%s is read by %s; change those first, then delete it",
			name, strings.Join(readers, ", "))
	}
	return nil
}

// revisionReadersOfMetric names the workflow conditions and data exports of
// a revision that name metric name. A condition counts even where a context
// variable shares the name: a metric resolves first when it is evaluated.
func revisionReadersOfMetric(ctx context.Context, q Querier, revisionID, name string) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT wd.name, COALESCE(NULLIF(s->>'name',''), s->>'id', ''), s->'condition'->>'formula'
		FROM workflow.workflow_def wd
		CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(wd.steps) = 'array' THEN wd.steps ELSE '[]'::jsonb END) s
		WHERE wd.revision_id = $1::uuid AND jsonb_typeof(s->'condition') = 'object'
		  AND COALESCE(s->'condition'->>'formula', '') <> ''
		ORDER BY wd.name`, revisionID)
	if err != nil {
		return nil, err
	}
	type cond struct{ workflow, step, text string }
	conds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cond, error) {
		var x cond
		err := r.Scan(&x.workflow, &x.step, &x.text)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	var readers []string
	for _, c := range conds {
		if FormulaReadsMetric(strings.TrimPrefix(strings.TrimSpace(c.text), "="), name) {
			readers = append(readers, "workflow "+c.workflow+" (condition step "+c.step+")")
		}
	}
	rows, err = q.Query(ctx, `
		SELECT name, COALESCE(config->'metrics', '[]'::jsonb) FROM model.integration_def
		WHERE revision_id = $1::uuid AND type = 'file_export' ORDER BY name`, revisionID)
	if err != nil {
		return nil, err
	}
	type export struct {
		name    string
		metrics []byte
	}
	exports, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (export, error) {
		var x export
		err := r.Scan(&x.name, &x.metrics)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	for _, e := range exports {
		var list []string
		if json.Unmarshal(e.metrics, &list) != nil {
			continue
		}
		for _, m := range list {
			if strings.EqualFold(strings.TrimSpace(m), name) {
				readers = append(readers, "data export "+e.name)
				break
			}
		}
	}
	return readers, nil
}

// FormulaReadsMetric reports whether formula text names metric name, in any
// case, wherever a metric can be read. A formula that no longer parses is
// matched as text (a whole-word, case-insensitive match), so an unreadable
// formula still protects what it names.
func FormulaReadsMetric(text, name string) bool {
	an, err := formula.Analyze(text)
	if err != nil {
		word := regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.])` + regexp.QuoteMeta(name) + `($|[^A-Za-z0-9_])`)
		return word.MatchString(text)
	}
	for _, r := range an.References {
		if strings.EqualFold(r.Name, name) {
			return true
		}
	}
	return false
}
