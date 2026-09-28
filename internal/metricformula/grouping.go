package metricformula

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Property groupings (dimension_def.source_dimension_id + source_property).
//
// A grouping dimension's members group the SOURCE dimension's members by the
// value of one of their properties: grouping member "North" stands for every
// source member whose property value is exactly "North" (rollup.relate). The
// grouping's members are ordinary rows of its own — nothing derives them at
// read time, which is also how a model import carries them — so a source
// value with no grouping member of that code is simply under no group.
// DeriveGroupingMembers adds the missing ones on request.

// CodeInvalidGrouping rejects a property grouping (ValidateGrouping).
const CodeInvalidGrouping = "INVALID_GROUPING"

// Grouping is a requested property grouping of one dimension.
type Grouping struct {
	// The grouping dimension: its model and revision ("" = a legacy
	// dimension outside any revision), its ID ("" while it is being
	// created), its type, and whether it has a parent dimension.
	ModelID, RevisionID, DimensionID string
	DimensionType                    string
	HasParentDimension               bool
	// The source dimension whose members are grouped, and the property
	// (declared on the source) whose value names the group.
	SourceDimensionID, SourceProperty string
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidateGrouping checks a property grouping and returns the property's
// declared spelling, which is what the caller stores as source_property.
// The one validator of the developer's dimension create/PATCH and the AI
// Developer's create_dimension. Rules:
//   - source_dimension_id and source_property go together;
//   - the source is a standard dimension of the same model and revision,
//     and not the dimension itself;
//   - the property is DECLARED on the source (a formula and the grouping
//     then read the same thing, and a rename carries the grouping along);
//   - neither side is a time dimension: time members are periods placed by
//     their dates, not grouped by a property (spec §4.1, and the
//     dimension_def_time_no_hierarchy_ck constraint);
//   - a dimension has a parent dimension OR a property grouping, not both:
//     rollup resolves the structural chain first and would ignore the
//     grouping;
//   - following source links from the source never returns to the
//     dimension (no cycle).
func ValidateGrouping(ctx context.Context, q Querier, g Grouping) (string, error) {
	g.SourceProperty = strings.TrimSpace(g.SourceProperty)
	if g.SourceDimensionID == "" || g.SourceProperty == "" {
		return "", invalidCode(CodeInvalidGrouping,
			"a property grouping needs both source_dimension_id and source_property")
	}
	if g.DimensionType == "time" {
		return "", invalidCode(CodeInvalidGrouping,
			"a time dimension cannot group another dimension's members by a property")
	}
	if g.HasParentDimension {
		return "", invalidCode(CodeInvalidGrouping,
			"a dimension has either a parent dimension or a property grouping, not both; clear parent_dimension_id first")
	}
	if g.DimensionID != "" && g.SourceDimensionID == g.DimensionID {
		return "", invalidCode(CodeInvalidGrouping, "a dimension cannot group its own members")
	}
	if !uuidPattern.MatchString(g.SourceDimensionID) {
		return "", invalidCode(CodeInvalidGrouping, "source dimension %q not found", g.SourceDimensionID)
	}
	var srcModel, srcRev, srcName, srcType string
	err := q.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text,''), name, dimension_type
		FROM model.dimension_def WHERE id=$1::uuid
	`, g.SourceDimensionID).Scan(&srcModel, &srcRev, &srcName, &srcType)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", invalidCode(CodeInvalidGrouping, "source dimension %s not found", g.SourceDimensionID)
	}
	if err != nil {
		return "", fmt.Errorf("load source dimension: %w", err)
	}
	if srcModel != g.ModelID {
		return "", invalidCode(CodeInvalidGrouping, "the source dimension must belong to the same model")
	}
	if srcRev != g.RevisionID {
		return "", invalidCode(CodeInvalidGrouping,
			"the source dimension %s belongs to another revision; group a dimension of the same revision", srcName)
	}
	if srcType == "time" {
		return "", invalidCode(CodeInvalidGrouping,
			"%s is a time dimension; a time dimension's periods cannot be grouped by a property", srcName)
	}
	if g.DimensionID != "" {
		cur := g.SourceDimensionID
		for hops := 0; cur != "" && hops < 64; hops++ {
			if cur == g.DimensionID {
				return "", invalidCode(CodeInvalidGrouping,
					"this would create a grouping cycle: %s is (through its own source) grouped from this dimension", srcName)
			}
			var next string
			if err := q.QueryRow(ctx,
				`SELECT COALESCE(source_dimension_id::text,'') FROM model.dimension_def WHERE id=$1::uuid`, cur,
			).Scan(&next); err != nil {
				break
			}
			cur = next
		}
	}
	var declared string
	err = q.QueryRow(ctx, `
		SELECT name FROM model.dimension_property
		WHERE dimension_id=$1::uuid AND lower(name)=lower($2)
	`, g.SourceDimensionID, g.SourceProperty).Scan(&declared)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", invalidCode(CodeInvalidGrouping,
			"property %q is not declared on %s; declare it first (its members' values of it name the groups)",
			g.SourceProperty, srcName)
	}
	if err != nil {
		return "", fmt.Errorf("load property declaration: %w", err)
	}
	return declared, nil
}

// GroupingValues returns the distinct non-blank values the source
// dimension's members hold for property prop (the member key matched
// case-insensitively, as formulas read it), sorted — the member codes a
// grouping by that property needs.
func GroupingValues(ctx context.Context, q Querier, sourceDimensionID, prop string) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT e.value
		FROM model.dimension_member m, jsonb_each_text(m.properties) AS e(key, value)
		WHERE m.dimension_id=$1::uuid AND lower(e.key)=lower($2) AND btrim(e.value) <> ''
		ORDER BY e.value
	`, sourceDimensionID, prop)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// MissingGroupingMembers returns the values of GroupingValues that the
// grouping dimension has no member for yet.
func MissingGroupingMembers(ctx context.Context, q Querier, dimensionID, sourceDimensionID, prop string) ([]string, error) {
	values, err := GroupingValues(ctx, q, sourceDimensionID, prop)
	if err != nil || len(values) == 0 {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		SELECT v FROM unnest($2::text[]) AS v
		WHERE NOT EXISTS (SELECT 1 FROM model.dimension_member x WHERE x.dimension_id=$1::uuid AND x.code=v)
		ORDER BY v
	`, dimensionID, values)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// DeriveGroupingMembers adds one member (code = label = the value) to the
// grouping dimension for each code, after its existing members; a code it
// already has is skipped. It returns the codes it added.
func DeriveGroupingMembers(ctx context.Context, q Querier, dimensionID string, codes []string) ([]string, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		INSERT INTO model.dimension_member (dimension_id, code, label, sort_order)
		SELECT $1::uuid, v.code, v.code,
		       (SELECT COALESCE(MAX(sort_order),0) FROM model.dimension_member WHERE dimension_id=$1::uuid) + v.n
		FROM unnest($2::text[]) WITH ORDINALITY AS v(code, n)
		ON CONFLICT (dimension_id, code) DO NOTHING
		RETURNING code
	`, dimensionID, codes)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// checkPropertyNotGrouping refuses deleting property prop of dimensionID
// while a dimension groups dimensionID's members by it (PROPERTY_IN_USE,
// naming the grouping dimensions): the grouping names a declared property,
// and a delete would leave it grouping by one no formula can read.
func checkPropertyNotGrouping(ctx context.Context, q Querier, dimensionID, prop string) error {
	rows, err := q.Query(ctx, `
		SELECT name FROM model.dimension_def
		WHERE source_dimension_id=$1::uuid AND lower(source_property)=lower($2)
		ORDER BY name
	`, dimensionID, prop)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(names) > 0 {
		return invalidCode(CodePropertyInUse,
			"%s groups this dimension's members by %s; clear or change that grouping first, then delete the property",
			strings.Join(names, ", "), prop)
	}
	return nil
}

// CheckDimensionNotGrouped refuses deleting a dimension while another
// dimension groups its members by a property (DIMENSION_IN_USE, naming the
// groupings): the grouping would be left with a property but no source, and
// every formula reading through it would fail. Clear or delete the grouping
// first.
func CheckDimensionNotGrouped(ctx context.Context, q Querier, dimensionID string) error {
	rows, err := q.Query(ctx, `
		SELECT name FROM model.dimension_def WHERE source_dimension_id=$1::uuid ORDER BY name
	`, dimensionID)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(names) > 0 {
		return invalidCode(CodeDimensionInUse,
			"%s groups this dimension's members by a property; clear that grouping (or delete it) first, then delete the dimension",
			strings.Join(names, ", "))
	}
	return nil
}
