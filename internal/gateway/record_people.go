package gateway

// People in records (2026-11-20, help desk): a person field names someone
// who reaches the table's application, and one with notify tells them in
// the console's notifications when a save names them. GET
// /api/forms/{id}/choices serves what a record form offers for the table's
// person fields (names only: owner decision 2026-11-20) and the members its
// dimension fields no longer offer (inactive).

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/notification"
)

// recordPersonNamedTemplate is the notification a person field with
// notify sends the person a save names.
const recordPersonNamedTemplate = "record_person_named"

// recordChoices serves GET /api/forms/{id}/choices: for each person field,
// the people it may name; the names of everyone the table's records name
// (so a list shows a person no longer offered); and for each dimension
// field, the members it no longer offers (inactive). Empty for a table
// with neither.
func (h *handler) recordChoices(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	scope, err := h.resolveFormRecordScope(ctx, act, r.PathValue("id"))
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !scope.reach {
		jsonRecordErr(w, errRecordNotReached, "table")
		return
	}
	people := map[string]any{}
	inactive := map[string][]string{}
	named := map[string]string{}
	var personFields []string
	for _, f := range scope.form.Fields {
		switch {
		case f.Type == crudapp.FieldPerson:
			list, err := h.records().ChoosablePeople(ctx, scope.appID, f.Roles)
			if err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			people[f.Name] = list
			for _, p := range list {
				named[p.ID] = p.Name
			}
			personFields = append(personFields, f.Name)
		case f.Type == "dimension" && f.DimensionID != "" && scope.form.ModelKeeps:
			codes, err := h.records().InactiveCodes(ctx, scope.form, f.DimensionID)
			if err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if len(codes) > 0 {
				list := make([]string, 0, len(codes))
				for c := range codes {
					list = append(list, c)
				}
				inactive[f.Name] = list
			}
		}
	}
	if len(personFields) > 0 {
		ids, err := h.personFieldValues(ctx, scope.form.ID, personFields)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		var missing []string
		for _, id := range ids {
			if _, ok := named[id]; !ok {
				missing = append(missing, id)
			}
		}
		more, err := h.records().PeopleNamed(ctx, missing)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		for id, n := range more {
			named[id] = n
		}
	}
	jsonOK(w, map[string]any{"people": people, "named": named, "inactive": inactive})
}

// personFieldValues are the distinct people the records of table formID
// name in fields.
func (h *handler) personFieldValues(ctx context.Context, formID string, fields []string) ([]string, error) {
	rows, err := h.db.Query(ctx, `
		SELECT DISTINCT btrim(r.data->>f) FROM runtime.form_record r, unnest($2::text[]) f
		WHERE r.form_id = $1::uuid AND COALESCE(btrim(r.data->>f), '') <> ''`, formID, fields)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// notifyNamedPeople tells each person a save of a record of the scope's
// table newly names in a person field with notify — or names through a
// dimension field with notify, as the person of the record it names — not
// the person saving, in the console's notifications. before is the record's data before the
// save (nil for a new record). Best effort: a notification that cannot be
// written is logged and the save stands.
func (h *handler) notifyNamedPeople(ctx context.Context, scope formRecordScope, recordID string, before, after map[string]any, actorID string) {
	if scope.form == nil {
		return
	}
	var actorName string
	for _, f := range scope.form.Fields {
		if !f.Notify || (f.Type != crudapp.FieldPerson && (f.Type != "dimension" || f.DimensionID == "")) {
			continue
		}
		to := crudapp.ValueText(after[f.Name])
		if to == "" || strings.EqualFold(to, crudapp.ValueText(before[f.Name])) {
			continue
		}
		// A dimension field names a record (an agent); its person is told.
		if f.Type == "dimension" {
			person, err := h.records().PersonOfMember(ctx, scope.form, f.DimensionID, to)
			if err != nil {
				h.log.Warn().Err(err).Str("record", recordID).Str("field", f.Name).Msg("finding who a record names failed")
				continue
			}
			to = person
		}
		if to == "" || to == actorID {
			continue
		}
		if actorName == "" {
			names, err := h.records().PeopleNamed(ctx, []string{actorID})
			if err != nil || names[actorID] == "" {
				actorName = "Someone"
			} else {
				actorName = names[actorID]
			}
		}
		table := firstNonEmptyString(scope.form.Label, scope.form.Name)
		what := recordTitle(scope.form, after)
		vars := map[string]string{
			"subject":    fmt.Sprintf("%s: you are its %s", what, firstNonEmptyString(f.Label, f.Name)),
			"message":    fmt.Sprintf("%s named you %s of %s in %s.", actorName, firstNonEmptyString(f.Label, f.Name), what, table),
			"table_name": table,
			"field":      f.Name,
		}
		if _, err := notification.NewStore(h.db.For(ctx)).Notify(ctx, to, recordPersonNamedTemplate, vars, "form_record", recordID); err != nil && err != notification.ErrRecipientDisabled {
			h.log.Warn().Err(err).Str("record", recordID).Str("field", f.Name).Msg("telling a person a record names them failed")
		}
	}
}

// recordTitle names a record for a sentence: its code and the first text
// field holding a value ("TKT-000004 · Printer offline").
func recordTitle(form *crudapp.FormDef, data map[string]any) string {
	var parts []string
	if cf := form.CodeField(); cf != nil {
		if c := crudapp.ValueText(data[cf.Name]); c != "" {
			parts = append(parts, c)
		}
	}
	for _, f := range form.Fields {
		if f.Type == "text" && !f.Code {
			if t := crudapp.ValueText(data[f.Name]); t != "" {
				parts = append(parts, t)
				break
			}
		}
	}
	if len(parts) == 0 {
		return "a record"
	}
	return strings.Join(parts, " · ")
}

func firstNonEmptyString(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
