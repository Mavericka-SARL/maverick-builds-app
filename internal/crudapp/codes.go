package crudapp

// Auto-code fields: a code the server gives each record of a table — the
// field's prefix and the next number, EXP-0042 — so records without an
// identifier of their own can be told apart and referred to. Numbers come
// from runtime.record_code_counter, one row per table and field, so a
// deleted record's code is never given out again. A table that has no
// counter yet (a new field, or a table copied into a new revision or
// another model) starts after the highest code its records already hold.

import (
	"context"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// Auto-code bounds: a prefix of at most maxCodePrefix characters, numbers
// padded to between 1 and maxCodeDigits digits (defaultCodeDigits when the
// field leaves it at 0).
const (
	defaultCodeDigits = 4
	maxCodeDigits     = 12
	maxCodePrefix     = 20
)

// codeDigits is the number of digits f's codes are padded to.
func codeDigits(f FormField) int {
	if f.CodeDigits <= 0 {
		return defaultCodeDigits
	}
	return f.CodeDigits
}

// FormatCode is the code number n of auto-code field f.
func FormatCode(f FormField, n int64) string {
	return fmt.Sprintf("%s%0*d", f.CodePrefix, codeDigits(f), n)
}

// nextCode takes the next number of formID's auto-code field f and returns
// its code. The counter row stays locked until db's transaction ends, so
// two records saved at once never get the same number.
func nextCode(ctx context.Context, db dbx.DB, formID string, f FormField) (string, error) {
	var n int64
	err := db.QueryRow(ctx, `
		INSERT INTO runtime.record_code_counter AS c (form_id, field, last_value)
		VALUES ($1::uuid, $2, COALESCE((
			SELECT max(substr(r.data->>$2, length($3) + 1)::bigint)
			FROM runtime.form_record r
			WHERE r.form_id = $1::uuid
			  AND left(r.data->>$2, length($3)) = $3
			  AND substr(r.data->>$2, length($3) + 1) ~ '^[0-9]{1,18}$'), 0) + 1)
		ON CONFLICT (form_id, field) DO UPDATE SET last_value = c.last_value + 1
		RETURNING last_value`, formID, f.Name, f.CodePrefix).Scan(&n)
	if err != nil {
		return "", fmt.Errorf("next code of %s: %w", f.Label, err)
	}
	return FormatCode(f, n), nil
}

// assignMissingCodes gives every record of formID without a value in
// auto-code field f a code, oldest first, and returns how many it gave.
func assignMissingCodes(ctx context.Context, db dbx.DB, formID string, f FormField) (int, error) {
	rows, err := db.Query(ctx, `
		SELECT id::text FROM runtime.form_record
		WHERE form_id = $1::uuid AND COALESCE(btrim(data->>$2), '') = ''
		ORDER BY created_at, id`, formID, f.Name)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		code, err := nextCode(ctx, db, formID, f)
		if err != nil {
			return 0, err
		}
		if _, err := db.Exec(ctx,
			`UPDATE runtime.form_record SET data = data || jsonb_build_object($2::text, $3::text) WHERE id = $1::uuid`,
			id, f.Name, code); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// KeepServerValues returns data with the values the server owns put back
// from the record's current data: an auto-code field keeps its code, and a
// status field holds nothing (the status is the record's own). A person
// saving a record changes neither.
func KeepServerValues(form *FormDef, current, data map[string]any) map[string]any {
	out := make(map[string]any, len(data))
	for k, v := range data {
		out[k] = v
	}
	for _, f := range form.Fields {
		switch f.Type {
		case FieldAutoCode:
			if v, ok := current[f.Name]; ok {
				out[f.Name] = v
			} else {
				delete(out, f.Name)
			}
		case FieldStatus:
			delete(out, f.Name)
		}
	}
	return out
}

// fieldTypes are the field types a table may have.
var fieldTypes = map[string]bool{
	"text": true, "number": true, "select": true, "date": true, "boolean": true,
	"dimension": true, "metric": true, FieldAutoCode: true, FieldStatus: true,
}

// ValidateFields says what is wrong with a table's fields, nil when
// nothing is: every field has a name of its own and a known type, a table
// has at most one status field, and an auto-code field's prefix and digits
// are within bounds.
func ValidateFields(fields []FormField) error {
	seen := map[string]bool{}
	statuses := 0
	for i, f := range fields {
		name := strings.TrimSpace(f.Name)
		if name == "" {
			return fmt.Errorf("field %d has no name", i+1)
		}
		if seen[name] {
			return fmt.Errorf("two fields are named %q: a field's name must be its own", name)
		}
		seen[name] = true
		if !fieldTypes[f.Type] {
			return fmt.Errorf("field %q has type %q, which is not a field type (text, number, select, date, boolean, dimension, metric, autocode, status)", name, f.Type)
		}
		switch f.Type {
		case FieldStatus:
			statuses++
			if statuses > 1 {
				return fmt.Errorf("a table has at most one status field")
			}
		case FieldAutoCode:
			if len([]rune(f.CodePrefix)) > maxCodePrefix {
				return fmt.Errorf("the code prefix of %q is longer than %d characters", name, maxCodePrefix)
			}
			if f.CodeDigits < 0 || f.CodeDigits > maxCodeDigits {
				return fmt.Errorf("the code digits of %q must be between 1 and %d", name, maxCodeDigits)
			}
		}
	}
	return nil
}
