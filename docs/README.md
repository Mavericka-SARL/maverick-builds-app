# Documentation Index

> **Classification:** Current — Index of the documentation set.

> **Last verified:** 2026-10-07

This index lists the project documentation and its status. Code, migrations,
tests, and build configuration remain authoritative.

## Current implementation references

| Document | Status | Purpose |
|---|---|---|
| [`README.md`](../README.md) | current | Repository entry point and quick start |
| [`ARCHITECTURE.md`](../ARCHITECTURE.md) | current | Implemented system architecture and known boundaries |
| [`docs/SELF_HOSTING.md`](SELF_HOSTING.md) | current | Deploying on your own infrastructure: requirements, Docker Compose and Kubernetes installs, backups, upgrades |
| [`docs/CHAT_CONNECTOR.md`](CHAT_CONNECTOR.md) | current | The chat connector at `/mcp` (ChatGPT, Claude): grid tools, charts and reports, entering cell values (`write_cells`, all or nothing), security model, configuration, per-host Keycloak clients |
| [`docs/DEVELOPMENT.md`](DEVELOPMENT.md) | current | Local development, generation, and validation |
| [`docs/API.md`](API.md) | current | Actual HTTP route groups and API conventions |
| [`docs/LICENSING.md`](LICENSING.md) | current | Editions, the signed license key, and the `ee/` enterprise tree |
| [`docs/PLANS_AND_SIGNUP.md`](PLANS_AND_SIGNUP.md) | current | Plans and their limits, the read-only state, and public self-service sign-up — no trials |
| [`docs/STAGING_AND_LOAD_TESTING.md`](STAGING_AND_LOAD_TESTING.md) | current | The staging environment, the load harness (`cmd/loadtest`) and the measured baseline |
| [`docs/BRAND.md`](BRAND.md) | current | The look of the pages a visitor sees before they have an account, and how a deployment overrides it |
| [`docs/LEGAL_AND_PRIVACY.md`](LEGAL_AND_PRIVACY.md) | current | The terms of service and privacy notice at `/terms` and `/privacy`, and the operator identity a deployment must configure |
| [`docs/TENANT_DATABASES.md`](TENANT_DATABASES.md) | current | A database per tenant: routing, provisioning, migrations, backups |
| [`docs/NOTIFICATIONS.md`](NOTIFICATIONS.md) | current | Notification producers, outbound e-mail and webhooks, task reminders |
| [`docs/AI_DEVELOPER.md`](AI_DEVELOPER.md) | current | The AI assistant's read and write tools, and what stays human |
| [`docs/AI_KEYS.md`](AI_KEYS.md) | current | Where the AI assistant's provider key comes from: user, tenant, deployment |
| [`docs/AUDIT_EXPORT.md`](AUDIT_EXPORT.md) | current | Audit export (CSV, JSON Lines for a SIEM) and retention |
| [`docs/CELL_HISTORY.md`](CELL_HISTORY.md) | current | Per-cell change history |
| [`docs/SSO_SCIM.md`](SSO_SCIM.md) | current | Enterprise single sign-on and SCIM provisioning |
| [`docs/USAGE_ANALYTICS.md`](USAGE_ANALYTICS.md) | current | Per-tenant usage counts |
| [`docs/WHITE_LABEL.md`](WHITE_LABEL.md) | current | A tenant's own branding and domain |
| [`ee/README.md`](../ee/README.md) | current | The enterprise source tree and its licence rule |
| [`docs/developer-manual/`](developer-manual/) | current | Developer-role manual (PDF, built from `parts/*.html` by `build.sh`): every developer console screen plus core concepts. Every deployment also serves its `manual.html` and `img/` at `/docs/developer-manual/manual.html` (copied into the web image by `web/Dockerfile`) |
| [`docs/formulas-manual/`](formulas-manual/) | current | Formulas manual for the developer role (PDF, built from `parts/*.html` by `build.sh`): the formula language, all 74 functions with their edge behaviour, totals, recipes, validation messages and limits; its function index is held equal to the engine by `TestFormulasManualIndexMatchesEngine`. Every deployment also serves its `manual.html` at `/docs/formulas-manual/manual.html` |
| [`web/README.md`](../web/README.md) | current | Frontend architecture and commands |
| [`web/src/ui/DESIGN_SYSTEM.md`](../web/src/ui/DESIGN_SYSTEM.md) | current | Shared UI primitives and regression gates |

## Contract documentation

- `proto/*/v1/*.proto` is authoritative for gRPC message/service contracts.
- `api/openapi.yaml` describes every gateway route (a CI parity test enforces
  it) and generates `internal/gateway/oas`.
- `migrations/*.sql` is authoritative for persisted schema history.
- `.github/workflows/ci.yml` is authoritative for CI commands.
