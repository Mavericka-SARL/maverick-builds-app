package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// resolveLegacyDimCode finds the dimension a legacy single `dim_code` cell
// write means: the one dimension, in this revision (or a pre-revision legacy
// dimension, as the grid shows them), that has a member with that code. No
// dimension name is special — standing rule 1; this used to prefer a
// dimension literally named "department".
//
// The search covers the metric's own dimensions — those of the grids it
// belongs to, the dimensions grid() keys its cells by — so a code shared
// with a dimension the metric is not dimensioned by neither makes the write
// ambiguous nor lands it where no cell shows it. A metric with no grid
// dimensions has no own dimensions, and the search covers the whole model.
//
// On failure it returns the HTTP status to answer with: 400 when no dimension
// has the code (there is nowhere to write it) or when more than one does (the
// caller must name the dimension with dim_codes), 500 on a database error.
func (h *handler) resolveLegacyDimCode(ctx context.Context, modelID, revisionID, metricID, code string) (string, int, error) {
	var hasOwnDims bool
	if err := h.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM model.grid_metric gm
			JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
			WHERE gm.metric_id = $1::uuid)
	`, metricID).Scan(&hasOwnDims); err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("resolve dimension: %w", err)
	}
	rows, err := h.db.Query(ctx, `
		SELECT d.id::text, d.name
		FROM model.dimension_def d
		WHERE d.model_id = $1::uuid
		  AND (d.revision_id IS NULL OR d.revision_id = $2::uuid)
		  AND EXISTS (SELECT 1 FROM model.dimension_member m WHERE m.dimension_id = d.id AND m.code = $3)
		  AND (NOT $5::bool OR d.id IN (
			SELECT gd.dimension_id FROM model.grid_metric gm
			JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
			WHERE gm.metric_id = $4::uuid))
		ORDER BY d.name, d.id
	`, modelID, revisionID, code, metricID, hasOwnDims)
	if err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("resolve dimension: %w", err)
	}
	defer rows.Close()
	var ids, names []string
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return "", http.StatusInternalServerError, fmt.Errorf("resolve dimension: %w", err)
		}
		ids = append(ids, id)
		names = append(names, fmt.Sprintf("%q", name))
	}
	if err := rows.Err(); err != nil {
		return "", http.StatusInternalServerError, fmt.Errorf("resolve dimension: %w", err)
	}
	scope := "this model"
	if hasOwnDims {
		scope = "this metric's grids"
	}
	switch len(ids) {
	case 1:
		return ids[0], 0, nil
	case 0:
		return "", http.StatusBadRequest, fmt.Errorf(
			"dim_code %q is not a member of any dimension of %s; send dim_codes {dimension_id: code}", code, scope)
	default:
		return "", http.StatusBadRequest, fmt.Errorf(
			"dim_code %q is a member of more than one dimension of %s (%s); send dim_codes {dimension_id: code} to name the dimension",
			code, scope, strings.Join(names, ", "))
	}
}
