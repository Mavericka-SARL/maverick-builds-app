package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Compose stack's Caddy asks GET /edge/reset-password before passing a
// request to Keycloak's reset form (deploy/compose/Caddyfile): submissions
// are five a minute per visitor, showing the form is free, and one visitor's
// budget is not another's.
func TestEdgeResetPasswordLimitsSubmissionsPerVisitor(t *testing.T) {
	h := &handler{resetLimit: discoverLimiter{rate: resetPasswordRate, burst: resetPasswordBurst}}
	ask := func(method, ip string) int {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/edge/reset-password", nil)
		r.Header.Set("X-Forwarded-Method", method)
		r.Header.Set("X-Forwarded-For", ip)
		w := httptest.NewRecorder()
		h.edgeResetPassword(w, r)
		return w.Code
	}
	for i := 0; i < resetPasswordBurst; i++ {
		if code := ask(http.MethodPost, "203.0.113.7"); code != http.StatusNoContent {
			t.Fatalf("submission %d: %d, want 204", i+1, code)
		}
	}
	if code := ask(http.MethodPost, "203.0.113.7"); code != http.StatusTooManyRequests {
		t.Errorf("submission %d: %d, want 429", resetPasswordBurst+1, code)
	}
	if code := ask(http.MethodGet, "203.0.113.7"); code != http.StatusNoContent {
		t.Errorf("showing the form after the budget is spent: %d, want 204", code)
	}
	if code := ask(http.MethodPost, "198.51.100.4"); code != http.StatusNoContent {
		t.Errorf("another visitor's first submission: %d, want 204", code)
	}
}
