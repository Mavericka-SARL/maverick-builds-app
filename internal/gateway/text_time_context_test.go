package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestTextCalculationKnowsItsPeriod: a text calculation on a time dimension
// is evaluated at its period, as a number calculation is — START() and END()
// name the period's dates and PREVIOUS reads a number at the period before.
// Every cell used to fail TIME_CONTEXT_REQUIRED, so the metric showed
// nothing at all.
//
// revenue at (region, month m = 1..6): UK 100+m, DE 200+m, US 300+m.
func TestTextCalculationKnowsItsPeriod(t *testing.T) {
	f := setupRestrictedFixture(t)
	for name, text := range map[string]string{
		"season":   `IF(MONTH(START()) = 1, "Opening", "Month " & MONTH(END()))`,
		"prev_txt": `"was " & PREVIOUS(revenue)`,
	} {
		f.metric[name] = f.dev_("POST", "/api/developer/metrics", map[string]any{"name": name, "formula": text,
			"revision_id": f.revID, "agg_rule": "none", "format": "text", "time_summary": "none"})
		f.dev_("POST", "/api/developer/grids/"+f.plan+"/metrics/"+f.metric[name], nil)
	}
	f.loadMetricDims()
	f.recalc()

	want := map[string]string{
		f.at("season", "UK", "2026-01"):   "Opening",
		f.at("season", "DE", "2026-03"):   "Month 3",
		f.at("prev_txt", "UK", "2026-02"): "was 101",
		f.at("prev_txt", "US", "2026-06"): "was 305",
	}
	var texts map[string]string
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, raw := f.req("GET", "/api/grid?grid_def_id="+f.plan, f.dev, nil)
		if status != http.StatusOK {
			t.Fatalf("grid: %d %s", status, raw)
		}
		var g struct {
			Texts map[string]string `json:"texts"`
		}
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatal(err)
		}
		texts = g.Texts
		done := true
		for k, w := range want {
			done = done && texts[k] == w
		}
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for k, w := range want {
		if texts[k] != w {
			t.Errorf("%s = %q, want %q", k, texts[k], w)
		}
	}
	// January has no period before it: PREVIOUS is blank there, as for a number.
	if v, ok := texts[f.at("prev_txt", "UK", "2026-01")]; ok && v != "was 0" && v != "was " {
		t.Errorf("prev_txt at January = %q", v)
	}
	var calcErr *string
	if err := f.pool.QueryRow(t.Context(), `SELECT calc_error FROM model.metric_def WHERE id=$1::uuid`, f.metric["season"]).Scan(&calcErr); err == nil && calcErr != nil && *calcErr != "" {
		t.Errorf("season calc_error = %q", *calcErr)
	}
}
