package formula

import (
	"fmt"
	"strconv"
	"strings"
)

type parser struct {
	tokens []Token
	pos    int
}

func parse(src string) (Node, error) {
	src = normalise(src)
	tokens := lex(src)
	p := &parser{tokens: tokens}
	node, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().Type != tokEOF {
		return nil, fmt.Errorf("unexpected token %q at position %d", p.peek().Val, p.peek().Pos)
	}
	return node, nil
}

// normalise strips a leading = and converts {name} → name (handled in lexer already).
func normalise(src string) string {
	return strings.TrimPrefix(strings.TrimSpace(src), "=")
}

func (p *parser) peek() Token {
	if p.pos >= len(p.tokens) {
		return Token{Type: tokEOF}
	}
	return p.tokens[p.pos]
}

func (p *parser) advance() Token {
	t := p.tokens[p.pos]
	p.pos++
	return t
}

func (p *parser) expect(tt TokenType) (Token, error) {
	t := p.peek()
	if t.Type != tt {
		return t, fmt.Errorf("expected token type %d, got %q", tt, t.Val)
	}
	return p.advance(), nil
}

// Grammar (low to high precedence):
// expr       → comparison
// comparison → concat (( "=" | "<>" | "<" | "<=" | ">" | ">=" ) concat)*
// concat     → addSub ("&" addSub)*
// addSub     → mulDiv (("+" | "-") mulDiv)*
// mulDiv     → power (("*" | "/") power)*
// power      → unary ("^" unary)*
// unary      → ("-" | "+") unary | primary
// primary    → NUMBER | STRING | BOOL | IDENT | call | "(" expr ")"

func (p *parser) parseExpr() (Node, error) {
	return p.parseComparison()
}

func (p *parser) parseComparison() (Node, error) {
	left, err := p.parseConcat()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		var op string
		switch t.Type {
		case tokEq:
			op = "="
		case tokNeq:
			op = "<>"
		case tokLt:
			op = "<"
		case tokLte:
			op = "<="
		case tokGt:
			op = ">"
		case tokGte:
			op = ">="
		default:
			return left, nil
		}
		p.advance()
		right, err := p.parseConcat()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: op, Left: left, Right: right}
	}
}

func (p *parser) parseConcat() (Node, error) {
	left, err := p.parseAddSub()
	if err != nil {
		return nil, err
	}
	for p.peek().Type == tokAmpersand {
		p.advance()
		right, err := p.parseAddSub()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: "&", Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseAddSub() (Node, error) {
	left, err := p.parseMulDiv()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Type != tokPlus && t.Type != tokMinus {
			break
		}
		op := t.Val
		p.advance()
		right, err := p.parseMulDiv()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: op, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseMulDiv() (Node, error) {
	left, err := p.parsePower()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Type != tokStar && t.Type != tokSlash {
			break
		}
		op := t.Val
		p.advance()
		right, err := p.parsePower()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: op, Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parsePower() (Node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	if p.peek().Type == tokCaret {
		p.advance()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &BinaryExpr{Op: "^", Left: left, Right: right}, nil
	}
	return left, nil
}

func (p *parser) parseUnary() (Node, error) {
	t := p.peek()
	if t.Type == tokMinus {
		p.advance()
		expr, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: "-", Expr: expr}, nil
	}
	if t.Type == tokPlus {
		p.advance()
		return p.parseUnary()
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Node, error) {
	t := p.peek()
	switch t.Type {
	case tokNumber:
		p.advance()
		n, err := strconv.ParseFloat(t.Val, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid number %q", t.Val)
		}
		return &NumberLit{Val: n}, nil

	case tokString:
		p.advance()
		return &StringLit{Val: t.Val}, nil

	case tokIdent:
		upper := strings.ToUpper(t.Val)
		if upper == "TRUE" {
			p.advance()
			return &BoolLit{Val: true}, nil
		}
		if upper == "FALSE" {
			p.advance()
			return &BoolLit{Val: false}, nil
		}
		// look ahead for function call
		if p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].Type == tokLParen {
			return p.parseCall()
		}
		p.advance()
		return &Ident{Name: t.Val}, nil

	case tokLParen:
		p.advance()
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen); err != nil {
			return nil, err
		}
		return expr, nil

	case tokEOF:
		return nil, fmt.Errorf("unexpected end of formula")

	default:
		return nil, fmt.Errorf("unexpected token %q at position %d", t.Val, t.Pos)
	}
}

func (p *parser) parseCall() (Node, error) {
	name := p.advance().Val // function name
	p.advance()             // consume '('

	var args []Node
	if p.peek().Type == tokRParen {
		p.advance()
		return &CallExpr{Name: strings.ToUpper(name), Args: args}, nil
	}

	for {
		arg, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		t := p.peek()
		if t.Type == tokRParen {
			p.advance()
			break
		}
		if t.Type == tokComma || t.Type == tokSemicolon {
			p.advance()
			continue
		}
		return nil, fmt.Errorf("expected ',' or ')' in function call, got %q", t.Val)
	}
	return &CallExpr{Name: strings.ToUpper(name), Args: args}, nil
}
