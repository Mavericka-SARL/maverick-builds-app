package metricformula

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// CheckMemberProperties checks the property values a developer or the AI
// assistant writes on a member and returns them keyed as they will be
// stored. A key matching a declared property in another case is stored under
// the declared name. A key no declaration names is kept when it could be
// declared later (an identifier), and refused when it never could: a value
// under "P&L_Line" is stored but no formula can ever read it — the assistant
// wrote that on every cost center, declared "p_and_l_line", and SUMIFS read
// nothing. A key the member already stores (stored; nil for a new member)
// passes as it is: the console's member edit sends back every key it shows,
// including values imported under a header or kept after their declaration
// was deleted. A new or changed value of a number or date property must
// read as one (PropertyValueError); a value the member already holds passes
// unchanged, so an old bad value does not block a label edit. CSV and
// connector imports resolve their headers with DeclareImportedProperties and
// do not call this.
func CheckMemberProperties(ctx context.Context, q Querier, dimensionID string, props, stored map[string]string) (map[string]string, error) {
	if len(props) == 0 {
		return props, nil
	}
	declared, err := declaredProperties(ctx, q, dimensionID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(props))
	for key, value := range props {
		if d, ok := declared[strings.ToLower(strings.TrimSpace(key))]; ok {
			if old, kept := stored[key]; !kept || old != value {
				if old, kept := stored[d.name]; !kept || old != value {
					if err := PropertyValueError(d.name, d.dataType, value); err != nil {
						return nil, err
					}
				}
			}
			out[d.name] = value
			continue
		}
		if _, kept := stored[key]; kept {
			out[key] = value
			continue
		}
		key = strings.TrimSpace(key)
		if err := CheckPropertyKey(key); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, nil
}

// CheckPropertyKey refuses a member property key no declaration could ever
// name, so no formula could read its values.
func CheckPropertyKey(key string) error {
	if !propertyNamePattern.MatchString(key) {
		return invalidCode(CodeInvalidPropertyName,
			"property %q could never be read by a formula: name it with letters, digits and underscores, starting with a letter or underscore (for example %q), and declare it under that same name",
			key, identifierFor(key))
	}
	return nil
}

// AdoptPropertyValues runs after a property is declared on a dimension: a
// member value stored under the same name in another case moves under the
// declared spelling. It returns how many members now have a value under the
// name, and — when none has — the undeclared keys the members do carry, so
// the caller can say the declaration and the values do not meet.
func AdoptPropertyValues(ctx context.Context, q Querier, dimensionID, name string) (withValue int, undeclared []string, err error) {
	rows, err := q.Query(ctx, `
		UPDATE model.dimension_member m
		SET properties = (m.properties - k.key) || jsonb_build_object($2::text, m.properties -> k.key)
		FROM (SELECT DISTINCT mm.id, e.key FROM model.dimension_member mm, jsonb_object_keys(mm.properties) AS e(key)
		      WHERE mm.dimension_id = $1::uuid AND lower(e.key) = lower($2) AND e.key <> $2) k
		WHERE m.id = k.id AND NOT (m.properties ? $2)
		RETURNING m.id`, dimensionID, name)
	if err != nil {
		return 0, nil, err
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	if err := q.QueryRow(ctx, `SELECT count(*) FROM model.dimension_member WHERE dimension_id=$1::uuid AND properties ? $2`,
		dimensionID, name).Scan(&withValue); err != nil {
		return 0, nil, err
	}
	if withValue > 0 {
		return withValue, nil, nil
	}
	declared, err := declaredProperties(ctx, q, dimensionID)
	if err != nil {
		return 0, nil, err
	}
	keyRows, err := q.Query(ctx, `
		SELECT DISTINCT e.key FROM model.dimension_member m, jsonb_object_keys(m.properties) AS e(key)
		WHERE m.dimension_id = $1::uuid ORDER BY 1`, dimensionID)
	if err != nil {
		return 0, nil, err
	}
	defer keyRows.Close()
	for keyRows.Next() {
		var k string
		if err := keyRows.Scan(&k); err != nil {
			return 0, nil, err
		}
		if _, ok := declared[strings.ToLower(k)]; !ok {
			undeclared = append(undeclared, k)
		}
	}
	return 0, undeclared, keyRows.Err()
}

// declaredProperty is a property declaration: its spelling and data type.
type declaredProperty struct{ name, dataType string }

// declaredProperties maps each declared property's lower-case name to its
// declaration.
func declaredProperties(ctx context.Context, q Querier, dimensionID string) (map[string]declaredProperty, error) {
	rows, err := q.Query(ctx, `SELECT name, data_type FROM model.dimension_property WHERE dimension_id=$1::uuid`, dimensionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	declared := map[string]declaredProperty{}
	for rows.Next() {
		var d declaredProperty
		if err := rows.Scan(&d.name, &d.dataType); err != nil {
			return nil, err
		}
		declared[strings.ToLower(d.name)] = d
	}
	return declared, rows.Err()
}

// PropertyKey is the name a property compares by: lower case, each run of
// characters other than ASCII letters and digits one underscore, none at
// either end — "Sales Segment", "sales_segment" and "SALES-SEGMENT" share
// sales_segment. It is "" for a name with no letter or digit.
func PropertyKey(name string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastUnderscore = false
		} else if !lastUnderscore && b.Len() > 0 {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

// identifierFor suggests an identifier for a name that is not one:
// "P&L Line" → "p_l_line", "2nd tier" → "p_2nd_tier".
func identifierFor(name string) string {
	s := PropertyKey(name)
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		s = "p_" + s
	}
	return s
}

// PropertyValueError refuses a member value a property of dataType cannot
// read (formula.TypedPropertyValue, which formulas use): not a number for a
// number property, not a yyyy-mm-dd date for a date property. A blank is
// no value and passes; a text property takes anything.
func PropertyValueError(name, dataType, value string) error {
	if !formula.TypedPropertyValue(value, dataType).IsError() {
		return nil
	}
	return invalidCode(CodeInvalidPropertyValue, "property %q is a %s property: %q is not %s", name, dataType, value, propertyTypeWant(dataType))
}

func propertyTypeWant(dataType string) string {
	if strings.EqualFold(strings.TrimSpace(dataType), "date") {
		return "a yyyy-mm-dd date"
	}
	return "a number"
}

// CheckPropertyValues refuses giving a dimension's property the type
// dataType while a member holds a value under it (any case) that the type
// cannot read; the error names up to five of those members.
func CheckPropertyValues(ctx context.Context, q Querier, dimensionID, name, dataType string) error {
	rows, err := q.Query(ctx, `
		SELECT m.code, e.value
		FROM model.dimension_member m, jsonb_each_text(COALESCE(m.properties, '{}'::jsonb)) AS e(key, value)
		WHERE m.dimension_id = $1::uuid AND lower(e.key) = lower($2)
		ORDER BY m.sort_order, m.code`, dimensionID, name)
	if err != nil {
		return err
	}
	defer rows.Close()
	var bad []string
	count := 0
	for rows.Next() {
		var code, value string
		if err := rows.Scan(&code, &value); err != nil {
			return err
		}
		if PropertyValueError(name, dataType, value) == nil {
			continue
		}
		count++
		if len(bad) < 5 {
			bad = append(bad, fmt.Sprintf("%s (%q)", code, value))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	more := ""
	if count > len(bad) {
		more = fmt.Sprintf(" and %d more", count-len(bad))
	}
	return invalidCode(CodeInvalidPropertyValue,
		"property %q cannot become a %s property: %d member(s) hold a value that is not %s — %s%s; change or clear those values first",
		name, dataType, count, propertyTypeWant(dataType), strings.Join(bad, ", "), more)
}

// ImportedPropertyNames resolves the property columns of a CSV member
// import or a REST connector's dimension pull — the text after "property:"
// in each header — to the property each one fills, keyed by the header as
// given. A header lands on the declaration of the same name whatever its
// case, or on the declaration of its identifier ("Sales Segment" fills a
// declared sales_segment); any other header is a new property named by its
// identifier (the owner's decision, 2026-10-06), returned in toDeclare in
// header order. A header with no letter or digit, or two headers filling
// one property, refuse the whole file. Imports used to declare each header
// as written: "Segment" next to a developer's "segment", whose values
// formulas then never saw, and "Sales Segment", which no formula can name.
func ImportedPropertyNames(ctx context.Context, q Querier, dimensionID string, headers []string) (names map[string]string, toDeclare []string, err error) {
	declared, err := declaredProperties(ctx, q, dimensionID)
	if err != nil {
		return nil, nil, err
	}
	names = make(map[string]string, len(headers))
	filledBy := map[string]string{}
	for _, h := range headers {
		header := strings.TrimSpace(h)
		if header == "" {
			continue
		}
		if PropertyKey(header) == "" {
			return nil, nil, invalidCode(CodeInvalidPropertyName,
				"column property:%s has no letter or digit, so no property can be named after it — rename the column", header)
		}
		d, ok := declared[strings.ToLower(header)]
		if !ok {
			d, ok = declared[identifierFor(header)]
		}
		name := d.name
		if !ok {
			name = identifierFor(header)
		}
		if prev, dup := filledBy[strings.ToLower(name)]; dup {
			return nil, nil, invalid("columns property:%s and property:%s both fill property %q — a file fills each property from one column", prev, header, name)
		}
		filledBy[strings.ToLower(name)] = header
		names[h] = name
		if !ok {
			toDeclare = append(toDeclare, name)
		}
	}
	return names, toDeclare, nil
}

// DeclareImportedProperties resolves an import's property headers
// (ImportedPropertyNames) and declares each new name as a text property,
// moving values members hold under it in another case under the declared
// spelling (AdoptPropertyValues). It returns the names each header fills
// and the names it declared, in header order; a name another writer
// declared meanwhile, in any case, is read back. Run it in the import's
// transaction, before the member writes.
func DeclareImportedProperties(ctx context.Context, q Querier, dimensionID string, headers []string) (names map[string]string, declared []string, err error) {
	names, toDeclare, err := ImportedPropertyNames(ctx, q, dimensionID, headers)
	if err != nil || len(toDeclare) == 0 {
		return names, nil, err
	}
	for _, name := range toDeclare {
		var id string
		err := q.QueryRow(ctx, `
			INSERT INTO model.dimension_property (dimension_id, name, data_type)
			VALUES ($1::uuid, $2, 'text') ON CONFLICT DO NOTHING RETURNING id::text`, dimensionID, name).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("declare property %q: %w", name, err)
		}
		if _, _, err := AdoptPropertyValues(ctx, q, dimensionID, name); err != nil {
			return nil, nil, fmt.Errorf("declare property %q: %w", name, err)
		}
		declared = append(declared, name)
	}
	if len(declared) < len(toDeclare) {
		names, _, err = ImportedPropertyNames(ctx, q, dimensionID, headers)
	}
	return names, declared, err
}

// DeclaredPropertyTypes maps each property declared on a dimension to its
// data type.
func DeclaredPropertyTypes(ctx context.Context, q Querier, dimensionID string) (map[string]string, error) {
	declared, err := declaredProperties(ctx, q, dimensionID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(declared))
	for _, d := range declared {
		out[d.name] = d.dataType
	}
	return out, nil
}

// MergeImportedPropertiesSQL is the properties an imported member row
// stores in an INSERT … ON CONFLICT (dimension_id, code) DO UPDATE: the
// member's stored properties without the keys whose lower case is in the
// text[] parameter param — the lower-cased names the row gives a value —
// then the row's own (EXCLUDED.properties). An imported value thus
// replaces the member's value under the name in any other case, instead of
// sitting next to it.
func MergeImportedPropertiesSQL(param string) string {
	return `(SELECT COALESCE(jsonb_object_agg(e.key, e.value), '{}'::jsonb)
	         FROM jsonb_each(COALESCE(model.dimension_member.properties, '{}'::jsonb)) AS e
	         WHERE lower(e.key) <> ALL(` + param + `::text[])) || EXCLUDED.properties`
}

// ValuesFromBefore counts the members of a dimension holding a value under
// name (any case), leaving out the members whose codes are in rewritten —
// those an import just gave a value under it. A declaration made again
// makes these values readable by formulas again; the answer says how many
// (PreexistingValuesNote) and offers to clear them (ClearPropertyValues).
func ValuesFromBefore(ctx context.Context, q Querier, dimensionID, name string, rewritten []string) (int, error) {
	if rewritten == nil {
		rewritten = []string{}
	}
	var n int
	err := q.QueryRow(ctx, `
		SELECT count(*) FROM model.dimension_member m
		WHERE m.dimension_id = $1::uuid AND m.code <> ALL($3::text[])
		  AND EXISTS (SELECT 1 FROM jsonb_object_keys(COALESCE(m.properties, '{}'::jsonb)) AS k WHERE lower(k) = lower($2))`,
		dimensionID, name, rewritten).Scan(&n)
	return n, err
}

// PreexistingValuesNote is what a property declaration answers when members
// already held values under its name — kept from a declaration deleted
// before, or imported before it was declared: deleting a property leaves
// its values (the owner's decision, 2026-11-09), and declaring the name
// again makes formulas read them. Each surface adds how to clear them.
func PreexistingValuesNote(name string, n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d member(s) already hold values under %q from before; formulas read them now", n, name)
}

// FoldPropertyVariant moves every member value of a dimension stored under
// variant — a spelling of keep differing only in case, whose declaration
// is not kept — under keep, the variant's value winning where a member
// holds both (as migration 170 merged them).
func FoldPropertyVariant(ctx context.Context, tx Execer, dimensionID, keep, variant string) error {
	if keep == variant {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE model.dimension_member
		SET properties = (properties - $3::text) || jsonb_build_object($2::text, properties -> $3::text)
		WHERE dimension_id = $1::uuid AND properties ? $3::text`, dimensionID, keep, variant)
	return err
}

// ClearPropertyValues removes a property's values — under its name in any
// case — from every member of the dimension, and returns how many members
// held one. Only an explicit request clears values: deleting or declaring
// a property never does.
func ClearPropertyValues(ctx context.Context, tx Execer, dimensionID, name string) (int64, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE model.dimension_member m
		SET properties = m.properties - ARRAY(SELECT k FROM jsonb_object_keys(m.properties) AS k WHERE lower(k) = lower($2))
		WHERE m.dimension_id = $1::uuid
		  AND EXISTS (SELECT 1 FROM jsonb_object_keys(COALESCE(m.properties, '{}'::jsonb)) AS k WHERE lower(k) = lower($2))`,
		dimensionID, name)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// nameConflict is PROPERTY_NAME_TAKEN raised by the database rather than by
// ValidatePropertyDeclaration: another change declared or renamed a
// property to the name, in some case, at the same moment, and the unique
// index on (dimension_id, lower(name)) (migration 170) refused the loser.
// The gateway answers it with 409.
type nameConflict struct{ ve *ValidationError }

func (e *nameConflict) Error() string { return e.ve.Error() }
func (e *nameConflict) Unwrap() error { return e.ve }

// PropertyNameConflict turns the database's refusal of a property name
// (a unique violation on model.dimension_property) into PROPERTY_NAME_TAKEN
// naming the property that holds it; any other error comes back as it is.
// q must not be the transaction the refusal aborted.
func PropertyNameConflict(ctx context.Context, q Querier, dimensionID, name string, err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.TableName != "dimension_property" {
		return err
	}
	taken := name
	_ = q.QueryRow(ctx, `SELECT name FROM model.dimension_property WHERE dimension_id=$1::uuid AND lower(name)=lower($2) LIMIT 1`,
		dimensionID, name).Scan(&taken)
	return &nameConflict{ve: &ValidationError{Code: CodePropertyNameTaken, Message: fmt.Sprintf(
		"the dimension already has a property named %q — another change declared it at the same moment; property names are unique regardless of case", taken)}}
}

// IsPropertyNameConflict reports whether err is PropertyNameConflict's.
func IsPropertyNameConflict(err error) bool {
	var nc *nameConflict
	return errors.As(err, &nc)
}
