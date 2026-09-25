# Documentation Index

> **Classification:** Current — Index of the documentation set.

> **Last verified:** 2026-07-15

This index separates current implementation documentation from historical
requirements and research. Code, migrations, tests, and build configuration
remain authoritative.

## Current implementation references

| Document | Status | Purpose |
|---|---|---|
| [`README.md`](../README.md) | current | Repository entry point and quick start |
| [`ARCHITECTURE.md`](../ARCHITECTURE.md) | current | Implemented system architecture and known boundaries |
| [`docs/SELF_HOSTING.md`](SELF_HOSTING.md) | current | Deploying on your own infrastructure: requirements, Docker Compose and Kubernetes installs, backups, upgrades |
| [`docs/DEVELOPMENT.md`](DEVELOPMENT.md) | current | Local development, generation, and validation |
| [`docs/API.md`](API.md) | current | Actual HTTP route groups and API conventions |
| [`docs/LICENSING.md`](LICENSING.md) | current | Editions, the signed license key, and the `ee/` enterprise tree |
| [`docs/PLANS_AND_SIGNUP.md`](PLANS_AND_SIGNUP.md) | current | Plans and their limits, the read-only state, and public self-service sign-up — no trials |
| [`docs/STAGING_AND_LOAD_TESTING.md`](STAGING_AND_LOAD_TESTING.md) | current | The staging environment, the load harness (`cmd/loadtest`) and the measured baseline |
| [`docs/BRAND.md`](BRAND.md) | current | The look of the pages a visitor sees before they have an account, and how a deployment overrides it |
| [`docs/LEGAL_AND_PRIVACY.md`](LEGAL_AND_PRIVACY.md) | current | The terms of service and privacy notice at `/terms` and `/privacy`, and the operator identity a deployment must configure |
| [`docs/TENANT_DATABASES.md`](TENANT_DATABASES.md) | current | A database per tenant: routing, provisioning, migrations, backups |
| [`docs/NOTIFICATIONS.md`](NOTIFICATIONS.md) | current | Notification producers, outbound e-mail and webhooks, task reminders |
| [`docs/developer-manual/`](developer-manual/) | current | Developer-role manual (PDF, built from `parts/*.html` by `build.sh`): every developer console screen plus core concepts |
| [`web/README.md`](../web/README.md) | current | Frontend architecture and commands |
| [`web/src/ui/DESIGN_SYSTEM.md`](../web/src/ui/DESIGN_SYSTEM.md) | current | Shared UI primitives and regression gates |
| [`examples/budgeting-demo/README.md`](../examples/budgeting-demo/README.md) | current | Running salary-budgeting demo |
| [`examples/budgeting-demo/model-spec.md`](../examples/budgeting-demo/model-spec.md) | current | Seeded model contract |

## Feature specifications and convergence guides

These documents preserve the design rationale and detailed acceptance criteria.
Their status block records current implementation and remaining gaps.

| Document | Classification |
|---|---|
| [`TIME_SERIES_FUNCTIONS_IMPLEMENTATION.md`](../TIME_SERIES_FUNCTIONS_IMPLEMENTATION.md) | Phase 1 implemented (explicit time dimensions, 11 time-series functions, causal recurrences); §13 lists the deferred later-parity functions |
| [`examples/budgeting-demo/BUILD_INSTRUCTIONS.md`](../examples/budgeting-demo/BUILD_INSTRUCTIONS.md) | implemented demo acceptance contract |

## UX decision history

| Document | Classification |
|---|---|

## Local ignored binary references

These artifacts exist in the working directory but are excluded by
`.gitignore`; they are not part of a clean repository checkout.

| Artifact | Classification |
|---|---|
| `mavericks_engine_technical_specification.docx` | original target architecture; the local copy's cover now marks it as historical and superseded by `ARCHITECTURE.md` |
| `mavericks_engine_design.pdf` | original product/design source; immutable research artifact |
| `mavericks_engine_research.pdf` | original research artifact; not an implementation-status source |

The PDF files are source artifacts without editable project sources in this
repository. Do not update implementation claims by editing generated PDF bytes;
record current decisions in Markdown and regenerate from an owned source if a
new published PDF is required.

Tracked files under root `node_modules/` are third-party package licenses and
READMEs, not Mavericks project documentation. They are governed by their
upstream packages and are intentionally excluded from this current-state edit;
future repository cleanup should untrack the vendored dependency tree rather
than rewriting those files.

## Contract documentation

- `proto/*/v1/*.proto` is authoritative for gRPC message/service contracts.
- `api/openapi.yaml` is a partial core HTTP contract and generates
  `internal/gateway/oas`. It is not a complete inventory of the manual gateway
  router.
- `migrations/*.sql` is authoritative for persisted schema history.
- `.github/workflows/ci.yml` is authoritative for CI commands.
