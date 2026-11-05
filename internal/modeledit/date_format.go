package modeledit

import (
	"context"
	"fmt"
	"strings"
)

// DateFormats are the ways a model may write dates for its readers; ""
// leaves it to the reader's browser (US English writes 03/05/2026 for
// 5 March). The console formats every date it shows and reads every date
// typed into it by the model's format.
var DateFormats = []string{"", "DD/MM/YYYY", "MM/DD/YYYY", "YYYY-MM-DD", "DD.MM.YYYY", "D MMM YYYY"}

// DateFormatError is a format no model may take.
type DateFormatError struct{ Msg string }

func (e *DateFormatError) Error() string { return e.Msg }

// SetDateFormat sets how a model writes dates — PUT
// /api/developer/models/{id}/date-format and the AI Developer's
// set_date_format. format is one of DateFormats.
func SetDateFormat(ctx context.Context, db DB, modelID, format string) error {
	format = strings.TrimSpace(format)
	ok := false
	for _, f := range DateFormats {
		if f == format {
			ok = true
		}
	}
	if !ok {
		return &DateFormatError{fmt.Sprintf("date format %q is none of %s (\"\" leaves it to the reader's browser)",
			format, strings.Join(DateFormats[1:], ", "))}
	}
	tag, err := db.Exec(ctx, `UPDATE core.model SET date_format = $2, updated_at = now() WHERE id = $1::uuid`, modelID, format)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &DateFormatError{"model not found"}
	}
	return nil
}
