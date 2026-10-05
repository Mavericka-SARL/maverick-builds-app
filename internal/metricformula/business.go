package metricformula

import "context"

// CheckBusinessMaintainable refuses marking a dimension business-maintained
// (migration 111: business users add, rename and remove its members) when
// its members are not a person's to type: a time dimension's periods, a
// property grouping's derived members.
func CheckBusinessMaintainable(ctx context.Context, q Querier, dimID string) error {
	var dimType string
	var grouping bool
	if err := q.QueryRow(ctx, `SELECT dimension_type, source_dimension_id IS NOT NULL FROM model.dimension_def WHERE id=$1::uuid`, dimID).Scan(&dimType, &grouping); err != nil {
		return invalid("dimension not found")
	}
	if dimType == "time" {
		return invalid("a time dimension's periods are the developer's: it cannot be business-maintained")
	}
	if grouping {
		return invalid("a property grouping's members are derived from its source dimension: it cannot be business-maintained")
	}
	return nil
}
