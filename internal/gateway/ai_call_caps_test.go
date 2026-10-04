package gateway

import "testing"

// The caps are deployment settings, today's values by default; a value that
// is not a positive whole number keeps the default.
func TestLLMCallCapsAreSettings(t *testing.T) {
	t.Setenv("AI_MAX_CALLS_PER_SESSION", "")
	t.Setenv("AI_MAX_CALLS_PER_DAY", "")
	if s, d := llmCallCaps(); s != 50 || d != 200 {
		t.Errorf("defaults = %d, %d; want 50, 200", s, d)
	}
	t.Setenv("AI_MAX_CALLS_PER_SESSION", "150")
	t.Setenv("AI_MAX_CALLS_PER_DAY", "1000")
	if s, d := llmCallCaps(); s != 150 || d != 1000 {
		t.Errorf("set = %d, %d; want 150, 1000", s, d)
	}
	t.Setenv("AI_MAX_CALLS_PER_SESSION", "-3")
	t.Setenv("AI_MAX_CALLS_PER_DAY", "lots")
	if s, d := llmCallCaps(); s != 50 || d != 200 {
		t.Errorf("invalid values = %d, %d; want the defaults", s, d)
	}
}

// A placeholder without a step number used to become the most recently
// created id — every KPI tile of a live dashboard plan pointed at the
// dashboard itself. A placeholder naming something becomes that name.
func TestResolveParamRefs(t *testing.T) {
	created := []string{"dash-1", ""}
	for in, want := range map[string]string{
		`{"dashboard_id":"<created in step 1>"}`:               `{"dashboard_id":"dash-1"}`,
		`{"ref_id":"<rolling_revenue_forecast id>"}`:           `{"ref_id":"rolling_revenue_forecast"}`,
		`{"ref_id":"<gross_profit_id>"}`:                       `{"ref_id":"gross_profit"}`,
		`{"ref_id":"<grid id for revenue by region>"}`:         `{"ref_id":"<grid id for revenue by region>"}`,
		`{"metric_ids":["<ebitda id>","<created in step 1>"]}`: `{"metric_ids":["ebitda","dash-1"]}`,
	} {
		if got := string(resolveParamRefs([]byte(in), created)); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}
