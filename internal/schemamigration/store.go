package schemamigration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type migrationRecord struct {
	ID            string
	ModelID       string
	RevisionID    string
	VersionNumber int32
	Status        string
	Files         []migrationFile
	Error         string
	CreatedAt     time.Time
	AppliedAt     *time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) NextVersion(ctx context.Context, modelID string) (int32, error) {
	var max *int32
	err := s.pool.QueryRow(ctx, `
		SELECT MAX(version_number) FROM deployment.schema_migration WHERE model_id = $1::uuid
	`, modelID).Scan(&max)
	if err != nil {
		return 0, err
	}
	if max == nil {
		return 1, nil
	}
	return *max + 1, nil
}

func (s *Store) Create(ctx context.Context, modelID, revisionID string, versionNum int32, files []migrationFile) (*migrationRecord, error) {
	filesJSON, err := json.Marshal(files)
	if err != nil {
		return nil, err
	}

	var id string
	var createdAt time.Time
	err = s.pool.QueryRow(ctx, `
		INSERT INTO deployment.schema_migration
		    (model_id, revision_id, version_number, files)
		VALUES ($1::uuid, NULLIF($2, '')::uuid, $3, $4)
		RETURNING id::text, created_at
	`, modelID, revisionID, versionNum, filesJSON).Scan(&id, &createdAt)
	if err != nil {
		return nil, err
	}

	return &migrationRecord{
		ID:            id,
		ModelID:       modelID,
		RevisionID:    revisionID,
		VersionNumber: versionNum,
		Status:        "pending",
		Files:         files,
		CreatedAt:     createdAt,
	}, nil
}

func (s *Store) Get(ctx context.Context, migrationID string) (*migrationRecord, error) {
	var r migrationRecord
	var filesJSON []byte
	var revisionID, errMsg *string
	var appliedAt *time.Time

	err := s.pool.QueryRow(ctx, `
		SELECT id::text, model_id::text, revision_id::text, version_number,
		       status, files, error, created_at, applied_at
		FROM deployment.schema_migration WHERE id = $1::uuid
	`, migrationID).Scan(
		&r.ID, &r.ModelID, &revisionID, &r.VersionNumber,
		&r.Status, &filesJSON, &errMsg, &r.CreatedAt, &appliedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("migration %s not found", migrationID)
	}
	if err != nil {
		return nil, err
	}

	if revisionID != nil {
		r.RevisionID = *revisionID
	}
	if appliedAt != nil {
		r.AppliedAt = appliedAt
	}
	if errMsg != nil {
		r.Error = *errMsg
	}
	if err := json.Unmarshal(filesJSON, &r.Files); err != nil {
		return nil, fmt.Errorf("unmarshal files: %w", err)
	}
	return &r, nil
}

func (s *Store) List(ctx context.Context, modelID string) ([]*migrationRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, model_id::text, revision_id::text, version_number,
		       status, files, error, created_at, applied_at
		FROM deployment.schema_migration
		WHERE model_id = $1::uuid
		ORDER BY version_number ASC
	`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []*migrationRecord
	for rows.Next() {
		var r migrationRecord
		var filesJSON []byte
		var revisionID, errMsg *string
		var appliedAt *time.Time

		if err := rows.Scan(
			&r.ID, &r.ModelID, &revisionID, &r.VersionNumber,
			&r.Status, &filesJSON, &errMsg, &r.CreatedAt, &appliedAt,
		); err != nil {
			return nil, err
		}
		if revisionID != nil {
			r.RevisionID = *revisionID
		}
		if appliedAt != nil {
			r.AppliedAt = appliedAt
		}
		if errMsg != nil {
			r.Error = *errMsg
		}
		if err := json.Unmarshal(filesJSON, &r.Files); err != nil {
			return nil, err
		}
		records = append(records, &r)
	}
	return records, rows.Err()
}

// Apply executes all files in the migration inside a single transaction.
// On failure, the transaction is rolled back and the record is marked 'failed'.
func (s *Store) Apply(ctx context.Context, migrationID string) error {
	rec, err := s.Get(ctx, migrationID)
	if err != nil {
		return err
	}
	if rec.Status != "pending" {
		return fmt.Errorf("migration is %s, not pending", rec.Status)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}

	for _, f := range rec.Files {
		if _, err := tx.Exec(ctx, f.SQL); err != nil {
			_ = tx.Rollback(ctx)
			_ = s.setStatus(ctx, migrationID, "failed", err.Error())
			return fmt.Errorf("execute %s: %w", f.Filename, err)
		}
	}

	_, err = tx.Exec(ctx, `
		UPDATE deployment.schema_migration
		SET status = 'applied', applied_at = now()
		WHERE id = $1::uuid
	`, migrationID)
	if err != nil {
		_ = tx.Rollback(ctx)
		return err
	}

	return tx.Commit(ctx)
}

// Rollback executes the rollback SQL for each file in reverse order
// and marks the migration as rolled_back.
func (s *Store) Rollback(ctx context.Context, migrationID string) error {
	rec, err := s.Get(ctx, migrationID)
	if err != nil {
		return err
	}
	if rec.Status != "applied" {
		return fmt.Errorf("migration is %s, not applied", rec.Status)
	}

	for i := len(rec.Files) - 1; i >= 0; i-- {
		if _, err := s.pool.Exec(ctx, rec.Files[i].RollbackSQL); err != nil {
			_ = s.setStatus(ctx, migrationID, "failed", err.Error())
			return fmt.Errorf("rollback %s: %w", rec.Files[i].Filename, err)
		}
	}

	_, err = s.pool.Exec(ctx, `
		UPDATE deployment.schema_migration
		SET status = 'rolled_back', rolled_back_at = now()
		WHERE id = $1::uuid
	`, migrationID)
	return err
}

func (s *Store) setStatus(ctx context.Context, migrationID, status, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE deployment.schema_migration SET status = $2, error = $3 WHERE id = $1::uuid
	`, migrationID, status, errMsg)
	return err
}
