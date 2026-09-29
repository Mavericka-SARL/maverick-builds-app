package formula

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
)

// arity is how many arguments each scalar built-in accepts; max -1 means no
// limit. The time and dimensional functions check their own argument shapes
// in Analyze. CheckArguments applies this table when a formula is saved, so a
// wrong count is refused with the function's name instead of failing every
// cell with #VALUE! later. The evaluator's requireArgCount calls enforce the
// same counts at run time; TestArityMatchesEvaluator holds the two equal.
var arity = map[string][2]int{
	"IF": {2, 3}, "IFS": {2, -1}, "AND": {1, -1}, "OR": {1, -1}, "NOT": {1, 1},
	"IFERROR": {2, 2}, "IFNA": {2, 2}, "SWITCH": {3, -1},

	"ABS": {1, 1}, "INT": {1, 1}, "ROUND": {2, 2}, "ROUNDUP": {2, 2}, "ROUNDDOWN": {2, 2},
	"CEILING": {2, 2}, "FLOOR": {2, 2}, "MOD": {2, 2}, "POWER": {2, 2}, "SQRT": {1, 1},

	"SUM": {1, -1}, "AVERAGE": {1, -1}, "MIN": {1, -1}, "MAX": {1, -1}, "COUNT": {1, -1}, "COUNTA": {1, -1},

	"CONCAT": {1, -1}, "TEXTJOIN": {3, -1}, "LEN": {1, 1}, "LEFT": {1, 2}, "RIGHT": {1, 2}, "MID": {3, 3},
	"UPPER": {1, 1}, "LOWER": {1, 1}, "TRIM": {1, 1}, "TEXT": {2, 2}, "SUBSTITUTE": {3, 4},

	"TODAY": {0, 0}, "DATE": {3, 3}, "YEAR": {1, 1}, "MONTH": {1, 1}, "DAY": {1, 1}, "DAYS": {2, 2},
	"EDATE": {2, 2}, "EOMONTH": {2, 2}, "DAYSINMONTH": {2, 2}, "DAYSINYEAR": {1, 1},
}

// CheckArguments parses text and refuses a call to a scalar built-in with a
// wrong number of arguments (IFS also needs condition, value pairs). It is
// the save-time complement of Analyze, which checks the time and dimensional
// functions.
func CheckArguments(text string) error {
	node, err := parse(text)
	if err != nil {
		return err
	}
	return checkArity(node)
}

func checkArity(node Node) error {
	switch n := node.(type) {
	case *UnaryExpr:
		return checkArity(n.Expr)
	case *BinaryExpr:
		if err := checkArity(n.Left); err != nil {
			return err
		}
		return checkArity(n.Right)
	case *CallExpr:
		if a, ok := arity[n.Name]; ok {
			k := len(n.Args)
			if k < a[0] || (a[1] >= 0 && k > a[1]) {
				return &AnalysisError{Code: ErrValue.Code,
					Message: fmt.Sprintf("%s: wrong number of arguments (%d); it takes %s", n.Name, k, describeArity(a))}
			}
			if n.Name == "IFS" && k%2 != 0 {
				return &AnalysisError{Code: ErrValue.Code,
					Message: fmt.Sprintf("IFS: wrong number of arguments (%d); it takes condition, value pairs", k)}
			}
		}
		for _, arg := range n.Args {
			if err := checkArity(arg); err != nil {
				return err
			}
		}
	}
	return nil
}

func describeArity(a [2]int) string {
	switch {
	case a[1] == 0:
		return "no arguments"
	case a[1] < 0:
		return fmt.Sprintf("at least %d", a[0])
	case a[0] == a[1] && a[0] == 1:
		return "1 argument"
	case a[0] == a[1]:
		return fmt.Sprintf("%d arguments", a[0])
	}
	return fmt.Sprintf("%d to %d arguments", a[0], a[1])
}

// ── Decimal rounding ─────────────────────────────────────────────────────────

// significant15 returns n on its 15 significant decimal digits — the
// precision a spreadsheet shows and rounds on — so binary noise such as
// 0.1 + 0.2 = 0.30000000000000004 reads as 0.3.
func significant15(n float64) float64 {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return n
	}
	r, err := strconv.ParseFloat(strconv.FormatFloat(n, 'g', 15, 64), 64)
	if err != nil {
		return n
	}
	return r
}

type roundMode int

const (
	roundHalfAway roundMode = iota // ROUND: half away from zero
	roundAway                      // ROUNDUP: away from zero
	roundToward                    // ROUNDDOWN: toward zero
)

// decimalRound rounds n to places decimal places (negative places round to
// tens, hundreds, ...) on its 15 significant digits with exact decimal
// arithmetic, so ROUND(1.005, 2) is 1.01, ROUND(2.675, 2) is 2.68 and
// ROUNDUP(0.1 + 0.2, 1) is 0.3, as in a spreadsheet — never the artefacts of
// scaling a binary float. places truncates to a whole number.
func decimalRound(n, places float64, mode roundMode) float64 {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return n
	}
	p := int64(math.Max(-330, math.Min(330, math.Trunc(places))))
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(n, 'g', 15, 64))
	if !ok {
		return n
	}
	exp := p
	if exp < 0 {
		exp = -exp
	}
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(exp), nil))
	if p >= 0 {
		r.Mul(r, scale)
	} else {
		r.Quo(r, scale)
	}
	q, m := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int)) // truncated toward zero
	if m.Sign() != 0 {
		step := big.NewInt(int64(r.Sign()))
		switch mode {
		case roundAway:
			q.Add(q, step)
		case roundHalfAway:
			twice := new(big.Int).Abs(m)
			twice.Lsh(twice, 1)
			if twice.Cmp(r.Denom()) >= 0 {
				q.Add(q, step)
			}
		case roundToward:
			// the truncated quotient is already rounded toward zero
		}
	}
	out := new(big.Rat).SetInt(q)
	if p >= 0 {
		out.Quo(out, scale)
	} else {
		out.Mul(out, scale)
	}
	f, _ := out.Float64()
	if f == 0 {
		return 0 // never -0
	}
	return f
}

// numberText15 renders n as text the way a formula shows a number: its 15
// significant digits, without an exponent between 1e-9 and 1e21 (so
// "Total " & 1234567 is "Total 1234567", and 0.1 + 0.2 is "0.3").
func numberText15(n float64) string {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return strconv.FormatFloat(n, 'g', -1, 64)
	}
	r := significant15(n)
	if r == 0 {
		return "0"
	}
	if a := math.Abs(r); a >= 1e21 || a < 1e-9 {
		return strconv.FormatFloat(r, 'g', -1, 64)
	}
	return strconv.FormatFloat(r, 'f', -1, 64)
}
