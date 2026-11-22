package plan

import "context"

// PruneDataChanges folds the rows each committed transaction left in
// runtime.data_change into the database's data generation (migration 198),
// so counting them stays cheap; the generation is the same before and
// after. A database whose migrations predate 198 has nothing to prune.
func PruneDataChanges(ctx context.Context, db DB) error {
	var ready bool
	if err := db.QueryRow(ctx, `SELECT to_regprocedure('runtime.prune_data_changes()') IS NOT NULL`).Scan(&ready); err != nil || !ready {
		return err
	}
	_, err := db.Exec(ctx, `SELECT runtime.prune_data_changes()`)
	return err
}
