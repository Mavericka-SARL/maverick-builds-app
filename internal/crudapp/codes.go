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
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

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
// saving a record changes neither. A hidden field the save does not name
// keeps its value: no form shows it, so no form sends it back (a save
// naming it, null included, sets it).
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
		default:
			if _, sent := data[f.Name]; f.Hidden && !sent {
				if v, ok := current[f.Name]; ok {
					out[f.Name] = v
				}
			}
		}
	}
	return out
}

// fieldTypes are the field types a table may have.
var fieldTypes = map[string]bool{
	"text": true, "number": true, "select": true, "date": true, "boolean": true,
	"dimension": true, "metric": true, FieldAutoCode: true, FieldStatus: true,
	FieldURL: true, FieldLongText: true,
}

// Field types holding text the person types: one line, a link, or
// paragraphs.
const (
	FieldURL      = "url"
	FieldLongText = "longtext"
)

// IsTextType reports whether a field of type t holds free text.
func IsTextType(t string) bool { return t == "text" || t == FieldURL || t == FieldLongText }

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
			return fmt.Errorf("field %q has type %q, which is not a field type (text, longtext, url, number, select, date, boolean, dimension, metric, autocode, status)", name, f.Type)
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
		if err := validateFieldOptions(f, fields); err != nil {
			return fmt.Errorf("field %q: %w", name, err)
		}
	}
	return nil
}

// validateFieldOptions checks f's options against its type and the
// table's other fields.
func validateFieldOptions(f FormField, fields []FormField) error {
	byName := func(n string) *FormField {
		for i := range fields {
			if fields[i].Name == n {
				return &fields[i]
			}
		}
		return nil
	}
	serverOwned := f.Type == FieldAutoCode || f.Type == FieldStatus
	if f.Default != nil {
		if serverOwned || f.Type == "metric" {
			return fmt.Errorf("a %s field has no default: its value is not the person's to give", f.Type)
		}
		if err := checkDefault(f); err != nil {
			return err
		}
	}
	if f.RequiredWith != "" {
		other := byName(f.RequiredWith)
		switch {
		case other == nil:
			return fmt.Errorf("required_with names %q, which is not a field of the table", f.RequiredWith)
		case other.Name == f.Name:
			return fmt.Errorf("required_with names the field itself")
		case serverOwned || other.Type == FieldAutoCode || other.Type == FieldStatus:
			return fmt.Errorf("required_with pairs fields the person fills in, not auto-code or status fields")
		}
	}
	if f.History && serverOwned {
		return fmt.Errorf("a %s field keeps no history of its own", f.Type)
	}
	if f.Hidden && f.Required && f.Default == nil {
		return fmt.Errorf("a hidden field nobody sees cannot be required without a default")
	}
	if f.OnDelete != "" {
		if f.Type != "dimension" {
			return fmt.Errorf("on_delete is a dimension field's rule")
		}
		switch f.OnDelete {
		case OnDeleteBlock, OnDeleteCascade:
		case OnDeleteClear:
			if f.Required {
				return fmt.Errorf("a required field cannot be cleared: its on_delete is %q or %q", OnDeleteBlock, OnDeleteCascade)
			}
		default:
			return fmt.Errorf("on_delete is %q, %q or %q, not %q", OnDeleteBlock, OnDeleteCascade, OnDeleteClear, f.OnDelete)
		}
	}
	if f.Format != "" && (f.Type != "number" || f.Format != "money") {
		return fmt.Errorf("format %q: a number field may be shown as \"money\"; other fields have no format", f.Format)
	}
	if f.Deadline && f.Type != "date" {
		return fmt.Errorf("a deadline is a date field")
	}
	if f.DoneField != "" {
		if !f.Deadline {
			return fmt.Errorf("done_field belongs to a deadline")
		}
		if other := byName(f.DoneField); other == nil || other.Name == f.Name {
			return fmt.Errorf("done_field names %q, which is not another field of the table", f.DoneField)
		}
	}
	if len(f.DoneValues) > 0 && f.DoneField == "" {
		return fmt.Errorf("done_values belong to a done_field")
	}
	return nil
}

// checkDefault checks f's default is a value of its type.
func checkDefault(f FormField) error {
	switch f.Type {
	case "number":
		if _, err := numberValue(f.Default); err != nil {
			return fmt.Errorf("default: %v", err)
		}
	case "boolean":
		switch t := f.Default.(type) {
		case bool:
		case string:
			if _, err := strconv.ParseBool(strings.TrimSpace(t)); err != nil {
				return fmt.Errorf("default is true or false, not %q", t)
			}
		default:
			return fmt.Errorf("default is true or false")
		}
	case "date":
		s, _ := textValue(f.Default)
		if s != DefaultToday {
			if _, err := time.Parse("2006-01-02", s); err != nil {
				return fmt.Errorf("default is a date (YYYY-MM-DD) or %q, not %q", DefaultToday, s)
			}
		}
	case "select":
		s, err := textValue(f.Default)
		if err != nil {
			return fmt.Errorf("default: %v", err)
		}
		if len(f.Options) > 0 && !slices.Contains(f.Options, s) {
			return fmt.Errorf("default %q is not one of its choices", s)
		}
	default:
		if _, err := textValue(f.Default); err != nil {
			return fmt.Errorf("default: %v", err)
		}
	}
	return nil
}

// CheckFieldChange says what is wrong with giving before, a table as it is
// saved, the fields fields: a core field may not be removed, and a table
// keeping a dimension keeps its auto-code field and the field labelling
// its members.
func CheckFieldChange(before *FormDef, fields []FormField) error {
	has := map[string]FormField{}
	for _, f := range fields {
		has[f.Name] = f
	}
	for _, f := range before.Fields {
		if _, ok := has[f.Name]; f.Core && !ok {
			return fmt.Errorf("%q is a core field of the table: it cannot be removed (unmark it as core first)", firstNonEmpty(f.Label, f.Name))
		}
	}
	if before.KeepsDimension() {
		if f, ok := has[before.CodeField().Name]; !ok || f.Type != FieldAutoCode {
			return fmt.Errorf("the table keeps a dimension: its auto-code field %q holds the members' codes and stays", before.CodeField().Name)
		}
		if f, ok := has[before.MemberLabelField]; before.MemberLabelField != "" && (!ok || !IsTextType(f.Type) && f.Type != "select") {
			return fmt.Errorf("the table keeps a dimension: its field %q labels the members and stays a text field", before.MemberLabelField)
		}
	}
	return nil
}

// NormalizeURL is s as a link field stores it: an absolute http(s) or
// mailto link, "https://" put before a bare address (example.com/a). False
// when s is no link.
func NormalizeURL(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t\n") {
		return "", false
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "mailto:") {
		return s, strings.Contains(s, "@")
	}
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		if strings.Contains(s, "://") {
			return "", false
		}
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || !strings.Contains(u.Host, ".") && u.Hostname() != "localhost" {
		return "", false
	}
	return s, true
}

// DefaultToday is a date field's default meaning the day the record is
// created.
const DefaultToday = "today"

// ApplyDefaults gives data each field's default that data holds no value
// for: a new record's values. today is the creating day (YYYY-MM-DD).
func ApplyDefaults(form *FormDef, data map[string]any, today string) {
	for _, f := range form.Fields {
		if f.Default == nil || f.Type == FieldAutoCode || f.Type == FieldStatus {
			continue
		}
		if v, ok := data[f.Name]; ok && v != nil {
			if s, isStr := v.(string); !isStr || strings.TrimSpace(s) != "" {
				continue
			}
		}
		if s, ok := f.Default.(string); ok && f.Type == "date" && s == DefaultToday {
			data[f.Name] = today
			continue
		}
		data[f.Name] = f.Default
	}
}
