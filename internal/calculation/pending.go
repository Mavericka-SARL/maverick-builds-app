package calculation

import (
	"context"

	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// A write that answers before its dependents are recalculated leaves the
// revision waiting for a pass: runtime.revision_recalc_state counts the
// recalculations asked for and those finished, and a grid read reports the
// gap (recalc_pending) so the console marks calculated cells as about to
// change. One definition for every writer that recalculates after
// answering: the gateway's cell writes and imports, and record postings in
// the gateway and the connector worker (internal/recordpost).

// RequestRecalc records that revisionID awaits one more recalculation and
// returns its number, for RecalcFinished.
func RequestRecalc(ctx context.Context, db dbx.DB, revisionID string) (int64, error) {
	var seq int64
	err := db.QueryRow(ctx, `
		INSERT INTO runtime.revision_recalc_state (revision_id, requested, updated_at) VALUES ($1::uuid, 1, now())
		ON CONFLICT (revision_id) DO UPDATE SET requested = runtime.revision_recalc_state.requested + 1, updated_at = now()
		RETURNING requested`, revisionID).Scan(&seq)
	return seq, err
}

// RecalcFinished records recalculation seq of revisionID finished.
func RecalcFinished(ctx context.Context, db dbx.DB, revisionID string, seq int64) error {
	_, err := db.Exec(ctx, `
		UPDATE runtime.revision_recalc_state SET done = GREATEST(done, $2), updated_at = now()
		WHERE revision_id = $1::uuid`, revisionID, seq)
	return err
}
