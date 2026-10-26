package modeledit

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// NotesDoNotConvertError refuses a text input's format change while some of
// its notes are not a value of the new format.
type NotesDoNotConvertError struct {
	Format   string
	Count    int
	Examples []string
}

func (e *NotesDoNotConvertError) Error() string {
	return fmt.Sprintf("%d of this metric's notes are not a %s value (for example %s): correct them first, or send drop_values true to clear them (the cell history keeps them)",
		e.Count, e.Format, strings.Join(e.Examples, ", "))
}

// ConvertTextNotes turns the notes of a text input into values of format,
// inside the caller's transaction, before the caller writes the new format.
// Its cells kept their notes beside a 0, so the grid showed "—" and formulas
// read 0: the HR model's Hire Date, imported as text and switched to Date,
// lost 100 dates without a word (2026-10-05).
//
// A note becomes a date's serial (yyyy-mm-dd), a number (number, currency,
// percentage), 1 or 0 (boolean: true/false, yes/no, 1/0), or a pick-list
// member's key (its code or label, picklistDimID's member). The converted
// value is written as a new entry of the cell, so its history keeps the
// note. When a note does not convert the change is refused
// (*NotesDoNotConvertError), unless dropUnconverted: those cells are then
// cleared, kept in the cell history.
func ConvertTextNotes(ctx context.Context, db DB, metricID, format, picklistDimID, userID string, dropUnconverted bool) error {
	parse, err := noteParser(ctx, db, format, picklistDimID)
	if err != nil {
		return err
	}
	rows, err := db.Query(ctx, `
		SELECT DISTINCT ON (dim_members) model_id::text, revision_id::text, dim_members::text, COALESCE(text_value, '')
		FROM runtime.fact_input
		WHERE metric_id = $1::uuid AND source_ref IS NULL
		ORDER BY dim_members, entered_at DESC, id DESC`, metricID)
	if err != nil {
		return fmt.Errorf("read notes: %w", err)
	}
	type note struct{ model, revision, cell, text string }
	var notes []note
	for rows.Next() {
		var n note
		if err := rows.Scan(&n.model, &n.revision, &n.cell, &n.text); err != nil {
			rows.Close()
			return fmt.Errorf("read notes: %w", err)
		}
		if strings.TrimSpace(n.text) != "" {
			notes = append(notes, n)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read notes: %w", err)
	}
	values := make([]float64, len(notes))
	var bad []note
	for i, n := range notes {
		v, ok := parse(n.text)
		if !ok {
			bad = append(bad, n)
			continue
		}
		values[i] = v
	}
	if len(bad) > 0 && !dropUnconverted {
		e := &NotesDoNotConvertError{Format: format, Count: len(bad)}
		for _, n := range bad[:min(3, len(bad))] {
			e.Examples = append(e.Examples, strconv.Quote(n.text))
		}
		return e
	}
	if len(bad) > 0 {
		if _, err := db.Exec(ctx, `SET LOCAL mvx.delete_reason = 'cleared'`); err != nil {
			return err
		}
	}
	unconverted := make(map[string]bool, len(bad))
	for _, n := range bad {
		unconverted[n.cell] = true
		if _, err := db.Exec(ctx, `
			DELETE FROM runtime.fact_input
			WHERE metric_id = $1::uuid AND dim_members = $2::jsonb AND source_ref IS NULL`, metricID, n.cell); err != nil {
			return fmt.Errorf("clear a note: %w", err)
		}
	}
	for i, n := range notes {
		if unconverted[n.cell] {
			continue
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::jsonb, $5, NULLIF($6,'')::uuid)`,
			n.model, n.revision, metricID, n.cell, values[i], userID); err != nil {
			return fmt.Errorf("convert a note: %w", err)
		}
	}
	return nil
}

// noteParser reads one note as a value of format.
func noteParser(ctx context.Context, db DB, format, picklistDimID string) (func(string) (float64, bool), error) {
	switch format {
	case "date":
		return func(s string) (float64, bool) { return formula.DateSerial(s) }, nil
	case "boolean":
		return func(s string) (float64, bool) {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true", "yes", "1":
				return 1, true
			case "false", "no", "0":
				return 0, true
			}
			return 0, false
		}, nil
	case "picklist":
		byCode, byLabel := map[string]string{}, map[string]string{}
		rows, err := db.Query(ctx, `SELECT code, COALESCE(label, '') FROM model.dimension_member WHERE dimension_id = NULLIF($1,'')::uuid`, picklistDimID)
		if err != nil {
			return nil, fmt.Errorf("read the pick-list's members: %w", err)
		}
		for rows.Next() {
			var code, label string
			if err := rows.Scan(&code, &label); err != nil {
				rows.Close()
				return nil, err
			}
			byCode[strings.ToLower(code)] = code
			if label != "" {
				byLabel[strings.ToLower(label)] = code
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return func(s string) (float64, bool) {
			key := strings.ToLower(strings.TrimSpace(s))
			code, ok := byCode[key]
			if !ok {
				code, ok = byLabel[key]
			}
			if !ok {
				return 0, false
			}
			return formula.PicklistKey(code), true
		}, nil
	default: // number, currency, percentage
		return func(s string) (float64, bool) {
			v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			return v, err == nil
		}, nil
	}
}
