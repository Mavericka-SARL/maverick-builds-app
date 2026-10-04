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
