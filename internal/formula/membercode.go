package formula

// CriterionEqualsCode reports whether criterion v, applied to a dimension's
// code range (SUMIFS(x, region, v)), is an equality with exactly the member
// code: no comparison operator other than "=", no wildcard, and an operand
// that matches code as the criterion would (case-insensitively; a number
// criterion as its text). A criterion that could match other members — an
// ordering, "<>", a wildcard pattern — does not name the member. Used to
// refuse deleting a member a formula names (MEMBER_IN_USE).
func CriterionEqualsCode(v Value, code string) bool {
	if code == "" || v.IsError() || v.IsBlank() {
		return false
	}
	c := parseCriterion(v)
	if c.op != opEq || c.wildcards {
		return false
	}
	return c.matches(StringVal(code))
}

// LiteralText returns the text a literal node compares as — a string as is,
// a number without an exponent — and ok false for any other node. Comparing
// a bare dimension (its member code) with a literal is case-insensitive
// text comparison (compareValues).
func LiteralText(node Node) (string, bool) {
	switch n := node.(type) {
	case *StringLit:
		return n.Val, true
	case *NumberLit:
		return numberText(n.Val), true
	}
	return "", false
}
