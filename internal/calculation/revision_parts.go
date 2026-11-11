package calculation

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// revisionParts remembers, per process, the revisions whose parts exist
// (the zero time) and when the last try for the others failed. Revision ids
// are unique across databases.
var revisionParts sync.Map

// revisionPartsRetry is how long a revision whose parts could not be made
// waits before the next pass tries again: each try may wait two seconds for
// its locks.
const revisionPartsRetry = time.Minute

// EnsureRevisionParts makes the revision's own parts of the tables kept one
// part per revision (runtime.add_revision_parts, migration 177) before a
// pass writes its results; until then they go to the table's default part,
// which a revision's delete empties only row by row. Made on the pool,
// outside any transaction: attaching a part holds the writes to models and
// metrics while it runs, so it must not wait for a caller's transaction to
// end. A store built on a transaction (a dry run) leaves it. Not making them
// is no error for the pass — its results go to the default part, and the
// next pass or compaction moves them.
func (s *Store) EnsureRevisionParts(ctx context.Context, revisionID string) {
	if s.pool == nil || revisionID == "" {
		return
	}
	if v, ok := revisionParts.Load(revisionID); ok {
		if failed := v.(time.Time); failed.IsZero() || time.Since(failed) < revisionPartsRetry {
			return
		}
	}
	var done bool
	err := s.pool.QueryRow(ctx, `SELECT runtime.add_revision_parts($1::uuid)`, revisionID).Scan(&done)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "42883":
		// A database without migration 177 keeps no table by revision.
		revisionParts.Store(revisionID, time.Time{})
	case err == nil && done:
		revisionParts.Store(revisionID, time.Time{})
	default:
		revisionParts.Store(revisionID, time.Now())
	}
}
