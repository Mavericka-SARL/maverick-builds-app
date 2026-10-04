package metricformula

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// Recalculation triggers (contract C8). Dimension, property and member
// references create no calc_dependency rows, so the metrics that read a
// dimension are found by re-analysing the formulas — no schema column, so
// nothing new for revision duplication or model export to remap.

// FormulaReferencesDimension reports whether a formula reads dimension dim:
// as a bare name (the cell's member code), as dim.property, as PARENT(dim),
// or as a LOOKUP / criteria-range dimension. Names match case-insensitively.
// A formula that does not parse reads nothing.
func FormulaReferencesDimension(formulaText, dim string) bool {
	if strings.TrimSpace(formulaText) == "" || dim == "" {
		return false
	}
	an, err := formula.Analyze(formulaText)
	if err != nil {
		return false
	}
	for _, r := range an.References {
		if strings.EqualFold(r.Name, dim) {
			return true
		}
	}
	for _, p := range an.PropertyRefs {
		if strings.EqualFold(p.Dim, dim) {
			return true
		}
	}
	for _, d := range an.DimensionArgs {
		if strings.EqualFold(d, dim) {
			return true
		}
	}
	return false
}

// MetricsReferencingDimension returns the calculated metrics whose formula
// references dimension dimName (FormulaReferencesDimension), grouped by
// revision ID. revisionID scopes the search to one revision; "" searches
// every revision of the model, for a revision-less dimension every revision
// sees. Metrics with no revision are never recalculated by the scheduler
// and are left out.
func MetricsReferencingDimension(ctx context.Context, q Querier, modelID, revisionID, dimName string) (map[string][]string, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, revision_id::text, formula
		FROM model.metric_def
		WHERE model_id=$1::uuid AND is_input = false AND formula IS NOT NULL AND formula <> ''
		  AND revision_id IS NOT NULL
		  AND ($2 = '' OR revision_id = NULLIF($2,'')::uuid)
		ORDER BY revision_id, name
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	type row struct{ id, rev, text string }
	collected, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.rev, &x.text)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, m := range collected {
		if FormulaReferencesDimension(m.text, dimName) {
			out[m.rev] = append(out[m.rev], m.id)
		}
	}
	return out, nil
}

// FormulaReadsProperty reports whether a formula reads dim.prop anywhere —
// as a value, a LOOKUP member or a criteria range. Names match
// case-insensitively; a formula that does not parse reads nothing.
func FormulaReadsProperty(formulaText, dim, prop string) bool {
	if strings.TrimSpace(formulaText) == "" {
		return false
	}
	an, err := formula.Analyze(formulaText)
	if err != nil {
		return false
	}
	for _, p := range an.PropertyRefs {
		if strings.EqualFold(p.Dim, dim) && strings.EqualFold(p.Property, prop) {
			return true
		}
	}
	return false
}

// resolvesToDimension restricts a metric_def query (aliased m; $2 the
// dimension's revision ID, empty for a revision-less dimension; $3 its name)
// to the metrics in which the dimension's name resolves to THIS dimension.
// A revision-less dimension is visible to every revision, but a revision
// that owns a dimension of the same name (case-insensitively) resolves the
// name to its own (loadRevisionDims.lookup), so its formulas never read the
// revision-less one's properties.
const resolvesToDimension = `($2 <> '' OR NOT EXISTS (
		SELECT 1 FROM model.dimension_def own
		WHERE own.model_id = m.model_id AND own.revision_id = m.revision_id AND lower(own.name) = lower($3)))`

// CheckPropertyNotInUse refuses deleting property prop of dimension
// dimensionID while a calculated metric of the dimension's revision still
// reads it: a metric whose every cell fails keeps its last good values, so
// allowing it would leave viewers looking at numbers computed from a
// property that no longer exists. The error names the metrics so their
// formulas can be changed first. A rename is never refused — it rewrites the
// formulas (RenamePropertyInFormulas) — and retyping keeps the name and
// recalculates.
func CheckPropertyNotInUse(ctx context.Context, q Querier, dimensionID, prop string) error {
	var modelID, revisionID, dimName string
	if err := q.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text, ''), name
		FROM model.dimension_def WHERE id=$1::uuid
	`, dimensionID).Scan(&modelID, &revisionID, &dimName); err != nil {
		return err
	}
	rows, err := q.Query(ctx, `
		SELECT name, formula FROM model.metric_def m
		WHERE model_id=$1::uuid AND is_input = false AND formula IS NOT NULL AND formula <> ''
		  AND ($2 = '' OR revision_id = NULLIF($2,'')::uuid)
		  AND `+resolvesToDimension+`
		ORDER BY name
	`, modelID, revisionID, dimName)
	if err != nil {
		return err
	}
	type row struct{ name, text string }
	metrics, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.name, &x.text)
		return x, err
	})
	if err != nil {
		return err
	}
	var readers []string
	for _, m := range metrics {
		if FormulaReadsProperty(m.text, dimName, prop) {
			readers = append(readers, m.name)
		}
	}
	if len(readers) > 0 {
		return invalidCode(CodePropertyInUse,
			"%s.%s is read by %s; change those formulas first, then delete the property",
			dimName, prop, strings.Join(readers, ", "))
	}
	// A property grouping by it (dimension_def.source_property) uses it too.
	return checkPropertyNotGrouping(ctx, q, dimensionID, prop)
}

// CheckDimensionNotInUse refuses deleting a dimension while a calculated
// metric's formula names it — as a bare name, dim.property, PARENT, or a
// LOOKUP / criteria-range dimension: every cell of such a metric would fail
// and keep serving its last values, exactly what CheckPropertyNotInUse
// prevents for one property (deleting the dimension deletes its property
// declarations with it). The error names the metrics.
func CheckDimensionNotInUse(ctx context.Context, q Querier, dimensionID string) error {
	var modelID, revisionID, dimName string
	if err := q.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text, ''), name
		FROM model.dimension_def WHERE id=$1::uuid
	`, dimensionID).Scan(&modelID, &revisionID, &dimName); err != nil {
		return err
	}
	rows, err := q.Query(ctx, `
		SELECT name, formula FROM model.metric_def m
		WHERE model_id=$1::uuid AND is_input = false AND formula IS NOT NULL AND formula <> ''
		  AND ($2 = '' OR revision_id = NULLIF($2,'')::uuid)
		  AND `+resolvesToDimension+`
		ORDER BY name
	`, modelID, revisionID, dimName)
	if err != nil {
		return err
	}
	type row struct{ name, text string }
	metrics, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.name, &x.text)
		return x, err
	})
	if err != nil {
		return err
	}
	var readers []string
	for _, m := range metrics {
		if FormulaReferencesDimension(m.text, dimName) {
			readers = append(readers, m.name)
		}
	}
	if len(readers) > 0 {
		return invalidCode(CodeDimensionInUse,
			"dimension %s is read by the formulas of %s; change those formulas first, then delete the dimension",
			dimName, strings.Join(readers, ", "))
	}
	return nil
}

// QueryExecer is a transaction: RenamePropertyInFormulas reads and writes.
type QueryExecer interface {
	Querier
	Execer
}

// RenamePropertyInFormulas carries a property rename into the formulas that
// read it: every dim.oldName in a calculated metric of the dimension's
// revision becomes dim.newName (formula.RenameProperty — only the property
// token changes, never anything else in the text). It runs in the rename's
// own transaction, after the declaration is renamed. Property references
// create no calc_dependency rows, so there are no edges to rewrite; the
// caller's recalculation of the dimension's dependents recomputes them. It
// returns the IDs of the metrics it rewrote.
func RenamePropertyInFormulas(ctx context.Context, tx QueryExecer, dimensionID, oldName, newName string) ([]string, error) {
	if strings.EqualFold(oldName, newName) {
		return nil, nil // formulas read property names regardless of case
	}
	var modelID, revisionID, dimName string
	if err := tx.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text, ''), name
		FROM model.dimension_def WHERE id=$1::uuid
	`, dimensionID).Scan(&modelID, &revisionID, &dimName); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT id::text, formula FROM model.metric_def m
		WHERE model_id=$1::uuid AND is_input = false AND formula IS NOT NULL AND formula <> ''
		  AND ($2 = '' OR revision_id = NULLIF($2,'')::uuid)
		  AND `+resolvesToDimension+`
		FOR UPDATE
	`, modelID, revisionID, dimName)
	if err != nil {
		return nil, err
	}
	type row struct{ id, text string }
	metrics, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.text)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	var rewritten []string
	for _, m := range metrics {
		next, changed := formula.RenameProperty(m.text, dimName, oldName, newName)
		if !changed {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE model.metric_def SET formula=$2 WHERE id=$1::uuid`, m.id, next); err != nil {
			return nil, err
		}
		rewritten = append(rewritten, m.id)
	}
	return rewritten, nil
}

// Property declarations (contract C9).

// Stable identifiers of a rejected property declaration.
const (
	CodeInvalidPropertyName = "INVALID_PROPERTY_NAME"
	CodePropertyNameTaken   = "PROPERTY_NAME_TAKEN"
	CodeInvalidPropertyType = "INVALID_PROPERTY_TYPE"
	// CodePropertyInUse refuses a delete while formulas read the property
	// (CheckPropertyNotInUse); the gateway answers it with 409.
	CodePropertyInUse = "PROPERTY_IN_USE"
	// CodeDimensionInUse refuses deleting a dimension while formulas name it
	// (CheckDimensionNotInUse); the gateway answers it with 409.
	CodeDimensionInUse = "DIMENSION_IN_USE"
)

// PropertyDataTypes are the declarable property types; formulas read a
// property typed by its declaration (formula.TypedPropertyValue).
var PropertyDataTypes = []string{"text", "number", "date"}

var propertyNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidPropertyName refuses a property name a formula cannot write as
// dimension.property.
func ValidPropertyName(name string) error {
	if !propertyNamePattern.MatchString(name) {
		return invalidCode(CodeInvalidPropertyName,
			"property name %q is not valid: use letters, digits and underscores, starting with a letter or underscore, so formulas can read it as dimension.%s", name, name)
	}
	return nil
}

// ValidatePropertyDeclaration checks a property declared (propertyID "")
// or changed (propertyID set, excluded from the uniqueness check) on a
// dimension by a developer or the AI assistant: the name must be an
// identifier a formula can write as dimension.property, unique on the
// dimension case-insensitively, and the type one of PropertyDataTypes.
// CSV and connector imports auto-declare any header name and do not call
// this.
func ValidatePropertyDeclaration(ctx context.Context, q Querier, dimensionID, propertyID, name, dataType string) error {
	if err := ValidPropertyName(name); err != nil {
		return err
	}
	valid := false
	for _, t := range PropertyDataTypes {
		if dataType == t {
			valid = true
		}
	}
	if !valid {
		return invalidCode(CodeInvalidPropertyType,
			"property type %q is not valid: use one of %s", dataType, strings.Join(PropertyDataTypes, ", "))
	}
	var taken string
	err := q.QueryRow(ctx, `
		SELECT name FROM model.dimension_property
		WHERE dimension_id=$1::uuid AND lower(name)=lower($2)
		  AND ($3 = '' OR id <> NULLIF($3,'')::uuid)
		LIMIT 1
	`, dimensionID, name, propertyID).Scan(&taken)
	if err == nil {
		return invalidCode(CodePropertyNameTaken,
			"the dimension already has a property named %q; property names are unique regardless of case", taken)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}

// RenamePropertyValues carries a property rename through to the data that
// names it: the key in every member's properties of the dimension (matched
// case-insensitively, as formulas read it) and any dimension grouped by it
// (dimension_def.source_property). Run in the transaction that renames the
// declaration, so no member is left holding a value under the old name.
func RenamePropertyValues(ctx context.Context, tx Execer, dimensionID, oldName, newName string) error {
	if oldName == newName {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE model.dimension_member m
		SET properties = (
		    SELECT COALESCE(jsonb_object_agg(CASE WHEN lower(e.k) = lower($2) THEN $3 ELSE e.k END, e.v), '{}'::jsonb)
		    FROM jsonb_each(m.properties) AS e(k, v)
		)
		WHERE m.dimension_id = $1::uuid
		  AND EXISTS (SELECT 1 FROM jsonb_object_keys(m.properties) AS k WHERE lower(k) = lower($2))
	`, dimensionID, oldName, newName); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE model.dimension_def SET source_property = $3
		WHERE source_dimension_id = $1::uuid AND lower(source_property) = lower($2)
	`, dimensionID, oldName, newName)
	return err
}
