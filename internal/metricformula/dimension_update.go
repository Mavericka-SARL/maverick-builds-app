package metricformula

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The structural rules of a dimension update, shared by the developer's
// POST/PATCH /api/developer/dimensions and the AI Developer's
// create_dimension/update_dimension, so the two paths cannot drift.

// CodeInvalidParentDimension rejects a parent dimension
// (ValidateParentDimension).
const CodeInvalidParentDimension = "INVALID_PARENT_DIMENSION"

// ParentDimension is a requested parent dimension of one dimension.
type ParentDimension struct {
	// The child: its model and revision ("" = a legacy dimension outside any
	// revision), its ID ("" while it is being created) and its type.
	ModelID, RevisionID, DimensionID string
	DimensionType                    string
	// The requested parent.
	ParentDimensionID string
}

// ValidateParentDimension checks a parent dimension:
//   - a time dimension has no parent dimension (its periods are placed by
//     their dates; the dimension_def_time_no_hierarchy_ck constraint);
//   - the parent exists and belongs to the same model AND the same
//     revision: a parent in another revision links two revisions'
//     structures, a revision copy remaps nothing for it, and rollup, which
//     loads one revision's dimensions, silently finds no relation;
//   - a dimension is not its own parent, and following parents from the
//     new parent never returns to the dimension (no cycle).
func ValidateParentDimension(ctx context.Context, q Querier, p ParentDimension) error {
	if p.DimensionType == "time" {
		return invalidCode(CodeInvalidParentDimension, "a time dimension cannot have a parent dimension")
	}
	if !uuidPattern.MatchString(p.ParentDimensionID) {
		return invalidCode(CodeInvalidParentDimension, "parent dimension %q not found", p.ParentDimensionID)
	}
	if p.DimensionID != "" && p.ParentDimensionID == p.DimensionID {
		return invalidCode(CodeInvalidParentDimension, "a dimension cannot be its own parent")
	}
	var parentModel, parentRev, parentName string
	err := q.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text,''), name
		FROM model.dimension_def WHERE id=$1::uuid
	`, p.ParentDimensionID).Scan(&parentModel, &parentRev, &parentName)
	if errors.Is(err, pgx.ErrNoRows) {
		return invalidCode(CodeInvalidParentDimension, "parent dimension %s not found", p.ParentDimensionID)
	}
	if err != nil {
		return fmt.Errorf("load parent dimension: %w", err)
	}
	if parentModel != p.ModelID {
		return invalidCode(CodeInvalidParentDimension, "parent dimension must belong to the same model")
	}
	if parentRev != p.RevisionID {
		return invalidCode(CodeInvalidParentDimension,
			"the parent dimension %s belongs to another revision; choose a dimension of the same revision", parentName)
	}
	if p.DimensionID != "" {
		cur := p.ParentDimensionID
		for hops := 0; cur != "" && hops < 64; hops++ {
			if cur == p.DimensionID {
				return invalidCode(CodeInvalidParentDimension, "this would create a dimension hierarchy cycle")
			}
			var next *string
			if err := q.QueryRow(ctx,
				`SELECT parent_dimension_id::text FROM model.dimension_def WHERE id=$1::uuid`, cur,
			).Scan(&next); err != nil || next == nil {
				break
			}
			cur = *next
		}
	}
	return nil
}

// GroupingPatchRequest is what a dimension update asks of its property
// grouping. A *Sent flag says the field was given at all; a nil pointer
// given means "clear".
type GroupingPatchRequest struct {
	SourceSent   bool
	Source       *string // source_dimension_id, already resolved to an ID
	PropertySent bool
	Property     *string // source_property
	Derive       bool    // derive_members
	// The update's parent dimension: ParentSent with nil NewParent detaches.
	ParentSent bool
	NewParent  *string
}

// GroupingPatch is what a dimension update does to the property grouping.
type GroupingPatch struct {
	Write            bool    // set source_dimension_id/source_property to the two below
	Source, Property *string // nil = cleared
	Derive           bool    // derive missing members afterwards
	SourceChanged    bool    // the grouping's source was cleared or replaced
	Changed          bool    // anything about the grouping changed: recalc
}

// PlanGroupingPatch reads a dimension update's grouping fields against the
// dimension's current state and the update's resulting parent dimension,
// and validates the result with ValidateGrouping.
//
// Clearing or replacing the source is refused (DIMENSION_IN_USE) while a
// formula names the dimension: such a formula read the source's metrics
// through the grouping, and every cell of it would fail and keep its last
// values — the reason a dimension delete is refused. Changing only the
// property keeps the relation and recalculates.
//
// Errors are ValidationErrors with INVALID_GROUPING (a bad request) or
// DIMENSION_IN_USE (a conflict); anything else is a server error.
func PlanGroupingPatch(ctx context.Context, q Querier, dimID string, req GroupingPatchRequest) (GroupingPatch, error) {
	var p GroupingPatch
	var modelID, revisionID, dimType string
	var curParent, curSource, curProp *string
	if err := q.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text,''), dimension_type,
		       parent_dimension_id::text, source_dimension_id::text, source_property
		FROM model.dimension_def WHERE id=$1::uuid
	`, dimID).Scan(&modelID, &revisionID, &dimType, &curParent, &curSource, &curProp); err != nil {
		return p, fmt.Errorf("load dimension: %w", err)
	}
	finalParent := curParent
	if req.ParentSent {
		finalParent = req.NewParent
	}
	if !req.SourceSent && !req.PropertySent && !req.Derive {
		if curSource != nil && finalParent != nil {
			return p, invalidCode(CodeInvalidGrouping,
				"a dimension has either a parent dimension or a property grouping, not both; clear source_dimension_id first")
		}
		return p, nil
	}
	src, prop := curSource, curProp
	if req.SourceSent {
		src = req.Source
		if src == nil && !req.PropertySent {
			prop = nil // clearing the source clears the pair
		}
	}
	if req.PropertySent {
		prop = req.Property
		if prop != nil && strings.TrimSpace(*prop) == "" {
			prop = nil
		}
	}
	p.SourceChanged = curSource != nil && (src == nil || *src != *curSource)
	if src == nil && prop == nil {
		if req.Derive {
			return p, invalidCode(CodeInvalidGrouping,
				"derive_members needs a property grouping (source_dimension_id and source_property)")
		}
	} else {
		declared, err := ValidateGrouping(ctx, q, Grouping{
			ModelID: modelID, RevisionID: revisionID, DimensionID: dimID, DimensionType: dimType,
			HasParentDimension: finalParent != nil,
			SourceDimensionID:  derefString(src), SourceProperty: derefString(prop),
		})
		if err != nil {
			return p, err
		}
		prop = &declared
	}
	if p.SourceChanged {
		if err := CheckDimensionNotInUse(ctx, q, dimID); err != nil {
			// The code stays first so a client can match on the prefix;
			// the context goes into the message after it.
			var ve *ValidationError
			if errors.As(err, &ve) {
				return p, invalidCode(ve.Code,
					"the grouping's source cannot be cleared or replaced while formulas read through it: %s", ve.Message)
			}
			return p, err
		}
	}
	p.Write, p.Source, p.Property, p.Derive = true, src, prop, req.Derive && src != nil
	p.Changed = derefString(src) != derefString(curSource) || derefString(prop) != derefString(curProp)
	return p, nil
}

// ValidationCode returns the stable code of a ValidationError err is or
// wraps, and "" for anything else.
func ValidationCode(err error) string {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Code
	}
	return ""
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
