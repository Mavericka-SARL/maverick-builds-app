package gateway

import "context"

// storedMetricLabels returns the label stored on each of the given metrics
// that has one (migration 108). A metric without one keeps the label derived
// from its technical name (toLabel), as every reader did before labels could
// be set.
func (h *handler) storedMetricLabels(ctx context.Context, ids []string) map[string]string {
	labels := map[string]string{}
	if len(ids) == 0 {
		return labels
	}
	rows, err := h.db.Query(ctx, `SELECT id::text, label FROM model.metric_def
		WHERE id = ANY($1::uuid[]) AND label IS NOT NULL AND btrim(label) <> ''`, ids)
	if err != nil {
		return labels
	}
	defer rows.Close()
	for rows.Next() {
		var id, label string
		if rows.Scan(&id, &label) == nil {
			labels[id] = label
		}
	}
	return labels
}

// labelMetricRows puts stored labels on rows loaded with derived ones.
func (h *handler) labelMetricRows(ctx context.Context, rows []metricRow) {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	labels := h.storedMetricLabels(ctx, ids)
	for i := range rows {
		if l, ok := labels[rows[i].ID]; ok {
			rows[i].Label = l
		}
	}
}
