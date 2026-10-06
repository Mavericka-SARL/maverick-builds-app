package writeguard

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// QueryRower is a pool or a transaction: ResolveWriteMembers also runs in
// the AI plan check's rolled-back transaction.
type QueryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// MemberError is a write coordinate no reader would ever read: a dimension
// that is not the revision's, a member code the dimension does not have, a
// parent member, or a calculated one. Code is the import's error code for
// the same refusal (UNKNOWN_DIMENSION, UNKNOWN_MEMBER, NOT_LEAF,
// CALCULATED_MEMBER), so every write path names it the same way.
type MemberError struct {
	Code    string
	Message string
}

func (e *MemberError) Error() string { return e.Message }

// IsMemberError reports whether err is a refused write coordinate.
func IsMemberError(err error) bool {
	var me *MemberError
	return errors.As(err, &me)
}

// ResolveWriteMembers resolves a write's {dimension id: member code} map to
// member ids, for the write guard, and refuses a coordinate no grid, chart
// or total reads. The interactive cell write, the gRPC write and the form
// posting all used to store a value at a code the dimension does not have
// (and an interactive write at a parent member, which the grid ignores but a
// pinned KPI counted); the file import already refused both.
//
// Leaf-ness is within the member's own dimension: a member of another
// dimension pointing at it through parent_dimension_id is a relation the
// rollup follows, not a level of this dimension's hierarchy.
//
// A *MemberError is a refusal (400); any other error is a failed read.
func ResolveWriteMembers(ctx context.Context, pool QueryRower, modelID, revisionID string, dimCodes map[string]string) ([]string, error) {
	ids := make([]string, 0, len(dimCodes))
	for dimID, code := range dimCodes {
		unknownDim := &MemberError{Code: "UNKNOWN_DIMENSION",
			Message: fmt.Sprintf("dim_codes names %q, which is no dimension of this revision", dimID)}
		if uuid.Validate(dimID) != nil {
			return nil, unknownDim
		}
		var dimName, memberID string
		var hasChildren, calculated bool
		err := pool.QueryRow(ctx, `
			SELECT d.name, COALESCE(m.id::text,''),
			       COALESCE(EXISTS(SELECT 1 FROM model.dimension_member c
			                       WHERE c.parent_member_id = m.id AND c.dimension_id = m.dimension_id), false),
			       COALESCE(NULLIF(btrim(m.formula),'') IS NOT NULL, false)
			FROM model.dimension_def d
			LEFT JOIN model.dimension_member m ON m.dimension_id = d.id AND m.code = $2
			WHERE d.id = $1::uuid AND d.model_id = $3::uuid
			  AND (d.revision_id = $4::uuid OR d.revision_id IS NULL)`,
			dimID, code, modelID, revisionID,
		).Scan(&dimName, &memberID, &hasChildren, &calculated)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, unknownDim
		}
		if err != nil {
			return nil, fmt.Errorf("resolve member %q: %w", code, err)
		}
		switch {
		case memberID == "":
			return nil, &MemberError{Code: "UNKNOWN_MEMBER",
				Message: fmt.Sprintf("there is no member %q in %s (member codes match exactly)", code, dimName)}
		case calculated:
			return nil, &MemberError{Code: "CALCULATED_MEMBER",
				Message: fmt.Sprintf("%q is a calculated member of %s: its values are computed from the other members, so it takes no input", code, dimName)}
		case hasChildren:
			return nil, &MemberError{Code: "NOT_LEAF",
				Message: fmt.Sprintf("%q is not a leaf member of %s (has child members): values are entered at its leaves", code, dimName)}
		}
		ids = append(ids, memberID)
	}
	return ids, nil
}
