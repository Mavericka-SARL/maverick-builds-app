package license

import (
	"fmt"
	"slices"
)

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
	// MinEdition is the lowest edition whose defaults in the LATEST
	// schedule include the feature — what a new key needs. A feature no
	// schedule includes reports enterprise: it is sold by naming it.
	MinEdition Edition `json:"min_edition"`
}

// catalogOrder is the fixed set of gated features. Order here is the order
// the console lists them in. Adding a feature here unlocks it for no key:
// which editions include it is decided by a new Schedule, deliberately.
var catalogOrder = []Info{
	{Key: FeatureSSO, Name: "Single sign-on", Description: "Sign in through the customer's SAML or OIDC identity provider."},
	{Key: FeatureSCIM, Name: "SCIM provisioning", Description: "Create, update and deactivate users from a directory automatically."},
	{Key: FeatureCellHistory, Name: "Cell history", Description: "Browse who changed which value, per intersection, with the full write history."},
	{Key: FeatureAuditExport, Name: "Audit export", Description: "Export or stream the audit log, with retention policies."},
	{Key: FeatureUsageAnalytics, Name: "Usage analytics", Description: "Active users, models, storage and integration runs per tenant."},
	{Key: FeatureWhiteLabel, Name: "White-labelling", Description: "Custom logo, colours and domain for the console."},
	{Key: FeatureTenantAIKeys, Name: "Tenant AI keys", Description: "One AI provider key per tenant, managed by the tenant admin, used by every developer."},
	{Key: FeatureDeploymentSettings, Name: "Deployment settings", Description: "Deployment-wide defaults for notification delivery, audit retention and the AI key, inherited by every tenant that has not set its own."},
}

// Schedule is a frozen statement of the features each paid edition
// includes by default. A key names the schedule it was sold under, and its
// default features come from that schedule — never from whatever the
// running binary happens to list — so a feature added later does not
// silently extend keys sold before it.
//
// A schedule is never edited once released (TestSchedulesAreFrozen holds
// them). Including a new feature in an edition by default means appending
// a new schedule; keys sold afterwards name it. A feature no schedule
// includes is sold only by naming it in a key's Features. Schedules only
// ever add features, so a key naming a schedule newer than the binary
// safely gets the newest one the binary knows.
type Schedule struct {
	// ID is the month the schedule was frozen, YYYY-MM; IDs sort in time.
	ID       string                `json:"id"`
	Editions map[Edition][]Feature `json:"editions"`
}

// schedules, oldest first.
var schedules = []Schedule{
	{ID: "2026-10", Editions: map[Edition][]Feature{
		EditionCommercial: {FeatureWhiteLabel},
		EditionEnterprise: {FeatureSSO, FeatureSCIM, FeatureCellHistory, FeatureAuditExport, FeatureUsageAnalytics, FeatureWhiteLabel, FeatureTenantAIKeys, FeatureDeploymentSettings},
	}},
}

// FirstSchedule is what a key that names no schedule was sold under: every
// key issued before 2026-10-07, when the defaults were still derived from
// the catalogue, which then held exactly these features.
const FirstSchedule = "2026-10"

// LatestSchedule is the schedule new keys are issued under by default.
func LatestSchedule() string { return schedules[len(schedules)-1].ID }

// Schedules returns every schedule this binary knows, oldest first.
func Schedules() []Schedule {
	out := make([]Schedule, len(schedules))
	copy(out, schedules)
	return out
}

// resolveSchedule maps a key's schedule to one this binary knows: the
// schedule itself, or for a key sold under a newer schedule than the binary
// knows, the newest one it does. Empty means FirstSchedule. A schedule
// older than any known is not one the vendor ever issued.
func resolveSchedule(id string) (Schedule, bool) {
	if id == "" {
		id = FirstSchedule
	}
	for i := len(schedules) - 1; i >= 0; i-- {
		if schedules[i].ID <= id {
			return schedules[i], true
		}
	}
	return Schedule{}, false
}

// knownSchedule reports whether id is exactly a schedule this binary has —
// what the vendor tool requires before signing.
func knownSchedule(id string) bool {
	for _, s := range schedules {
		if s.ID == id {
			return true
		}
	}
	return false
}

var catalog = func() map[Feature]Info {
	latest := schedules[len(schedules)-1]
	m := make(map[Feature]Info, len(catalogOrder))
	for i, info := range catalogOrder {
		info.MinEdition = EditionEnterprise
		for _, ed := range []Edition{EditionCommercial, EditionEnterprise} {
			if slices.Contains(latest.Editions[ed], info.Key) {
				info.MinEdition = ed
				break
			}
		}
		catalogOrder[i] = info
		m[info.Key] = info
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
