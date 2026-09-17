package formula

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// builtins maps UPPER-CASE function names to implementations.
// Lazy functions receive unevaluated []Node; eager helpers call evalArgs.
var builtins map[string]CustomFunc

func init() {
	builtins = map[string]CustomFunc{
		// Logic — lazy
		"IF":      fnIF,
		"IFS":     fnIFS,
		"AND":     fnAND,
		"OR":      fnOR,
		"NOT":     fnNOT,
		"IFERROR": fnIFERROR,
		"IFNA":    fnIFNA,
		"SWITCH":  fnSWITCH,

		// Math
		"ABS":       fnABS,
		"INT":       fnINT,
		"ROUND":     fnROUND,
		"ROUNDUP":   fnROUNDUP,
		"ROUNDDOWN": fnROUNDDOWN,
		"CEILING":   fnCEILING,
		"FLOOR":     fnFLOOR,
		"MOD":       fnMOD,
		"POWER":     fnPOWER,
		"SQRT":      fnSQRT,

		// Aggregation
		"SUM":     fnSUM,
		"AVERAGE": fnAVERAGE,
		"MIN":     fnMIN,
		"MAX":     fnMAX,
		"COUNT":   fnCOUNT,
		"COUNTA":  fnCOUNTA,

		// Text
		"CONCAT":     fnCONCAT,
		"TEXTJOIN":   fnTEXTJOIN,
		"LEN":        fnLEN,
		"LEFT":       fnLEFT,
		"RIGHT":      fnRIGHT,
		"MID":        fnMID,
		"UPPER":      fnUPPER,
		"LOWER":      fnLOWER,
		"TRIM":       fnTRIM,
		"TEXT":       fnTEXT,
		"SUBSTITUTE": fnSUBSTITUTE,

		// Date
		"TODAY":   fnTODAY,
		"DATE":    fnDATE,
		"YEAR":    fnYEAR,
		"MONTH":   fnMONTH,
		"DAY":     fnDAY,
		"DAYS":    fnDAYS,
		"EDATE":   fnEDATE,
		"EOMONTH": fnEOMONTH,
	}
}

// ── Logic ────────────────────────────────────────────────────────────────────

func fnIF(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("IF", args, 2, 3); ferr != nil {
		return ErrorVal(ferr)
	}
	cond := ctx.eval(args[0])
	if cond.IsError() {
		return cond
	}
	if cond.Bool() {
		return ctx.eval(args[1])
	}
	if len(args) == 3 {
		return ctx.eval(args[2])
	}
	return BoolVal(false)
}

func fnIFS(ctx *EvalContext, args []Node) Value {
	if len(args) < 2 || len(args)%2 != 0 {
		return ErrorVal(errValue("IFS: requires pairs of condition, value"))
	}
	for i := 0; i < len(args); i += 2 {
		cond := ctx.eval(args[i])
		if cond.IsError() {
			return cond
		}
		if cond.Bool() {
			return ctx.eval(args[i+1])
		}
	}
	return ErrorVal(ErrNA)
}

func fnAND(ctx *EvalContext, args []Node) Value {
	if len(args) == 0 {
		return ErrorVal(errValue("AND: requires at least one argument"))
	}
	for _, a := range args {
		v := ctx.eval(a)
		if v.IsError() {
			return v
		}
		if !v.Bool() {
			return BoolVal(false)
		}
	}
	return BoolVal(true)
}

func fnOR(ctx *EvalContext, args []Node) Value {
	if len(args) == 0 {
		return ErrorVal(errValue("OR: requires at least one argument"))
	}
	for _, a := range args {
		v := ctx.eval(a)
		if v.IsError() {
			return v
		}
		if v.Bool() {
			return BoolVal(true)
		}
	}
	return BoolVal(false)
}

func fnNOT(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("NOT", args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() {
		return v
	}
	return BoolVal(!v.Bool())
}

func fnIFERROR(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("IFERROR", args, 2, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() {
		return ctx.eval(args[1])
	}
	return v
}

func fnIFNA(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("IFNA", args, 2, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() && v.err.Code == "#N/A" {
		return ctx.eval(args[1])
	}
	return v
}

func fnSWITCH(ctx *EvalContext, args []Node) Value {
	if len(args) < 3 {
		return ErrorVal(errValue("SWITCH: requires at least 3 arguments"))
	}
	expr := ctx.eval(args[0])
	if expr.IsError() {
		return expr
	}
	// pairs: value, result; optional default at end (odd total args)
	i := 1
	for i+1 < len(args) {
		matchVal := ctx.eval(args[i])
		if matchVal.IsError() {
			return matchVal
		}
		cmp, ferr := compareValues(expr, matchVal)
		if ferr != nil {
			return ErrorVal(ferr)
		}
		if cmp == 0 {
			return ctx.eval(args[i+1])
		}
		i += 2
	}
	if i < len(args) {
		// default value
		return ctx.eval(args[i])
	}
	return ErrorVal(ErrNA)
}

// ── Math ─────────────────────────────────────────────────────────────────────

func fnABS(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("ABS", args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() {
		return v
	}
	n, ok := v.Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	return NumberVal(math.Abs(n))
}

func fnINT(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("INT", args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() {
		return v
	}
	n, ok := v.Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	return NumberVal(math.Floor(n))
}

func fnROUND(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("ROUND", args, 2, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	n, ok := vals[0].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	places, ok := vals[1].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	factor := math.Pow(10, places)
	return NumberVal(math.Round(n*factor) / factor)
}

func fnROUNDUP(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("ROUNDUP", args, 2, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	n, ok := vals[0].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	places, ok := vals[1].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	factor := math.Pow(10, places)
	if n >= 0 {
		return NumberVal(math.Ceil(n*factor) / factor)
	}
	return NumberVal(math.Floor(n*factor) / factor)
}

func fnROUNDDOWN(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("ROUNDDOWN", args, 2, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	n, ok := vals[0].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	places, ok := vals[1].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	factor := math.Pow(10, places)
	if n >= 0 {
		return NumberVal(math.Floor(n*factor) / factor)
	}
	return NumberVal(math.Ceil(n*factor) / factor)
}

func fnCEILING(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("CEILING", args, 2, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	n, ok := vals[0].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	sig, ok := vals[1].Number()
	if !ok || sig == 0 {
		return ErrorVal(ErrDiv0)
	}
	return NumberVal(math.Ceil(n/sig) * sig)
}

func fnFLOOR(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("FLOOR", args, 2, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	n, ok := vals[0].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	sig, ok := vals[1].Number()
	if !ok || sig == 0 {
		return ErrorVal(ErrDiv0)
	}
	return NumberVal(math.Floor(n/sig) * sig)
}

func fnMOD(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("MOD", args, 2, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	n, ok := vals[0].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	d, ok := vals[1].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	if d == 0 {
		return ErrorVal(ErrDiv0)
	}
	result := n - math.Floor(n/d)*d
	return NumberVal(result)
}

func fnPOWER(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("POWER", args, 2, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	base, ok := vals[0].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	exp, ok := vals[1].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	result := math.Pow(base, exp)
	if math.IsNaN(result) || math.IsInf(result, 0) {
		return ErrorVal(ErrNum)
	}
	return NumberVal(result)
}

func fnSQRT(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("SQRT", args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() {
		return v
	}
	n, ok := v.Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	if n < 0 {
		return ErrorVal(ErrNum)
	}
	return NumberVal(math.Sqrt(n))
}

// ── Aggregation ───────────────────────────────────────────────────────────────

func collectNumbers(ctx *EvalContext, args []Node) ([]float64, Value) {
	var nums []float64
	for _, a := range args {
		v := ctx.eval(a)
		if v.IsError() {
			return nil, v
		}
		if v.IsBlank() {
			continue
		}
		n, ok := v.Number()
		if !ok {
			return nil, ErrorVal(ErrValue)
		}
		nums = append(nums, n)
	}
	return nums, Value{}
}

func fnSUM(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("SUM", args, 1, -1); ferr != nil {
		return ErrorVal(ferr)
	}
	nums, errv := collectNumbers(ctx, args)
	if errv.IsError() {
		return errv
	}
	var sum float64
	for _, n := range nums {
		sum += n
	}
	return NumberVal(sum)
}

func fnAVERAGE(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("AVERAGE", args, 1, -1); ferr != nil {
		return ErrorVal(ferr)
	}
	nums, errv := collectNumbers(ctx, args)
	if errv.IsError() {
		return errv
	}
	if len(nums) == 0 {
		return ErrorVal(ErrDiv0)
	}
	var sum float64
	for _, n := range nums {
		sum += n
	}
	return NumberVal(sum / float64(len(nums)))
}

func fnMIN(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("MIN", args, 1, -1); ferr != nil {
		return ErrorVal(ferr)
	}
	nums, errv := collectNumbers(ctx, args)
	if errv.IsError() {
		return errv
	}
	if len(nums) == 0 {
		return NumberVal(0)
	}
	min := nums[0]
	for _, n := range nums[1:] {
		if n < min {
			min = n
		}
	}
	return NumberVal(min)
}

func fnMAX(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("MAX", args, 1, -1); ferr != nil {
		return ErrorVal(ferr)
	}
	nums, errv := collectNumbers(ctx, args)
	if errv.IsError() {
		return errv
	}
	if len(nums) == 0 {
		return NumberVal(0)
	}
	max := nums[0]
	for _, n := range nums[1:] {
		if n > max {
			max = n
		}
	}
	return NumberVal(max)
}

func fnCOUNT(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("COUNT", args, 1, -1); ferr != nil {
		return ErrorVal(ferr)
	}
	var count float64
	for _, a := range args {
		v := ctx.eval(a)
		if v.IsError() {
			continue
		}
		if _, ok := v.Number(); ok && !v.IsBlank() {
			count++
		}
	}
	return NumberVal(count)
}

func fnCOUNTA(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("COUNTA", args, 1, -1); ferr != nil {
		return ErrorVal(ferr)
	}
	var count float64
	for _, a := range args {
		v := ctx.eval(a)
		if v.IsError() {
			continue
		}
		if !v.IsBlank() {
			count++
		}
	}
	return NumberVal(count)
}

// ── Text ──────────────────────────────────────────────────────────────────────

func fnCONCAT(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("CONCAT", args, 1, -1); ferr != nil {
		return ErrorVal(ferr)
	}
	var sb strings.Builder
	for _, a := range args {
		v := ctx.eval(a)
		if v.IsError() {
			return v
		}
		sb.WriteString(v.String())
	}
	return StringVal(sb.String())
}

func fnTEXTJOIN(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("TEXTJOIN", args, 3, -1); ferr != nil {
		return ErrorVal(ferr)
	}
	delimVal := ctx.eval(args[0])
	if delimVal.IsError() {
		return delimVal
	}
	ignoreEmptyVal := ctx.eval(args[1])
	if ignoreEmptyVal.IsError() {
		return ignoreEmptyVal
	}
	delim := delimVal.String()
	ignoreEmpty := ignoreEmptyVal.Bool()

	var parts []string
	for _, a := range args[2:] {
		v := ctx.eval(a)
		if v.IsError() {
			return v
		}
		s := v.String()
		if ignoreEmpty && s == "" {
			continue
		}
		parts = append(parts, s)
	}
	return StringVal(strings.Join(parts, delim))
}

func fnLEN(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("LEN", args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() {
		return v
	}
	return NumberVal(float64(utf8.RuneCountInString(v.String())))
}

func fnLEFT(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("LEFT", args, 1, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() {
		return v
	}
	runes := []rune(v.String())
	n := 1
	if len(args) == 2 {
		nv := ctx.eval(args[1])
		if nv.IsError() {
			return nv
		}
		num, ok := nv.Number()
		if !ok {
			return ErrorVal(ErrValue)
		}
		n = int(num)
	}
	if n < 0 {
		return ErrorVal(ErrValue)
	}
	if n > len(runes) {
		n = len(runes)
	}
	return StringVal(string(runes[:n]))
}

func fnRIGHT(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("RIGHT", args, 1, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() {
		return v
	}
	runes := []rune(v.String())
	n := 1
	if len(args) == 2 {
		nv := ctx.eval(args[1])
		if nv.IsError() {
			return nv
		}
		num, ok := nv.Number()
		if !ok {
			return ErrorVal(ErrValue)
		}
		n = int(num)
	}
	if n < 0 {
		return ErrorVal(ErrValue)
	}
	if n > len(runes) {
		n = len(runes)
	}
	return StringVal(string(runes[len(runes)-n:]))
}

func fnMID(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("MID", args, 3, 3); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	runes := []rune(vals[0].String())
	startNum, ok := vals[1].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	numChars, ok := vals[2].Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	start := int(startNum) - 1 // Excel 1-based
	if start < 0 {
		start = 0
	}
	if start >= len(runes) {
		return StringVal("")
	}
	end := start + int(numChars)
	if end > len(runes) {
		end = len(runes)
	}
	return StringVal(string(runes[start:end]))
}

func fnUPPER(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("UPPER", args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() {
		return v
	}
	return StringVal(strings.ToUpper(v.String()))
}

func fnLOWER(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("LOWER", args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() {
		return v
	}
	return StringVal(strings.ToLower(v.String()))
}

func fnTRIM(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("TRIM", args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() {
		return v
	}
	// Excel TRIM removes leading/trailing and collapses internal runs
	words := strings.Fields(v.String())
	return StringVal(strings.Join(words, " "))
}

func fnTEXT(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("TEXT", args, 2, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	n, ok := vals[0].Number()
	fmtStr := vals[1].String()
	if !ok {
		return StringVal(vals[0].String())
	}
	// Basic format support
	upper := strings.ToUpper(fmtStr)
	switch {
	case upper == "0" || upper == "#":
		return StringVal(fmt.Sprintf("%.0f", n))
	case upper == "0.00":
		return StringVal(fmt.Sprintf("%.2f", n))
	case upper == "0.0":
		return StringVal(fmt.Sprintf("%.1f", n))
	case strings.Contains(upper, "YYYY") && strings.Contains(upper, "MM"):
		// Date formatting from serial number — simplified
		t := serialToTime(n)
		result := fmtStr
		result = strings.ReplaceAll(result, "YYYY", fmt.Sprintf("%04d", t.Year()))
		result = strings.ReplaceAll(result, "yyyy", fmt.Sprintf("%04d", t.Year()))
		result = strings.ReplaceAll(result, "MM", fmt.Sprintf("%02d", t.Month()))
		result = strings.ReplaceAll(result, "DD", fmt.Sprintf("%02d", t.Day()))
		result = strings.ReplaceAll(result, "dd", fmt.Sprintf("%02d", t.Day()))
		return StringVal(result)
	}
	return StringVal(fmt.Sprintf("%g", n))
}

func fnSUBSTITUTE(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("SUBSTITUTE", args, 3, 4); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	text := vals[0].String()
	oldText := vals[1].String()
	newText := vals[2].String()
	if len(args) == 4 {
		// Replace only the nth occurrence
		nthNum, ok := vals[3].Number()
		if !ok {
			return ErrorVal(ErrValue)
		}
		nth := int(nthNum)
		count := 0
		idx := 0
		var sb strings.Builder
		for {
			i := strings.Index(text[idx:], oldText)
			if i < 0 {
				sb.WriteString(text[idx:])
				break
			}
			count++
			if count == nth {
				sb.WriteString(text[idx : idx+i])
				sb.WriteString(newText)
				idx += i + len(oldText)
			} else {
				sb.WriteString(text[idx : idx+i+len(oldText)])
				idx += i + len(oldText)
			}
		}
		return StringVal(sb.String())
	}
	return StringVal(strings.ReplaceAll(text, oldText, newText))
}

// ── Date ─────────────────────────────────────────────────────────────────────

// Excel serial date: Jan 1 1900 = 1 (with the 1900 leap year bug baked in)
var excelEpoch = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)

func timeToSerial(t time.Time) float64 {
	days := t.Sub(excelEpoch).Hours() / 24
	return days
}

func serialToTime(serial float64) time.Time {
	return excelEpoch.Add(time.Duration(serial * 24 * float64(time.Hour)))
}

func fnTODAY(ctx *EvalContext, args []Node) Value {
	t := time.Now().UTC().Truncate(24 * time.Hour)
	return NumberVal(timeToSerial(t))
}

func fnDATE(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("DATE", args, 3, 3); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	y, ok1 := vals[0].Number()
	m, ok2 := vals[1].Number()
	d, ok3 := vals[2].Number()
	if !ok1 || !ok2 || !ok3 {
		return ErrorVal(ErrValue)
	}
	t := time.Date(int(y), time.Month(int(m)), int(d), 0, 0, 0, 0, time.UTC)
	return NumberVal(timeToSerial(t))
}

func fnYEAR(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("YEAR", args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() {
		return v
	}
	n, ok := v.Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	return NumberVal(float64(serialToTime(n).Year()))
}

func fnMONTH(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("MONTH", args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() {
		return v
	}
	n, ok := v.Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	return NumberVal(float64(serialToTime(n).Month()))
}

func fnDAY(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("DAY", args, 1, 1); ferr != nil {
		return ErrorVal(ferr)
	}
	v := ctx.eval(args[0])
	if v.IsError() {
		return v
	}
	n, ok := v.Number()
	if !ok {
		return ErrorVal(ErrValue)
	}
	return NumberVal(float64(serialToTime(n).Day()))
}

func fnDAYS(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("DAYS", args, 2, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	end, ok1 := vals[0].Number()
	start, ok2 := vals[1].Number()
	if !ok1 || !ok2 {
		return ErrorVal(ErrValue)
	}
	return NumberVal(end - start)
}

func fnEDATE(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("EDATE", args, 2, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	serial, ok1 := vals[0].Number()
	months, ok2 := vals[1].Number()
	if !ok1 || !ok2 {
		return ErrorVal(ErrValue)
	}
	t := serialToTime(serial)
	t = t.AddDate(0, int(months), 0)
	return NumberVal(timeToSerial(t))
}

func fnEOMONTH(ctx *EvalContext, args []Node) Value {
	if ferr := requireArgCount("EOMONTH", args, 2, 2); ferr != nil {
		return ErrorVal(ferr)
	}
	vals, errv := ctx.evalArgs(args)
	if errv.IsError() {
		return errv
	}
	serial, ok1 := vals[0].Number()
	months, ok2 := vals[1].Number()
	if !ok1 || !ok2 {
		return ErrorVal(ErrValue)
	}
	t := serialToTime(serial)
	// Move to first day of target month, then go back one day
	t = t.AddDate(0, int(months)+1, 0)
	t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	return NumberVal(timeToSerial(t))
}
