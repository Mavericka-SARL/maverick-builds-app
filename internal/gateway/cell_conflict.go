package gateway

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// Two people changing one cell: a save names the cell as its writer read it
// (expected), and a cell someone changed since is not written over — the
// save is refused, 409 cell_changed, with what the cell holds now, as a
// record save with expected_updated_at is (2026-11-27: a country manager's
// 40% replaced a project manager's 30% typed a moment before, and neither was
// told). A save without expected is made as before: last write wins.
//
// The check and the write share the write's transaction, behind a lock of
// that one cell, so two checked saves of it cannot both pass.

// cellExpected is the cell as its writer read it: a number, a text, or
// nothing (blank). Empty, it checks nothing.
type cellExpected struct {
	Value *float64 `json:"value,omitempty"`
	Text  *string  `json:"text,omitempty"`
	Blank bool     `json:"blank,omitempty"`
}

func (x *cellExpected) checks() bool {
	return x != nil && (x.Blank || x.Text != nil || x.Value != nil)
}

// holds reports whether the cell as stored (held: a typed value is there)
// is what the writer read.
func (x *cellExpected) holds(held bool, value float64, text *string) bool {
	switch {
	case x.Blank:
		return !held
	case x.Text != nil:
		return held && text != nil && *text == *x.Text
	case x.Value != nil:
		return held && text == nil && value == *x.Value
	}
	return true
}

// cellChangedError is a checked save of a cell someone changed since its
// writer read it.
type cellChangedError struct {
	index   int // the cell's place in its batch
	metric  string
	members string
	held    bool
	value   float64
	text    *string
}

func (e *cellChangedError) Error() string {
	now := "nothing"
	switch {
	case e.held && e.text != nil:
		now = strconv.Quote(*e.text)
	case e.held:
		now = strconv.FormatFloat(e.value, 'f', -1, 64)
	}
	return fmt.Sprintf("someone changed this cell since you opened it: it now holds %s — look at it again and save once more if you still mean to change it", now)
}

func (e *cellChangedError) body() map[string]any {
	var current any
	switch {
	case e.held && e.text != nil:
		current = *e.text
	case e.held:
		current = e.value
	}
	return map[string]any{"error": e.Error(), "code": "cell_changed", "index": e.index, "metric_id": e.metric, "current": current}
}

func asCellChanged(err error) *cellChangedError {
	var e *cellChangedError
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// lockCell holds the one cell for the rest of tx: a checked save of it waits
// for another's to commit and then compares with what that one wrote.
func lockCell(ctx context.Context, tx pgx.Tx, modelID, revisionID, metricID, dimMembers string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"cell|"+modelID+"|"+revisionID+"|"+metricID+"|"+dimMembers)
	return err
}
