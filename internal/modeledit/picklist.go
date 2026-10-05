package modeledit

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// Pick-lists (metric format "picklist", migration 110): each cell holds a
// member of model.metric_def.picklist_dimension_id, stored as
// formula.PicklistKey of the member's code.

// RemapPicklistDimensions points every pick-list metric of revision newRev,
// copied from srcRev, at newRev's own copy of its dimension (matched by
// name), as revision copies remap every other cross-entity column. Run it
// once both the metrics and the dimensions are copied. A dimension that is
// not the revision's own (a model-wide one) keeps its id.
func RemapPicklistDimensions(ctx context.Context, db DB, modelID, newRev, srcRev string) error {
	_, err := db.Exec(ctx, `
		UPDATE model.metric_def nm
		SET picklist_dimension_id = COALESCE(
		        (SELECT nd.id FROM model.dimension_def nd
		         WHERE nd.model_id = nm.model_id AND nd.revision_id = nm.revision_id AND nd.name = od.name
		         LIMIT 1),
		        CASE WHEN od.revision_id IS NULL THEN od.id END)
		FROM model.metric_def om
		JOIN model.dimension_def od ON od.id = om.picklist_dimension_id
		WHERE nm.model_id = $1::uuid AND nm.revision_id = $2::uuid
		  AND om.model_id = $1::uuid AND om.revision_id = $3::uuid
		  AND om.name = nm.name`, modelID, newRev, srcRev)
	if err != nil {
		return fmt.Errorf("remap pick-list dimensions: %w", err)
	}
	return nil
}

// PicklistMetricsOn returns the names of the pick-list metrics whose cells
// hold members of dimID.
func PicklistMetricsOn(ctx context.Context, db DB, dimID string) ([]string, error) {
	rows, err := db.Query(ctx, `SELECT name FROM model.metric_def WHERE picklist_dimension_id = $1::uuid ORDER BY name`, dimID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ErrDimensionHasPicklists refuses deleting a dimension that pick-lists hold
// members of.
var ErrDimensionHasPicklists = errors.New("pick-lists hold members of this dimension")

// CheckDimensionDeletable refuses deleting dimID while a pick-list metric
// holds its members; the message names them.
func CheckDimensionDeletable(ctx context.Context, db DB, dimID string) error {
	names, err := PicklistMetricsOn(ctx, db, dimID)
	if err != nil || len(names) == 0 {
		return err
	}
	return fmt.Errorf("%w: %s — change their format or delete them first", ErrDimensionHasPicklists, strings.Join(names, ", "))
}

// ErrMemberInPicklist refuses deleting a member that pick-list cells hold.
var ErrMemberInPicklist = errors.New("pick-list cells hold this member")

// CheckMemberDeletable refuses deleting the member code of dimID while a
// pick-list cell holds it (its latest value is the member's key); the
// message names the metrics.
func CheckMemberDeletable(ctx context.Context, db DB, dimID, code string) error {
	rows, err := db.Query(ctx, `
		SELECT DISTINCT m.name
		FROM model.metric_def m
		JOIN LATERAL (
		    SELECT DISTINCT ON (f.dim_members) f.value
		    FROM runtime.fact_input f
		    WHERE f.metric_id = m.id
		    ORDER BY f.dim_members, f.entered_at DESC
		) latest ON latest.value = $2::numeric
		WHERE m.picklist_dimension_id = $1::uuid
		ORDER BY m.name`, dimID, formula.PicklistKey(code))
	if err != nil {
		return err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil || len(names) == 0 {
		return err
	}
	return fmt.Errorf("%w (%s): choose another member in those cells first", ErrMemberInPicklist, strings.Join(names, ", "))
}

// rekeyPicklistValues rewrites the cells of every pick-list on dimID that
// hold the member oldCode to its new code's key. Calculated pick-lists are
// recalculated by the member edit itself.
func rekeyPicklistValues(ctx context.Context, db DB, dimID, oldCode, newCode string) error {
	_, err := db.Exec(ctx, `
		UPDATE runtime.fact_input f SET value = $3::numeric
		FROM model.metric_def m
		WHERE m.picklist_dimension_id = $1::uuid AND f.metric_id = m.id AND f.value = $2::numeric`,
		dimID, formula.PicklistKey(oldCode), formula.PicklistKey(newCode))
	if err != nil {
		return fmt.Errorf("re-key pick-list values: %w", err)
	}
	return nil
}
