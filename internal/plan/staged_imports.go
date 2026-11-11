package plan

import (
	"context"
	"fmt"
)

// stagedImportExpiry is how long an import may wait between its check and
// its confirmation. The import wizard confirms within minutes; an import
// older than this was left, and nothing will confirm it.
const stagedImportExpiry = "1 day"

// ReclaimImportStaging removes the staged rows no import will read again. An
// import's rows are staged (import.import_staging) while it is checked, and
// removed when it is confirmed; an import refused for its errors, or left
// unconfirmed in the wizard, kept them for good — 119,740 rows (28 MB) of
// imports from 2026-10-14 to 10-22 in one Playground workspace, counted in
// its storage. An import still waiting after stagedImportExpiry is ended
// (aborted); the rows of every import no longer in progress go.
// Returns the rows removed.
func ReclaimImportStaging(ctx context.Context, db DB) (int64, error) {
	tag, err := db.Exec(ctx, `
		WITH ended AS (
		    UPDATE import.import_job SET status = 'aborted', completed_at = now()
		    WHERE status IN ('validating', 'staged') AND created_at < now() - interval '`+stagedImportExpiry+`'
		    RETURNING id)
		DELETE FROM import.import_staging s
		WHERE s.job_id IN (SELECT id FROM ended)
		   OR s.job_id IN (SELECT id FROM import.import_job WHERE status IN ('committed', 'failed', 'aborted'))`)
	if err != nil {
		return 0, fmt.Errorf("reclaim import staging: %w", err)
	}
	return tag.RowsAffected(), nil
}
