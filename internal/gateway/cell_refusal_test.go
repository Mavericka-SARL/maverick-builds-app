package gateway

import (
	"strconv"
	"strings"
	"testing"
)

// TestCellWriteRefusalNamesTheCell: a write an access rule refuses says
// which metric or member refuses it and why — it answered "access denied"
// alone (2026-10-20) — and never names what is hidden from the writer.
func TestCellWriteRefusalNamesTheCell(t *testing.T) {
	d := setupSalesDemo(t)
	d.seedFacts()
	d.setAccessRules(d.westRepID,
		map[string]string{"rule_type": "metric", "ref_id": d.metric["target"], "access": "read"},
		map[string]string{"rule_type": "metric", "ref_id": d.metric["cost"], "access": "hidden"},
		member(d.geo["US"], "read"),
		member(d.geo["AMER"], "hidden"),
	)
	for _, tc := range []struct {
		label, metric, geo string
		want               []string
		not                []string
	}{
		{"a read-only metric", "target", "UK", []string{"403", "target is read-only for you"}, nil},
		{"a hidden metric", "cost", "UK", []string{"403", "this metric is hidden from you"}, []string{"cost"}},
		{"a read-only member", "revenue", "US", []string{"403", "geography US is"}, nil},
		{"a member under a hidden one", "revenue", "CA", []string{"403", "the member of geography this write names is hidden from you"}, []string{"AMER", "CA"}},
	} {
		status, raw := d.writeCell(d.westRep, tc.metric, tc.geo, "LAPTOP", "Q1", 1)
		got := strconv.Itoa(status) + " " + string(raw)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %s, want %q in it", tc.label, got, w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(got, n) {
				t.Errorf("%s: %s names %q, which is hidden from the writer", tc.label, got, n)
			}
		}
	}
}
