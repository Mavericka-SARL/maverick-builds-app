package formula

import (
	"fmt"
	"math"
	"strings"
)

type Kind int

const (
	KindNumber Kind = iota
	KindString
	KindBool
	KindBlank
	KindError
)

type FormulaError struct {
	Code    string
	Message string
}

func (e *FormulaError) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

var (
	ErrDiv0  = &FormulaError{Code: "#DIV/0!", Message: "Division by zero"}
	ErrValue = &FormulaError{Code: "#VALUE!", Message: "Wrong value type"}
	ErrRef   = &FormulaError{Code: "#REF!", Message: "Invalid reference"}
	ErrName  = &FormulaError{Code: "#NAME?", Message: "Unknown name"}
	ErrNA    = &FormulaError{Code: "#N/A", Message: "Value not available"}
	ErrNum   = &FormulaError{Code: "#NUM!", Message: "Invalid numeric value"}
)

func errName(name string) *FormulaError {
	return &FormulaError{Code: "#NAME?", Message: fmt.Sprintf("Unknown name: %s", name)}
}

func errValue(msg string) *FormulaError {
	return &FormulaError{Code: "#VALUE!", Message: msg}
}

type Value struct {
	kind Kind
	num  float64
	str  string
	b    bool
	err  *FormulaError
}

func NumberVal(n float64) Value      { return Value{kind: KindNumber, num: n} }
func StringVal(s string) Value       { return Value{kind: KindString, str: s} }
func BoolVal(b bool) Value           { return Value{kind: KindBool, b: b} }
func BlankVal() Value                { return Value{kind: KindBlank} }
func ErrorVal(e *FormulaError) Value { return Value{kind: KindError, err: e} }

func (v Value) Kind() Kind         { return v.kind }
func (v Value) IsError() bool      { return v.kind == KindError }
func (v Value) IsBlank() bool      { return v.kind == KindBlank }
func (v Value) Err() *FormulaError { return v.err }

func (v Value) Number() (float64, bool) {
	switch v.kind {
	case KindNumber:
		return v.num, true
	case KindBool:
		if v.b {
			return 1, true
		}
		return 0, true
	case KindBlank:
		return 0, true
	case KindString, KindError:
		return 0, false
	}
	return 0, false
}

func (v Value) String() string {
	switch v.kind {
	case KindNumber:
		if v.num == math.Trunc(v.num) && !math.IsInf(v.num, 0) {
			return fmt.Sprintf("%g", v.num)
		}
		return fmt.Sprintf("%g", v.num)
	case KindString:
		return v.str
	case KindBool:
		if v.b {
			return "TRUE"
		}
		return "FALSE"
	case KindBlank:
		return ""
	case KindError:
		return v.err.Code
	}
	return ""
}

func (v Value) Bool() bool {
	switch v.kind {
	case KindBool:
		return v.b
	case KindNumber:
		return v.num != 0
	case KindString:
		return strings.EqualFold(v.str, "true")
	case KindBlank:
		return false
	case KindError:
		return false
	}
	return false
}

func compareValues(a, b Value) (int, *FormulaError) {
	if a.kind == KindError {
		return 0, a.err
	}
	if b.kind == KindError {
		return 0, b.err
	}
	// Both numbers or coercible
	an, aok := a.Number()
	bn, bok := b.Number()
	if aok && bok && a.kind != KindString && b.kind != KindString {
		if an < bn {
			return -1, nil
		}
		if an > bn {
			return 1, nil
		}
		return 0, nil
	}
	// String comparison (case-insensitive like Excel)
	as := strings.ToUpper(a.String())
	bs := strings.ToUpper(b.String())
	if as < bs {
		return -1, nil
	}
	if as > bs {
		return 1, nil
	}
	return 0, nil
}
