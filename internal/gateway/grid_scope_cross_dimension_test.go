package gateway

import (
	"fmt"
	"net/url"
	"testing"
)

// A scope pin on a dimension that other dimensions roll up INTO
// (parent_dimension_id) must reach the facts keyed by those dimensions:
// the rollup fixture's amounts are keyed by staff, staff rolls up into
// departments, so pinning departments=DEPT_A is the two A staff (150), not
// nothing. Before 2026-09-21 the pin matched facts by the pinned dimension's
// own key only, every staff fact was excluded, and a regional total on the
// Regional Expense Planning demo read "$0" (see internal/rollup for the
// chain rule this now shares).
func TestGridScopeReachesDimensionsThatRollUpIntoThePin(t *testing.T) {
	f := setupRollupFixture(t)
	const dev = "rollup-test-approver"

	totals := func(scope map[string]string) map[string]float64 {
		t.Helper()
		q := url.Values{"grid_id": {f.gridDeptsID}, "totals_only": {"1"}}
		if scope != nil {
			b := "{"
			for k, v := range scope {
				b += fmt.Sprintf("%q:%q,", k, v)
			}
			q.Set("scope", b[:len(b)-1]+"}")
		}
		code, body := f.do(t, "GET", "/api/grid?"+q.Encode(), dev, nil)
		if code != 200 {
			t.Fatalf("grid: %d %v", code, body)
		}
		out := map[string]float64{}
		for k, v := range body["totals"].(map[string]any) {
			out[k] = v.(float64)
		}
		return out
	}

	if all := totals(nil); all[f.amountMetricID] != 350 {
		t.Fatalf("unscoped amount = %v, want 350", all[f.amountMetricID])
	}
	if a := totals(map[string]string{f.deptsDimID: "DEPT_A"}); a[f.amountMetricID] != 150 {
		t.Fatalf("departments=DEPT_A amount = %v, want 150 (the staff under it)", a[f.amountMetricID])
	}
	if b := totals(map[string]string{f.deptsDimID: "DEPT_B"}); b[f.amountMetricID] != 200 {
		t.Fatalf("departments=DEPT_B amount = %v, want 200", b[f.amountMetricID])
	}
	// A pin on the leaf dimension itself still works as before.
	if s := totals(map[string]string{f.staffDimID: "STAFF_A2"}); s[f.amountMetricID] != 50 {
		t.Fatalf("staff=STAFF_A2 amount = %v, want 50", s[f.amountMetricID])
	}
}
