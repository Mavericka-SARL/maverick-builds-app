package writeguard

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostedReason reports why the first of cells takes no typed value — its
// metric is one a table of records posts into — or "" when none is. Such a
// metric takes its values from its records: a value typed into one of its
// cells was added to the posting (a sum posting: €85,746.50 of POS sales and
// a typed 90,000 read €175,746.50), hidden by it (a "replace" or "last"
// posting), or stood as an actual no record explains (2026-11-05). The
// records are where it changes.
//
// Every write entry point of typed values calls it next to ClosedReason —
// the grid's cells and batches (and so the AI Developer's
// write_input_values and the chat connector's write_cells) and file
// imports — but not the postings themselves. Clearing a cell stays allowed:
// it removes a typed value, never a posting.
func PostedReason(ctx context.Context, pool *pgxpool.Pool, revisionID string, cells []Cell) (string, error) {
	if len(cells) == 0 {
		return "", nil
	}
	metricIDs := make([]string, 0, len(cells))
	seen := map[string]bool{}
	for _, c := range cells {
		if !seen[c.MetricID] {
			seen[c.MetricID] = true
			metricIDs = append(metricIDs, c.MetricID)
		}
	}
	var metric, table string
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(m.label, ''), m.name), COALESCE(NULLIF(f.label, ''), f.name)
		FROM model.form_metric_mapping mm
		JOIN model.form_def f ON f.id = mm.form_id
		JOIN model.metric_def m ON m.id = mm.target_metric_id
		WHERE mm.target_metric_id = ANY($2::uuid[]) AND (mm.revision_id IS NULL OR mm.revision_id = $1::uuid)
		ORDER BY 1, 2 LIMIT 1`, revisionID, metricIDs).Scan(&metric, &table)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("posted metrics: %w", err)
	}
	return fmt.Sprintf("%s comes from the records of %q: change those records, not its cells", metric, table), nil
}
