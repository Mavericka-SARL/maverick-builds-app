package crudapp

// Field history: each value a field with the "history" option took (a deal's
// stage), who gave it and when — runtime.record_field_history, which goes
// with its record. A record's first values count as changes from nothing,
// so its history starts where it began.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// HistoryEntry is one value a field of a record took.
type HistoryEntry struct {
	Field      string `json:"field"`
	FieldLabel string `json:"field_label"`
	// Old and New are the field's values before and after (nil: empty).
	Old any `json:"old"`
	New any `json:"new"`
	// ChangedBy is who changed it, nil when unknown or since deleted.
	ChangedBy     *string   `json:"changed_by"`
	ChangedByName string    `json:"changed_by_name"`
	ChangedAt     time.Time `json:"changed_at"`
}

// HistoryFields are the names of form's fields that keep a history.
func (f *FormDef) HistoryFields() []string {
	var out []string
	for _, fd := range f.Fields {
		if fd.History && fd.Type != FieldStatus && fd.Type != FieldAutoCode {
			out = append(out, fd.Name)
		}
	}
	return out
}

// LogFieldChanges records, for each of form's history fields, a value that
// differs between before (nil for a new record) and after. Nothing is
// written for a table without history fields.
func LogFieldChanges(ctx context.Context, db dbx.DB, form *FormDef, recordID string, before, after map[string]any, userID string) error {
	fields := form.HistoryFields()
	if len(fields) == 0 {
		return nil
	}
	for _, name := range fields {
		oldV, newV := emptyAsNil(before[name]), emptyAsNil(after[name])
		if sameJSON(oldV, newV) {
			continue
		}
		oldJ, err := json.Marshal(oldV)
		if err != nil {
			return err
		}
		newJ, err := json.Marshal(newV)
		if err != nil {
			return err
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO runtime.record_field_history (record_id, field, old_value, new_value, changed_by)
			VALUES ($1::uuid, $2, $3::jsonb, $4::jsonb, NULLIF($5,'')::uuid)`,
			recordID, name, string(oldJ), string(newJ), userID); err != nil {
			return fmt.Errorf("record field history: %w", err)
		}
	}
	return nil
}

// FieldHistory lists recordID's field history, oldest first, labelled by
// form's fields (a field since removed keeps its name).
func (s *Store) FieldHistory(ctx context.Context, form *FormDef, recordID string) ([]HistoryEntry, error) {
	rows, err := s.db.Query(ctx, `
		SELECT h.field, h.old_value, h.new_value, h.changed_by::text,
		       COALESCE(NULLIF(btrim(u.display_name), ''), u.email, ''), h.changed_at
		FROM runtime.record_field_history h
		LEFT JOIN identity.user u ON u.id = h.changed_by
		WHERE h.record_id = $1::uuid
		ORDER BY h.changed_at, h.id`, recordID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryEntry{}
	for rows.Next() {
		var e HistoryEntry
		var oldJ, newJ []byte
		if err := rows.Scan(&e.Field, &oldJ, &newJ, &e.ChangedBy, &e.ChangedByName, &e.ChangedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(oldJ, &e.Old)
		_ = json.Unmarshal(newJ, &e.New)
		e.FieldLabel = e.Field
		if f := form.Field(e.Field); f != nil && f.Label != "" {
			e.FieldLabel = f.Label
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func emptyAsNil(v any) any {
	if s, ok := v.(string); ok && s == "" {
		return nil
	}
	return v
}

func sameJSON(a, b any) bool {
	aj, errA := json.Marshal(a)
	bj, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(aj) == string(bj)
}
