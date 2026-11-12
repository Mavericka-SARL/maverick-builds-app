package metricformula

import (
	"context"
	"errors"
	"strings"

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
		"the revision already has a dimension named %q (dimension names are compared without regard to case); choose another name", name)
}

// MetricNameTaken returns the METRIC_NAME_TAKEN refusal for name when err is
// a unique violation, and err unchanged otherwise.
func MetricNameTaken(err error, name string) error {
	if !IsUniqueViolation(err) {
		return err
	}
	return invalidCode(CodeMetricNameTaken,
		"the revision already has a metric named %q (metric names are compared without regard to case); choose another name", name)
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

// CodeInvalidMetricName refuses a metric name a formula cannot write as it
// is.
const CodeInvalidMetricName = "INVALID_METRIC_NAME"

// ValidMetricName holds a new or renamed metric to the technical-name rule
// the console states (snake_case): letters, digits and underscores, starting
// with a letter or underscore. A name such as "Planning % for Revenue" is
// only reachable from a formula as {Planning % for Revenue}; the assistant
// named every metric that way and lost three rounds to parse errors. Metrics
// that already carry such a name keep it.
func ValidMetricName(name string) error {
	if !propertyNamePattern.MatchString(name) {
		return invalidCode(CodeInvalidMetricName,
			"metric name %q is not a technical name: use letters, digits and underscores, starting with a letter or underscore, for example %q", name, identifierFor(name))
	}
	return nil
}

// CodeInvalidMetricFormat refuses a display format the console does not have.
const CodeInvalidMetricFormat = "INVALID_METRIC_FORMAT"

// MetricFormats are the display formats the console offers.
var MetricFormats = []string{"number", "percentage", "currency", "boolean", "text", "picklist", "date"}

// ValidMetricFormat refuses a format outside MetricFormats ("" keeps the
// default, number). Any string used to be stored: the AI Developer wrote
// "percent", which every grid then showed as a plain number.
func ValidMetricFormat(format string) error {
	if format == "" {
		return nil
	}
	for _, f := range MetricFormats {
		if format == f {
			return nil
		}
	}
	hint := ""
	if strings.HasPrefix(strings.ToLower(format), "percent") {
		hint = ` — did you mean "percentage"?`
	}
	return invalidCode(CodeInvalidMetricFormat, "format %q is not one of %s%s", format, strings.Join(MetricFormats, ", "), hint)
}

// CodeNameIsOtherKind refuses a metric named like a dimension of its
// revision, or a dimension named like a metric: in a formula the bare name
// would mean the dimension where a cell pins it and the metric elsewhere,
// and a formula saved while only one of them existed stays bound to that
// one when the other is added.
const CodeNameIsOtherKind = "NAME_IS_OTHER_KIND"

// CheckNameFreeOfOtherKind refuses name for a new or renamed definition of
// kind ("metric" or "dimension") when the revision (or the model, for a
// revision-less row) has a definition of the other kind with that name,
// compared without regard to case. Existing pairs are left alone: revision
// copies and model imports keep what they copy.
func CheckNameFreeOfOtherKind(ctx context.Context, q Querier, modelID, revisionID, name, kind string) error {
	table, other := "model.dimension_def", "dimension"
	if kind == "dimension" {
		table, other = "model.metric_def", "metric"
	}
	var taken bool
	if err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM `+table+`
		               WHERE model_id=$1::uuid AND lower(name)=lower($2)
		                 AND (revision_id IS NOT DISTINCT FROM NULLIF($3,'')::uuid OR revision_id IS NULL))`,
		modelID, strings.TrimSpace(name), revisionID).Scan(&taken); err != nil {
		return err
	}
	if !taken {
		return nil
	}
	hint := ""
	if kind == "metric" {
		hint = ` — a pick-list over that dimension is usually named for what it holds per row (act_status over activity_statuses)`
	}
	return invalidCode(CodeNameIsOtherKind,
		"%q is already the name of a %s in this revision; a %s cannot share it, or a formula naming it could mean either%s",
		name, other, kind, hint)
}

// CheckRenameFreeOfOtherKind is CheckNameFreeOfOtherKind for renaming the
// metric or dimension id (kind) to name; keeping its name, in any case, is
// always allowed.
func CheckRenameFreeOfOtherKind(ctx context.Context, q Querier, id, name, kind string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	table := "model.metric_def"
	if kind == "dimension" {
		table = "model.dimension_def"
	}
	var modelID, revisionID, current string
	if err := q.QueryRow(ctx, `SELECT model_id::text, COALESCE(revision_id::text,''), name FROM `+table+` WHERE id=$1::uuid`, id).
		Scan(&modelID, &revisionID, &current); err != nil {
		return nil // the caller's own lookup reports a missing row
	}
	if strings.EqualFold(current, name) {
		return nil
	}
	return CheckNameFreeOfOtherKind(ctx, q, modelID, revisionID, name, kind)
}

// CodeGridNameTaken refuses a grid name its revision already has. Grids had
// no such rule: the AI Developer made "SaaS calculated", "Sites Logistics
// calculated" and "Headcount totals" twice each in one draft (2026-11-11),
// and every reference by name then picked one of a pair. Existing pairs are
// left alone (revision copies and model imports keep what they copy); a new
// grid or a rename may not make one.
const CodeGridNameTaken = "GRID_NAME_TAKEN"

// CheckGridNameFree refuses name for a new grid (exceptID "") or for
// renaming grid exceptID, when another grid of the revision (or, with no
// revision, of the model's revision-less grids) has it, compared without
// regard to case.
func CheckGridNameFree(ctx context.Context, q Querier, modelID, revisionID, name, exceptID string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	var taken bool
	if err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM model.grid_def
		               WHERE model_id = $1::uuid AND lower(btrim(name)) = lower($2)
		                 AND revision_id IS NOT DISTINCT FROM NULLIF($3,'')::uuid
		                 AND id::text <> $4)`,
		modelID, name, revisionID, exceptID).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return invalidCode(CodeGridNameTaken,
			"the revision already has a grid named %q (grid names are compared without regard to case); choose another name, or add to that grid", name)
	}
	return nil
}

// CheckGridRenameFree is CheckGridNameFree for renaming grid id.
func CheckGridRenameFree(ctx context.Context, q Querier, id, name string) error {
	var modelID, revisionID, current string
	if err := q.QueryRow(ctx, `SELECT model_id::text, COALESCE(revision_id::text,''), name FROM model.grid_def WHERE id=$1::uuid`, id).
		Scan(&modelID, &revisionID, &current); err != nil {
		return nil // the caller's own lookup reports a missing grid
	}
	if strings.EqualFold(strings.TrimSpace(current), strings.TrimSpace(name)) {
		return nil
	}
	return CheckGridNameFree(ctx, q, modelID, revisionID, name, id)
}
