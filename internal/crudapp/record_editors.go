package crudapp

import (
	"context"
	"fmt"
	"strings"
)

// SetEditorField sets who changes each record of a table that keeps a
// dimension (migration 200): field names a person field of the table, and
// to a holder of one of roles the members whose record names someone else
// there are read-only — they change their own and read the rest. field ""
// clears it (roles with it). The developer API and the AI Developer set it
// alike.
func (s *Store) SetEditorField(ctx context.Context, formID, field string, roles []string) error {
	field = strings.TrimSpace(field)
	clean := make([]string, 0, len(roles))
	for _, r := range roles {
		if r = strings.TrimSpace(r); r != "" {
			clean = append(clean, r)
		}
	}
	if field == "" {
		_, err := s.db.Exec(ctx, `UPDATE model.form_def SET editor_field = '', editor_roles = '{}' WHERE id = $1::uuid`, formID)
		return err
	}
	form, err := s.GetForm(ctx, formID)
	if err != nil {
		return err
	}
	if !form.KeepsDimension() {
		return &MappingRefError{msg: fmt.Sprintf("editor_field: %s keeps no dimension — the person a record names changes its member, so the table must keep one", form.Label)}
	}
	var persons []string
	found := false
	for _, f := range form.Fields {
		if f.Type == FieldPerson {
			persons = append(persons, f.Name)
			if strings.EqualFold(f.Name, field) {
				field, found = f.Name, true
			}
		}
	}
	if !found {
		if len(persons) == 0 {
			return &MappingRefError{msg: fmt.Sprintf("editor_field: %s has no person field — add one (the project's manager) first", form.Label)}
		}
		return &MappingRefError{msg: fmt.Sprintf("editor_field: %q is no person field of %s (its person fields: %s)", field, form.Label, strings.Join(persons, ", "))}
	}
	if len(clean) == 0 {
		return &MappingRefError{msg: "editor_roles: name the business roles whose members change only their own records (project managers) — everyone else keeps the access they have"}
	}
	if err := CheckEditorRoles(ctx, s.db, form.KeepsDimensionID, clean); err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `UPDATE model.form_def SET editor_field = $2, editor_roles = $3 WHERE id = $1::uuid`, formID, field, clean)
	return err
}
