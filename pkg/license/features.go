package license

import "fmt"

// Feature names a gated capability. The strings are part of the license key
// contract: a key issued today must still unlock the same feature on a
// binary built next year, so never rename one — add a new one and keep the
// old as an alias if the capability is split.
type Feature string

const (
	// FeatureSSO is identity-provider brokering (SAML, OIDC) for sign-in.
	FeatureSSO Feature = "sso"
	// FeatureSCIM is automated user provisioning from a directory.
	FeatureSCIM Feature = "scim"
	// FeatureCellHistory is the per-intersection change history browser.
	FeatureCellHistory Feature = "cell_history"
	// FeatureAuditExport is audit log export and streaming.
	FeatureAuditExport Feature = "audit_export"
	// FeatureUsageAnalytics is the per-tenant usage dashboard.
	FeatureUsageAnalytics Feature = "usage_analytics"
	// FeatureWhiteLabel is custom branding of the console.
	FeatureWhiteLabel Feature = "white_label"
	// FeatureTenantAIKeys is tenant-level AI provider keys shared by all
	// developers of a tenant (community and commercial keep per-user keys).
	FeatureTenantAIKeys Feature = "tenant_ai_keys"
	// FeatureDeploymentSettings is a deployment-wide row of the per-tenant
	// settings (notification delivery, audit retention, the AI key) that
	// every tenant inherits until it sets its own; without it each tenant
	// only ever has its own.
	FeatureDeploymentSettings Feature = "deployment_settings"
)

// Info describes a feature for the console and the documentation.
type Info struct {
	Key         Feature `json:"key"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	// MinEdition is the lowest edition whose defaults include the feature.
	MinEdition Edition `json:"min_edition"`
}

// catalog is the fixed set of gated features. Order here is the order the
// console lists them in.
var catalogOrder = []Info{
	{FeatureSSO, "Single sign-on", "Sign in through the customer's SAML or OIDC identity provider.", EditionEnterprise},
	{FeatureSCIM, "SCIM provisioning", "Create, update and deactivate users from a directory automatically.", EditionEnterprise},
	{FeatureCellHistory, "Cell history", "Browse who changed which value, per intersection, with the full write history.", EditionEnterprise},
	{FeatureAuditExport, "Audit export", "Export or stream the audit log, with retention policies.", EditionEnterprise},
	{FeatureUsageAnalytics, "Usage analytics", "Active users, models, storage and integration runs per tenant.", EditionEnterprise},
	{FeatureWhiteLabel, "White-labelling", "Custom logo, colours and domain for the console.", EditionCommercial},
	{FeatureTenantAIKeys, "Tenant AI keys", "One AI provider key per tenant, managed by the tenant admin, used by every developer.", EditionEnterprise},
	{FeatureDeploymentSettings, "Deployment settings", "Deployment-wide defaults for notification delivery, audit retention and the AI key, inherited by every tenant that has not set its own.", EditionEnterprise},
}

var catalog = func() map[Feature]Info {
	m := make(map[Feature]Info, len(catalogOrder))
	for _, info := range catalogOrder {
		m[info.Key] = info
	}
	return m
}()

// editionFeatures are the defaults each edition unlocks. Enterprise includes
// everything; commercial only what its MinEdition allows.
var editionFeatures = func() map[Edition][]Feature {
	m := map[Edition][]Feature{EditionCommunity: {}}
	for _, ed := range []Edition{EditionCommercial, EditionEnterprise} {
		for _, info := range catalogOrder {
			if info.MinEdition.rank() <= ed.rank() {
				m[ed] = append(m[ed], info.Key)
			}
		}
	}
	return m
}()

// Catalog returns every gated feature in display order.
func Catalog() []Info {
	out := make([]Info, len(catalogOrder))
	copy(out, catalogOrder)
	return out
}

// Lookup returns the catalog entry for f.
func Lookup(f Feature) (Info, bool) {
	info, ok := catalog[f]
	return info, ok
}

// Describe returns a human name for f, falling back to the key itself.
func Describe(f Feature) string {
	if info, ok := catalog[f]; ok {
		return info.Name
	}
	return string(f)
}

// RequiredEdition is the lowest edition that includes f by default.
func RequiredEdition(f Feature) Edition {
	if info, ok := catalog[f]; ok {
		return info.MinEdition
	}
	return EditionEnterprise
}

// UnavailableError explains why a gated route refused a request. Its text is
// what the console shows, so it names the feature and both editions.
func UnavailableError(f Feature, current Edition) error {
	return fmt.Errorf("%s requires the %s edition; this deployment runs the %s edition",
		Describe(f), RequiredEdition(f), current)
}
