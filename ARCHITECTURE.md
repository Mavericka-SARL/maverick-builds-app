# maverickbuilds.app Architecture

> **Classification:** Current — Implementation reference for the system as built.

> **Status:** Current implementation reference
> **Last verified:** 2026-09-25
> **Authority:** Running code, migrations, build configuration, and tests take
> precedence if this document drifts.

## System purpose

maverickbuilds.app is a metadata-driven business application platform. One
application can combine three operating modes:

- planning: dimensions, metrics, formulas, grids, revisions, writeback, and
  dashboards;
- CRUD: forms, records, mappings, integrations, and audit history; and
- execution: workflows, tasks, approvals, automations, notifications, and
  controlled business actions.

The current ownership graph is:

```text
Customer
  +-> Workspace (role/access grouping)
  +-> Application -- optional workspace_id compatibility link
        -> Model
          -> Revision
```

Migration 030 flattened application ownership: `core.application.customer_id`
is required and `workspace_id` is nullable. Workspaces remain useful for role
assignments and compatibility, but they are not a mandatory ownership hop.
Applications carry the user-facing mode and status. A model owns semantic and
runtime definitions. A revision is the isolation boundary for model content,
including metrics, dimensions, grids, dashboards, forms, integrations,
workflows, automations, and facts. `core.model.active_revision_id` selects the
default runtime revision.

## Runtime topology

### Primary local and browser runtime

The path exercised by `dev.sh`, the React application, demo scripts, and most
end-to-end tests is a modular monolith:

```text
Browser
  |  React/Vite, one console whose sections are composed from the user's roles
  |  Authorization: Bearer <Keycloak token>; X-Dev-User only when DEV_MODE=true
  |  X-App-Id selects the active application context
  v
cmd/gateway
  |  net/http ServeMux + OpenTelemetry HTTP wrapper
  |  role and resource-scope guards
  |  internal packages called in-process
  v
PostgreSQL
```

`cmd/gateway/main.go` connects to PostgreSQL, applies embedded migrations, and
serves `internal/gateway.NewHandler` on port 8080. The gateway directly composes
the calculation, CRUD, formula, import, query, schema-migration, workflow, and
write-guard packages. This path does not make gRPC calls to the sibling service
binaries.

Cell writes, imports, form postings, approval copies, and relevant definition
changes invoke recalculation in-process. This keeps the primary local runtime
functional when NATS is unavailable.

### Distributed service topology

The repository also builds separate gRPC binaries:

| Binary | Domain package | External dependency beyond PostgreSQL |
|---|---|---|
| `identity` | `internal/identity` | Keycloak JWKS and Redis session cache |
| `tenant` | `internal/tenant` | none |
| `model` | `internal/model` | none |
| `schema-migration` | `internal/schemamigration` | none |
| `policy` | `internal/policy` | Redis decision cache |
| `query` | `internal/query` | NATS JetStream and policy gRPC client |
| `calculation` | `internal/calculation` | NATS JetStream consumer |
| `workflow` | `internal/workflow` | policy gRPC client |
| `import` | `internal/importpkg` | optional NATS publisher |
| `audit` | `internal/audit` | none |
| `notification` | `internal/notification` | none |
| `ai-assistant` | `internal/aiassistant` | Anthropic for the legacy gRPC path |
| `integration` | `internal/integration` | outbound HTTPS to connector targets (worker, not a gRPC service) |

These binaries register protobuf services from `proto/*/v1` using the shared
gRPC server helper (`pkg/grpcutil.NewServer`), and every one installs
`grpcutil.AuthInterceptor`, which resolves a real actor per call (bearer token
validated against the JWKS, or the dev persona in `DEV_MODE`), plus the audit
interceptor. When the `MTLS_*` variables are set the transport additionally
requires a CA-signed client certificate (`pkg/grpcutil/tls.go`). The Kubernetes base manifests deploy this topology together
with PostgreSQL, PgBouncer, NATS, Redis, Keycloak, and MinIO.

Query treats NATS startup as mandatory. Query and workflow always construct a
policy client, although gRPC dialing is lazy; an unreachable policy service can
therefore surface later as request-time fail-open or fail-closed behavior.
Import alone treats its NATS publisher as optional.

The distributed topology is deployed (see Infrastructure and operations) and
exercised by `cmd/verify-topology`, but it is not the default local
composition: `make dev-up` starts only infrastructure, and `dev.sh` starts
only the HTTP gateway. The gateway's browser API remains the most complete
product path. Several gRPC stores still use legacy, model-wide or scenario-era
semantics and do not match current revision isolation, hierarchy fields, formula
references, or gateway validation. Production readiness of this composition
must therefore be demonstrated, not inferred from successful compilation.

## Backend composition

### HTTP gateway

`internal/gateway/handler.go` is the current product composition root. Its route
groups are:

- context and planning: `/api/apps`, `/api/me`, `/api/demo`, `/api/grid`,
  `/api/metrics`, `/api/cells`, `/api/formula/refs`;
- dashboards and charts: `/api/dashboards`, `/api/folders`, and
  `/api/dashboard-widgets/{id}/chart-data`;
- tasks, workflows, and notifications: `/api/tasks`, `/api/workflow/*`, and
  `/api/notifications`;
- forms and automations: `/api/forms`, `/api/records`, and `/api/automation/*`;
- imports and integrations: `/api/import/*`, `/api/integrations/*`, and
  developer integration configuration;
- developer model-building: `/api/developer/*`;
- AI assistant: `/api/ai/*`;
- business administration: `/api/business-admin/*`; and
- tenant/platform administration: `/api/admin/*`.

The router is self-documenting: every route is registered through
`register()`/`BuildRoutes()`, and `route_spec_parity_test.go` fails CI when a
route and `api/openapi.yaml` disagree, so the spec covers every operation
(250). The generated ogen package in `internal/gateway/oas` is built from that
spec but is not the router `cmd/gateway` serves. [docs/API.md](docs/API.md)
is the readable overview.

### Domain packages

| Package | Responsibility |
|---|---|
| `internal/model` | Semantic definitions, dependency graph, cycle detection, revision publishing |
| `internal/formula` | Lexer, parser, AST, references, typed values, functions, and Excel-style evaluation |
| `internal/calculation` | Dependency-aware affected-metric traversal, partition state, evaluation, result persistence, optional JetStream consumer |
| `internal/query` | Fact write/query service, chart data resolution, access-filtered aggregation, and JetStream publishing |
| `internal/crudapp` | Form definitions and record persistence |
| `internal/workflow` | Definitions, instances, steps, routing, joins, automations, and execution logs |
| `internal/importpkg` | CSV/XLSX parsing, name/ID resolution, staging, validation, and commit |
| `internal/writeguard` | Shared system-managed, member-access, ancestor-cascade, and workflow-lock checks |
| `internal/identity` | User persistence, JWKS validation, and Redis-backed session caching |
| `internal/policy` | Role, RACI, metric/dimension/cell/ABAC evaluation and Redis caching for the gRPC path |
| `internal/aiassistant` | Provider adapters, chat/session stores, documents, proposals, read tools, and isolated-revision writes |
| `internal/schemamigration` | Model-driven schema migration generation and tracking |
| `internal/deployment` | Standalone deployment package builder (tar.gz: entity graph via `internal/modeltransfer`, full migrations tree, provenance manifest, infra-only compose), fully in-memory — no longer a standalone binary or gRPC service, and no longer writes to local disk; retention goes through `pkg/objectstore` |
| `internal/modeltransfer` | Model entity-graph export/import — the shared core behind both the admin JSON export/import HTTP endpoints and `internal/deployment`'s tarball builder |
| `internal/audit` | Partitioned audit events |
| `internal/notification` | Notification persistence, e-mail dispatch (SMTP, STARTTLS or implicit TLS) and task reminders |
| `internal/integration` | REST API connector definitions, sealed connections, the durable run queue and the worker |
| `internal/rollup` | Shared hierarchy rollups used by the grid, charts and the calculation scheduler |

## Data architecture

### PostgreSQL as system of record

PostgreSQL 16 is the sole authoritative database in the current product path.
Migrations are embedded from `migrations/` and applied by the gateway at
startup. PgBouncer is available locally on port 5433, while default development
connections use PostgreSQL directly on port 5432.

`pkg/migrate` serializes concurrent runs with a transaction-scoped Postgres
advisory lock (`pg_advisory_xact_lock`, safe through PgBouncer), and verifies
each applied file's checksum, so a released migration must never be edited
(`migrations/checksums.txt`). An individual migration plus its registry insert
is still not one transaction, so a mid-file failure can leave schema state
ahead of the registry.

The migration history currently ends at `095_image_widget.sql` (97 files).
Fourteen named schemas organize the data:

| Schema | Main ownership |
|---|---|
| `core` | customers, workspaces, applications, models, schema-version history |
| `identity` | users, role assignments, business roles, dashboard membership, access rules, app/model grants, API keys |
| `security` | RACI, dimension-member, metric, cell, and ABAC policy definitions |
| `model` | revisions, dimensions, members, properties, metrics, dependencies, grids, dashboards, forms, mappings, integrations |
| `runtime` | input facts, calculated results, partition state, form records, form postings |
| `workflow` | workflow definitions and runtime, steps, automation rules and executions |
| `import` | import jobs, errors, and staging rows |
| `audit` | partitioned audit events and partition registry |
| `notification` | in-app/email/webhook notification state |
| `deployment` | per-model auto-generated reporting-view schema migrations (`deployment.schema_migration`) |
| `ai_assistant` | sessions, messages, settings, proposals, documents, and legacy actions |
| `ops` | node statistics for the infrastructure view |
| `platform` | cross-tenant directories and settings: plans, SSO and branding domains, tenant databases, user/application directories |
| `storage` | object-store metadata (`storage.object`) for `pkg/objectstore` |

Some early tables were renamed or retired. In particular, the current model
revision table is `model.revision`; legacy `scenario`, `snapshot`, and
`core.revision` terminology in old planning documents is not current.

Two similarly named concepts must remain distinct:

- `model.revision` isolates editable/published model definitions and facts —
  the only unit of change; and
- `core.schema_version` records structural schema publication history.

The former `model.version` catalog object was dropped by migration 057.

Row-level-security policies exist on core workspace/application/model tables,
but current services do not set the `app.user_id` session variable that activates
them. The policies also retain workspace-based joins that predate flattened
application ownership. Normal development connections use the generic database
account rather than the least-privilege service roles created by migration 012.
RLS and service roles are therefore defense-in-depth assets awaiting runtime
wiring, not active authorization boundaries.

### Revision isolation

Revision isolation was introduced incrementally and completed by migration 056.
Definitions that participate in a built application are revision-scoped. When a
revision is duplicated, IDs and internal references are remapped so the copy is
self-contained, including grid rollup sources, property-derived dimensions,
form mappings, dashboards, workflows, and automations.

Duplication runs in one transaction (`duplicateRevision` takes a `pgx.Tx`), so
a failed copy leaves no partial revision behind, and the copy is recalculated
afterwards. Model export/import (`internal/modeltransfer`) applies the same
remapping.

Runtime facts use `revision_id`. `system_managed` revisions are read-only to
interactive cell writeback and import; workflow approval actions can populate
them through controlled copy operations.

### REST API connector

`internal/integration` + `cmd/integration` implement the Integrations → REST
API constructor. The gateway only validates, persists and enqueues (202 + run
id); the dedicated worker claims runs from a durable Postgres queue
(`integration_run`, `FOR UPDATE SKIP LOCKED` with lease reclaim) and is the
ONLY process that performs connector HTTP — through a pinned-dial client that
refuses loopback/private/link-local/CGNAT/metadata/multicast destinations
(IPv4+IPv6, inet_aton spellings included), re-validates every redirect,
strips credentials across origins, ignores proxy env vars, and cannot skip
TLS verification. Credentials live in application-scoped
`integration_connection` rows sealed with mandatory AES-256-GCM
(INTEGRATION_CRED_KEY; associated data binds app+connection; no plaintext
mode) and never leave the server. Pull commits ride the standard import
pipeline (ResolveRows → CommitImport → writeguard → recalculation) under a
real acting principal; schedules fire in the worker with per-tick dedup. The
worker's NetworkPolicy allows DNS + pgbouncer + public 443 only.

### Planning facts and calculations

Input values are append-style rows in `runtime.fact_input`; readers select the
latest direct value per metric/dimensional intersection and combine active form
posting contributions. "Latest" is deterministic: every reader orders by
`entered_at DESC, id DESC` — the tiebreak matters because a bulk import writes
many rows in one transaction and they all share the transaction's `now()`, so
without it different readers could legitimately pick different rows for the
same cell. Calculated values are stored in `runtime.calc_result`: one row per
leaf intersection, a `'{}'` whole-model aggregate per metric, rollup-member
rows for `formula`/`rate` metrics, and — for every metric with two or more
dimensions — one single-dimension "slice" row per (dimension, member) with all
other dimensions aggregated. The grid endpoint uses those shapes for its fast
reads: `meta_only=1` (structure only), `totals_only=1` (per-metric totals, no
cells — the dashboard-KPI shape), and `scope=` (a pinned-subtree read whose
calc totals come from slice rows when every visible calc metric has one,
falling back to a live re-resolution otherwise).

Metric formulas are named expressions rather than spreadsheet cell addresses.
The shared `internal/formula` engine supports arithmetic, comparison, logical
operations, brace references for backward compatibility, and named dimension
values. Its registered functions are:

- logic: `IF`, `IFS`, `AND`, `OR`, `NOT`, `IFERROR`, `IFNA`, `SWITCH`;
- math: `ABS`, `INT`, `ROUND`, `ROUNDUP`, `ROUNDDOWN`, `CEILING`, `FLOOR`,
  `MOD`, `POWER`, `SQRT`;
- aggregation: `SUM`, `AVERAGE`, `MIN`, `MAX`, `COUNT`, `COUNTA`;
- text: `CONCAT`, `TEXTJOIN`, `LEN`, `LEFT`, `RIGHT`, `MID`, `UPPER`,
  `LOWER`, `TRIM`, `TEXT`, `SUBSTITUTE`; and
- date: `TODAY`, `DATE`, `YEAR`, `MONTH`, `DAY`, `DAYS`, `EDATE`, `EOMONTH`.

Time-series functions over an explicit time dimension live in
`internal/formula/time.go`: `PREVIOUS`, `NEXT`, `LAG`, `LEAD`, `OFFSET`,
`CUMULATE`, `DECUMULATE`, `MOVINGSUM`, `YEARTODATE`, `QUARTERTODATE`,
`MONTHTODATE` and time summaries (see
[TIME_SERIES_FUNCTIONS_IMPLEMENTATION.md](TIME_SERIES_FUNCTIONS_IMPLEMENTATION.md)).
Conditional aggregations such as `SUMIF`/`SUMIFS`, lookup functions, and dotted
property references are not implemented. Dependencies are explicit in
`model.calc_dependency` and are cycle-checked.

`internal/calculation.Scheduler.RecalcAffected` traverses transitive dependents,
orders them topologically, evaluates affected metrics for every known
dimension combination, and persists one `calc_result` row per intersection
(plus the aggregate, rollup and slice rows described above). A full recalc
resolves the model's `active_revision_id`, and a combination that fails to
evaluate is reported as an error rather than silently dropped from an
aggregate. The grid, dashboards and charts all read these server-computed
values: `ChartWidget` calls `POST /api/dashboard-widgets/{id}/chart-data`,
which resolves and rolls up server-side with the viewer's access rules applied.
There is no formula evaluator in the browser.

### Dimension relationships and rollups

Mavericks distinguishes:

- same-dimension hierarchy through `dimension_member.parent_member_id`;
- cross-dimension structure through `dimension_def.parent_dimension_id` plus
  member parent links; and
- property-derived grouping through `dimension_def.source_dimension_id` and
  `source_property`.

`grid_def.rollup_source_grid_id` lets a read-only grid present another grid's
metrics at a related grain. Hidden ancestor access rules cascade to descendants
before grid and chart facts are serialized; a rule-lookup error fails closed.

## Security architecture

### Authentication

The browser signs in with Keycloak OIDC (PKCE) and the API client sends the
token as `Authorization: Bearer` on every request, including streaming and
uploads. Outside `DEV_MODE`, `cmd/gateway` builds a JWKS validator at startup
and exits if it cannot; `resolveActor` then accepts only a valid token, and
with no validator it refuses the request ("authentication not configured")
rather than falling back to a persona.

With `DEV_MODE=true`, `X-Dev-User` selects a seeded persona and
`/api/dev/personas` exposes the available identities. This is local and test
behavior only.

A few routes are public by design: `/healthz`, self-service sign-up and its
options, the legal documents, branding, SSO discovery, the OAuth callback, and
SCIM (which authenticates with its own per-tenant token).

### Authorization layers

Authorization is layered rather than represented by one role check:

1. platform role assignment: `platform_admin`, `tenant_admin`, `developer`,
   `business_admin`, or `business_user`;
2. customer/workspace scope derived from role assignments;
3. explicit application/model grants in `identity.user_app_access` and
   `identity.user_model_access`;
4. business-role membership and dashboard assignment;
5. member and metric access rules (`write`, `read`, `hidden`);
6. RACI rules used by policy evaluation and server-resolved workflow context;
7. workflow locks and system-managed revision protection for mutations.

The HTTP gateway applies role guards plus explicit app/model access checks,
and checks resource ownership for IDs taken from the path and the request
body. Direct HTTP cells, forms, imports and the gRPC writeback use the shared
write guard. `writeguard.HiddenAccess` fails closed on a database error, and
`HiddenInChain` / `ExpandHidden` cascade a hidden rule from any ancestor to its
descendants. Two known gaps remain: import does not refuse a non-input
(`is_input=false`) metric, and `/api/cells` skips unknown dimension codes
rather than rejecting them.

The gRPC policy path has additional ambiguity: query reads proceed unrestricted
when policy evaluation fails, an empty allowed-metric list is interpreted as no
restriction, and the query request places a model ID in the proto's
`application_id` field.

Model export/import is a tenant-admin and platform-admin operation; developers
do not receive it.

### Audit

Business, admin, model, access, workflow, and deployment mutations can append to
the partitioned `audit.audit_event` log. Audit coverage is handler-specific; do
not assume that the presence of the audit schema alone proves every read or
mutation is recorded.

## Workflow and automation architecture

Workflow definitions contain JSON step definitions and typed configuration for
task, approval, notification, condition, and join steps. Definitions can carry
a subject type/config and a context schema. Runtime instances own step rows,
decisions, comments, assignees, timestamps, and status.

The workflow engine supports sequential routing, conditional routes, parallel
fan-out, join synchronization, required comments, role/user assignment, inbox
and history views, and dashboard actions that start workflows.

Automation rules can be triggered manually, through the API, on form submit,
on form approval, on grid change, when an integration run completes or fails,
or on a cron schedule. Event-driven rules are dispatched by the HTTP gateway;
schedule rules are claimed and fired by `internal/workflow/scheduler.go`
(migration 059: cron expression, IANA time zone, misfire policy, retries), and
are created from the developer's Triggers tab for any manual workflow.

Workflow definitions and automation rules are revision-scoped. Workflow
approval can declare a generic `on_approve` fact-copy action, which writes the
approved scope into a system-managed revision.

The approval copy runs inside the approval's own transaction, so a failed copy
rolls the approval back, and it selects the latest row per metric and
intersection (`DISTINCT ON (metric_id, dim_members)`), so sibling metrics at
the same intersection are all copied.

## Import, integration, and transfer

The generic fact import accepts CSV and native XLSX. Files are parsed into a
common raw-row representation, resolved against metric names/IDs and dimension
members, validated, staged, and committed through the shared write guard.
Supported commit behavior and security validation are properties of the generic
import service, not demo-specific endpoints.

Name- and UUID-based import resolution is scoped to the selected
model/revision. Import does not yet refuse a calculated (non-input) metric.
HTTP and gRPC import expose different file formats, staging, and commit
semantics.

Form-to-metric mappings post approved or selected record states into planning
facts with sum/replace-style aggregation. Integration definitions and saved
import jobs are managed from the Developer Console.

Tenant and platform admins can export (with or without data) and import a revision-aware model package over the
admin HTTP API (`internal/modeltransfer`), and download a standalone
deployment package (`GET /api/admin/models/{id}/export/package`) — a tar.gz
containing that same entity graph, the complete `migrations/` tree, a
provenance manifest (source commit, build command, facts export policy), and
an infra-only docker-compose. `internal/deployment`'s builder delegates to
`internal/modeltransfer` rather than its own queries — the previous
implementation queried a removed `dimension_def.code` column and suppressed
query/scan errors, so a schema mismatch could emit an empty local archive
instead of failing; it also had no revision scoping and covered only
dimensions/metrics. The former gRPC transport (`cmd/deployment`, an
async job/status model with zero callers anywhere in the codebase) has been
retired in favor of one synchronous HTTP route. The built package is
retained via `pkg/objectstore` (a thin `minio-go/v7` client plus a
`storage.object` PostgreSQL metadata table, one row per revision — a
re-export overwrites the previous package rather than accumulating
history) instead of the local-disk write the builder used before: that was
the one production code path anywhere in this repository that touched a
filesystem, and it was concretely broken under the real Kubernetes topology
(`readOnlyRootFilesystem: true`, no writable volume on the gateway pod).
Access-control configuration
(`security.*`, `identity.business_role*`) and per-model test definitions are
deliberately not included — both are documented as known gaps in the
package's own manifest rather than silently omitted.

## AI assistant architecture

The Developer Console AI surface is implemented inside the HTTP gateway. It has
provider-neutral chat interfaces with OpenAI, Anthropic, Mistral, and DeepSeek
adapters; per-user provider settings; per-tenant keys
(`/api/admin/ai-settings`, optionally enforced); deployment-wide
`*_API_KEY` environment keys as the last fallback; persisted sessions and
messages; streamed responses; PDF/XLSX/DOCX/CSV/text document context; read
tools; validation tools; and proposal confirmation.

Provider keys use AES-256-GCM (`internal/secretbox`) when
`SECRETS_ENCRYPTION_KEY` (or the legacy `AI_KEY_ENCRYPTION_SECRET`) is set. The
store preserves legacy plaintext behavior when it is absent, so deployed
environments must configure that secret before accepting user keys.

AI write proposals execute against an isolated draft revision. A developer must
promote or discard that draft; the assistant does not write directly into the
active revision. The write-tool set covers model, dimension, metric, grid,
dashboard, form, workflow, automation, business-role, revision and migration
changes. One tool deliberately writes outside the draft:
`set_user_access_rules` writes live `identity.user_access_rule` rows against
the active revision. The assistant is available to the developer role only
(`/api/ai/*`). Per-action rollback is not implemented (`RollbackAction` is a
stub); see `AI_ASSISTANT_SOW.md`.

The separate `cmd/ai-assistant` gRPC binary is an older Anthropic-oriented
file-diff service and is not the implementation used by the current Developer
Console chat UI.

## Frontend architecture

The web application uses React 19, TypeScript 6, Vite 8, React Router 7,
TanStack React Query 5, Recharts 3, Lucide, Tailwind 4, and CSS custom
properties.

`web/src/main.tsx` composes BrowserRouter, React Query, authentication, and
`RoleRouter`. There is one console, `UnifiedConsole`: each role contributes a
section of sidebar groups (`web/src/router/sections.ts`) and a user with
several roles sees the union in one sidebar — never a second console or a
switcher. Where two roles offer the same screens the wider one wins
(business_admin over business_user, platform_admin over tenant_admin).

The application does not define React Router routes: navigation is
component-local state, so views are not deep-linkable and browser history
does not represent section changes. `web/src/App.tsx` is an unused duplicate
entry composition; `main.tsx` is authoritative.

Every console uses the shared `web/src/ui` design-system package and shell,
although coverage and visible context remain uneven. Application selection is
stored in `localStorage` and sent through `X-App-Id`; the reusable `AppPicker`
has no current consumer, and consoles implement ad-hoc selection/reload flows.
React Query owns server-state caching and invalidation, but some manually
composed keys omit application or revision context.

The business section contains planning grids, dashboards (whose widgets
include forms, imports, automation buttons and images), the task inbox,
history, and runtime actions. `NotificationCenter` sits in the console header
for every role. The developer section contains model definitions, revisions, grids, dashboard canvas,
workflows, automations, forms, integrations, access-aware user administration,
and AI. Schema migration runs automatically from definition changes rather than
through a general migrations tab; client methods and AI tools can also
generate/apply migrations, but there is no general Developer migrations UI.
Business Admin manages business roles and access rules. Platform/Tenant Admin
manages hierarchy, revisions, users, grants, audit (with export and
retention), SSO/SCIM, branding, AI keys, usage and tenant-owned model
transfer.

Dashboard charts are live grid views. Configuration lives in
`dashboard_widget.widget_props`. `ChartWidget` fetches its series from
`POST /api/dashboard-widgets/{id}/chart-data`, resolved and rolled up on the
server with the viewer's access rules; the browser only renders them.

The Developer dashboard editor stores absolute 20 px canvas coordinates and
stages move/resize/property changes. Business rendering clusters saved Y
positions into responsive rows and maps widths to a 60-column layout, so it
preserves order and approximate sizing rather than exact vertical canvas
geometry. Widget add/delete currently persist immediately.

Playwright covers console smoke behavior, UX gates, and mocked end-to-end
surfaces. It runs with the development persona system and does not, by itself,
exercise a real Keycloak deployment or every database workflow.

## Infrastructure and operations

The Docker development stack provides:

- PostgreSQL 16 with local WAL archiving;
- PgBouncer in transaction mode;
- NATS 2.10 with JetStream;
- Redis 7;
- Keycloak 24 with realm import;
- MinIO; and
- optional OpenTelemetry Collector, Prometheus, Loki, Tempo, and Grafana.

The gateway initializes OpenTelemetry and continues if the collector is
unreachable. The optional observability profile is started with `make obs-up`.

Production runs on Kubernetes (k3s on Hetzner; see
docs/HETZNER_DEPLOYMENT.md, and
[docs/SELF_HOSTING.md](docs/SELF_HOSTING.md) for other operators). The
repository carries:

- a shared distroless `deploy/docker/Dockerfile` for every service, plus
  `pg-backup` and `postgres-walg` images; CI publishes all of them to GHCR,
  tagged `:latest` and with the commit SHA, and a `workflow_dispatch` deploy
  job rolls the cluster onto them;
- Kustomize bases with Traefik `ingress.yaml`, cert-manager TLS,
  `networkpolicy.yaml` and a gateway PodDisruptionBudget; `prod.example` and
  `staging.example` overlays show the shape (a real production overlay lives in
  its operator's private repository);
- PostgreSQL WAL archived by WAL-G to object storage with point-in-time
  recovery, nightly `pg_dump` backups, and a backup watchdog; and
- optional service-to-service mTLS.

## Build, generation, and tests

Three contract generators are present:

- Buf generates Go protobuf and gRPC code into `gen/go`;
- sqlc generates typed audit and tenant query code; and
- ogen generates the OpenAPI package in `internal/gateway/oas` (pinned
  versions: `make gen`).

CI runs Go lint, race-enabled tests with coverage, Go build, sqlc and ogen drift
checks, Buf lint, frontend ESLint, TypeScript checking, Playwright smoke tests,
and the production web build.

Most package tests use package-local SQL fixtures. Gateway integration tests use
Testcontainers with a real PostgreSQL instance for access, revision, model
transfer, chart, workflow, import, and rollup behavior.

## Current architectural limitations

- The browser runtime and distributed gRPC topology overlap rather than sharing
  one complete, behaviorally equivalent composition path. Several gRPC policy
  error/empty-result paths fail open.
- RLS policies (migration 013) and least-privilege database roles exist in
  schema history, but no service sets `app.user_id`, so they are not active
  boundaries.
- A migration file and its registry insert are not applied in one transaction.
- Import accepts a non-input metric; `/api/cells` skips unknown dimension codes.
- Standalone deployment packages omit access-control configuration and
  per-model test definitions by design (see `internal/deployment`'s
  manifest.json known_gaps).
- AI provider-key encryption is configuration-dependent, and uploaded document
  content (extracted text, not the original file) is persisted in PostgreSQL.
  Neither AI document upload nor CSV/XLSX import retains original file bytes.
- AI proposals have no per-action rollback.
- Console navigation has no URL routes/deep links, context selection and query
  keys are uneven, and some large console modules remain dense despite the
  shared design system.

These are current boundaries, not promises that the associated target designs
are already delivered. The prioritized follow-up work is maintained in
IMPLEMENTATION_PLAN.md.
