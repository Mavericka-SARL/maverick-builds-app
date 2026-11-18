package recordpost

// A load that matches by a field (2026-11-17): a table numbering its records
// with an auto-code — a CRM's deals — had no way to refresh them from the
// source system's next extract. Only a typed code matched a row to a record,
// so every row of the extract was added again, and the AI Developer stopped
// rather than duplicate the deals. A load naming a field (the source's deal
// id, or the auto-code itself) updates the record holding a row's value of
// it and adds the rows no record holds.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// matchTypes are the field types a load may match records by: values that
// name one record, not a slice of the table or an amount.
var matchTypes = map[string]bool{"text": true, "number": true, crudapp.FieldAutoCode: true}

// MatchField is the field of form named name (or labelled so) a load may
// match the table's records by; an error, prefixed "match_by:", says why it
// may not be. A table whose records have a typed code matches by that code
// only: a second key could give one record two identities.
func MatchField(form *crudapp.FormDef, name string) (*crudapp.FormField, error) {
	want := strings.ToLower(strings.TrimSpace(name))
	for i := range form.Fields {
		f := &form.Fields[i]
		if strings.ToLower(f.Name) != want && strings.ToLower(f.Label) != want {
			continue
		}
		if !matchTypes[f.Type] {
			return nil, fmt.Errorf("match_by: %s is a %s field; a load matches records by a text, number or auto-code field", firstNonEmpty(f.Label, f.Name), f.Type)
		}
		if tc := form.TypedCodeField(); tc != nil && tc.Name != f.Name {
			return nil, fmt.Errorf("match_by: the records of %s are matched by their code (%s); name that field or leave match_by out", form.Label, firstNonEmpty(tc.Label, tc.Name))
		}
		return f, nil
	}
	return nil, fmt.Errorf("match_by: %s has no field %q", form.Label, name)
}

// matchKey is the value a record holding v in field f is matched by: a
// number as a number (765, 765.0 and "765" are one key), anything else as
// written, trimmed.
func matchKey(f *crudapp.FormField, v any) string {
	if v == nil {
		return ""
	}
	s := strings.TrimSpace(fmt.Sprint(v))
	if f.Type == "number" && s != "" {
		if n, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64); err == nil {
			return strconv.FormatFloat(n, 'f', -1, 64)
		}
	}
	return s
}

// recordMatches is a table's records by the value one field holds.
type recordMatches struct {
	field *crudapp.FormField
	by    map[string][]*crudapp.FormRecord
}

// lookup is the record holding key, or how many do when more than one does.
func (m *recordMatches) lookup(key string) (*crudapp.FormRecord, int) {
	if m == nil || key == "" {
		return nil, 0
	}
	switch recs := m.by[key]; len(recs) {
	case 0:
		return nil, 0
	case 1:
		return recs[0], 0
	default:
		return nil, len(recs)
	}
}

// keepVisible drops the records holding one of keys that visibleIDs does
// not keep: a record the person loading may not see is not there for them
// — a row neither updates it nor learns it exists (a value it holds, or how
// many records hold one).
func (m *recordMatches) keepVisible(ctx context.Context, keys []string, visibleIDs func(context.Context, []string) (map[string]bool, error)) error {
	if m == nil || visibleIDs == nil {
		return nil
	}
	var ids []string
	for _, k := range keys {
		for _, r := range m.by[k] {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	seen, err := visibleIDs(ctx, ids)
	if err != nil {
		return err
	}
	for _, k := range keys {
		recs := m.by[k]
		if len(recs) == 0 {
			continue
		}
		kept := recs[:0]
		for _, r := range recs {
			if seen[r.ID] {
				kept = append(kept, r)
			}
		}
		m.by[k] = kept
	}
	return nil
}

// recordsByMatch reads formID's records by the value field holds.
func recordsByMatch(ctx context.Context, db dbx.DB, formID string, field *crudapp.FormField) (*recordMatches, error) {
	rows, err := db.Query(ctx, `
		SELECT id::text, COALESCE(status::text, ''), data, created_by::text, created_at, updated_at
		FROM runtime.form_record
		WHERE form_id = $1::uuid AND COALESCE(data->>$2, '') <> ''`, formID, field.Name)
	if err != nil {
		return nil, fmt.Errorf("read the table's records by %s: %w", firstNonEmpty(field.Label, field.Name), err)
	}
	defer rows.Close()
	out := &recordMatches{field: field, by: map[string][]*crudapp.FormRecord{}}
	for rows.Next() {
		r := &crudapp.FormRecord{FormID: formID}
		var raw []byte
		if err := rows.Scan(&r.ID, &r.Status, &raw, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &r.Data); err != nil {
			return nil, err
		}
		key := matchKey(field, r.Data[field.Name])
		out.by[key] = append(out.by[key], r)
	}
	return out, rows.Err()
}

// RecordIDsMatching is, for each of values, the ids of form's records
// holding it in field, as a load matches them (matchKey): none when no
// record does, several when several do.
func RecordIDsMatching(ctx context.Context, db dbx.DB, form *crudapp.FormDef, field *crudapp.FormField, values []string) (map[string][]string, error) {
	keys := make(map[string]string, len(values)) // match key → the value given
	for _, v := range values {
		if k := matchKey(field, v); k != "" {
			keys[k] = v
		}
	}
	out := make(map[string][]string, len(values))
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx, `
		SELECT id::text, data->$2
		FROM runtime.form_record
		WHERE form_id = $1::uuid AND COALESCE(data->>$2, '') <> ''
		ORDER BY created_at, id`, form.ID, field.Name)
	if err != nil {
		return nil, fmt.Errorf("read the table's records by %s: %w", firstNonEmpty(field.Label, field.Name), err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if given, ok := keys[matchKey(field, v)]; ok {
			out[given] = append(out[given], id)
		}
	}
	return out, rows.Err()
}
