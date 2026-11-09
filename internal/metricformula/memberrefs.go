package metricformula

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// CodeMemberInUse refuses deleting a dimension member while a formula names
// it (CheckMemberNotInUse); the gateway answers it with 409.
const CodeMemberInUse = "MEMBER_IN_USE"

// FormulaNamesMember reports whether a formula names member code of
// dimension dim, so that deleting the member would leave the formula
// reading something that no longer exists: a literal LOOKUP member, a
// TIMESUM bound on the time dimension (timeCodes), an equality criterion on
// the dimension's code range, a comparison with the bare dimension or
// PARENT(dim), or a SWITCH match value (formula.NamesMember lists them).
// A member-code rename rewrites exactly these literals
// (RenameMemberInFormulas), from the same list. A formula that does not
// parse names nothing.
func FormulaNamesMember(formulaText, dim, code string, timeCodes bool) bool {
	return formula.NamesMember(formulaText, dim, code, timeCodes)
}

// CheckMemberNotInUse refuses deleting member memberID of dimension
// dimensionID while a calculated metric of the dimension's revision names
// its code (FormulaNamesMember): every cell of such a metric would fail, or
// a comparison would silently stop matching, exactly what
// CheckPropertyNotInUse and CheckDimensionNotInUse prevent for a property
// and a dimension. A TIMESUM code counts only for a metric whose grid
// carries this (time) dimension. The error names the metrics. A member that
// does not exist in the dimension returns pgx.ErrNoRows.
func CheckMemberNotInUse(ctx context.Context, q Querier, dimensionID, memberID string) error {
	var modelID, revisionID, dimName, dimType, code string
	if err := q.QueryRow(ctx, `
		SELECT d.model_id::text, COALESCE(d.revision_id::text, ''), d.name, COALESCE(d.dimension_type, ''), mb.code
		FROM model.dimension_member mb
		JOIN model.dimension_def d ON d.id = mb.dimension_id
		WHERE mb.id=$1::uuid AND mb.dimension_id=$2::uuid
	`, memberID, dimensionID).Scan(&modelID, &revisionID, &dimName, &dimType, &code); err != nil {
		return err
	}
	rows, err := q.Query(ctx, `
		SELECT m.name, m.formula,
		       EXISTS (SELECT 1 FROM model.grid_metric gm
		               JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
		               WHERE gm.metric_id = m.id AND gd.dimension_id = $4::uuid)
		FROM model.metric_def m
		WHERE model_id=$1::uuid AND is_input = false AND formula IS NOT NULL AND formula <> ''
		  AND ($2 = '' OR revision_id = NULLIF($2,'')::uuid)
		  AND `+resolvesToDimension+`
		ORDER BY name
	`, modelID, revisionID, dimName, dimensionID)
	if err != nil {
		return err
	}
	type row struct {
		name, text string
		onGrid     bool
	}
	metrics, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.name, &x.text, &x.onGrid)
		return x, err
	})
	if err != nil {
		return err
	}
	var readers []string
	for _, m := range metrics {
		if FormulaNamesMember(m.text, dimName, code, dimType == "time" && m.onGrid) {
			readers = append(readers, m.name)
		}
	}
	// A calculated member of the dimension reading the code would read 0.
	calcRows, err := q.Query(ctx, `SELECT code, formula FROM model.dimension_member
		WHERE dimension_id=$1::uuid AND id <> $2::uuid AND NULLIF(btrim(formula),'') IS NOT NULL ORDER BY code`, dimensionID, memberID)
	if err != nil {
		return err
	}
	for calcRows.Next() {
		var calcCode, text string
		if calcRows.Scan(&calcCode, &text) != nil {
			continue
		}
		if node, perr := formula.Parse(text); perr == nil {
			if refs, _ := memberFormulaRefs(node); containsFold(refs, code) {
				readers = append(readers, "calculated member "+calcCode)
			}
		}
	}
	calcRows.Close()
	if len(readers) > 0 {
		return invalidCode(CodeMemberInUse,
			"member %s of %s is read by %s; change those formulas first, then delete the member",
			code, dimName, strings.Join(readers, ", "))
	}
	return nil
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
