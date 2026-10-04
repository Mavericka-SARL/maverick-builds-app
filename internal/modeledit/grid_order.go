package modeledit

import (
	"context"
	"fmt"
	"strings"
)

// GridOrderError is a reorder request that does not name exactly the grid's
// metrics; the route answers it with 400.
type GridOrderError struct{ msg string }

func (e *GridOrderError) Error() string { return e.msg }

// ReorderGridMetrics sets the order a grid shows its metrics in — the row or
// column order of every grid, grid widget and export reading it. metricIDs
// must be exactly the grid's metrics, each once. A P&L must read Net Revenue,
// COGS, Gross Profit … in statement order; before this, metrics added in the
// console all had sort_order 0 and showed alphabetically. The console's
// route and the AI Developer's reorder_grid_metrics both run this.
func ReorderGridMetrics(ctx context.Context, db DB, gridID string, metricIDs []string) error {
	rows, err := db.Query(ctx, `SELECT gm.metric_id::text, m.name FROM model.grid_metric gm
		JOIN model.metric_def m ON m.id = gm.metric_id WHERE gm.grid_id=$1::uuid`, gridID)
	if err != nil {
		return err
	}
	onGrid := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		onGrid[id] = name
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, raw := range metricIDs {
		id := strings.ToLower(strings.TrimSpace(raw))
		if _, ok := onGrid[id]; !ok {
			name := raw
			_ = db.QueryRow(ctx, `SELECT name FROM model.metric_def WHERE id=$1::uuid`, id).Scan(&name)
			return &GridOrderError{fmt.Sprintf("metric %s is not on this grid — add it to the grid first (an earlier step), or leave it out of the order", name)}
		}
		if seen[id] {
			return &GridOrderError{fmt.Sprintf("metric %s is listed twice", onGrid[id])}
		}
		seen[id] = true
	}
	var missing []string
	for id, name := range onGrid {
		if !seen[id] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return &GridOrderError{fmt.Sprintf("list every metric of the grid once; missing: %s", strings.Join(missing, ", "))}
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	for i, raw := range metricIDs {
		if _, err := tx.Exec(ctx, `UPDATE model.grid_metric SET sort_order=$3 WHERE grid_id=$1::uuid AND metric_id=$2::uuid`,
			gridID, strings.ToLower(strings.TrimSpace(raw)), i+1); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
