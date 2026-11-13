package gateway

import (
	"strings"
	"testing"
)

// AI Settings recommends a strong model at low effort for builds from a
// workbook (the five-way comparison of 2026-11-12).
func TestBuildAdviceRecommendsStrongModelAtLowEffort(t *testing.T) {
	for _, c := range []struct{ model, effort, want string }{
		{"gpt-6.1-sol", "low", ""},
		{"gpt-6.1-sol", "medium", ""},
		{"gpt-6.1-sol", "high", "Low effort is usually enough"},
		{"gpt-5-mini", "low", "strong reasoning model (gpt-6.1-sol, gpt-5.5) at low effort"},
		{"gpt-6.1-sol", "none", "choose low or higher"},
	} {
		got := buildAdvice("openai", c.model, c.effort)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("buildAdvice(%s, %s) = %q, want %q", c.model, c.effort, got, c.want)
		}
	}
	if buildAdvice("anthropic", "x", "high") != "" {
		t.Error("advice for a provider it does not know")
	}
}
