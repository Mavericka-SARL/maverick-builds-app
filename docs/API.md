# HTTP API Guide

> **Classification:** Current — HTTP surface as served by internal/gateway.

> **Last verified:** 2026-09-25
> **Router authority:** `internal/gateway.NewHandler` (`registerRoutes`)

## Transport and context

The browser-facing API is served by `cmd/gateway` on port 8080. Development
Vite proxies `/api` requests to that port.

Request context:

- `Authorization: Bearer <Keycloak access token>` — sent by the console's API
  client on every request, and the only accepted credential when
  `DEV_MODE=false`: the gateway validates it against the realm's JWKS, and a
  gateway that cannot build its validator refuses to start;
- `X-Dev-User: <persona-or-keycloak-sub>` only when the gateway runs with
  `DEV_MODE=true`; and
- `X-App-Id: <application UUID>` to select the current application.

The server normally resolves model and active revision from the selected
application. Resource handlers check explicit model, revision, grid, form,
dashboard, widget and job IDs — from the path and from the request body —
against the actor's accessible scope.

Public (no actor) routes: `/healthz`; `/api/signup` and
`/api/signup/options`; `/api/legal`; `/api/branding`; `/api/sso/discover`;
and the OAuth provider callback `GET /api/integrations/oauth/callback`.
`/api/scim/v2/*` authenticates with the tenant's own SCIM bearer token, not a
user. The development persona catalog (`/api/dev/personas`) answers only when
`DEV_MODE=true`.

## Route groups

The table documents route families rather than every method and suffix;
`api/openapi.yaml` has every operation. "Authenticated" means any signed-in
user with a role; the handler then applies the scope checks above.

| Route family | Purpose | Role guard |
|---|---|---|
| `/healthz` | process health | public |
| `/api/signup`, `/api/signup/options` | self-service sign-up and the plan it offers | public |
| `/api/legal`, `/api/branding`, `/api/sso/discover` | legal document values, sign-in branding, SSO discovery by e-mail domain | public |
| `/api/scim/v2/*` | SCIM 2.0 user and group provisioning (enterprise) | SCIM token |
| `/api/dev/personas` | development persona catalog | development mode |
| `/api/me`, `/api/license` | actor and roles; the deployment's edition and features | authenticated |
| `/api/apps` | actor-visible applications | authenticated + app grants |
| `/api/demo` | active app/model/revision/persona context | authenticated + app/model access |
| `/api/grid`, `GET /api/grid/export` | grid definition, cells, totals, access metadata; CSV/XLSX export of input values | authenticated + model/member/metric access |
| `/api/metrics`, `/api/dimensions`, `/api/formula/refs` | runtime metric and dimension summaries, formula reference catalog | authenticated + model access |
| `/api/cells` | input fact writeback and recalculation | authenticated + shared write guard |
| `GET /api/cells/history` | one input cell's change history, including archived rows (enterprise) | authenticated + metric and member access, hidden ancestors included |
| `/api/dashboards`, `/api/folders` | role-visible dashboard runtime | authenticated + business-role assignment |
| `POST /api/dashboard-widgets/{id}/chart-data` | server-resolved chart series used by every chart widget | dashboard + member + metric access |
| `/api/tasks`, `/api/tasks/{id}` | inbox and task decisions | authenticated + task eligibility |
| `/api/workflow/submit`, `/api/workflow/instances`, `/api/workflow/my-history` | start, inspect and act on instances | authenticated; admin actions `business_admin` |
| `/api/workflow/history` | all instances | `business_admin` |
| `/api/notifications`, `…/mark-read` | list and mark read (own notifications only) | authenticated |
| `/api/notifications/settings*` | per-tenant e-mail delivery settings and a test send | `platform_admin` or `tenant_admin` |
| `/api/forms*`, `/api/records*` | form runtime and record actions; CSV/XLSX form export/import | authenticated + model/form context |
| form definition create/update/delete under `/api/forms` | building forms | `developer` |
| `/api/automation/rules*` | automation rules (list: authenticated; create/update/delete, including cron schedules) | `developer` |
| `/api/automation/trigger/{id}`, `/api/automation/executions` | fire a manual rule, execution log | authenticated + app scope |
| `/api/import/upload`, `/api/import/jobs*`, `/api/import/sheets/*` | CSV/XLSX/Google Sheets stage, validate, commit, jobs | authenticated + model scope + shared write guard |
| `POST /api/import/dimension-members` | bulk member import | `developer` |
| `/api/integrations/*` | run saved integrations | authenticated + model scope |
| `/api/developer/applications*` | application list/create for builders | `developer`, `platform_admin` or `tenant_admin` |
| `/api/developer/*` (everything else) | models, revisions, metrics, dimensions, grids, dashboards, folders, workflows, workflow roles and trigger events, integrations and connections, form integrations, migrations, debug facts | `developer` |
| `/api/ai/*` | AI sessions, documents, settings, proposals, draft promotion/discard | `developer` |
| `/api/business-admin/roles*` | business roles and their members | `business_admin` or `developer` |
| `/api/business-admin/*` (everything else) | users, dashboards, access rules | `business_admin` |
| `/api/admin/users*`, `/api/admin/workspaces*` | user and workspace administration | `platform_admin`, `tenant_admin` or `developer` |
| `/api/admin/*` (everything else) | tenants, applications, models, revisions, grants, audit (listing, export, retention settings), usage, plans, branding, SSO, SCIM tokens, AI settings, infrastructure nodes | `platform_admin` or `tenant_admin` |
| `GET /api/admin/models/{id}/export`, `…/export/package`, `POST /api/admin/models/import` | revision-aware model export (`?include_data=`), standalone package, import | `platform_admin` or `tenant_admin` |

Enterprise and commercial features answer **403** with the feature name when
the deployment's licence does not include them. A tenant whose plan has ended
or run out of storage is read-only: mutations answer **402**.

## Access and error behavior

Role admission is only the first gate. Handlers also verify customer/workspace,
application/model, dashboard, dimension-member, metric, revision, and workflow
scope as applicable.

Cell, form and import mutations share `internal/writeguard.CheckWrite`, which
rejects:

- system-managed target revisions;
- hidden or read-only dimension members;
- descendants of hidden ancestors; and
- scopes locked by running or approved workflows.

The query gRPC writeback applies the same guard. A rule lookup that fails
answers an error: the guard fails closed.

Known gaps: import does not refuse a calculated (non-input) metric, and
`POST /api/cells` skips dimension codes it cannot resolve rather than rejecting
them. The HTTP and gRPC import services are different implementations: HTTP
supports the current CSV/XLSX/name-resolution flow, while gRPC retains a legacy
CSV/UUID layout and different staging/commit semantics.

HTTP errors are returned as JSON (`{"error": "..."}`) by the gateway helpers.

## OpenAPI status

`api/openapi.yaml` describes every route the gateway registers (250
operations); `route_spec_parity_test.go` fails CI when the router and the spec
disagree. It generates `internal/gateway/oas`, but `cmd/gateway` serves the
hand-written router, not the generated ogen server. When adding a route,
register it and add it to the spec in the same change.

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
