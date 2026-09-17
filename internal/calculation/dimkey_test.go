package calculation

import "testing"

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
