package modeledit

import (
	"context"
	"errors"
	"fmt"
)

// ErrRollupSource refuses a grid's rollup source (SetGridRollupSource).
var ErrRollupSource = errors.New("invalid rollup source")

// SetGridRollupSource makes gridID mirror sourceID's metrics through the
// cross-dimension rollup (model.grid_def.rollup_source_grid_id), or — with
// sourceID "" — mirror nothing. The source is another grid of the same model
// and revision, and following sources from it must not come back to gridID.
// Until 2026-10-26 only a model import set it; the developer and the AI
// Developer now do, through the same rule (standing rule 2).
func SetGridRollupSource(ctx context.Context, db DB, gridID, sourceID string) error {
	if sourceID == "" {
		_, err := db.Exec(ctx, `UPDATE model.grid_def SET rollup_source_grid_id = NULL WHERE id = $1::uuid`, gridID)
		return err
	}
	if sourceID == gridID {
		return fmt.Errorf("%w: a grid cannot mirror itself", ErrRollupSource)
	}
	var sameRevision bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM model.grid_def g JOIN model.grid_def s ON s.id::text = $2
		               WHERE g.id = $1::uuid AND s.model_id = g.model_id
		                 AND s.revision_id IS NOT DISTINCT FROM g.revision_id)`, gridID, sourceID).Scan(&sameRevision); err != nil {
		return err
	}
	if !sameRevision {
		return fmt.Errorf("%w: the grid it mirrors must be another grid of the same model and revision", ErrRollupSource)
	}
	var loops bool
	if err := db.QueryRow(ctx, `
		WITH RECURSIVE chain AS (
			SELECT id, rollup_source_grid_id, 0 AS depth FROM model.grid_def WHERE id = $2::uuid
			UNION ALL
			SELECT g.id, g.rollup_source_grid_id, c.depth + 1
			FROM model.grid_def g JOIN chain c ON g.id = c.rollup_source_grid_id
			WHERE c.depth < 64
		)
		SELECT EXISTS (SELECT 1 FROM chain WHERE id = $1::uuid)`, gridID, sourceID).Scan(&loops); err != nil {
		return err
	}
	if loops {
		return fmt.Errorf("%w: that grid already mirrors this one, directly or through others", ErrRollupSource)
	}
	_, err := db.Exec(ctx, `UPDATE model.grid_def SET rollup_source_grid_id = $2::uuid WHERE id = $1::uuid`, gridID, sourceID)
	return err
}
