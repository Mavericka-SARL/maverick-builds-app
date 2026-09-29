package metricformula

import (
	"context"
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
func CheckMetricNotInUse(ctx context.Context, q Querier, metricID string) error {
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
	}
	metrics, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.name, &x.text, &x.operand, &x.shadowed)
		return x, err
	})
	if err != nil {
		return err
	}
	var readers []string
	for _, m := range metrics {
		switch {
		case !m.shadowed && m.text != "" && FormulaReadsMetric(m.text, name):
			readers = append(readers, m.name)
		case m.operand:
			readers = append(readers, m.name+" (its Rate total)")
		}
	}
	if len(readers) > 0 {
		return invalidCode(CodeMetricInUse,
			"%s is read by %s; change those metrics first, then delete it",
			name, strings.Join(readers, ", "))
	}
	return nil
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
