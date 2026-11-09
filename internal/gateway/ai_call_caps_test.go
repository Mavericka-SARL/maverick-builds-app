package gateway

import (
	"strings"
	"testing"
)

// The caps are deployment settings, today's values by default; a value that
// is not a positive whole number keeps the default.
func TestLLMCallCapsAreSettings(t *testing.T) {
	t.Setenv("AI_MAX_CALLS_PER_SESSION", "")
	t.Setenv("AI_MAX_CALLS_PER_DAY", "")
	if s, d := llmCallCaps(); s != 200 || d != 1000 {
		t.Errorf("defaults = %d, %d; want 200, 1000", s, d)
	}
	t.Setenv("AI_MAX_CALLS_PER_SESSION", "150")
	t.Setenv("AI_MAX_CALLS_PER_DAY", "400")
	if s, d := llmCallCaps(); s != 150 || d != 400 {
		t.Errorf("set = %d, %d; want 150, 400", s, d)
	}
	t.Setenv("AI_MAX_CALLS_PER_SESSION", "-3")
	t.Setenv("AI_MAX_CALLS_PER_DAY", "lots")
	if s, d := llmCallCaps(); s != 200 || d != 1000 {
		t.Errorf("invalid values = %d, %d; want the defaults", s, d)
	}
}

// At the cap, a session with confirmed work says how to carry on from it:
// a new session continues the draft, with its task plan and checked rows.
func TestSessionCapMessageSaysANewSessionContinuesTheDraft(t *testing.T) {
	if m := sessionCapMessage(200, "draft-1"); !strings.Contains(m, "continues that draft") || !strings.Contains(m, "task plan") {
		t.Errorf("with a draft: %q", m)
	}
	if m := sessionCapMessage(200, ""); strings.Contains(m, "promote") || !strings.Contains(m, "new session") {
		t.Errorf("without a draft: %q", m)
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

// From 70% of its calls a session asks the model to suggest a new one.
func TestSessionLengthNote(t *testing.T) {
	if n := sessionLengthNote(139, 200); n != "" {
		t.Errorf("at 139 of 200: %q", n)
	}
	if n := sessionLengthNote(140, 200); !strings.Contains(n, "140 of its 200") || !strings.Contains(n, "new session") {
		t.Errorf("at 140 of 200: %q", n)
	}
}
