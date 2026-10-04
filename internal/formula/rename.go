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
		d, p := tok.Dim, tok.Prop
		if !strings.EqualFold(d, dim) || !strings.EqualFold(p, oldProp) {
			continue
		}
		// tok.Pos is a rune index and tok.Val is the token's exact source,
		// which ends with the property name in both spellings (dim.prop and
		// {Dim Name}.prop).
		propStart := tok.Pos + len([]rune(tok.Val)) - len([]rune(p))
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

// RenameIdent rewrites every name reference to old (bare or in braces,
// matched case-insensitively) to new, written as QuoteName writes it. A
// function call of that name, string literals and everything else stay
// byte-for-byte. It is how a calculated member's formula follows a renamed
// member code.
func RenameIdent(text, old, new string) (out string, changed bool) {
	src := []rune(text)
	toks := lex(text)
	var b strings.Builder
	last := 0
	for i, tok := range toks {
		if tok.Type != tokIdent || !strings.EqualFold(tok.Val, old) {
			continue
		}
		if i+1 < len(toks) && toks[i+1].Type == tokLParen {
			continue // a function, not a name
		}
		width := len([]rune(tok.Val))
		if tok.Pos < len(src) && src[tok.Pos] == '{' {
			width += 2
		}
		b.WriteString(string(src[last:tok.Pos]))
		b.WriteString(QuoteName(new))
		last = tok.Pos + width
		changed = true
	}
	if !changed {
		return text, false
	}
	b.WriteString(string(src[last:]))
	return b.String(), true
}
