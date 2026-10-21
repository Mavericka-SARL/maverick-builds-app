package calculation

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
)

// TestDimKeyStableRegardlessOfInsertionOrder guards the exact bug class
// LoadInputValueMap/LoadCalcValueMap's keying exists to avoid: Postgres's
// own JSONB key ordering is not guaranteed to match Go's encoding/json
// (which sorts map keys deterministically), so both the map-building side
// and the lookup side must go through this same function to ever agree.
func TestDimKeyStableRegardlessOfInsertionOrder(t *testing.T) {
	a := map[string]string{"dept": "SALES", "period": "JAN"}
	b := map[string]string{"period": "JAN", "dept": "SALES"} // same content, built in a different order

	ka, kb := dimKey(a), dimKey(b)
	if ka != kb {
		t.Errorf("dimKey should be insertion-order-independent, got %q vs %q", ka, kb)
	}
}

func TestDimKeyDistinguishesDifferentCombos(t *testing.T) {
	a := dimKey(map[string]string{"dept": "SALES"})
	b := dimKey(map[string]string{"dept": "ENG"})
	if a == b {
		t.Errorf("dimKey collapsed two distinct combos to the same key: %q", a)
	}
}

func TestDimKeyEmptyCombo(t *testing.T) {
	if got := dimKey(map[string]string{}); got != "{}" {
		t.Errorf("dimKey(empty) = %q, want \"{}\"", got)
	}
	if got := dimKey(nil); got != "null" {
		t.Errorf("dimKey(nil) = %q, want \"null\" (json.Marshal's own nil-map behavior — LoadInputValueMap/LoadCalcValueMap never pass a nil map, only Resolve's zero-dimension shortcut ever calls fetch with map[string]string{})", got)
	}
}

// TestDimKeyIsJSONMarshal: dimKey's direct path writes exactly what
// json.Marshal writes, and the codes it cannot write directly (escapes,
// HTML characters, non-ASCII, more than eight dimensions) take
// json.Marshal itself — rows written and rows read must meet on one key.
func TestDimKeyIsJSONMarshal(t *testing.T) {
	odd := []string{"", "A&B", "<x>", `q"uote`, `back\slash`, "tab\t", "é", "日本", " ", "plain-01", "2026-07"}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		m := map[string]string{}
		for j := rng.Intn(11); j > 0; j-- {
			k := fmt.Sprintf("%08x-dim-%d", rng.Uint32(), rng.Intn(3))
			v := fmt.Sprintf("M%d", rng.Intn(50))
			if rng.Intn(6) == 0 {
				v = odd[rng.Intn(len(odd))]
			}
			if rng.Intn(15) == 0 {
				k = odd[rng.Intn(len(odd))]
			}
			m[k] = v
		}
		want, _ := json.Marshal(m)
		if got := dimKey(m); got != string(want) {
			t.Fatalf("dimKey(%v) = %s, json.Marshal = %s", m, got, want)
		}
	}
	if got := dimKey(nil); got != "null" {
		t.Errorf("dimKey(nil) = %s, json.Marshal writes null", got)
	}
}

// TestKeyOfReadsPostgresText: keyOf gives dimKey of the combo for
// PostgreSQL's jsonb text, its own spacing and key order included, and
// takes json.Unmarshal's answer for everything its direct path declines.
func TestKeyOfReadsPostgresText(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
		ok   bool
	}{
		{`{}`, `{}`, true},
		{`{"b": "2", "a": "1"}`, `{"a":"1","b":"2"}`, true},
		{`{"a":"1","b":"2"}`, `{"a":"1","b":"2"}`, true},
		{`{"region": "EU", "dept": "R&D"}`, "MARSHAL", true},
		{`{"a": "q\"x"}`, `{"a":"q\"x"}`, true},
		{`{"a": "é"}`, `{"a":"é"}`, true},
		{`{"a": 1}`, ``, false},
		{`null`, `null`, true},
		{`{"a": "1"} x`, ``, false},
		{`{"a": "1", "a": "2"}`, `{"a":"2"}`, true},
	} {
		got, ok := keyOf(tc.raw)
		if tc.want == "MARSHAL" {
			var m map[string]string
			_ = json.Unmarshal([]byte(tc.raw), &m)
			b, _ := json.Marshal(m)
			tc.want = string(b)
		}
		if ok != tc.ok || got != tc.want {
			t.Errorf("keyOf(%s) = %s %v, want %s %v", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 3000; i++ {
		m := map[string]string{}
		for j := rng.Intn(9); j > 0; j-- {
			m[fmt.Sprintf("%08x-%04x-4000-8000-%012x", rng.Uint32(), rng.Intn(65536), rng.Int63n(1<<40))] = fmt.Sprintf("M%d", rng.Intn(60))
		}
		// PostgreSQL's order: by length, then bytes; ", " and ": ".
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		for a := 1; a < len(keys); a++ {
			for b := a; b > 0 && (len(keys[b]) < len(keys[b-1]) || len(keys[b]) == len(keys[b-1]) && keys[b] < keys[b-1]); b-- {
				keys[b], keys[b-1] = keys[b-1], keys[b]
			}
		}
		raw := "{"
		for j, k := range keys {
			if j > 0 {
				raw += ", "
			}
			raw += fmt.Sprintf("%q: %q", k, m[k])
		}
		raw += "}"
		if got, ok := keyOf(raw); !ok || got != dimKey(m) {
			t.Fatalf("keyOf(%s) = %s %v, want %s", raw, got, ok, dimKey(m))
		}
	}
}
