package modeledit

import (
	"context"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// SetMemberFormula makes member code of a dimension a calculated member
// (formula) or an ordinary one again (""), after
// metricformula.ValidateMemberFormula. The developer's route and the AI
// Developer's set_member_formula both call it.
func SetMemberFormula(ctx context.Context, db DB, dimensionID, code, text string) error {
	text = strings.TrimSpace(text)
	if err := metricformula.ValidateMemberFormula(ctx, db, dimensionID, code, text); err != nil {
		return err
	}
	tag, err := db.Exec(ctx, `UPDATE model.dimension_member SET formula=NULLIF($3,'') WHERE dimension_id=$1::uuid AND code=$2`, dimensionID, code, text)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("the dimension has no member %q", code)
	}
	return nil
}

// RewriteMemberFormulas follows a renamed member code in the formulas of
// the dimension's calculated members, so {RF} - {LY} keeps reading the
// member after RF becomes FCST.
func RewriteMemberFormulas(ctx context.Context, db DB, dimensionID, oldCode, newCode string) error {
	if oldCode == newCode {
		return nil
	}
	rows, err := db.Query(ctx, `SELECT id::text, formula FROM model.dimension_member WHERE dimension_id=$1::uuid AND NULLIF(btrim(formula),'') IS NOT NULL`, dimensionID)
	if err != nil {
		return err
	}
	type upd struct{ id, text string }
	var updates []upd
	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			rows.Close()
			return err
		}
		if out, changed := formula.RenameIdent(text, oldCode, newCode); changed {
			updates = append(updates, upd{id, out})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range updates {
		if _, err := db.Exec(ctx, `UPDATE model.dimension_member SET formula=$2 WHERE id=$1::uuid`, u.id, u.text); err != nil {
			return err
		}
	}
	return nil
}
