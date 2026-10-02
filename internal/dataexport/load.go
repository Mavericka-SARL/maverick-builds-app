package dataexport

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Querier is the slice of a pgx pool or transaction LoadGrid needs.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// LoadGrid reads a grid's structure — what a spec is validated against —
// with its dimensions in /api/grid's order (by name) and members in display
// order. It returns the grid's model and revision too. Metric ids are those
// of the grid's own revision, resolved by name as the grid export does.
func LoadGrid(ctx context.Context, q Querier, gridID string) (g Grid, modelID, revisionID string, err error) {
	if err = q.QueryRow(ctx, `
		SELECT name, model_id::text, COALESCE(revision_id::text,'')
		FROM model.grid_def WHERE id=$1::uuid`, gridID).Scan(&g.Name, &modelID, &revisionID); err != nil {
		return Grid{}, "", "", fmt.Errorf("grid %s not found", gridID)
	}

	rows, err := q.Query(ctx, `
		SELECT d.id::text, d.name, m.code, m.label, COALESCE(pm.code,'')
		FROM model.grid_dimension gd
		JOIN model.dimension_def d ON d.id = gd.dimension_id
		LEFT JOIN model.dimension_member m ON m.dimension_id = d.id
		LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE gd.grid_id = $1::uuid
		ORDER BY d.name, d.id, m.time_index NULLS LAST, m.sort_order, m.code`, gridID)
	if err != nil {
		return Grid{}, "", "", err
	}
	byID := map[string]int{}
	for rows.Next() {
		var id, name string
		var code, label *string
		var parent string
		if err := rows.Scan(&id, &name, &code, &label, &parent); err != nil {
			rows.Close()
			return Grid{}, "", "", err
		}
		i, ok := byID[id]
		if !ok {
			g.Dimensions = append(g.Dimensions, Dimension{ID: id, Name: name})
			i = len(g.Dimensions) - 1
			byID[id] = i
		}
		if code != nil {
			m := Member{Code: *code, ParentCode: parent}
			if label != nil {
				m.Label = *label
			}
			g.Dimensions[i].Members = append(g.Dimensions[i].Members, m)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Grid{}, "", "", err
	}

	mrows, err := q.Query(ctx, `
		SELECT rev.id::text, rev.name
		FROM model.grid_metric gm
		JOIN model.metric_def orig ON orig.id = gm.metric_id
		JOIN model.metric_def rev
		     ON rev.model_id = orig.model_id AND rev.name = orig.name
		    AND rev.revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid
		WHERE gm.grid_id = $1::uuid
		ORDER BY gm.sort_order, rev.name`, gridID, revisionID)
	if err != nil {
		return Grid{}, "", "", err
	}
	defer mrows.Close()
	for mrows.Next() {
		var m Metric
		if err := mrows.Scan(&m.ID, &m.Name); err != nil {
			return Grid{}, "", "", err
		}
		m.Label = MetricLabel(m.Name)
		g.Metrics = append(g.Metrics, m)
	}
	return g, modelID, revisionID, mrows.Err()
}
