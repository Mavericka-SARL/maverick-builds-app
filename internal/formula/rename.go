package formula

import "strings"

// RenameProperty rewrites every dim.oldProp reference in a formula to
// dim.newProp. Dimension and property names match case-insensitively, and
// the dimension is kept exactly as written. Only dotted property tokens
// change: string literals, {brace} metric names and everything else stay
// byte-for-byte, so a rename can never alter what else a formula says.
// changed reports whether anything was rewritten.
func RenameProperty(text, dim, oldProp, newProp string) (out string, changed bool) {
	src := []rune(text)
	var b strings.Builder
	last := 0
	for _, tok := range lex(text) {
		if tok.Type != tokDotted {
			continue
		}
		d, p, ok := strings.Cut(tok.Val, ".")
		if !ok || !strings.EqualFold(d, dim) || !strings.EqualFold(p, oldProp) {
			continue
		}
		// tok.Pos is a rune index and tok.Val is the token's exact source.
		propStart := tok.Pos + len([]rune(d)) + 1
		b.WriteString(string(src[last:propStart]))
		b.WriteString(newProp)
		last = propStart + len([]rune(p))
		changed = true
	}
	if !changed {
		return text, false
	}
	b.WriteString(string(src[last:]))
	return b.String(), true
}
