package recordpost

// A load that replaces by a field (2026-11-07): loading a month's file into
// a table again — the corrected file, or the same one twice — appended every
// row again, so the month's tonnes and cash doubled and only a check that
// compared volumes caught it. A loader naming a field (its period) deletes
// the table's records holding a value of it the file holds, in the same
// transaction that creates the file's records, and says how many went.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
)

// ReplaceError is a load refused because a record it replaces may not be
// deleted by whoever loads it: nothing was deleted or created.
type ReplaceError struct{ Msg string }

func (e *ReplaceError) Error() string { return e.Msg }

// IsReplaceRefused reports whether err is a ReplaceError.
func IsReplaceRefused(err error) bool {
	var re *ReplaceError
	return errors.As(err, &re)
}

// replaceTypes are the field types a load may replace by: values that name
// a slice of the table (a period, a site, a ledger), not amounts.
var replaceTypes = map[string]bool{"dimension": true, "select": true, "text": true, "date": true}

// ReplaceField is the field of form named name (or labelled so) that a load
// may replace by; an error says why it may not be.
func ReplaceField(form *crudapp.FormDef, name string) (*crudapp.FormField, error) {
	want := strings.ToLower(strings.TrimSpace(name))
	for i := range form.Fields {
		f := &form.Fields[i]
		if strings.ToLower(f.Name) != want && strings.ToLower(f.Label) != want {
			continue
		}
		if !replaceTypes[f.Type] {
			return nil, fmt.Errorf("replace_by: %s is a %s field; a load replaces by a dimension, choice, text or date field", firstNonEmpty(f.Label, f.Name), f.Type)
		}
		if form.KeepsDimension() {
			return nil, fmt.Errorf("replace_by: the records of %s are members of a dimension, which a load adds and updates by their code; it cannot replace them by a field", form.Label)
		}
		return f, nil
	}
	return nil, fmt.Errorf("replace_by: %s has no field %q", form.Label, name)
}

// replaceKey is the value a record holding v in field f is replaced by: a
// date's month (a file of July's postings replaces July, whatever days it
// holds), any other field's value as stored.
func replaceKey(f *crudapp.FormField, v any) string {
	s := strings.TrimSpace(fmt.Sprint(v))
	if v == nil {
		s = ""
	}
	if f.Type == "date" && len(s) >= 7 {
		return s[:7]
	}
	return s
}

// replaceCondition is the SQL that picks the records of a field holding one
// of the keys ($2): a date by its month.
func replaceCondition(f *crudapp.FormField) string {
	if f.Type == "date" {
		return `left(data->>$3, 7) = ANY($2::text[])`
	}
	return `data->>$3 = ANY($2::text[])`
}

type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// recordsToReplace reads the records of form whose field f holds one of
// keys — locked for the transaction q is, when lock.
func recordsToReplace(ctx context.Context, q rowQuerier, form *crudapp.FormDef, f *crudapp.FormField, keys []string, lock bool) ([]*crudapp.FormRecord, error) {
	sql := `SELECT id::text, data, COALESCE(status::text,''), created_by::text, updated_at
		FROM runtime.form_record WHERE form_id = $1::uuid AND ` + replaceCondition(f)
	if lock {
		sql += ` FOR UPDATE`
	}
	rows, err := q.Query(ctx, sql, form.ID, keys, f.Name)
	if err != nil {
		return nil, fmt.Errorf("read the records the load replaces: %w", err)
	}
	defer rows.Close()
	var out []*crudapp.FormRecord
	for rows.Next() {
		r := &crudapp.FormRecord{FormID: form.ID}
		var raw []byte
		if err := rows.Scan(&r.ID, &raw, &r.Status, &r.CreatedBy, &r.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &r.Data)
		out = append(out, r)
	}
	return out, rows.Err()
}

// replaceRefusal says why the load may not delete recs ("" when it may):
// each must be one req's creator may delete (req.DeleteRefusal), and what it
// posted must come out of cells they may write — a closed month's records
// stay, whoever loads its file again.
func (ps *Poster) replaceRefusal(ctx context.Context, req ImportRequest, tp *tablePosting, f *crudapp.FormField, recs []*crudapp.FormRecord) (string, error) {
	for _, rec := range recs {
		what := fmt.Sprintf("a record of %s %s", firstNonEmpty(f.Label, f.Name), replaceKey(f, rec.Data[f.Name]))
		if req.DeleteRefusal != nil {
			if msg := req.DeleteRefusal(rec); msg != "" {
				return fmt.Sprintf("the load would replace %s: %s; nothing was loaded", what, msg), nil
			}
		}
		if req.CreatorID == "" || tp == nil {
			continue
		}
		code, msg, err := ps.postingRefusal(ctx, tp, req.CreatorID, req.ByPerson, rec.ID, nil)
		if err != nil {
			return "", err
		}
		if code != 0 {
			return fmt.Sprintf("the load would replace %s: %s; nothing was loaded", what, msg), nil
		}
	}
	return "", nil
}
