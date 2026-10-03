package formula

import (
	"fmt"
	"strings"
	"unicode"
)

type TokenType int

const (
	tokEOF TokenType = iota
	tokNumber
	tokString
	tokIdent
	tokPlus
	tokMinus
	tokStar
	tokSlash
	tokCaret
	tokLParen
	tokRParen
	tokComma
	tokEq
	tokNeq
	tokLt
	tokLte
	tokGt
	tokGte
	tokAmpersand
	tokSemicolon
	// tokDotted is dim.property: an identifier, one dot, and a second
	// identifier starting with a letter or _. Val is the whole text.
	tokDotted
	// tokError is a lexically malformed token; Val is the message.
	tokError
)

type Token struct {
	Type TokenType
	Val  string // the token's exact source text (a string's or {name}'s content)
	Pos  int
	// Dim and Prop are a tokDotted's two names: region.factor, or a
	// dimension whose name is not an identifier, {Cost Center}.p_and_l_line.
	Dim, Prop string
}

type lexer struct {
	src []rune
	pos int
}

func lex(src string) []Token {
	l := &lexer{src: []rune(src), pos: 0}
	var tokens []Token
	for {
		tok := l.next()
		tokens = append(tokens, tok)
		if tok.Type == tokEOF {
			break
		}
	}
	return tokens
}

func (l *lexer) peek() rune {
	if l.pos >= len(l.src) {
		return 0
	}
	return l.src[l.pos]
}

func (l *lexer) advance() rune {
	r := l.src[l.pos]
	l.pos++
	return r
}

func (l *lexer) next() Token {
	for l.pos < len(l.src) && unicode.IsSpace(l.peek()) {
		l.advance()
	}
	if l.pos >= len(l.src) {
		return Token{Type: tokEOF, Pos: l.pos}
	}
	start := l.pos
	ch := l.peek()

	switch {
	case ch == '"':
		return l.readString(start)
	case unicode.IsDigit(ch) || (ch == '.' && l.pos+1 < len(l.src) && unicode.IsDigit(l.src[l.pos+1])):
		return l.readNumber(start)
	case unicode.IsLetter(ch) || ch == '_':
		return l.readIdent(start)
	case ch == '{':
		return l.readBraceIdent(start)
	}

	l.advance()
	switch ch {
	case '+':
		return Token{Type: tokPlus, Val: "+", Pos: start}
	case '-':
		return Token{Type: tokMinus, Val: "-", Pos: start}
	case '*':
		return Token{Type: tokStar, Val: "*", Pos: start}
	case '/':
		return Token{Type: tokSlash, Val: "/", Pos: start}
	case '^':
		return Token{Type: tokCaret, Val: "^", Pos: start}
	case '(':
		return Token{Type: tokLParen, Val: "(", Pos: start}
	case ')':
		return Token{Type: tokRParen, Val: ")", Pos: start}
	case ',':
		return Token{Type: tokComma, Val: ",", Pos: start}
	case ';':
		return Token{Type: tokSemicolon, Val: ";", Pos: start}
	case '&':
		return Token{Type: tokAmpersand, Val: "&", Pos: start}
	case '=':
		return Token{Type: tokEq, Val: "=", Pos: start}
	case '<':
		if l.peek() == '>' {
			l.advance()
			return Token{Type: tokNeq, Val: "<>", Pos: start}
		}
		if l.peek() == '=' {
			l.advance()
			return Token{Type: tokLte, Val: "<=", Pos: start}
		}
		return Token{Type: tokLt, Val: "<", Pos: start}
	case '>':
		if l.peek() == '=' {
			l.advance()
			return Token{Type: tokGte, Val: ">=", Pos: start}
		}
		return Token{Type: tokGt, Val: ">", Pos: start}
	}
	// Unknown character — return as ident to surface a parse error
	return Token{Type: tokIdent, Val: string(ch), Pos: start}
}

func (l *lexer) readString(start int) Token {
	l.advance() // consume opening "
	var sb strings.Builder
	for l.pos < len(l.src) {
		ch := l.advance()
		if ch == '"' {
			if l.peek() == '"' {
				l.advance() // escaped quote ""
				sb.WriteRune('"')
			} else {
				return Token{Type: tokString, Val: sb.String(), Pos: start}
			}
		} else {
			sb.WriteRune(ch)
		}
	}
	return Token{Type: tokError, Pos: start,
		Val: fmt.Sprintf("the text starting at position %d has no closing quote (\")", start)}
}

func (l *lexer) readNumber(start int) Token {
	for l.pos < len(l.src) && (unicode.IsDigit(l.peek()) || l.peek() == '.') {
		l.advance()
	}
	// scientific notation
	if l.pos < len(l.src) && (l.peek() == 'e' || l.peek() == 'E') {
		l.advance()
		if l.pos < len(l.src) && (l.peek() == '+' || l.peek() == '-') {
			l.advance()
		}
		for l.pos < len(l.src) && unicode.IsDigit(l.peek()) {
			l.advance()
		}
	}
	return Token{Type: tokNumber, Val: string(l.src[start:l.pos]), Pos: start}
}

func isIdentRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

func (l *lexer) readWord() {
	for l.pos < len(l.src) && isIdentRune(l.peek()) {
		l.advance()
	}
}

// readIdent reads an identifier, or a dim.property reference when the
// identifier is directly followed by a dot and a letter or _. Exactly one
// dot is allowed, and a digit after the dot is an error: x.5 and
// region.2026 are never property references.
func (l *lexer) readIdent(start int) Token {
	l.readWord()
	if l.peek() != '.' || l.pos+1 >= len(l.src) {
		return Token{Type: tokIdent, Val: string(l.src[start:l.pos]), Pos: start}
	}
	after := l.src[l.pos+1]
	switch {
	case unicode.IsLetter(after) || after == '_':
		l.advance() // the dot
		l.readWord()
		if l.peek() == '.' && l.pos+1 < len(l.src) && isIdentRune(l.src[l.pos+1]) {
			for l.peek() == '.' && l.pos+1 < len(l.src) && isIdentRune(l.src[l.pos+1]) {
				l.advance()
				l.readWord()
			}
			return Token{Type: tokError, Pos: start,
				Val: fmt.Sprintf("%s: a property reference has exactly one dot (dimension.property)", string(l.src[start:l.pos]))}
		}
		val := string(l.src[start:l.pos])
		dim, prop, _ := strings.Cut(val, ".")
		return Token{Type: tokDotted, Val: val, Pos: start, Dim: dim, Prop: prop}
	case unicode.IsDigit(after):
		l.advance() // the dot
		l.readWord()
		return Token{Type: tokError, Pos: start,
			Val: fmt.Sprintf("%s: a property name must start with a letter or _", string(l.src[start:l.pos]))}
	}
	return Token{Type: tokIdent, Val: string(l.src[start:l.pos]), Pos: start}
}

// readBraceIdent reads a name in braces: any metric or dimension name, the
// way to write one that is not an identifier ({Setup Item}, {P&L line}).
// A dot and a property name after it make a property reference of a
// dimension with such a name: {Cost Center}.p_and_l_line.
func (l *lexer) readBraceIdent(start int) Token {
	l.advance() // consume '{'
	inner := l.pos
	for l.pos < len(l.src) && l.peek() != '}' {
		l.advance()
	}
	val := string(l.src[inner:l.pos])
	if l.pos >= len(l.src) {
		return Token{Type: tokError, Pos: start,
			Val: fmt.Sprintf("the name starting at position %d has no closing brace (})", start)}
	}
	l.advance() // consume '}'
	if l.peek() == '.' && l.pos+1 < len(l.src) {
		after := l.src[l.pos+1]
		switch {
		case unicode.IsLetter(after) || after == '_':
			l.advance() // the dot
			propStart := l.pos
			l.readWord()
			if l.peek() == '.' {
				return Token{Type: tokError, Pos: start,
					Val: fmt.Sprintf("%s: a property reference has exactly one dot (dimension.property)", string(l.src[start:l.pos]))}
			}
			return Token{Type: tokDotted, Val: string(l.src[start:l.pos]), Pos: start, Dim: val, Prop: string(l.src[propStart:l.pos])}
		case unicode.IsDigit(after):
			l.advance()
			l.readWord()
			return Token{Type: tokError, Pos: start,
				Val: fmt.Sprintf("%s: a property name must start with a letter or _", string(l.src[start:l.pos]))}
		}
	}
	return Token{Type: tokIdent, Val: val, Pos: start}
}

// QuoteName is how a formula writes a metric or dimension name: as it is
// when it is an identifier, in braces otherwise ({Setup Item}).
func QuoteName(name string) string {
	plain := name != ""
	for i, r := range name {
		if !isIdentRune(r) || (i == 0 && unicode.IsDigit(r)) {
			plain = false
			break
		}
	}
	if plain {
		return name
	}
	return "{" + name + "}"
}
