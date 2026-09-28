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
// reading something that no longer exists:
//   - a literal LOOKUP member for dim (any level of the hierarchy), matched
//     exactly, as LOOKUP resolves a member;
//   - when timeCodes is set (dim is the time dimension of the metric's
//     grid), a literal TIMESUM start or end code, matched exactly;
//   - an *IFS/*IF criterion on dim's code range that is an equality with
//     the code — no ordering, "<>" or wildcard, which can match other
//     members too (formula.CriterionEqualsCode, case-insensitive);
//   - a comparison (=, <>) between the bare dimension or PARENT(dim) and a
//     literal equal to the code, case-insensitively as formulas compare
//     text;
//   - a SWITCH on the bare dimension or PARENT(dim) with a literal match
//     value equal to the code (SWITCH compares as "=" does; the trailing
//     default is a result, not a match value).
//
// Dimension names match case-insensitively. A formula that does not parse
// names nothing. A criterion on a dim.property range, or a code computed at
// runtime, names no member.
func FormulaNamesMember(formulaText, dim, code string, timeCodes bool) bool {
	if strings.TrimSpace(formulaText) == "" || dim == "" || code == "" {
		return false
	}
	an, err := formula.Analyze(formulaText)
	if err != nil {
		return false
	}
	for _, call := range an.DimensionalCalls {
		for _, m := range call.Members {
			if strings.EqualFold(m.Dim, dim) {
				if c, ok := m.LiteralCode(); ok && c == code {
					return true
				}
			}
		}
		for _, c := range call.Criteria {
			if strings.EqualFold(c.Dim, dim) && c.Property == "" && c.Kind == formula.ArgLiteral &&
				formula.CriterionEqualsCode(c.Value, code) {
				return true
			}
		}
	}
	if timeCodes {
		for _, ts := range an.TimeSums {
			if !ts.Ranged {
				continue
			}
			for _, a := range []formula.DimensionalArg{ts.Start, ts.End} {
				if c, ok := a.LiteralCode(); ok && c == code {
					return true
				}
			}
		}
	}
	node, err := formula.Parse(formulaText)
	if err != nil {
		return false
	}
	return comparesMemberCode(node, dim, code)
}

// comparesMemberCode walks the AST for dim = "code" / dim <> "code" (either
// side, bare dimension or PARENT(dim)).
func comparesMemberCode(node formula.Node, dim, code string) bool {
	switch n := node.(type) {
	case *formula.BinaryExpr:
		if n.Op == "=" || n.Op == "<>" {
			if namesDimCode(n.Left, n.Right, dim, code) || namesDimCode(n.Right, n.Left, dim, code) {
				return true
			}
		}
		return comparesMemberCode(n.Left, dim, code) || comparesMemberCode(n.Right, dim, code)
	case *formula.UnaryExpr:
		return comparesMemberCode(n.Expr, dim, code)
	case *formula.CallExpr:
		// SWITCH(dim, "A", x, "B", y[, default]) compares its first argument
		// with each match value exactly as dim = "A" does (compareValues).
		if strings.EqualFold(n.Name, "SWITCH") && len(n.Args) >= 3 && isDimOrParent(n.Args[0], dim) {
			for i := 1; i+1 < len(n.Args); i += 2 {
				if text, ok := formula.LiteralText(n.Args[i]); ok && strings.EqualFold(text, code) {
					return true
				}
			}
		}
		for _, a := range n.Args {
			if comparesMemberCode(a, dim, code) {
				return true
			}
		}
	}
	return false
}

// namesDimCode reports whether side is dim (bare or PARENT(dim)) and other
// a literal whose text equals code.
func namesDimCode(side, other formula.Node, dim, code string) bool {
	if !isDimOrParent(side, dim) {
		return false
	}
	text, ok := formula.LiteralText(other)
	return ok && strings.EqualFold(text, code)
}

// isDimOrParent reports whether node is the bare dimension dim or PARENT(dim).
func isDimOrParent(node formula.Node, dim string) bool {
	switch s := node.(type) {
	case *formula.Ident:
		return strings.EqualFold(s.Name, dim)
	case *formula.CallExpr:
		if strings.EqualFold(s.Name, "PARENT") && len(s.Args) == 1 {
			if id, ok := s.Args[0].(*formula.Ident); ok {
				return strings.EqualFold(id.Name, dim)
			}
		}
	}
	return false
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
	if len(readers) > 0 {
		return invalidCode(CodeMemberInUse,
			"member %s of %s is named by the formulas of %s; change those formulas first, then delete the member",
			code, dimName, strings.Join(readers, ", "))
	}
	return nil
}
