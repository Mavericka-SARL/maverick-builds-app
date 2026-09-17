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
| [`docs/DEVELOPMENT.md`](DEVELOPMENT.md) | current | Local development, generation, and validation |
| [`docs/API.md`](API.md) | current | Actual HTTP route groups and API conventions |
| [`docs/LICENSING.md`](LICENSING.md) | current | Editions, the signed license key, and the `ee/` enterprise tree |
| [`docs/PLANS_AND_TRIALS.md`](PLANS_AND_TRIALS.md) | current | Plans and their limits, trials, the read-only state, and public self-service sign-up |
| [`docs/PUBLIC_RELEASE.md`](PUBLIC_RELEASE.md) | current | How the public repository snapshot is produced from this one, and what stays private |
| [`docs/STAGING_AND_LOAD_TESTING.md`](STAGING_AND_LOAD_TESTING.md) | current | The staging environment, the load harness (`cmd/loadtest`) and the measured baseline |
| [`docs/TENANT_DATABASES.md`](TENANT_DATABASES.md) | current | A database per tenant: routing, provisioning, migrations, backups |
| [`docs/NOTIFICATIONS.md`](NOTIFICATIONS.md) | current | Notification producers, outbound e-mail and webhooks, task reminders |
| [`docs/developer-manual/`](developer-manual/) | current | Developer-role manual (PDF, built from `parts/*.html` by `build.sh`): every developer console screen plus core concepts |
| [`IMPLEMENTATION_PLAN.md`](../IMPLEMENTATION_PLAN.md) | current roadmap | Delivered capability matrix and prioritized gaps |
| [`web/README.md`](../web/README.md) | current | Frontend architecture and commands |
| [`web/src/ui/DESIGN_SYSTEM.md`](../web/src/ui/DESIGN_SYSTEM.md) | current | Shared UI primitives and regression gates |
| [`examples/budgeting-demo/README.md`](../examples/budgeting-demo/README.md) | current | Running salary-budgeting demo |
| [`examples/budgeting-demo/model-spec.md`](../examples/budgeting-demo/model-spec.md) | current | Seeded model contract |

## Feature specifications and convergence guides

These documents preserve the design rationale and detailed acceptance criteria.
Their status block records current implementation and remaining gaps.

| Document | Classification |
|---|---|
| [`DASHBOARD_CANVAS_REQUIREMENTS.md`](../DASHBOARD_CANVAS_REQUIREMENTS.md) | core implemented; broader specification partial |
| [`DEVELOPER_WORKFLOWS_TAB_REQUIREMENTS.md`](../DEVELOPER_WORKFLOWS_TAB_REQUIREMENTS.md) | workflow-builder MVP implemented; extended requirements partial |
| [`DIMENSION_HIERARCHY_UI_INSTRUCTIONS.md`](../DIMENSION_HIERARCHY_UI_INSTRUCTIONS.md) | tree/structural UI implemented; property-derived builder partial |
| [`GRID_CHART_WIDGET_MVP_INSTRUCTIONS.md`](../GRID_CHART_WIDGET_MVP_INSTRUCTIONS.md) | UI implemented with nonconformant client resolver |
| [`FORMULA_CALCULATION_INSTRUCTIONS.md`](../FORMULA_CALCULATION_INSTRUCTIONS.md) | current support matrix and convergence plan |
| [`AI_ASSISTANT_SOW.md`](../AI_ASSISTANT_SOW.md) | partially implemented SOW and remaining delta |
| [`examples/budgeting-demo/BUILD_INSTRUCTIONS.md`](../examples/budgeting-demo/BUILD_INSTRUCTIONS.md) | implemented demo acceptance contract |

## UX decision history

| Document | Classification |
|---|---|
| [`UI_UX_CONSISTENCY_AUDIT.md`](../UI_UX_CONSISTENCY_AUDIT.md) | historical audit with completed and remaining findings |
| [`UX_UPGRADE_INSTRUCTIONS.md`](../UX_UPGRADE_INSTRUCTIONS.md) | active UX backlog/reference, not architecture authority |

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
