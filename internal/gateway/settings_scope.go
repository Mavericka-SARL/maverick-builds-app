package gateway

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/mavericks-engine/mavericks/pkg/license"
)

// Per-tenant settings — notification delivery, audit retention, the tenant
// AI key, the SSO provider, SCIM tokens — are rows keyed by tenant
// (migration 091). One rule says which row a request is about:
//
//   - X-Tenant-Id names a tenant the caller administers: that one. The
//     platform admin picks any tenant this way; a tenant admin their own.
//   - Without it, a tenant admin's own tenant — the one they belong to, or
//     the one they administer when it is exactly one.
//   - Without it, the platform admin acts on the DEPLOYMENT's own row: the
//     defaults every tenant inherits until it sets its own. That row is an
//     enterprise capability (license.FeatureDeploymentSettings), and the
//     identity settings have no such row at all — a provider or a token
//     without a tenant has nowhere to put the people it signs in.
//
// The scope is reported back with every settings response, so the console
// always says whose settings it is showing.
type settingsScope struct {
	// CustomerID is empty for the deployment's own row.
	CustomerID string `json:"customer_id,omitempty"`
	Deployment bool   `json:"deployment"`
	// Inherited is set by the handler when a tenant has no row of its own
	// and the values shown are the deployment's.
	Inherited bool `json:"inherited"`
	// DeploymentSettingsAvailable says whether this edition has a
	// deployment row for tenants to inherit from.
	DeploymentSettingsAvailable bool `json:"deployment_settings_available"`
}

// settingsScopeFor resolves the scope, answering the request itself when it
// cannot. deploymentRow says whether this setting has a deployment row.
func (h *handler) settingsScopeFor(w http.ResponseWriter, r *http.Request, act *actor, deploymentRow bool) (settingsScope, bool) {
	ctx := r.Context()
	available := h.lic != nil && h.lic.Has(license.FeatureDeploymentSettings)
	all, mine, err := h.adminScopeCustomerIDs(ctx, act)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return settingsScope{}, false
	}
	if cid := r.Header.Get(tenantHeader); cid != "" {
		if !all && !slices.Contains(mine, cid) {
			jsonErr(w, fmt.Errorf("forbidden: not your tenant"), http.StatusForbidden)
			return settingsScope{}, false
		}
		return settingsScope{CustomerID: cid, DeploymentSettingsAvailable: available}, true
	}
	if !all {
		switch len(mine) {
		case 1:
			return settingsScope{CustomerID: mine[0], DeploymentSettingsAvailable: available}, true
		case 0:
			jsonErr(w, fmt.Errorf("forbidden: you administer no tenant"), http.StatusForbidden)
		default:
			jsonErr(w, fmt.Errorf("you administer several tenants: name one in the %s header", tenantHeader), http.StatusConflict)
		}
		return settingsScope{}, false
	}
	if !deploymentRow {
		jsonErr(w, fmt.Errorf("this setting belongs to a tenant: name one in the %s header", tenantHeader), http.StatusConflict)
		return settingsScope{}, false
	}
	if !available {
		jsonErr(w, fmt.Errorf("deployment-wide settings are an enterprise capability (deployment_settings); name a tenant in the %s header to edit its own", tenantHeader), http.StatusForbidden)
		return settingsScope{}, false
	}
	return settingsScope{Deployment: true, DeploymentSettingsAvailable: true}, true
}
