package importpkg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// CheckGridColumns refuses a mapped column that names a dimension the target
// grid does not have. ResolveRows reads any dimension of the model, so a
// cost sheet's "Region" column mapped to the Region dimension was resolved
// for a grid of cost centers and months: "Global" failed as an unknown
// region, and had every value resolved, each fact would have carried a
// dimension the grid has not got. Such a column is information for people;
// it is ignored, or it belongs on another grid.
func CheckGridColumns(ctx context.Context, q Querier, gridID string, header []string) error {
	var gridName string
	if err := q.QueryRow(ctx, `SELECT name FROM model.grid_def WHERE id=$1::uuid`, gridID).Scan(&gridName); err != nil {
		return nil // not a grid: the import's own checks speak to that
	}
	for _, col := range header {
		switch strings.ToLower(strings.TrimSpace(col)) {
		case "", "ignore", "metric", "metric_id", "value":
			continue
		}
		var dimName string
		var onGrid bool
		err := q.QueryRow(ctx, `
			SELECT d.name, EXISTS (SELECT 1 FROM model.grid_dimension gd WHERE gd.grid_id = g.id AND gd.dimension_id = d.id)
			FROM model.grid_def g
			JOIN model.dimension_def d ON d.model_id = g.model_id
			  AND (d.revision_id IS NOT DISTINCT FROM g.revision_id OR d.revision_id IS NULL)
			WHERE g.id = $1::uuid AND lower(d.name) = lower($2)
			ORDER BY (d.revision_id IS NOT NULL) DESC LIMIT 1`, gridID, strings.TrimSpace(col)).Scan(&dimName, &onGrid)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if !onGrid {
			return fmt.Errorf("column %q is the dimension %q, which grid %q does not have — map that column to \"ignore\" (or import into a grid that has %q)",
				col, dimName, gridName, dimName)
		}
	}
	// And every dimension of the grid needs a column: a value without one is
	// a fact the grid cannot place. A preview mapped a cost sheet's "Cost
	// Center" column to "ignore" and still reported 189 values to import.
	mapped := map[string]bool{}
	for _, col := range header {
		mapped[strings.ToLower(strings.TrimSpace(col))] = true
	}
	for i := 0; ; i++ {
		var dimName string
		err := q.QueryRow(ctx, `SELECT d.name FROM model.grid_dimension gd JOIN model.dimension_def d ON d.id = gd.dimension_id
			WHERE gd.grid_id = $1::uuid ORDER BY d.name OFFSET $2 LIMIT 1`, gridID, i).Scan(&dimName)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !mapped[strings.ToLower(dimName)] {
			return fmt.Errorf("grid %q has the dimension %q, but no column maps to it — map the column holding its codes or labels to %q (a value the file does not say goes in the reshape's \"constants\")",
				gridName, dimName, dimName)
		}
	}
}
