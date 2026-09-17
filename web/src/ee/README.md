# Enterprise frontend source (`web/src/ee/`)

> **Classification:** Current — Frontend counterpart of `ee/`.

Files under this directory are licensed under the
[maverickbuilds.app Enterprise License](../../../ee/LICENSE), not the Sustainable
Use License that covers the rest of the repository.

- One folder per enterprise feature, mirroring the Go package under `ee/`.
- Gated screens and controls render through `<FeatureGate feature="…">` from
  `web/src/license/FeatureGate.tsx`, or check `useFeature("…")`, so a locked
  feature is shown with the edition it needs instead of failing on the call.
- Feature keys come from `pkg/license/features.go` and are reported by
  `GET /api/license`; never invent one on the frontend.

Feature folders (2026-09-16): `aikeys/` — **Admin › AI keys** (`tenant_ai_keys`);
`sso/` — **Admin › Single sign-on** (`sso`); `scim/` — **Admin › Provisioning
(SCIM)** (`scim`); `cellhistory/` — the grid's cell **History** drawer
(`cell_history`); `auditexport/` — **Admin › Audit Log** export and retention
(`audit_export`); `usage/` — **Admin › Usage** (`usage_analytics`); `branding/` — **Admin ›
Branding** (`white_label`). Applying a brand lives outside this tree, in
`web/src/branding/`, because every edition must apply what the server reports.
