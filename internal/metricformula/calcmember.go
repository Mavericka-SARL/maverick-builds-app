package metricformula

import (
	"context"
	"sort"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// CodeInvalidMemberFormula marks a calculated member's formula the save
// refuses.
const CodeInvalidMemberFormula = "INVALID_MEMBER_FORMULA"

// ValidateMemberFormula checks a calculated member's formula before it is
// saved (dimension_member.formula; "" makes an ordinary member). The formula
// reads the dimension's other members by code — {RF} - {LY} — and
// METRICFORMAT(), the format of the metric it is evaluated for; it is the
// same for every metric. Refused: a time or derived dimension (their totals
// are periods or a property's groups), a reference to anything but another
// member of the dimension, a dimensional or time function (they read
// metrics, which a member formula does not name), a cycle through other
// calculated members, and a member with members under it or above it (a
// calculated member is a top-level member: computed, never a total of
// children, and in no total).
func ValidateMemberFormula(ctx context.Context, q Querier, dimensionID, code, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var dimName, dimType string
	var derived bool
	if err := q.QueryRow(ctx, `SELECT name, COALESCE(dimension_type,''), source_dimension_id IS NOT NULL FROM model.dimension_def WHERE id=$1::uuid`,
		dimensionID).Scan(&dimName, &dimType, &derived); err != nil {
		return invalidCode(CodeInvalidMemberFormula, "dimension not found")
	}
	if dimType == "time" {
		return invalidCode(CodeInvalidMemberFormula, "%s is a time dimension: a calculated member belongs to a standard dimension (a Scenario, a Version)", dimName)
	}
	if derived {
		return invalidCode(CodeInvalidMemberFormula, "%s is derived from another dimension's property: its members are that property's values", dimName)
	}
	node, err := formula.Parse(text)
	if err != nil {
		return invalidCode(CodeInvalidMemberFormula, "the formula of %s does not parse: %v", code, err)
	}
	refs, problem := memberFormulaRefs(node)
	if problem != "" {
		return invalidCode(CodeInvalidMemberFormula, "the formula of %s %s", code, problem)
	}

	type member struct {
		id, code, formula string
		children, parent  bool
	}
	rows, err := q.Query(ctx, `
		SELECT m.id::text, m.code, COALESCE(m.formula, ''),
		       EXISTS (SELECT 1 FROM model.dimension_member c WHERE c.parent_member_id = m.id), m.parent_member_id IS NOT NULL
		FROM model.dimension_member m WHERE m.dimension_id=$1::uuid ORDER BY m.sort_order, m.code`, dimensionID)
	if err != nil {
		return err
	}
	var members []member
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.id, &m.code, &m.formula, &m.children, &m.parent); err != nil {
			rows.Close()
			return err
		}
		members = append(members, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	find := func(c string) *member {
		for i := range members {
			if strings.EqualFold(members[i].code, c) {
				return &members[i]
			}
		}
		return nil
	}
	if self := find(code); self != nil && self.children {
		return invalidCode(CodeInvalidMemberFormula, "%s has members under it: a calculated member is computed from its siblings, never a total of children", code)
	}
	if self := find(code); self != nil && self.parent {
		return invalidCode(CodeInvalidMemberFormula, "%s is under another member: a calculated member is a top-level member, so no total includes it — move it to the top level first", code)
	}
	if self := find(code); self != nil {
		var holds bool
		_ = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runtime.fact_input WHERE dim_members->>$1 = $2)`, dimensionID, self.code).Scan(&holds)
		if holds {
			return invalidCode(CodeInvalidMemberFormula, "%s holds entered values: a calculated member holds none — move or clear them first", code)
		}
	}
	var codes []string
	for _, m := range members {
		if !strings.EqualFold(m.code, code) {
			codes = append(codes, m.code)
		}
	}
	for _, ref := range refs {
		if strings.EqualFold(ref, code) {
			return invalidCode(CodeInvalidMemberFormula, "the formula of %s reads %s itself", code, code)
		}
		if find(ref) == nil {
			return invalidCode(CodeInvalidMemberFormula, "the formula of %s reads %q, which is not a member of %s — its members are %s",
				code, ref, dimName, strings.Join(codes, ", "))
		}
	}
	// No cycle through the dimension's other calculated members.
	formulaOf := map[string]string{strings.ToUpper(code): text}
	for _, m := range members {
		if m.formula != "" && !strings.EqualFold(m.code, code) {
			formulaOf[strings.ToUpper(m.code)] = m.formula
		}
	}
	state := map[string]int{} // 1 visiting, 2 done
	var path []string
	var visit func(c string) []string
	visit = func(c string) []string {
		switch state[c] {
		case 1:
			return append(append([]string{}, path...), c)
		case 2:
			return nil
		}
		f, ok := formulaOf[c]
		if !ok {
			return nil
		}
		state[c] = 1
		path = append(path, c)
		n, err := formula.Parse(f)
		if err == nil {
			rs, _ := memberFormulaRefs(n)
			for _, r := range rs {
				if cyc := visit(strings.ToUpper(r)); cyc != nil {
					return cyc
				}
			}
		}
		path = path[:len(path)-1]
		state[c] = 2
		return nil
	}
	if cyc := visit(strings.ToUpper(code)); cyc != nil {
		return invalidCode(CodeInvalidMemberFormula, "the calculated members of %s would read each other in a circle: %s", dimName, strings.Join(cyc, " → "))
	}
	return nil
}

// memberFormulaFuncs are the functions a member formula may call: the set
// the planning grid's own evaluator (memberFormula.ts) implements, so a
// value at a total is the same in the browser as on the server.
var memberFormulaFuncs = map[string]bool{"IF": true, "ABS": true, "MIN": true, "MAX": true, "ROUND": true, "AND": true, "OR": true, "NOT": true}

// memberFormulaRefs lists the member codes a member formula reads, or says
// what it reads that a member formula cannot.
func memberFormulaRefs(node formula.Node) ([]string, string) {
	seen := map[string]bool{}
	var refs []string
	problem := ""
	var walk func(formula.Node)
	walk = func(n formula.Node) {
		if problem != "" || n == nil {
			return
		}
		switch x := n.(type) {
		case *formula.Ident:
			if !seen[strings.ToUpper(x.Name)] {
				seen[strings.ToUpper(x.Name)] = true
				refs = append(refs, x.Name)
			}
		case *formula.DimProperty:
			problem = "reads a property (" + x.Dim + "." + x.Property + "): a member formula reads the dimension's other members only"
		case *formula.UnaryExpr:
			walk(x.Expr)
		case *formula.BinaryExpr:
			if x.Op == "^" || x.Op == "&" {
				problem = "uses " + x.Op + ": a member formula uses + - * / and comparisons"
				return
			}
			walk(x.Left)
			walk(x.Right)
		case *formula.CallExpr:
			name := strings.ToUpper(x.Name)
			switch {
			case name == rollup.MetricFormatFunc:
				if len(x.Args) != 0 {
					problem = "passes arguments to METRICFORMAT(), which takes none"
				}
				return
			case formula.IsDimensionalFunction(name) || formula.IsTimeFunction(name):
				problem = "calls " + name + ", which reads metrics: a member formula reads the dimension's other members, the same for every metric"
				return
			case !memberFormulaFuncs[name]:
				problem = "calls " + name + ": a member formula calls IF, ABS, MIN, MAX, ROUND, AND, OR, NOT and METRICFORMAT() — the planning grid evaluates it too"
				return
			}
			for _, a := range x.Args {
				walk(a)
			}
		}
	}
	walk(node)
	sort.Strings(refs)
	return refs, problem
}
