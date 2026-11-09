package formula

import (
	"sort"
	"strings"
)

// memberLiteral is a literal in a formula that names a member code of a
// dimension (memberCodeLiterals).
type memberLiteral struct {
	node Node
	// criterion: the literal is a criterion on the dimension's code range,
	// and prefix its comparison operator as written ("", "=" or "<>").
	criterion bool
	prefix    string
}

// memberCodeLiterals collects the literals of a parsed formula (node, an
// its analysis) that name member code of dimension dim. The member-delete
// guard (NamesMember) and the member-code rename (RenameMemberCode) both
// read this one list, so what a delete is refused for is exactly what a
// rename rewrites:
//   - a literal LOOKUP member for dim (any level of the hierarchy), matched
//     exactly, as LOOKUP resolves a member;
//   - when timeCodes is set (dim is the time dimension of the metric's
//     grid), a literal TIMESUM start or end code, matched exactly;
//   - an *IFS/*IF criterion on dim's code range that is an equality with
//     the code — no ordering or wildcard, which can match other members
//     too; with forRename also a "<>" criterion, which a rename must carry
//     or it silently starts matching the renamed member;
//   - a comparison (=, <>) between the bare dimension or PARENT(dim) and a
//     literal equal to the code, case-insensitively as formulas compare
//     text;
//   - a SWITCH on the bare dimension or PARENT(dim) with a literal match
//     value equal to the code (SWITCH compares as "=" does; the trailing
//     default is a result, not a match value).
//
// Dimension names match case-insensitively. A criterion on a dim.property
// range, or a code computed at runtime, names no member.
func memberCodeLiterals(node Node, an *Analysis, dim, code string, timeCodes, forRename bool) []memberLiteral {
	var out []memberLiteral
	for _, call := range an.DimensionalCalls {
		for _, m := range call.Members {
			if strings.EqualFold(m.Dim, dim) {
				if c, ok := m.LiteralCode(); ok && c == code {
					out = append(out, memberLiteral{node: m.Expr})
				}
			}
		}
		for _, c := range call.Criteria {
			if !strings.EqualFold(c.Dim, dim) || c.Property != "" || c.Kind != ArgLiteral {
				continue
			}
			if prefix, ok := criterionNamesCode(c.Value, code, forRename); ok {
				out = append(out, memberLiteral{node: c.Expr, criterion: true, prefix: prefix})
			}
		}
	}
	if timeCodes {
		for _, ts := range an.TimeSums {
			if !ts.Ranged {
				continue
			}
			for _, a := range []DimensionalArg{ts.Start, ts.End} {
				if c, ok := a.LiteralCode(); ok && c == code {
					out = append(out, memberLiteral{node: a.Expr})
				}
			}
		}
	}
	comparedLiterals(node, dim, code, &out)
	return out
}

// criterionNamesCode reports whether criterion v on a dimension's code range
// is an equality with exactly code (or, with allowNe, an inequality with
// it), and returns its operator as written.
func criterionNamesCode(v Value, code string, allowNe bool) (prefix string, ok bool) {
	if code == "" || v.IsError() || v.IsBlank() {
		return "", false
	}
	c := parseCriterion(v)
	if c.wildcards || (c.op != opEq && (!allowNe || c.op != opNe)) {
		return "", false
	}
	eq := c
	eq.op = opEq
	if !eq.matches(StringVal(code)) {
		return "", false
	}
	if v.Kind() == KindString {
		s := v.String()
		prefix = s[:len(s)-len(c.operand)]
	}
	return prefix, true
}

// comparedLiterals walks the AST for dim = "code" / dim <> "code" (either
// side, bare dimension or PARENT(dim)) and SWITCH match values.
func comparedLiterals(node Node, dim, code string, out *[]memberLiteral) {
	switch n := node.(type) {
	case *BinaryExpr:
		if n.Op == "=" || n.Op == "<>" {
			for _, s := range [][2]Node{{n.Left, n.Right}, {n.Right, n.Left}} {
				if isDimOrParent(s[0], dim) && literalIs(s[1], code) {
					*out = append(*out, memberLiteral{node: s[1]})
				}
			}
		}
		comparedLiterals(n.Left, dim, code, out)
		comparedLiterals(n.Right, dim, code, out)
	case *UnaryExpr:
		comparedLiterals(n.Expr, dim, code, out)
	case *CallExpr:
		// SWITCH(dim, "A", x, "B", y[, default]) compares its first argument
		// with each match value exactly as dim = "A" does (compareValues).
		if strings.EqualFold(n.Name, "SWITCH") && len(n.Args) >= 3 && isDimOrParent(n.Args[0], dim) {
			for i := 1; i+1 < len(n.Args); i += 2 {
				if literalIs(n.Args[i], code) {
					*out = append(*out, memberLiteral{node: n.Args[i]})
				}
			}
		}
		for _, a := range n.Args {
			comparedLiterals(a, dim, code, out)
		}
	}
}

// literalIs reports whether node is a literal whose text equals code, as a
// comparison with a member code compares it (case-insensitively).
func literalIs(node Node, code string) bool {
	text, ok := LiteralText(node)
	return ok && strings.EqualFold(text, code)
}

// isDimOrParent reports whether node is the bare dimension dim or PARENT(dim).
func isDimOrParent(node Node, dim string) bool {
	switch s := node.(type) {
	case *Ident:
		return strings.EqualFold(s.Name, dim)
	case *CallExpr:
		if strings.EqualFold(s.Name, "PARENT") && len(s.Args) == 1 {
			if id, ok := s.Args[0].(*Ident); ok {
				return strings.EqualFold(id.Name, dim)
			}
		}
	}
	return false
}

// NamesMember reports whether a formula names member code of dimension dim
// (memberCodeLiterals lists where), so that deleting the member would leave
// the formula reading something that no longer exists. A formula that does
// not parse names nothing.
func NamesMember(text, dim, code string, timeCodes bool) bool {
	if strings.TrimSpace(text) == "" || dim == "" || code == "" {
		return false
	}
	node, err := parse(text)
	if err != nil {
		return false
	}
	an, err := AnalyzeNode(node)
	if err != nil {
		return false
	}
	return len(memberCodeLiterals(node, an, dim, code, timeCodes, false)) > 0
}

// RenameMemberCode carries a member code's rename into a formula: every
// literal that names oldCode of dimension dim (memberCodeLiterals, with
// "<>" criteria too) is rewritten as a string literal of newCode. A
// criterion keeps its operator and escapes newCode's wildcard characters,
// so it still matches exactly that member. Every other character stays
// byte-for-byte, and a literal written as an expression ("EM" & "EA") is
// left as it is. A formula that does not parse is returned unchanged.
// changed reports whether anything was rewritten.
func RenameMemberCode(text, dim, oldCode, newCode string, timeCodes bool) (out string, changed bool) {
	if strings.TrimSpace(text) == "" || dim == "" || oldCode == "" || newCode == "" || oldCode == newCode {
		return text, false
	}
	node, spans, lead, err := parseLiterals(text)
	if err != nil {
		return text, false
	}
	an, err := AnalyzeNode(node)
	if err != nil {
		return text, false
	}
	type edit struct {
		start, end int
		with       string
	}
	var edits []edit
	seen := map[Node]bool{}
	for _, l := range memberCodeLiterals(node, an, dim, oldCode, timeCodes, true) {
		sp, ok := spans[l.node]
		if !ok || seen[l.node] {
			continue
		}
		seen[l.node] = true
		content := newCode
		if l.criterion {
			prefix := l.prefix
			if prefix == "" && strings.ContainsAny(newCode[:1], "<>=") {
				prefix = "=" // the code would read as an operator
			}
			content = prefix + escapeWildcards(newCode)
		}
		edits = append(edits, edit{start: lead + sp.start, end: lead + sp.end,
			with: `"` + strings.ReplaceAll(content, `"`, `""`) + `"`})
	}
	if len(edits) == 0 {
		return text, false
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	src := []rune(text)
	for _, e := range edits {
		src = append(src[:e.start:e.start], append([]rune(e.with), src[e.end:]...)...)
	}
	return string(src), true
}

// escapeWildcards writes s so a criterion matches it literally: ~ before
// each *, ? and ~ (compileWildcard).
func escapeWildcards(s string) string {
	if !strings.ContainsAny(s, "*?~") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if r == '*' || r == '?' || r == '~' {
			b.WriteRune('~')
		}
		b.WriteRune(r)
	}
	return b.String()
}
