package modeledit

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RenameRevision's refusals; callers map each to their own status.
var (
	ErrRevisionNameEmpty = errors.New("name is required")
	ErrRevisionNameTaken = errors.New("another revision of this model already has that name")
	ErrRevisionNotFound  = errors.New("revision not found")
)

// RenameRevision gives a revision of modelID a new name (and, when
// description is not nil, a new description) and returns the name it now
// has — PATCH /api/developer/revisions/{id}, the admin's PATCH, the name
// given as an AI draft is promoted, and the AI Developer's rename_draft.
// modelID "" accepts a revision of any model; the caller has checked access.
//
// The model keeps its live revision's name beside its id
// (core.model.active_revision_name), which the console's header, the Live
// badge and the admin's by-name route read: renaming the live revision
// renames that copy in the same transaction, or the header went on showing
// the old name and the Live badge left the revisions list.
func RenameRevision(ctx context.Context, db DB, modelID, id, name string, description *string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrRevisionNameEmpty
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	var revModel string
	err = tx.QueryRow(ctx, `
		UPDATE model.revision SET name = $2, description = COALESCE($3, description)
		WHERE id::text = $1 AND ($4 = '' OR model_id::text = $4)
		RETURNING model_id::text`, id, name, description, modelID).Scan(&revModel)
	var pe *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrRevisionNotFound
	case errors.As(err, &pe) && pe.Code == "23505":
		return "", ErrRevisionNameTaken
	case err != nil:
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE core.model SET active_revision_name = $2
		WHERE id = $1::uuid AND active_revision_id::text = $3`, revModel, name, id); err != nil {
		return "", err
	}
	return name, tx.Commit(ctx)
}
