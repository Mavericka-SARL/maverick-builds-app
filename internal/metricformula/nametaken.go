package metricformula

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// Stable identifiers of a definition whose name is already taken in its
// revision (or, for a revision-less definition, in the model). The unique
// indexes metric_def_*_uq and dimension_def_*_uq enforce it; the writers
// turn their violation into these codes (HTTP 409) instead of a server
// error carrying the SQL text.
const (
	CodeDimensionNameTaken = "DIMENSION_NAME_TAKEN"
	CodeMetricNameTaken    = "METRIC_NAME_TAKEN"
	// CodeMemberCodeTaken refuses a member code already used in its
	// dimension (dimension_member_dimension_id_code_key).
	CodeMemberCodeTaken = "MEMBER_CODE_TAKEN"
)

// memberCodeKey is the unique constraint on (dimension_id, code) of
// model.dimension_member. A time dimension's other unique constraints
// (time_index, period_start) are not a taken code and are left alone.
const memberCodeKey = "dimension_member_dimension_id_code_key"

// IsUniqueViolation reports whether err is (or wraps) a Postgres unique
// violation (SQLSTATE 23505).
func IsUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// DimensionNameTaken returns the DIMENSION_NAME_TAKEN refusal for name when
// err is a unique violation, and err unchanged otherwise.
func DimensionNameTaken(err error, name string) error {
	if !IsUniqueViolation(err) {
		return err
	}
	return invalidCode(CodeDimensionNameTaken,
		"the revision already has a dimension named %q; choose another name", name)
}

// MetricNameTaken returns the METRIC_NAME_TAKEN refusal for name when err is
// a unique violation, and err unchanged otherwise.
func MetricNameTaken(err error, name string) error {
	if !IsUniqueViolation(err) {
		return err
	}
	return invalidCode(CodeMetricNameTaken,
		"the revision already has a metric named %q; choose another name", name)
}

// IsMemberCodeTaken reports whether err is (or wraps) the unique violation
// of a member code within its dimension.
func IsMemberCodeTaken(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == memberCodeKey
}

// MemberCodeTaken returns the MEMBER_CODE_TAKEN refusal for code when err is
// the member-code unique violation, and err unchanged otherwise.
func MemberCodeTaken(err error, code string) error {
	if !IsMemberCodeTaken(err) {
		return err
	}
	return invalidCode(CodeMemberCodeTaken,
		"the dimension already has a member with code %q; choose another code", code)
}
