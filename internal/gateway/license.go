package gateway

import (
	"fmt"
	"net/http"

	"github.com/mavericks-engine/mavericks/pkg/license"
)

// licenseInfo reports the edition in force, the features it unlocks and the
// full feature catalog, so every console can label what is available and
// what would need a different edition. Any signed-in user may read it: the
// edition is not a secret, and business users see locked features too.
func (h *handler) licenseInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	if _, err := h.resolveActor(r.Context(), r); err != nil {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	jsonOK(w, h.lic.Status())
}

// requireFeature is the gate every enterprise/commercial route goes through:
// it answers 403 with a message naming the feature and both editions unless
// the license in force unlocks f. Wrap it inside the role guard so an
// unauthenticated caller still gets 401 first:
//
//	register("GET", "/api/audit/export", "admin", adm(h.requireFeature(license.FeatureAuditExport, h.auditExport)))
//
// During a key's transition period (license.StateTransition) the feature
// can still be read, exported and switched off — see transitionAllowed —
// but not configured or changed. The check runs per request, so a key that
// expires while the gateway runs moves through both without a restart.
func (h *handler) requireFeature(f license.Feature, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h.licenseGate(r.Method)(f); err != nil {
			jsonErr(w, err, http.StatusForbidden)
			return
		}
		fn(w, r)
	}
}

// transitionAllowed is what a paid feature still answers to while its key
// is in the transition period: reads and exports (GET, HEAD), and DELETE,
// which switches paid configuration off — an administrator moving off a
// feature, or revoking a SCIM token, must not have to renew first.
func transitionAllowed(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodDelete
}

// licenseGate picks the licence check for a request method: RequireUse for
// what the transition period allows, the strict Require for the rest.
func (h *handler) licenseGate(method string) func(license.Feature) error {
	if transitionAllowed(method) {
		return h.lic.RequireUse
	}
	return h.lic.Require
}
