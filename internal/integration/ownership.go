package integration

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RowQuerier is what the ownership check reads through: a pool, the
// gateway's tenant handle, or a transaction.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// targetTables maps every target type an integration row can name to the
// table that holds it. rest_api configs name grid, form or dimension; a
// legacy csv_import/google_sheets row may also name a dashboard, which
// revision duplication remaps too.
var targetTables = map[string]string{
	string(TargetGrid):      "model.grid_def",
	string(TargetForm):      "model.form_def",
	string(TargetDimension): "model.dimension_def",
	"dashboard":             "model.dashboard_def",
}

// foreignError is an ownership failure on a row that exists but is another
// model's or application's — as opposed to a missing one (a target
// deleted since it was chosen), which is a plain error.
type foreignError struct{ msg string }

func (e *foreignError) Error() string { return e.msg }

// IsForeign reports whether err (from CheckOwnership) names a row that
// exists in another model or application. A save that keeps a target it
// does not change refuses only these: a target deleted since it was chosen
// must not block a rename or a copy, and every use re-checks it anyway.
func IsForeign(err error) bool {
	var fe *foreignError
	return errors.As(err, &fe)
}

// CheckOwnership verifies that an integration's target and connection are
// its own. A named target must be a row of modelID, which also keeps it
// inside the tenant, since a model belongs to one application. A named
// connection must belong to modelID's application.
//
// Every path that stores or uses a target calls it: each save in the
// gateway, and the worker immediately before it reads or writes, because a
// stored row may predate a check. An empty targetID or connectionID means
// "not chosen yet" (a draft) and passes.
func CheckOwnership(ctx context.Context, q RowQuerier, modelID, targetType, targetID, connectionID string) error {
	if targetID != "" {
		table, ok := targetTables[targetType]
		if !ok {
			return fmt.Errorf("unknown target type %q", targetType)
		}
		if _, err := uuid.Parse(targetID); err != nil {
			return fmt.Errorf("target %s not found", targetType)
		}
		var owner string
		if err := q.QueryRow(ctx,
			"SELECT model_id::text FROM "+table+" WHERE id=$1::uuid", targetID).Scan(&owner); err != nil {
			return fmt.Errorf("target %s not found", targetType)
		}
		if owner != modelID {
			return &foreignError{msg: "target belongs to a different model"}
		}
	}
	if connectionID != "" {
		if _, err := uuid.Parse(connectionID); err != nil {
			return fmt.Errorf("connection not found")
		}
		var sameApp bool
		if err := q.QueryRow(ctx, `
			SELECT c.application_id = m.application_id
			FROM model.integration_connection c, core.model m
			WHERE c.id=$1::uuid AND m.id=$2::uuid
		`, connectionID, modelID).Scan(&sameApp); err != nil {
			return fmt.Errorf("connection not found")
		}
		if !sameApp {
			return &foreignError{msg: "connection belongs to a different application"}
		}
	}
	return nil
}

// checkDefinitionOwnership is CheckOwnership for a definition about to run.
// Running needs a target, so an empty one fails here rather than passing
// as a draft's would.
func checkDefinitionOwnership(ctx context.Context, q RowQuerier, def *Definition) error {
	if def == nil || def.Config == nil {
		return fmt.Errorf("configuration missing")
	}
	if def.ModelID == "" || def.Config.TargetID == "" {
		return fmt.Errorf("target %s not found", def.Config.TargetType)
	}
	return CheckOwnership(ctx, q, def.ModelID, string(def.Config.TargetType), def.Config.TargetID, def.ConnectionID)
}
