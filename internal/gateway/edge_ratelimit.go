package gateway

import (
	"io"
	"net/http"
	"strings"
)

// "Forgot password?" mails a reset link to whatever registered address is
// typed, and Keycloak does not limit how often. On Kubernetes, Traefik limits
// the reset form per visitor (deploy/k8s/base/ingress.yaml). The Compose
// stack's Caddy has no rate limiting of its own, but its forward_auth asks
// another service before passing a request on: deploy/compose/Caddyfile asks
// this endpoint about every request to the reset form, and passes it to
// Keycloak only on a 2xx. Same budget as Traefik's: five submissions a
// minute per visitor, five at once.
const (
	resetPasswordRate  = 5.0 / 60
	resetPasswordBurst = 5
)

// edgeResetPassword serves GET /edge/reset-password. Outside /api, so no
// public route reaches it: Caddy calls it inside the stack's network, with
// the visitor's address in X-Forwarded-For and the request's method in
// X-Forwarded-Method. Showing the form is free; each submission, which may
// send a mail, takes one from the visitor's budget.
func (h *handler) edgeResetPassword(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("X-Forwarded-Method"), http.MethodPost) || h.resetLimit.allow(clientIP(r)) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Retry-After", "60")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, "Too many password reset requests from your address. Try again in a minute.\n")
}
