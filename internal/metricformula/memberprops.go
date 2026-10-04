package metricformula

import (
	"context"
	"strings"
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
// was deleted. CSV and connector imports declare their headers themselves
// and do not call this.
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
		if name, ok := declared[strings.ToLower(strings.TrimSpace(key))]; ok {
			out[name] = value
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

// declaredProperties maps each declared property's lower-case name to its
// declared spelling.
func declaredProperties(ctx context.Context, q Querier, dimensionID string) (map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT name FROM model.dimension_property WHERE dimension_id=$1::uuid`, dimensionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	declared := map[string]string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		declared[strings.ToLower(name)] = name
	}
	return declared, rows.Err()
}

// identifierFor suggests an identifier for a name that is not one:
// "P&L Line" → "p_l_line".
func identifierFor(name string) string {
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
	s := strings.TrimSuffix(b.String(), "_")
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		s = "p_" + s
	}
	return s
}
