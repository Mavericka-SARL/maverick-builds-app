package gateway

import "context"

// loadPostedMetrics maps each of inputMetrics a table of records posts into
// to that table. Such a metric takes no typed value in any of its cells
// (writeguard.PostedReason): the console shows them read-only and says which
// records to change. A value typed over €85,746.50 of posted POS sales was
// added to it (2026-11-05).
func (h *handler) loadPostedMetrics(ctx context.Context, revisionID string, inputMetrics []string) (map[string]string, error) {
	if len(inputMetrics) == 0 {
		return nil, nil
	}
	rows, err := h.db.Query(ctx, `
		SELECT DISTINCT ON (mm.target_metric_id) mm.target_metric_id::text, COALESCE(NULLIF(f.label, ''), f.name)
		FROM model.form_metric_mapping mm
		JOIN model.form_def f ON f.id = mm.form_id
		WHERE mm.target_metric_id::text = ANY($2::text[]) AND (mm.revision_id IS NULL OR mm.revision_id = $1::uuid)
		ORDER BY mm.target_metric_id, f.name`, revisionID, inputMetrics)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out map[string]string
	for rows.Next() {
		var metricID, table string
		if err := rows.Scan(&metricID, &table); err != nil {
			return nil, err
		}
		if out == nil {
			out = map[string]string{}
		}
		out[metricID] = table
	}
	return out, rows.Err()
}
