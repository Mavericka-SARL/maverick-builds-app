package metricformula

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// ValidateAggRule checks that an aggregation rule is one the engine implements
// and that a "rate" carries the two operands it divides.
//
// It lived in the gateway's metric handler, so it only ever ran for a metric
// saved by a developer through the console. The AI Developer's create_metric
// and update_metric tools wrote agg_rule straight to the column with no check
// at all, which let it save an unknown rule, or a "rate" with nothing to
// divide — and a rate with no operands does not fail on save. It fails in the
// scheduler, on every recalculation, for as long as it exists.
//
// metricID is empty on create and set on update, so an update cannot make a
// metric its own numerator or denominator.
func ValidateAggRule(rule string, isInput bool, numeratorID, denominatorID, metricID string) error {
	switch rule {
	case "", "sum", "average", "count":
		return nil
	case string(rollup.AggFormula):
		if isInput {
			return invalid("aggregation rule %q applies to calculated metrics only: it means "+
				"evaluating the metric's formula at the total level, and an input metric has no formula", rule)
		}
		return nil
	case string(rollup.AggRate):
		// A ratio needs both halves or it has nothing to divide, and it is the
		// pair that makes the rule mean anything — this is the check whose
		// absence let "rate" sit in the console for months quietly summing.
		if numeratorID == "" || denominatorID == "" {
			return invalid("aggregation rule %q needs both a numerator and a denominator metric: "+
				"the total is numerator ÷ denominator", rule)
		}
		if numeratorID == denominatorID {
			return invalid("aggregation rule %q divides a metric by itself, which is always 1", rule)
		}
		// Self-reference would make the metric's own total an input to itself.
		if metricID != "" && (numeratorID == metricID || denominatorID == metricID) {
			return invalid("aggregation rule %q cannot use this metric as its own numerator or denominator", rule)
		}
		return nil
	default:
		return invalid("unknown aggregation rule %q (expected sum, average, count, formula or rate)", rule)
	}
}

// ValidateAggOperands additionally confirms both ratio operands are metrics of
// the same model and revision. Pointing at another revision's metric would
// resolve against values this one never sees.
func ValidateAggOperands(ctx context.Context, pool *pgxpool.Pool, rule, modelID, revisionID, numeratorID, denominatorID string) error {
	if rule != string(rollup.AggRate) {
		return nil
	}
	for label, id := range map[string]string{"numerator": numeratorID, "denominator": denominatorID} {
		var ok bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
			    SELECT 1 FROM model.metric_def
			    WHERE id=$1::uuid AND model_id=$2::uuid
			      AND (revision_id IS NOT DISTINCT FROM NULLIF($3,'')::uuid)
			)`, id, modelID, revisionID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return invalid("%s metric is not part of this model revision", label)
		}
	}
	return nil
}
