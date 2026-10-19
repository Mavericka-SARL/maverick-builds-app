package modeledit

import (
	"context"
	"fmt"
	"strings"
)

// DashboardOrderSQL orders dashboards as the Dashboards page shows them:
// the developer's order first, then any dashboard never ordered, by name.
// alias is the dashboard_def table's alias in the query ("" for none).
func DashboardOrderSQL(alias string) string {
	if alias != "" {
		alias += "."
	}
	return alias + "sort_order NULLS LAST, " + alias + "name"
}

// ReorderDashboards sets the order of a revision's dashboards —
// PUT /api/developer/dashboards/order and the AI Developer's
// reorder_dashboards. ids must be exactly the dashboards of the revision,
// each once; they are numbered 1..N in that order. It returns their names in
// the new order, for the caller's audit record. A wrong list is a
// *ReorderError.
func ReorderDashboards(ctx context.Context, db DB, modelID, revisionID string, ids []string) ([]string, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `SELECT id::text, name FROM model.dashboard_def
		WHERE model_id=$1::uuid AND revision_id=$2::uuid FOR UPDATE`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return nil, err
		}
		names[id] = name
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	ordered := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.ToLower(strings.TrimSpace(raw))
		name, ok := names[id]
		if !ok {
			return nil, reorderErr("dashboard %s is not a dashboard of this revision", raw)
		}
		if seen[id] {
			return nil, reorderErr("dashboard %q is listed twice", name)
		}
		seen[id] = true
		ordered = append(ordered, name)
	}
	if len(seen) != len(names) {
		var missing []string
		for id, name := range names {
			if !seen[id] {
				missing = append(missing, fmt.Sprintf("%q", name))
			}
		}
		return nil, reorderErr("every dashboard of the revision must be listed once; missing %s", strings.Join(missing, ", "))
	}
	for i, raw := range ids {
		if _, err := tx.Exec(ctx, `UPDATE model.dashboard_def SET sort_order=$2 WHERE id=$1::uuid`,
			strings.ToLower(strings.TrimSpace(raw)), i+1); err != nil {
			return nil, err
		}
	}
	return ordered, tx.Commit(ctx)
}
