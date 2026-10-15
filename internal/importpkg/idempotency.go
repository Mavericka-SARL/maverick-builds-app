package importpkg

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// An import a client sends again under the same Idempotency-Key is not
// applied twice (migration 140): a client whose request ended without an
// answer cannot tell whether the import was saved, and an incremental import
// sent again added every value a second time.

// ErrImportKeyInUse is a key another import of the same person and model is
// running under.
var ErrImportKeyInUse = errors.New("an import sent with this Idempotency-Key is still running")

// KeyedJob is the import job a person sent under an Idempotency-Key.
type KeyedJob struct {
	ID         string
	Status     string
	RevisionID string
	TotalRows  int
	CreatedAt  time.Time
}

// JobByKey is the import job userID sent for modelID under key; nil when
// there is none.
func (s *Store) JobByKey(ctx context.Context, modelID, userID, key string) (*KeyedJob, error) {
	var j KeyedJob
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, status::text, COALESCE(revision_id::text, ''), valid_rows, created_at
		FROM import.import_job
		WHERE model_id = $1::uuid AND created_by = $2::uuid AND idempotency_key = $3
	`, modelID, userID, key).Scan(&j.ID, &j.Status, &j.RevisionID, &j.TotalRows, &j.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// SetJobKey records the Idempotency-Key jobID was sent with. A key one of
// the person's imports of the model holds already is ErrImportKeyInUse: two
// requests sent with one key at once run once.
func (s *Store) SetJobKey(ctx context.Context, jobID, key string) error {
	_, err := s.pool.Exec(ctx, `UPDATE import.import_job SET idempotency_key = $2 WHERE id = $1::uuid`, jobID, key)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return ErrImportKeyInUse
	}
	return err
}

// ReleaseKey frees the key of an import that never finished — validated
// and staged, then cut off before its commit — so the import can be sent
// again under it: nothing of it was saved. A committed import keeps its key.
func (s *Store) ReleaseKey(ctx context.Context, jobID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE import.import_job SET idempotency_key = NULL,
		       status = CASE WHEN status IN ('validating', 'staged') THEN 'aborted'::import.import_status ELSE status END
		WHERE id = $1::uuid AND status <> 'committed'`, jobID)
	return err
}
