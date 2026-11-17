package gateway

import (
	"strings"
	"testing"
)

// A number field's accepted values (min, max, whole) refuse a record outside
// them — saved by hand or loaded from a file — naming the field. A CRM
// intent of 7 and an FTE of 1.5 came in through records unchecked, though
// the metrics they post into refused the same values typed (Territory &
// Quota build, 2026-11-16).
func TestNumberFieldAcceptedValues(t *testing.T) {
	d := setupSalesDemo(t)
	table := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{
		"name": "accounts", "label": "Accounts",
		"fields": []map[string]any{
			{"name": "name", "label": "Account", "type": "text", "required": true},
			{"name": "intent", "label": "Intent (1–5)", "type": "number", "min": 1, "max": 5, "whole": true},
			{"name": "fte", "label": "FTE", "type": "number", "min": 0, "max": 1},
		},
	}))
	for _, c := range []struct {
		data map[string]any
		want string // "" accepted
	}{
		{map[string]any{"name": "A", "intent": 3, "fte": 0.8}, ""},
		{map[string]any{"name": "B", "intent": 7}, "Intent (1–5) takes a whole number from 1 to 5, not 7"},
		{map[string]any{"name": "C", "intent": 2.5}, "not 2.5"},
		{map[string]any{"name": "D", "fte": 1.5}, "FTE takes a number from 0 to 1, not 1.5"},
	} {
		status, raw := d.req("POST", "/api/forms/"+table+"/records", d.dev, map[string]any{"data": c.data})
		switch {
		case c.want == "" && status >= 300:
			t.Errorf("record %v: %d %s, want it saved", c.data, status, raw)
		case c.want != "" && (status < 400 || !strings.Contains(string(raw), c.want)):
			t.Errorf("record %v: %d %s, want a refusal saying %q", c.data, status, raw, c.want)
		}
	}
	status, raw := d.req("POST", "/api/forms/"+table+"/import", d.dev, map[string]any{
		"csv": "Account,Intent (1–5),FTE\nE,4,1\nF,0,1\n"})
	if status < 400 || !strings.Contains(string(raw), "not 0") {
		t.Errorf("a file with an intent of 0: %d %s, want it refused", status, raw)
	}
	// What a table can be given: a range on a number field only, lowest
	// not above highest.
	for _, f := range []map[string]any{
		{"name": "n", "label": "N", "type": "text", "max": 3},
		{"name": "n", "label": "N", "type": "number", "min": 5, "max": 1},
	} {
		if status, raw := d.req("POST", "/api/forms", d.dev, map[string]any{"name": "bad", "label": "Bad", "fields": []map[string]any{f}}); status < 400 {
			t.Errorf("a table with field %v: %d %s, want it refused", f, status, raw)
		}
	}
}
