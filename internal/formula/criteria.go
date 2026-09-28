package formula

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Criteria matching for the conditional aggregations (contract C3), Excel
// rules:
//   - a string starting with =, <>, <, <=, > or >= is a comparison with
//     the rest of the string; anything else is an equality test;
//   - the comparison is numeric when the operand parses as a number (or an
//     ISO yyyy-mm-dd date, compared as its date serial) and the candidate
//     is a number; a text (non-numeric) operand never equals or orders a
//     number candidate, which then matches only <> (as in Excel, where
//     "1*" or "<abc" never match the number 10);
//   - an error candidate (an unparsable typed property) likewise matches
//     only <>: it is skipped by every other criterion, never an error of
//     the whole call (Excel skips error cells the same way);
//   - otherwise the comparison is text, case-insensitive;
//   - a member code is always text, so a numeric operand compares with a
//     code as text ("100" equals the code 100, ">5" orders codes as text);
//   - in text equality and inequality * and ? are wildcards, ~ escapes;
//   - "" and "=" match blank; "<>" matches non-blank; an ordering
//     comparison never matches a blank.

type criterionOp int

const (
	opEq criterionOp = iota
	opNe
	opLt
	opLe
	opGt
	opGe
)

type criterion struct {
	op        criterionOp
	operand   string // the text after the operator
	num       float64
	isNum     bool
	pattern   []wildToken
	wildcards bool
}

// criterionOperators is checked in order: two-character operators first.
var criterionOperators = []struct {
	prefix string
	op     criterionOp
}{
	{"<=", opLe}, {">=", opGe}, {"<>", opNe}, {"<", opLt}, {">", opGt}, {"=", opEq},
}

func parseCriterion(v Value) criterion {
	var c criterion
	switch v.Kind() {
	case KindBlank:
		c = criterion{op: opEq}
	case KindNumber:
		n, _ := v.Number()
		if n == 0 {
			n = 0 // negative zero is 0, as memberCode renders it
		}
		c = criterion{op: opEq, operand: numberText(n), num: n, isNum: true}
		c.pattern, c.wildcards = compileWildcard(c.operand)
		return c
	case KindString:
		s := v.String()
		c = criterion{op: opEq, operand: s}
		for _, o := range criterionOperators {
			if strings.HasPrefix(s, o.prefix) {
				c = criterion{op: o.op, operand: s[len(o.prefix):]}
				break
			}
		}
	default:
		c = criterion{op: opEq, operand: v.String()}
	}
	c.num, c.isNum = parseCriteriaOperand(c.operand)
	c.pattern, c.wildcards = compileWildcard(c.operand)
	return c
}

// numberText renders n as text without an exponent; negative zero is "0".
func numberText(n float64) string {
	if n == 0 {
		n = 0
	}
	return strconv.FormatFloat(n, 'f', -1, 64)
}

// parseCriteriaNumber parses a finite number; NaN and infinities are text.
func parseCriteriaNumber(s string) (float64, bool) {
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	return n, true
}

func parseCriteriaOperand(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}
	if n, ok := parseCriteriaNumber(t); ok {
		return n, true
	}
	if d, err := time.Parse(isoDateLayout, t); err == nil {
		return timeToSerial(d), true
	}
	return 0, false
}

func (c criterion) matches(candidate Value) bool {
	if candidate.IsError() {
		return c.op == opNe
	}
	if candidate.IsBlank() || (candidate.Kind() == KindString && candidate.String() == "") {
		switch c.op {
		case opEq:
			return c.operand == ""
		case opNe:
			return c.operand != ""
		default:
			return false // an ordering never matches a blank
		}
	}
	if c.operand == "" {
		// "=" matches only blank, "<>" everything that is not blank, and an
		// ordering against nothing matches nothing.
		return c.op == opNe
	}
	if candidate.Kind() == KindNumber {
		if !c.isNum {
			return c.op == opNe // text never equals or orders a number
		}
		n, _ := candidate.Number()
		return compareOrdered(c.op, cmpFloat(n, c.num))
	}
	text, _ := memberCode(candidate)
	switch c.op {
	case opEq:
		return c.textEqual(text)
	case opNe:
		return !c.textEqual(text)
	default:
	}
	return compareOrdered(c.op, strings.Compare(strings.ToUpper(text), strings.ToUpper(c.operand)))
}

func (c criterion) textEqual(text string) bool {
	if !c.wildcards {
		return strings.EqualFold(text, unescapeWildcard(c.operand))
	}
	return matchWildcard(c.pattern, []rune(strings.ToUpper(text)))
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func compareOrdered(op criterionOp, cmp int) bool {
	switch op {
	case opEq:
		return cmp == 0
	case opNe:
		return cmp != 0
	case opLt:
		return cmp < 0
	case opLe:
		return cmp <= 0
	case opGt:
		return cmp > 0
	case opGe:
		return cmp >= 0
	}
	return false
}

type wildKind int

const (
	wildLiteral wildKind = iota
	wildAny              // *
	wildOne              // ?
)

type wildToken struct {
	kind wildKind
	r    rune
}

// compileWildcard upper-cases pattern and splits it into literal runes and
// wildcards. ~ escapes the next *, ? or ~; before any other character it is
// a literal ~. wildcards reports whether any unescaped wildcard exists.
func compileWildcard(pattern string) ([]wildToken, bool) {
	runes := []rune(strings.ToUpper(pattern))
	out := make([]wildToken, 0, len(runes))
	wild := false
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case '~':
			if i+1 < len(runes) && (runes[i+1] == '*' || runes[i+1] == '?' || runes[i+1] == '~') {
				i++
				out = append(out, wildToken{kind: wildLiteral, r: runes[i]})
				continue
			}
			out = append(out, wildToken{kind: wildLiteral, r: r})
		case '*':
			wild = true
			out = append(out, wildToken{kind: wildAny})
		case '?':
			wild = true
			out = append(out, wildToken{kind: wildOne})
		default:
			out = append(out, wildToken{kind: wildLiteral, r: r})
		}
	}
	return out, wild
}

func unescapeWildcard(pattern string) string {
	toks, _ := compileWildcard(pattern)
	// compileWildcard upper-cases; equality is case-insensitive anyway.
	var sb strings.Builder
	for _, t := range toks {
		sb.WriteRune(t.r)
	}
	return sb.String()
}

// matchWildcard matches text (already upper-cased) against the pattern,
// greedy with backtracking to the last *.
func matchWildcard(p []wildToken, text []rune) bool {
	pi, ti := 0, 0
	star, mark := -1, 0
	for ti < len(text) {
		switch {
		case pi < len(p) && (p[pi].kind == wildOne || (p[pi].kind == wildLiteral && p[pi].r == text[ti])):
			pi++
			ti++
		case pi < len(p) && p[pi].kind == wildAny:
			star = pi
			mark = ti
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ti = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi].kind == wildAny {
		pi++
	}
	return pi == len(p)
}
