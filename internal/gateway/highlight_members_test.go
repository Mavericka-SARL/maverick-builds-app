package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestHighlightRulesForMembers: a highlight rule may be limited to members
// (a variance column) — named by code, of a dimension named or of any —
// is checked against the revision's members, keeps the dimension's own
// spelling, and follows a member's new code when it is renamed.
func TestHighlightRulesForMembers(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()
	patch := func(rules any) (int, string) {
		status, raw := d.req("PATCH", "/api/developer/metrics/"+d.metric["margin"], d.dev, map[string]any{"highlight_rules": rules})
		return status, string(raw)
	}
	rules := []map[string]any{{"op": "<", "value": 0, "tone": "negative", "members": []string{"UK", "DE"}, "dimension": "GEOGRAPHY"}}
	if status, body := patch(rules); status != 200 {
		t.Fatalf("a rule for members: %d %s", status, body)
	}
	stored := func() []map[string]any {
		var raw []byte
		if err := d.pool.QueryRow(ctx, `SELECT highlight_rules FROM model.metric_def WHERE id=$1::uuid`, d.metric["margin"]).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var out []map[string]any
		_ = json.Unmarshal(raw, &out)
		return out
	}
	if got := stored(); len(got) != 1 || got[0]["dimension"] != "geography" {
		t.Fatalf("stored rule %v, want the dimension as it is spelled", got)
	}
	for _, bad := range []map[string]any{
		{"op": "<", "value": 0, "tone": "negative", "members": []string{"MARS"}},
		{"op": "<", "value": 0, "tone": "negative", "members": []string{"LAPTOP"}, "dimension": "geography"},
		{"op": "<", "value": 0, "tone": "negative", "dimension": "geography"},
	} {
		if status, body := patch([]map[string]any{bad}); status < 400 || !strings.Contains(body, "highlight rule") {
			t.Errorf("rule %v: %d %s, want it refused", bad, status, body)
		}
	}
	if status, body := d.req("PATCH", "/api/developer/dimensions/"+d.geoDim+"/members/"+d.geo["UK"], d.dev, map[string]any{"code": "GB", "label": "United Kingdom"}); status != 200 {
		t.Fatalf("rename UK: %d %s", status, body)
	}
	if got := stored(); len(got) != 1 || strings.Join(toStrings(got[0]["members"]), ",") != "GB,DE" {
		t.Errorf("after UK became GB the rule is %v", got)
	}
}

func toStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}
