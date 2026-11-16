package gateway

import (
	"strings"
	"testing"
)

// The caps are deployment settings; by default a session has none (a session
// is its draft and goes on, 2026-11-13) and a developer has 1000 calls a day;
// a value that is not a positive whole number keeps the default.
func TestLLMCallCapsAreSettings(t *testing.T) {
	t.Setenv("AI_MAX_CALLS_PER_SESSION", "")
	t.Setenv("AI_MAX_CALLS_PER_DAY", "")
	if s, d := llmCallCaps(); s != 0 || d != 1000 {
		t.Errorf("defaults = %d, %d; want 0 (no session cap), 1000", s, d)
	}
	t.Setenv("AI_MAX_CALLS_PER_SESSION", "150")
	t.Setenv("AI_MAX_CALLS_PER_DAY", "400")
	if s, d := llmCallCaps(); s != 150 || d != 400 {
		t.Errorf("set = %d, %d; want 150, 400", s, d)
	}
	t.Setenv("AI_MAX_CALLS_PER_SESSION", "-3")
	t.Setenv("AI_MAX_CALLS_PER_DAY", "lots")
	if s, d := llmCallCaps(); s != 0 || d != 1000 {
		t.Errorf("invalid values = %d, %d; want the defaults", s, d)
	}
}

// At a cap the message says who can lift it: the developer their own call
// budget; the server's operator a deployment's limit, which no role in the
// app can change ("an administrator" left people looking for it in the
// app) — never a new session, which would start a fresh draft.
func TestSessionCapMessageSaysWhoCanLiftIt(t *testing.T) {
	if m := sessionCapMessage(40, true); !strings.Contains(m, "call budget of 40") || !strings.Contains(m, "raise or clear") || strings.Contains(m, "new session") {
		t.Errorf("budget: %q", m)
	}
	if m := sessionCapMessage(300, false); !strings.Contains(m, "whoever runs this server") || !strings.Contains(m, "no role in the app") ||
		!strings.Contains(m, "AI_MAX_CALLS_PER_SESSION") || strings.Contains(m, "administrator") || strings.Contains(m, "new session") {
		t.Errorf("deployment limit: %q", m)
	}
}

// The daily limits combine as the owner decided (2026-11-16): the lower of
// the developer's own and the tenant admin's, either above the server's
// default if they like; with neither, the server's default; on the server's
// own key the server's number is the ceiling.
func TestEffectiveDailyLimit(t *testing.T) {
	n := func(v int) *int { return &v }
	for _, c := range []struct {
		name        string
		own, tenant *int
		serverPays  bool
		want        int
		wantBy      string
	}{
		{"none set: the server's default", nil, nil, false, 1000, "server"},
		{"own above the default", n(3000), nil, false, 3000, "own"},
		{"own below the default", n(50), nil, false, 50, "own"},
		{"tenant above the default", nil, n(5000), false, 5000, "tenant"},
		{"own lower than tenant", n(200), n(5000), false, 200, "own"},
		{"tenant lower than own", n(9000), n(5000), false, 5000, "tenant"},
		{"server's key: default is the ceiling", n(3000), nil, true, 1000, "server_key"},
		{"server's key, none set", nil, nil, true, 1000, "server_key"},
		{"server's key, own below", n(50), nil, true, 50, "own"},
		{"zero is no limit", n(0), nil, false, 1000, "server"},
	} {
		got := effectiveDailyLimit(c.own, c.tenant, 1000, c.serverPays)
		if got.n != c.want || got.by != c.wantBy {
			t.Errorf("%s: %+v, want %d by %s", c.name, got, c.want, c.wantBy)
		}
	}
}

// Each daily limit's message names who set it and where it changes.
func TestDailyCapMessageNamesWhoSetIt(t *testing.T) {
	for by, want := range map[string][]string{
		"own":        {"your own limit of 40", "AI Developer settings", "raise or clear"},
		"tenant":     {"your tenant's limit of 40", "a tenant admin", "AI keys"},
		"server":     {"default limit of 40", "AI_MAX_CALLS_PER_DAY", "set a limit of your own in AI Developer settings"},
		"server_key": {"limit of 40", "server's own AI key", "whoever runs this server", "no role in the app"},
	} {
		m := dailyCapMessage(dailyLimit{n: 40, by: by})
		for _, w := range want {
			if !strings.Contains(m, w) {
				t.Errorf("%s: %q lacks %q", by, m, w)
			}
		}
		if strings.Contains(m, "administrator") {
			t.Errorf("%s: %q says administrator", by, m)
		}
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

// From 70% of its call budget a session asks the model to finish the part it
// is on and say how far it got; with no budget it says nothing.
func TestSessionLengthNote(t *testing.T) {
	if n := sessionLengthNote(139, 200); n != "" {
		t.Errorf("at 139 of 200: %q", n)
	}
	if n := sessionLengthNote(140, 200); !strings.Contains(n, "140 of its 200-call budget") || strings.Contains(n, "new session") {
		t.Errorf("at 140 of 200: %q", n)
	}
	if n := sessionLengthNote(5000, 0); n != "" {
		t.Errorf("no budget: %q", n)
	}
}
