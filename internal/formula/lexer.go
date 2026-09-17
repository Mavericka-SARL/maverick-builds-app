package formula

import (
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
)

type Token struct {
	Type TokenType
	Val  string
	Pos  int
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
				break
			}
		} else {
			sb.WriteRune(ch)
		}
	}
	return Token{Type: tokString, Val: sb.String(), Pos: start}
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

func (l *lexer) readIdent(start int) Token {
	for l.pos < len(l.src) && (unicode.IsLetter(l.peek()) || unicode.IsDigit(l.peek()) || l.peek() == '_') {
		l.advance()
	}
	val := string(l.src[start:l.pos])
	return Token{Type: tokIdent, Val: val, Pos: start}
}

// readBraceIdent handles legacy {metric_name} syntax
func (l *lexer) readBraceIdent(start int) Token {
	l.advance() // consume '{'
	inner := l.pos
	for l.pos < len(l.src) && l.peek() != '}' {
		l.advance()
	}
	val := string(l.src[inner:l.pos])
	if l.pos < len(l.src) {
		l.advance() // consume '}'
	}
	return Token{Type: tokIdent, Val: val, Pos: start}
}
