# HTTP API Guide

> **Classification:** Current — HTTP surface as served by internal/gateway.

> **Last verified:** 2026-07-20
> **Router authority:** `internal/gateway.NewHandler`

## Transport and context

The browser-facing API is served by `cmd/gateway` on port 8080. Development
Vite proxies `/api` requests to that port.

Intended request context:

- `Authorization: Bearer <JWT>` in production;
- `X-Dev-User: <persona-or-keycloak-sub>` when the gateway runs with
  `DEV_MODE=true`; and
- `X-App-Id: <application UUID>` to select the current application.

The server normally resolves model and active revision from the selected
application. Most resource handlers also check explicit model, revision, grid,
form, dashboard, or widget IDs against the actor's accessible scope. The
exceptions listed below are current defects, not supported authorization
semantics.

`/healthz` is unauthenticated. The development persona catalog is available
only when `DEV_MODE=true`. Do not assume every other route resolves an actor:
the fact-debug, import-job deletion, and notification mark-read exceptions are
documented under Security exceptions.

The production authentication contract is currently incomplete: the frontend
API client never sends the Keycloak token held by `AuthProvider`, and
`cmd/gateway` never initializes its JWKS validator. Because actor resolution
falls back when that validator is nil, the current HTTP runtime uses
development-persona resolution even with `DEV_MODE=false`.

## Route groups

The table intentionally documents route families rather than every supported
method and suffix. Check the handler before constructing a mutation.

| Route family | Purpose | Primary authorization |
|---|---|---|
| `/healthz` | process health | public |
| `/api/dev/personas` | development persona catalog | development mode |
| `/api/me` | actor and role resolution without app/model context | authenticated actor |
| `/api/apps` | actor-visible applications | authenticated + app grants |
| `/api/demo` | active app/model/revision/persona context | authenticated + app/model access |
| `/api/grid` | grid definition, visible dimensions, cells, totals, access metadata | authenticated + model/member/metric access |
| `GET /api/grid/export` | one grid's raw input-metric values as CSV/XLSX (dimension/metric names as columns, one row per member combination); round-trips through `/api/import/upload` | authenticated + model/member/metric access, same hidden-member filtering as `/api/grid` |
| `/api/metrics` | runtime metric summary | authenticated + model/metric access |
| `/api/cells` | input fact writeback and recalculation | authenticated + shared write guard; incomplete cross-model ID validation |
| `/api/formula/refs` | formula reference catalog | authenticated/model context |
| `/api/dashboards`, `/api/folders` | role-visible dashboard runtime | authenticated + business-role assignment |
| `/api/dashboard-widgets/{id}/chart-data` | server-resolved chart series; implemented but unused by the runtime chart widget | dashboard + member + metric access |
| `/api/tasks`, `/api/tasks/{id}` | inbox and task decisions | authenticated + task eligibility |
| `/api/workflow/*` | submit/start, history, instance administration | mixed actor/business-admin rules |
| `/api/notifications*` | list and mark read | list resolves actor; mark-read currently lacks ownership validation |
| `/api/forms*`, `/api/records*` | form definition runtime and record actions | authenticated + model/form context |
| `GET /api/forms/{id}/export` | that form's records as CSV/XLSX (field names as columns, plus `id`/`status`/`created_at`) | authenticated + model/form context |
| `POST /api/forms/{id}/import` | bulk-creates records from an uploaded CSV/XLSX (same `{csv \| xlsx_base64}` envelope as `/api/import/upload`); columns match by field name or label; whole-file validation, applies live form-metric mappings per created record | authenticated + model/form context |
| `/api/automation/*` | rules, manual triggers, and execution logs | authenticated; mutations validate app/revision context |
| `/api/import/*` | CSV/XLSX stage, validate, commit, jobs | normal paths use actor/model + shared write guard; delete/legacy UUID exceptions below |
| `/api/integrations/*` | saved integration runtime | authenticated/model context |
| `/api/developer/*` | model, revision, metric, dimension, grid, dashboard, form, workflow, automation, integration, and migration building | `developer`, with explicitly shared admin routes where coded; debug-facts exception below |
| `/api/ai/*` | developer AI sessions, documents, settings, proposals, draft promotion/discard | `developer`, `platform_admin`, or `tenant_admin` through the current shared guard |
| `/api/business-admin/*` | business roles, members, dashboards, and access rules | `business_admin` |
| `/api/admin/*` | customers/workspaces/apps/models/revisions/users/grants/audit | `platform_admin` or `tenant_admin`, with scoped exceptions |
| `GET /api/admin/models/{id}/export` | revision-aware model export | `tenant_admin` only |
| `POST /api/admin/models/import` | revision-aware model import | `tenant_admin` only |

## Access and error behavior

Role admission is only the first gate. Handlers also verify customer/workspace,
application/model, dashboard, dimension-member, metric, revision, and workflow
scope as applicable.

Cell and import mutations share `internal/writeguard.CheckWrite`, which rejects:

- system-managed target revisions;
- hidden or read-only dimension members;
- descendants of hidden ancestors; and
- scopes locked by running or approved workflows.

That statement applies to the HTTP cell path and import commit paths. The query
gRPC writeback uses a separate policy path. Also, the current member-rule lookup
fails open on SQL errors; production security hardening must distinguish “no
rule” from “could not evaluate rule.”

HTTP errors are returned as JSON by gateway helpers. Do not depend on every
route having the generated OpenAPI `Error` shape until it is covered by the
contract.

## Security and integrity exceptions

These are verified current gaps and should be treated as release blockers:

| Route/path | Current behavior |
|---|---|
| `GET /api/developer/debug/facts` | resolves neither actor nor model and returns the latest facts across tenants |
| `DELETE /api/import/jobs/{id}` | deletes by known job ID without actor/model ownership resolution |
| `POST /api/notifications/mark-read` | updates supplied notification IDs without actor ownership resolution |
| `POST /api/cells` | verifies `is_input` but not metric ownership by the supplied model/revision; unknown dimension identifiers can bypass guard resolution while raw JSON is stored |
| import with direct `metric_id` UUID | does not verify model/revision ownership or `is_input`; prefer scoped name resolution until fixed |

Full-reload import also deletes the target model/revision's facts after checking
only members represented in the staged file. Import-job listing is model-scoped,
not per-actor. The HTTP and gRPC import services are different implementations:
HTTP supports the current CSV/XLSX/name-resolution flow, while gRPC retains a
legacy CSV/UUID layout and different staging/commit semantics.

## OpenAPI status

`api/openapi.yaml` covers the stabilized original core endpoints and generates
`internal/gateway/oas`. It does not currently describe the full manual router,
and `cmd/gateway` does not register the generated ogen server.

When extending the HTTP API, either:

1. add the route to the manual gateway and update this guide, or
2. make the OpenAPI contract authoritative for that route and wire the
   generated interface into the composition root.

Do not imply complete OpenAPI coverage while these two routing surfaces remain
separate.

## gRPC contracts

Internal service contracts live under `proto/*/v1`. They are independently
buildable and deployed by the distributed Kubernetes topology, but they are not
a one-to-one mirror of the browser HTTP API. Consult the service's proto and
`cmd/<service>/main.go` together.

## REST API connector

Connector endpoints (developer role) follow an atomic contract: for
`type: rest_api`, `POST/PATCH /api/developer/integrations[/{id}]` carry
metadata, the typed `rest_api/v1` config document and the schedule in one
call — there is no create-then-raw-config sequence. `POST {id}/test`
(`?dry_run=1` for the mapping dry-run report) and
`POST /api/integrations/{id}/run` return **202** with a `run_id`; poll
`GET /api/developer/integration-runs/{runId}` only while it reports
`queued`/`running`, and cancel via `POST …/cancel`. Testing a non-GET method
requires `acknowledge_side_effects=1`. Activation is refused until the
CURRENT configuration hash has a successful test; editing any
request-critical field (request, auth, response, pagination, direction,
target) invalidates it.

Credential connections (`/api/developer/integration-connections`) hold
secrets write-only — `secret` absent = keep, `null` = remove, object =
replace; responses carry only `has_secret`, and a connection referenced by an
integration cannot be deleted. Secrets are sealed with mandatory AES-256-GCM
under `INTEGRATION_CRED_KEY` (32+ bytes; required by the gateway and the
integration worker, no plaintext fallback). Run rows carry a machine
`error_code` — `dns | blocked_host | tls | timeout | auth | rate_limit |
http_error | invalid_data | too_large | cancelled | internal` — with
sanitized messages and query-stripped attempt URLs. The gateway never
executes connector requests; `cmd/integration` claims queued runs and is the
only egress path (dev-only `INTEGRATION_ALLOW_INSECURE=1`/`DEV_MODE=true`
permit loopback fixtures).

Connections of auth type `oauth2_authorization_code` (added 2026-09-21; the
flow the connector had reserved) are connected once by a developer: `POST
/api/developer/integration-connections/{id}/oauth/start` records a pending
authorisation (PKCE S256, 15 minutes, a row so any gateway replica can
finish it) and answers the provider URL; the provider returns the browser
to the public `GET /api/integrations/oauth/callback`, which exchanges the
code with the connection's client secret — HTTP Basic, or in the body when
`meta.token_client_auth` is `post` — seals `access_token`, `refresh_token`
and `expires_at` next to the secret, and redirects to the console with
`?oauth=connected` or `?oauth=error&oauth_error=…`, never with a token. The
tokens belong to the connection: scheduled runs use them as they use a
bearer, refreshing (and storing a rotated refresh token) when expired.
`…/oauth/disconnect` forgets the tokens and keeps the client. Register
`<CONSOLE_URL>/api/integrations/oauth/callback` with the provider.

Google Sheets imports (`POST /api/import/sheets/fetch`, integrations of type
`google_sheets`) read a link-shared sheet through Google's CSV export, or a
PRIVATE sheet through the tenant's own Google service account once someone
who builds in the application has stored its key file
(`PUT /api/developer/integrations/google-service-account`, under
Integrations › Google Sheets; `POST …/test` proves the key by obtaining a
token; developers and the tenant's administrators, scoped to the tenant of
the application in `X-App-Id`). One account per tenant, sealed in
`core.tenant_credential` with the same mandatory encryption, AAD-bound to
the tenant; only the address and the private key are kept. The fetch is the
Sheets API under a JWT bearer grant scoped to `spreadsheets.readonly`; the
sheet's owner shares it with the account's address like any collaborator.
There is no deployment-wide account.
