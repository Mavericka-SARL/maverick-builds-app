# Enterprise source (`ee/`)

> **Classification:** Current — How enterprise and commercial code is organised and gated.

Everything under this directory (and under [`web/src/ee/`](../web/src/ee/README.md)
on the frontend) is licensed under the [maverickbuilds.app Enterprise License](LICENSE),
not the Sustainable Use License that covers the rest of the repository.

## How it works

- **One build, gated at runtime.** Enterprise packages are compiled into the
  same binaries and images as the community code. Nothing here is hidden
  behind a build tag. What unlocks them is the license key the gateway reads
  at start-up (`MAVERICKS_LICENSE_KEY` or `MAVERICKS_LICENSE_FILE`); see
  [`docs/LICENSING.md`](../docs/LICENSING.md).
- **Every entry point checks the license.** An HTTP route that belongs to a
  gated feature is registered through `handler.requireFeature(license.FeatureX, …)`
  in `internal/gateway`, which answers `403` with a message naming the feature
  and the edition it needs; a public route that must answer everywhere checks
  inline and answers "no" (`/api/sso/discover`). Background jobs, which the
  gateway runs, check `license.Manager.Has` before doing gated work. Only the
  gateway loads a licence; no gRPC service serves a gated feature. The frontend wraps gated
  UI in `<FeatureGate feature="…">` (`web/src/license/FeatureGate.tsx`) so a
  locked feature is visible but inert.
- **Feature names are a contract.** They live in `pkg/license/features.go`
  (community code, because the community gateway must be able to say "this
  needs enterprise"). A key issued today must unlock the same feature on a
  binary built later, so names are never renamed.

## Layout

```text
ee/
  LICENSE            the Enterprise License
  README.md          this file
  <feature>/         one Go package per enterprise feature (added with the feature)
web/src/ee/
  README.md          the frontend counterpart
  <feature>/         one folder per enterprise feature
```

Feature packages so far (2026-09-16): `aikeys` — the tenant-level AI
provider key (`license.FeatureTenantAIKeys`, [`docs/AI_KEYS.md`](../docs/AI_KEYS.md));
`sso` — a tenant's own identity provider through Keycloak brokering
(`license.FeatureSSO`); `scim` — SCIM 2.0 provisioning (`license.FeatureSCIM`),
both in [`docs/SSO_SCIM.md`](../docs/SSO_SCIM.md); `cellhistory` — every value
a cell held (`license.FeatureCellHistory`, [`docs/CELL_HISTORY.md`](../docs/CELL_HISTORY.md));
`auditexport` — the audit log as CSV / JSON Lines with a collector cursor,
and retention (`license.FeatureAuditExport`, [`docs/AUDIT_EXPORT.md`](../docs/AUDIT_EXPORT.md));
`usage` — per-tenant usage (`license.FeatureUsageAnalytics`, [`docs/USAGE_ANALYTICS.md`](../docs/USAGE_ANALYTICS.md));
`branding` — white-labelling (`license.FeatureWhiteLabel`, commercial and
enterprise, [`docs/WHITE_LABEL.md`](../docs/WHITE_LABEL.md)). Every item of
the 2026-09 editions programme now has its package.

## Rules for contributors

1. Community code may import `pkg/license` and gate on features. It may
   reference an `ee/` package only from a route registration or a resolver
   that is itself behind a licence check, never for behaviour a community
   deployment depends on — deleting `ee/` must cost only enterprise features.
   (`internal/gateway/tenant_ai_settings.go` is the reference shape: the four
   routes are wrapped in `requireFeature`, and `tenantAIKey`, which the chat
   path calls on every request, returns empty before touching `ee/aikeys`
   unless the licence unlocks it.) Enterprise code may import anything.
2. A gated route is registered in `internal/gateway` (the router is the single
   HTTP surface, and `api/openapi.yaml` parity is enforced there); the handler
   body lives in the `ee/` package.
3. Tests for enterprise code run in the ordinary `go test ./...`; use
   `license.Static(&license.Claims{Edition: license.EditionEnterprise, …})`
   to build an unlocked handler.
