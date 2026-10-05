package modeledit

import (
	"context"
	"errors"
	"fmt"
)

// ErrMetricHoldsValues refuses turning an input that holds values into a
// calculated metric without saying the values may go.
var ErrMetricHoldsValues = errors.New("the metric holds values")

// SwitchMetricKind changes a metric between input (typed) and calculated
// (formula), inside the caller's transaction, before the caller writes the
// metric's new settings and its is_input. A developer used to delete the
// metric, recreate it and repoint the formulas naming it — the HR model's LY
// total, typed in the workbook, had been built as a sum.
//
//   - To input: its dependency edges and stored results go (dependents then
//     read the values typed into it).
//   - To calculated: refused while it holds values, unless dropValues — they
//     are then deleted, and kept in the cell history ("became calculated").
func SwitchMetricKind(ctx context.Context, db DB, metricID string, toInput, dropValues bool) error {
	if toInput {
		for _, sql := range []string{
			`DELETE FROM model.calc_dependency WHERE metric_id = $1::uuid`,
			`DELETE FROM runtime.calc_result WHERE metric_id = $1::uuid`,
			`DELETE FROM runtime.metric_partition_state WHERE metric_id = $1::uuid`,
		} {
			if _, err := db.Exec(ctx, sql, metricID); err != nil {
				return fmt.Errorf("make input: %w", err)
			}
		}
		return nil
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input WHERE metric_id = $1::uuid`, metricID).Scan(&n); err != nil {
		return fmt.Errorf("count values: %w", err)
	}
	if n == 0 {
		return nil
	}
	if !dropValues {
		return fmt.Errorf("%w: %d cell value(s) — send drop_values true to drop them (the cell history keeps them) and make it calculated", ErrMetricHoldsValues, n)
	}
	if _, err := db.Exec(ctx, `SET LOCAL mvx.delete_reason = 'metric_became_calculated'`); err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `DELETE FROM runtime.fact_input WHERE metric_id = $1::uuid`, metricID); err != nil {
		return fmt.Errorf("drop values: %w", err)
	}
	return nil
}
