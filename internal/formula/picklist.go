package formula

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// Pick-lists: a metric whose format is "picklist" holds, in each cell, a
// member of one dimension — an activity's Region, a Status of Draft /
// Committed / Cancelled, a Yes / No — the way a dimension field of a form
// does. Cells store numbers, so a pick-list cell stores PicklistKey of the
// member's code: the key travels unchanged through revision copies, model
// export and import (members are matched by code there too), and re-keying a
// member's code rewrites it with the facts' own coordinates.
//
// In a formula a pick-list metric reads as the member's CODE (text), never
// as its key: act_status = "Cancelled", LOOKUP(fx, currency, act_currency),
// SUMIFS(sales, act_region, region). A pick-list calculated metric's formula
// gives a member code, which is stored as its key (PicklistResult).

// picklistKeyBase keeps every key at or above 2^50 and below 2^51: far from
// the small numbers a person types, and exact in a float64, a JSON number
// and a JavaScript number.
const picklistKeyBase = 1 << 50

// PicklistKey is the number a pick-list cell stores for the member with
// code: a 50-bit FNV-1a hash of the code, offset by 2^50. The code is taken
// exactly as stored.
func PicklistKey(code string) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(code))
	return float64(picklistKeyBase + h.Sum64()%picklistKeyBase)
}

// PicklistCodec maps a pick-list metric's stored keys to member codes of its
// dimension and back.
type PicklistCodec struct {
	// Dim is the dimension's name.
	Dim string
	// Code returns the code of the member a stored key names; ok is false
	// for a key no member of Dim has.
	Code func(key float64) (code string, ok bool)
	// Key returns the key of the member with code, matched exactly first
	// and then ignoring case; ok is false when Dim has no such member.
	Key func(code string) (key float64, ok bool)
}

// picklist returns the codec of metric when it is a pick-list.
func (d *DimEvalContext) picklist(metric string) (PicklistCodec, bool) {
	if d == nil || d.Picklist == nil {
		return PicklistCodec{}, false
	}
	return d.Picklist(metric)
}

// decodePicklist turns a pick-list metric's stored value into the member
// code it names. Any other metric's value is returned unchanged; a blank
// stays blank, and a key no member has is #N/A.
func (ctx *EvalContext) decodePicklist(metric string, v Value) Value {
	if v.Kind() != KindNumber || ctx == nil {
		return v
	}
	codec, ok := ctx.Dim.picklist(metric)
	if !ok {
		return v
	}
	n, _ := v.Number()
	return decodeKey(codec, metric, n)
}

func decodeKey(codec PicklistCodec, metric string, key float64) Value {
	if key == 0 {
		// Nothing chosen: evaluators bind an absent value as 0, and no key
		// is ever 0.
		return BlankVal()
	}
	if codec.Code == nil {
		return ErrorVal(errValue(fmt.Sprintf("%s is a pick-list of %s, whose members this evaluation does not have", metric, codec.Dim)))
	}
	code, ok := codec.Code(key)
	if !ok {
		return ErrorVal(MemberNotAvailable(fmt.Sprintf("%s holds a member %s no longer has", metric, codec.Dim)))
	}
	return StringVal(code)
}

// PicklistResult converts a pick-list metric's formula result into the
// number its cell stores: a member code (text; a number is read as its
// digits) becomes that member's key, a blank stays blank. A code its
// dimension does not have is #N/A, an error stays as it is.
func PicklistResult(codec PicklistCodec, metric string, v Value) Value {
	if v.IsError() || v.IsBlank() {
		return v
	}
	code, ok := memberCode(v)
	if !ok {
		return BlankVal()
	}
	if codec.Key == nil {
		return ErrorVal(errValue(fmt.Sprintf("%s is a pick-list of %s, whose members this evaluation does not have", metric, codec.Dim)))
	}
	key, ok := codec.Key(strings.TrimSpace(code))
	if !ok {
		return ErrorVal(MemberNotAvailable(fmt.Sprintf("%s is a pick-list of %s, which has no member %q", metric, codec.Dim, code)))
	}
	return NumberVal(key)
}
