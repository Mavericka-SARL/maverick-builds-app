# Model and operational reporting in ChatGPT and Claude — implementation instructions

> **Classification:** Target — Implementation brief; this feature is not yet implemented.
>
> **Prepared:** 2026-10-02. Repository evidence and external documentation checked on this date.
>
> **Status (2026-10-02):** implemented and tested against the gateway and a real
> Keycloak, **with the scope narrowed by the owner to grid data only**: charts
> and reports are made in ChatGPT or Claude from grids, never from dashboards,
> and nothing is saved in maverickbuilds.app. Forms, dashboards, workflows,
> triggers, notifications and the related-resource families below are out of
> the connector's scope, and the sections on them are superseded. Business
> users read only each model's active revision. Built: nine grid tools incl.
> `compare_grid`, `render_chart`, `render_report` (MCP Apps view + PNG
> fallback); per-host registered clients (`cmd/connector-clients`); token
> separation. Remaining: verification inside ChatGPT and Claude with a
> deployed connector. See [`docs/CHAT_CONNECTOR.md`](docs/CHAT_CONNECTOR.md).

## 1. Outcome

Build a read-only connection from ChatGPT and Claude to maverickbuilds.app.
After connecting their account once, a business user can discover their models,
read grids, forms, developer-created dashboards, workflows, triggers and related
operational resources, and receive reports, tables and charts inside the conversation. Users can read
an existing dashboard through chat without entering its screen in maverickbuilds.app.
Direct grid/form reporting must also work for a model with no dashboards.

Use one authenticated remote Model Context Protocol (MCP) server for both chat
clients. Add an MCP Apps interface for charts and reports inside the conversation.
ChatGPT and Claude perform the conversational reasoning; the engine supplies
authorized data, deterministic calculations and presentation data.

The connection always acts as the signed-in user. Enforce the effective access
restrictions configured by business administrators, developers and tenant
administrators within their authorized scope. Connecting a chat account grants
no additional role, data visibility or administrative authority.

Example requests that must work against configured model data:

- “Which models can I access, and what data is available in Sales Planning?”
- “Show monthly revenue this year as a line chart, filtered to Europe.”
- “Compare actual revenue with budget by product and show the variance.”
- “Read the expense form and summarize approved expenses by department.”
- “List the dashboards available to me in Sales Planning.”
- “Read the Executive Overview dashboard and explain its KPIs and charts.”
- “Show the revenue chart from that dashboard for Europe and summarize what changed.”
- “Which approval tasks are waiting for me, and which are overdue?”
- “Explain this published workflow and show the progress of requests I can access.”
- “List the triggers for this model and summarize their recent executions.”
- “Show the permitted integration run history and explain the recorded failures.”
- “Create a report of workflow completion times and trigger failures this month.”
- “Create a report with revenue trends, the five largest expense categories,
  and a table of exceptions.”
- “Use the same filters, change the chart to bars, and show last quarter.”

Names in examples are illustrative. Discover every model, source, metric,
field, dimension and member from configuration; do not hardcode them.

## 2. Scope and delivery boundary

Deliver account connection, authorized discovery, grid queries, form queries,
form aggregation, dashboard definition and widget-data reads, workflow/task
reads, trigger/execution reads, permitted related-resource reads, comparisons,
conversational reports and in-chat charts.
Support both business users and builders within each person's existing reach.
An ordinary business user must not need a developer or administrator role.

The first delivery includes bar, line, pie, scatter and histogram charts, plus
tables and KPI summaries. Reports may combine independent sections from grids,
forms, existing dashboards and operational activity. Dashboard reading includes its structure, readable
content, configuration and live permitted widget values, not just a screenshot
or a link. Describe supported operations and limits through tool schemas.
For workflows, distinguish published summaries, full builder definitions and
running/historical instances. “Other” means the explicitly supported resource
families in section 7, with each family retaining its own read policy.

Reads must not modify model definitions, input values, records, approvals,
revisions, dashboards, workflow/trigger state, notification read state or
integration configuration. Temporary query results,
operational audit records and rendered artifacts are permitted implementation
state. A “create report” request creates a conversational result, not a saved
dashboard or model object.

Keep model authoring, writeback, workflow execution/approval, firing or scheduling
triggers, integration runs, scheduled report delivery, unsolicited
notifications, public report publishing and arbitrary cross-source joins outside
this delivery. File downloads are a useful follow-up; they cannot substitute
for readable reports and rendered charts in the chat itself.

This document specifies the generic read capabilities needed for the requested
feature. Follow CLAUDE.md: reuse existing primitives, make extensions
available to the real business role, and raise any required permission widening
before implementing it. Adding a connector must not silently broaden data access.

## 3. Existing foundations and gaps

Code and registered routes are authoritative. Recheck these entry points when
implementation starts; do not assume the current REST API is already an MCP server.

| Need | Existing foundation | Required work |
|---|---|---|
| Applications and models | `GET /api/apps`; `userApps` and `appsIn` in `internal/gateway/handler.go` return accessible applications and their models | Adapt this discovery for chat; preserve tenant, application and model identities |
| Selected model and revision | Gateway context resolution, `X-App-Id`, `X-Model-Id`, explicit revision handling | Require explicit resolved context on subsequent tool calls; reject mismatched IDs |
| Grid values and metadata | `GET /api/grid`, including `scope`, `meta_only`, `totals_only`; `internal/gateway/scoped_calc.go` | Extract/reuse the read services for bounded, typed reporting queries |
| Grid catalog | `GET /api/developer/grids` is developer-gated | Add a minimal catalog for callers who may read the underlying grids; do not expose the developer route or its write capabilities |
| Metrics and dimensions | `GET /api/metrics`, `GET /api/dimensions`, grid metadata | Return only authorized metadata; add bounded member search/pagination where necessary |
| Forms and fields | `GET /api/forms`; `internal/crudapp`; `internal/gateway/form_record_access.go` | Expose form schemas and read capabilities without including write actions |
| Form records | `GET /api/forms/{id}/records`, `GET /api/records/{id}` | Add typed filters, stable pagination, projections and server aggregation with shared visibility checks |
| Dashboard discovery and structure | `GET /api/folders`, `GET /api/dashboards`, `GET /api/dashboards/{id}`; `businessFolders`, `businessDashboards`, `businessDashboardDetail` | Adapt the business-facing reads for chat with folder context, revision, widget configuration and dashboard permissions |
| Dashboard widget values | Chart-data endpoint, grid/form reads, `web/src/consoles/business/DashboardWidgets.tsx` | Add a bounded dashboard-widget read service preserving saved settings and per-widget source authorization |
| Published workflow summaries | `GET /api/workflow/definitions` | Preserve its published/application/revision scope and distinguish the summary from a full definition |
| Full workflow definitions | `GET /api/developer/workflows`, `GET /api/developer/workflows/{id}` | Expose approved definition fields only to a caller who already has the relevant builder read capability |
| Tasks and workflow history | `GET /api/tasks`, `GET /api/workflow/my-history`, `GET /api/workflow/history`; `internal/workflow/assignee` | Reuse task eligibility, participation and scoped administrative history policies separately; add complete bounded queries/aggregates |
| Triggers and executions | `GET /api/automation/rules`, `GET /api/automation/executions`; `internal/workflow/store.go` | Return authorized rule/schedule metadata and execution outcomes without triggering anything |
| Integrations and run history | `GET /api/integrations`, developer integration/run read routes | Add explicit safe projections and preserve each read route's role/resource policy |
| Notifications, revisions and history | Own `/api/notifications`, developer revision reads, `/api/cells/history`, `/api/admin/audit` | Add typed read adapters with recipient, role, source and licence checks as applicable |
| Grid chart calculations | `internal/query/chart.go`, `ChartResolver.Resolve` | Provide a standalone authorized query entry point taking source and chart configuration |
| Chart presentation | `web/src/consoles/dashboard/ChartWidget.tsx`, `chartTypes.ts`, Recharts | Extract reusable presentation parts for an MCP Apps component |
| Identity and tenant routing | Gateway actor resolution, `internal/identity`, `pkg/auth`, tenant database routing | Add MCP OAuth discovery and token validation without replacing engine authorization |

Specific constraints found during inspection:

- Operational read routes are not interchangeable: published workflow summaries
  omit full step definitions, personal history is participant-based, administrative
  history is separately scoped, and the inbox uses task-assignee rules. Reuse
  these distinctions rather than choosing the broadest available endpoint.
- Workflow history, automation executions and notifications currently include
  fixed latest-50 queries. These are not complete datasets for monthly totals,
  failure rates or completion-time reports. Add server filtering, pagination and
  full permitted aggregation before describing them as reporting sources.
- The current form list handler calls `ListRecords(..., 100)` and filters those
  records afterward. It is not a complete reporting dataset. Never compute a
  purported full total from that response.
- The current chart HTTP endpoint is
  `POST /api/dashboard-widgets/{id}/chart-data`. It loads a widget and verifies
  dashboard access. Reuse it or its shared service for existing chart widgets;
  standalone grid/form reporting still needs an independent entry point.
- Dashboard detail returns widget configuration, not one fully evaluated data
  response. The UI separately resolves grid, form, KPI and chart values and
  maintains synchronized selector state. The dashboard read service must assemble
  these results with equivalent context semantics.
- Dashboard list/chart handlers have an explicit administrative role bypass of
  the dashboard-assignment filter; the detail handler currently applies the
  assignment check without that bypass. Verify and resolve this policy mismatch
  before assuming all listed dashboards can be read through one adapter. Do not
  route around a denial through developer endpoints.
- The current chart resolver handles grids. Form charts require a generic form
  aggregation path. The grid-only restriction in
  GRID_CHART_WIDGET_MVP_INSTRUCTIONS.md
  remains the contract for existing dashboard widgets; this brief adds a separate
  conversational reporting capability.
- The chart resolver may substitute a visible default for a hidden context
  member. Reporting must validate explicit filters before resolution and refuse
  an unavailable requested scope, rather than silently report a different one.
- `GET /api/grid/export` describes an input-value round-trip export. The newer
  `file_export` integration supports calculated values but depends on a configured
  integration. Neither is the general query interface for this feature.

## 4. Architecture

```text
ChatGPT or Claude
    | OAuth-authenticated MCP tools
    v
Gateway /mcp
    | resolved user + tenant + application + model + revision
    v
Shared reporting read services
    | existing access rules + calculation/form/workflow/trigger semantics
    v
Tenant database and calculation results

Tool results --> conversational analysis and tables
Render tools --> MCP Apps charts and reports inside the same conversation
```

Prefer hosting `/mcp` in the Go gateway so authentication, tenant routing and
read services stay in the exercised runtime. Separate protocol handling from
reporting logic; suggested packages are `internal/mcp` and `internal/reporting`.
These are proposed locations, not existing packages.

Use a maintained MCP SDK, verify its current Streamable HTTP and authorization
support, and pin the selected dependency. Support protocol initialization,
capability negotiation, tool listing/calling and UI resource retrieval.
Treat transport sessions as protocol state, never as authentication.

Extract shared read operations from handlers where necessary. HTTP and MCP must
use the same business semantics and access checks. The reporting service can
query the database through normal repository code after resolving the actor and
tenant; the MCP adapter must not execute arbitrary SQL or create a second,
privileged data path.

Add documented, narrowly scoped HTTP read/query routes where the missing generic
capabilities belong in the engine. Update `api/openapi.yaml` and run `make oas`
for REST changes. MCP's JSON-RPC contract is described separately; do not pretend
that the existing OpenAPI schema implements MCP.

Do not route reporting through `/api/ai/sessions` or require the AI Developer's
provider key. The chat host already supplies the assistant. Reporting tools
retrieve and calculate data without a second LLM call.

## 5. Account connection and permissions

Use per-user OAuth authorization through the existing identity infrastructure.
Implement the MCP protected-resource metadata, authorization-server discovery,
authorization-code flow with PKCE, and the registration mechanism verified to
work with each target host. Allowlist the exact callback URLs supplied by the
host setup flow. Configure resource/audience and a narrow read scope, proposed
as `models:read`; do not assume the current Keycloak setup already supports the
complete MCP flow. See [OpenAI authentication](https://developers.openai.com/plugins/build/auth)
and the [MCP authorization specification](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization).

Validate issuer, signature, expiry, audience and granted scope on requests.
Resolve the user's current engine permissions and tenant membership. The OAuth
scope permits use of the connector; it does not grant access to any model.

### Effective access is inherited from maverickbuilds.app

The engine's current authorization decision is the authority. OAuth consent and
any connector-specific allowlist can narrow that decision, never widen it:

```text
Permitted read = engine authorizes this user/resource/operation/fields
                 AND required feature entitlement is present
                 AND OAuth scope permits the operation
                 AND any configured connector restriction permits it
```

Resolve policy through the same services/predicates as the application. Do not
copy selected role names into an independent connector ACL or use a simple
“administrator beats restrictions” rule. Apply existing grant, restriction,
inheritance and scoped-bypass precedence exactly; an empty grant list must retain
the engine's documented meaning, not an invented connector default.

| Configuration authority | Restrictions the connection must honor |
|---|---|
| Business administrator | Business-role membership, dashboard assignments, user/member/metric access rules, button restrictions where applicable, and administrative reach within the appropriate workspace |
| Developer | Developer-configured resource/revision relationships, workflow step roles and context rules, dashboard source/filter configuration, and any access settings the developer is authorized to manage |
| Tenant administrator | Current tenant/account status, scoped platform roles, workspace/application/model grants or restrictions, and applicable tenant feature/access settings |

This table identifies policy inputs, not new grants for those roles. Determine
the actual settings and reach from the existing implementation. A person who
created a dashboard, workflow or connector does not lend their privileges to
people using it. A tenant administrator may make the connection available, but
every user must connect under their own identity. Never share that administrator's
token between users or combine one tenant's role with another tenant's data.

Reading a workflow, seeing a trigger button and being eligible to execute or
approve it are separate capabilities. This connector exposes only authorized
reads even when the user's application role also permits mutations.

Check current permissions at connection/discovery time and on every tool call,
pagination request, widget refresh and result render. Recheck before releasing
the result of a long-running query. A policy change that removes access must
invalidate or make inaccessible old cursors/results and prevent later reads;
do not wait for the user to reconnect or rely solely on roles in a long-lived
token. When policy lookup fails, fail closed instead of serving a cached allowed
response. Information already returned to a host cannot be recalled.

Expose a minimal `get_connection_access` tool describing the current caller's
permitted resource families/operations and selected scope. Show a safe reason
when a requested operation is unavailable. It must not enumerate other users'
permissions, hidden objects or protected policy configuration. Tool discovery or
UI visibility is only a convenience; guessed tool names and direct calls still
need the same server authorization.

For an in-process adapter, invoke reporting with the verified actor context.
If a separate downstream resource is introduced, use an explicitly supported
delegation/token-exchange design with the correct audience; do not blindly
forward the MCP bearer token to a different resource server.

Apply these rules to discovery, schemas, records, totals, comparisons, rendered
results and artifact retrieval alike:

1. Resolve tenant and local user identity from trusted authentication and engine
   membership. Treat requested IDs as selections to authorize, not credentials.
2. Enforce application/model reach, revision scope, dashboard visibility and
   assignments, metric restrictions, member restrictions including hidden
   ancestors, form-record visibility, workflow participation/task eligibility,
   trigger/integration scope and notification ownership.
3. Preserve permitted read access to read-only cells; read-only does not mean hidden.
4. Filter before aggregation, counts, rankings and pagination. Preserve the
   engine's rule for withholding calculations that depend on inaccessible data.
5. Use the existing non-disclosing response for an inaccessible or unknown object.
   Do not return hidden names, counts or schema details through error messages.
6. Reauthorize every follow-up query, chart interaction and result retrieval.
   A cached result ID, cursor or MCP session ID is never an access grant.
7. Never use an administrator service account, stored browser cookie or
   development-persona header to serve production users.

Dashboard permissions govern dashboard discovery, definitions and widget-derived
results. Reading a dashboard requires both access to that dashboard and access
to each queried source/value. Standalone reports use the existing underlying
data-read policy. If inspection shows any source is
intended to be accessible only through a dashboard grant, resolve that policy
explicitly; do not infer wider access from the fact that the UI hides the source.

The one-time account flow explains that requested results are returned to the
selected chat service. Provide disconnect/revocation behavior and verify it with
the identity provider. Server revocation prevents subsequent reads; it cannot
remove information already delivered into a chat history.

## 6. MCP tool contract

Use this initial tool set. Names are proposed; keep names, descriptions,
schemas and behavior consistent. Each data tool returns structured results and
a concise text explanation. It must work when the host cannot render custom UI.

| Tool | Inputs | Result |
|---|---|---|
| `get_connection_access` | Optional selected application/model context | Minimal effective read capabilities for this caller; no credential or hidden-resource details |
| `list_models` | Optional name search and cursor | Authorized tenants/applications/models with stable IDs and default/active context |
| `list_sources` | Application, model, optional revision, source kind, search, cursor | Readable grids and forms with IDs, names and supported query operations |
| `describe_source` | Explicit context, source kind and source ID | Grid metrics/dimensions or form fields, types, formats, relationships and query capabilities |
| `list_members` | Context, source, dimension, search/parent, cursor | Permitted member IDs/codes/labels in hierarchy or calendar order |
| `query_grid` | Context, grid ID, metric IDs, member filters, group-by, order and limit/cursor | Engine-resolved input and calculated values, aggregates and provenance |
| `query_form` | Context, form ID, field projection, typed filters, optional group-by/aggregates, order and limit/cursor | Visible records or aggregates over the entire matching visible set |
| `list_dashboards` | Application/model context, optional folder/search, cursor | Permitted dashboard IDs, names, tags, folder path and resolved revision |
| `get_dashboard` | Context, dashboard ID, optional widget cursor | Authorized dashboard definition, widget order/layout, readable content, saved settings, source references and supported read capabilities |
| `query_dashboard_widgets` | Context, dashboard ID, bounded widget ID selection/cursor, explicit shared and per-widget filter overrides | Per-widget result IDs, effective filters, live permitted data, freshness and completion status |
| `list_workflows` | Context, permitted definition view, status/search, cursor | Authorized published summaries or builder-visible definitions; the requested view never grants a role |
| `get_workflow` | Context, workflow ID | Only the definition fields permitted to this caller; identify summary versus full detail |
| `query_workflow_activity` | Context, kind (`instances`, `tasks`, `history`), permitted IDs/status/date filters, projection/grouping, cursor | Authorized instance progress, task/approval status, history or full-population aggregates |
| `list_triggers` | Context, supported trigger type/status/source filter, cursor | Permitted trigger rules and schedule metadata |
| `get_trigger` | Context, trigger ID | Safe permitted rule configuration and authorized source/workflow references |
| `query_trigger_executions` | Context, trigger/status/date filters, grouping, cursor | Permitted execution outcomes or aggregate counts/rates |
| `list_related_resources` | Context, supported resource-kind enum, search, cursor | Authorized integration/revision/metadata references from the section 7 catalog |
| `read_related_resource` | Context, supported kind and ID | A safe projection from the registered read adapter; no arbitrary route or SQL access |
| `query_related_activity` | Context, kind (`integration_runs`, `notifications`, `cell_history`, `audit_events`), typed filters, grouping, cursor | Permitted historical or operational records/aggregates with the relevant role and feature checks |
| `compare_results` | Two compatible query result IDs, join keys and comparison options | Matched rows, absolute differences and explicitly defined percentage differences |
| `render_chart` | Authorized result ID, chart kind, axis/series field IDs and display options | Chart UI resource plus underlying accessible table and concise description |
| `render_report` | Title and ordered sections referring to authorized result IDs, chart specs and optional commentary | Report UI resource plus readable text/tables and per-section provenance, including originating dashboard/widget when applicable |

Data tools must have accurate `readOnlyHint` and `destructiveHint` annotations.
Classify render tools according to their actual effects, including any temporary
artifact storage; annotations describe behavior and do not replace access checks.
Expose no write, publish, integration-run or arbitrary-code tool in this server.

For model-scoped tools, validate `application_id`, `model_id`, `revision_id` and
every source/metric/dimension/member/field/dashboard/widget ID as one coherent context. Discovery may
omit revision and resolve the active one; return the resolved revision ID, then
pin it for the query/report. A revision name is descriptive, not an identifier.
Do not silently move an existing report to another revision after activation.

Workflow, trigger and related-resource tools must also validate definition,
instance, task, rule, integration, run and referenced subject IDs against their
actual owning scope. Application-owned resources can omit model/revision when
those concepts do not apply; do not invent a model to satisfy the schema. Personal
inbox reads can span permitted applications only when explicitly requested, and
must label their scope instead of inheriting an unrelated browser selection.
Implement the related-resource tools through a fixed adapter registry with typed
schemas, allowed fields, authorization and limits for every kind. Unknown kinds
are unsupported. Do not accept caller-supplied URL, table, SQL or role selectors.

Discover before querying. When two permitted sources have the same name, return
enough application/model context for clarification. Ask the user only when the
choice materially changes the answer; do not guess IDs or reveal hidden matches.

## 7. Query semantics and completeness

### Grids

Use existing calculation, rollup, time-summary and scoped-read behavior. Respect
each metric's native dimensions, formula rules, rollup source and units. A mean,
ratio, percentage or end-of-period balance must not be treated as an additive sum.
Avoid counting parent totals together with their children.

Support metric selection, permitted dimension-member filters, time ranges using
the model's actual time dimension, grouping, sort and bounded results. Advertise
supported combinations; return a useful unsupported-operation error for anything
the engine cannot correctly resolve. Never approximate an unsupported query by
silently dropping a filter.

Retain separate states for a real zero, missing/null value, calculation error and
a value withheld under engine policy. Only describe withheld information at the
level existing permissions permit. Chart code must not convert these states to zero.

### Forms

Discover field types from the selected form. Validate field IDs and operators;
use typed parameters and parameterized repository queries. Support equality,
membership, numeric/date ranges, status, text search where appropriate, projection,
ordering, cursor pagination, grouping, count, sum, mean, minimum and maximum.
Only offer operations compatible with a field's declared type.

Reuse the semantics of `resolveFormRecordScope`, `filterFormRecords` and their
supporting access logic. Move or share those predicates so queries and aggregate
counts operate on the complete visible population, not the first 100 records.
Raw SQL/expressions supplied by the assistant are not an input format.

Use stable ordering with an ID tie-breaker. Bind cursors to the user, context,
query and result snapshot; reject tampering and expired or mismatched cursors.
Define null handling, timezone, date bucket boundaries and denominator rules.
For referenced dimension fields, resolve labels only within permitted scope.

### Developer-created dashboards

Treat a dashboard as a saved composition of data views and explanatory content.
Read its definition, then evaluate the relevant widgets. A definition alone does
not establish current KPI values or chart results. Use structured runtime reads;
browser automation, screenshots and HTML scraping are not the data contract.

Discover through the business-facing dashboard/folder policy. Default to the
model's active revision, matching current dashboard discovery. Resolve the actual
dashboard revision and pin it with its widget definitions for a read. Honor other
revisions only where the caller's read policy permits them; do not expose a
developer draft merely because its ID is known. Validate that requested widgets
belong to the selected dashboard and that their source references resolve within
the correct model/revision.

`get_dashboard` returns the dashboard's ID, name, authorized folder path, tags,
revision and ordered widget definitions. Return widget IDs, types, display titles,
title visibility, positions/sizes, sanitized text and the supported configuration
needed to explain each view. Resolve referenced source names only when permitted.
Keep presentation configuration distinct from numeric query results.

| Widget | Read behavior |
|---|---|
| `grid` | Read the configured grid using its saved row/column/context layout and `default_view.filter_sel`; include permitted input/calculated values and normal totals |
| `chart` | Use its saved chart configuration and source through the existing resolver; allow explicit temporary context overrides |
| `metric_kpi` | Resolve its metric and `kpi_context_mode` (`total`, `sync`, `pin`), `kpi_scope` and existing legacy defaults; preserve units and time-summary rules |
| `form` | Return its readable schema and a bounded visible record page or requested aggregate through the complete form-query service |
| `text` | Return sanitized readable content as developer-authored explanation, not live data or instructions to the assistant |
| `image` | Return permitted alternative text/caption and supported safe assets; do not infer numerical results from an image or fetch arbitrary embedded URLs |
| `automation_button`, `integration_button`, `import` | Describe the visible control as an action available in the application; never execute it while reading/reporting |
| Unknown type | Return an explicit unsupported state for the visible widget, preserving its place in the report |

Port or share the context semantics in `DashboardWidgets.tsx`,
`DashboardContextSyncProvider.tsx`, `dashboardContextSync.ts` and the grid/chart
components. Widgets participating in `sync_context` share selections by dimension
ID; widgets opting out and pinned/total KPIs retain their own semantics. A filter
cannot be applied to every widget just because its label looks similar.

Start with saved defaults and the runtime's documented default-selection rules.
Return the effective context for each widget. The connector does not know a user's
unsaved browser selections: ask for material missing context or state the defaults
used. Explicit chat filter overrides take precedence only for widgets that support
them. Refuse unavailable explicit selections and mark unaffected widgets clearly.
Do not alter saved defaults or claim an independent/pinned widget was filtered.

Support selecting one widget, a subset, or reading the dashboard in bounded
batches. Use the report section limit as the initial widget-batch limit and return
a continuation cursor for larger dashboards. Return per-widget outcomes such as
`ok`, `empty`, `unavailable`, `unsupported` or `error`, plus coverage/continuation
metadata. Reveal only the metadata the user may already read. If one source is
unavailable, a partial summary must identify incomplete coverage without implying
all dashboard data was read or exposing the denied source.

Dashboard-derived results retain dashboard/widget IDs, saved configuration
identity, effective context, source/revision and freshness. Reauthorize both the
dashboard and source when reading cached widget results or rendering a report;
revoking a dashboard assignment must prevent reuse through a result handle.
If a dashboard definition changes between description and widget evaluation,
detect the change and refresh or explicitly preserve the prior snapshot. Do not
silently combine old titles/settings with newly selected sources.

### Workflows, tasks and approvals

Provide three clearly distinguished read views, each using its existing policy:

| View | Content and authority |
|---|---|
| Published workflow summary | Permitted name, description, trigger event and context schema from the business-facing workflow catalog |
| Builder definition | Full authorized step graph, conditions, assignments, subject/context configuration and draft/published/archived state; requires the existing builder read capability |
| Runtime activity | Instances, eligible tasks, approval progress, decisions, permitted comments, due dates and timestamps under personal-participation or scoped administrative-history rules |

Permission to read the published catalog does not expose a draft or full builder
definition. A task assignment permits the corresponding task read; it does not
automatically grant every step, subject record or instance in that application.
Do not merge personal history, task inbox and administrative history into one
unrestricted dataset. Determine which view and fields the caller may read before
querying, then apply any additional application/model/date selection.

Use the engine's shared assignment logic, including
`internal/workflow/assignee`, for task eligibility. Match roles in the correct
tenant/workspace/application. A business role named `tenant_admin` is not the
platform role, and holding an administrative role in one tenant grants no such
role in another. Apply disabled-account and current role-membership checks.

Describe historical/running instances from their captured step/context-schema
snapshots when present, with the engine's documented legacy fallback. A later
edit to the workflow definition must not rewrite a report's explanation of an
older instance. These definition snapshots do not freeze a user's permissions;
current read restrictions still apply.

Return permitted statuses, requester/assignee labels, current steps, recorded
decisions and comments, timing, due dates, and links/IDs to subjects only as
authorized. Redact hidden context members and restricted subject fields in
structured data, display labels and narrative inputs alike. Any drill-through
to a form record, grid or related workflow uses that resource's own read policy.

Support reports such as pending tasks by workflow, overdue approvals, counts by
instance state and completion-time distributions. Define the population, timezone
and selected timestamp (`started_at`, `completed_at`, or task due time). Count
instances distinctly instead of multiplying them by joined steps; label task-level
and instance-level measures separately. Compute completion duration only from
appropriate recorded timestamps, and overdue status only where a due date and
applicable pending state exist. Distinguish test runs from production instances;
exclude test runs from business summaries unless explicitly selected and allowed.

Every read is passive. Do not start, advance, approve, reject, reassign, cancel,
publish or test-run a workflow, or mark a task complete. A request to perform
such an action must explain that this connection provides reads only.

### Triggers and execution history

Read the trigger types actually configured in the engine: manual, scheduled and
supported event-driven rules. Return authorized rule IDs, names, enabled/state
information, event type, source and target workflow references, and schedule
configuration when applicable. Validate source/target visibility separately;
readable rule metadata does not grant access to a referenced form or integration.

For schedules, preserve timezone, cron/interval semantics where supported and
recorded next/last-fire timestamps. Distinguish a configured schedule, a computed
next occurrence and an observed execution. Do not imply a trigger ran just because
its schedule says it should have. Resolve name-based workflow bindings using the
engine's actual rules; do not invent a workflow ID from a matching display label.

Execution queries expose permitted timestamps, status, attempts, durations,
sanitized recorded error summaries and related run/instance references. Aggregate
over all permitted matching executions before presenting counts, rates or charts.
Define whether retries count as separate attempts or one logical execution and
state the denominator; do not mix a per-attempt failure rate with a per-run count.
Reading metadata or history must never fire a rule, enable/disable it, recalculate
its schedule, retry an execution, or cause an import/integration to run.

### Other supported resources

The initial related-resource adapter registry is explicitly bounded below.
“Read access” means the intersection of the current engine policy, connector
scope and feature entitlement described in section 5, not simply authentication.

| Resource family | Read/report capability | Boundary |
|---|---|---|
| Metrics and dimensions | Definitions, units, hierarchies, member properties and permitted declared dependencies | Filter hidden metadata and validate every referenced metric/member; no inference of a protected formula dependency |
| Revisions | Permitted revision metadata, active context and comparable snapshots | Honor current revision read policy; never expose a builder draft because the caller may read the active revision |
| Integrations | Safe name/type/status/target metadata and permitted configuration summaries | Explicit field allowlist; no credentials, OAuth tokens, secret-bearing headers, connection strings or arbitrary raw config |
| Integration runs | Permitted run outcomes, timing, row counts and sanitized error summaries | Preserve the existing run-history role guard and source restrictions; do not grant developer history to all business users |
| Notifications | The caller's own permitted notifications, delivery/read status and safe related-resource references | Reading never marks them read; opening a referenced object requires that object's read policy |
| Cell history | Permitted changes to a selected cell, timestamps and allowed actor/value fields | Same source/member/metric access and feature entitlement as the existing cell-history capability |
| Audit events | Authorized scoped event summaries, filters and aggregate activity | Existing audit role, scope, retention and edition/licence gates; no general audit feed for ordinary users |

Use approved projections even if an existing endpoint returns broader JSON.
Permission to inspect an integration does not justify putting credentials or raw
request/response bodies in a chat result. Sanitize error text as well as structured
fields. Do not expose identity-provider settings, users' private profiles, full ACL
tables, billing data or infrastructure through an unbounded “other” tool.

Additional resource families require a registered read contract with ownership,
field visibility, relationship checks, pagination, completeness, safe projection
and acceptance tests. Reuse an existing policy where available. If no suitable
read policy exists, report that gap and obtain the required product/access decision
under repository rules; do not fall back to an administrator endpoint or direct
database access as a substitute for authorization.

### Comparisons and reports across sources

Support comparisons of compatible results: the same measure, compatible units,
matching grouping keys and explicitly selected periods/scenarios. Define absolute
variance as current minus baseline; define percent variance as that difference
divided by the absolute baseline, times 100. Return null plus a reason when the
baseline is zero or unavailable. Do not confuse percent with percentage points.
Never join results by label alone or treat unmatched rows as zero implicitly.

A report can display several independently sourced sections. Joining grid data
to form records requires a declared relationship and compatible grain; absent
that, keep separate sections and explain the limitation. Do not invent joins,
currency conversion, missing budget values or causality.

Operational charts use the same authorized tabular result contract as grid/form
charts. Compare only populations with matching grain, status definitions, date
windows and units. Cross-resource reports must not reveal a hidden subject through
a workflow comment, trigger label, run error, aggregate or notification reference.

### Result envelope and limits

Each query returns a typed table/series and a provenance envelope containing:

- Result ID and expiry; application/model/revision/source IDs and readable labels.
- Originating dashboard/widget IDs and saved configuration identity for results
  read through a dashboard, alongside any temporary filter overrides.
- Resource family and relevant workflow definition/instance/task/trigger/run IDs
  for operational results; identify the definition snapshot or live metadata used.
- Requested and effective filters, grouping, measure definitions and aggregation.
- Column types, units, currencies, numeric precision and time/calendar semantics.
- Query timestamp and actual data/calculation freshness when known. A request
  timestamp must not be presented as the last data update.
- Returned row count, next cursor, completeness/truncation status and whether
  totals cover all matching permitted data. A total count is optional and must
  itself be authorized.
- Warnings for stale calculations, unsupported freshness guarantees or unavailable
  values, without disclosing restricted data.

Set configurable server limits. Proposed initial defaults are 200 rows per page,
1,000 rows maximum per page, 200 plotted categories, 5 series, 10 report sections,
a 30-second query deadline and a 15-minute result retention period. These are
starting engineering defaults, not claims about either host's payload limits.
Bound serialized bytes and total query work as well as row counts; tune against
representative data and actual host limits.

Aggregate full permitted data before applying a top-N presentation limit.
Mark omitted categories and partial results explicitly. A timeout or failed
source must not produce a report claiming full coverage. Honor cancellation.

A single query should use a consistent read snapshot where practical. A model
revision does not freeze its business data. State the consistency guarantee;
for multi-source reports show per-section read times and do not claim an atomic
snapshot unless the implementation actually provides one.

Temporary results must be scoped to the authenticated principal, tenant, exact
query and revision. Recheck access at render/read time and invalidate or recompute
when permission changes make the stored result inappropriate. Display when a
result was generated and let the user explicitly refresh it.

## 8. Reports and charts inside the conversation

Use the shared MCP Apps standard for the initial interface. ChatGPT documents
standard UI resources and bridge methods; Claude documents interactive connectors
within its conversations. These provide a common foundation, with actual host
behavior to be verified during acceptance testing.
[OpenAI UI documentation](https://developers.openai.com/plugins/build/chatgpt-ui),
[Claude interactive connectors](https://support.claude.com/en/articles/13454812-use-interactive-connectors-in-claude).

Separate query tools from presentation tools. Attach `_meta.ui.resourceUri` only
to tools that render UI. Use the standard MCP Apps bridge and capability detection;
keep any host-specific adapter small. See the
[MCP Apps overview](https://modelcontextprotocol.io/extensions/apps/overview).

The report component contains a title, optional narrative, KPI/table/chart
sections, applied filters, units, revision and source/freshness labels. Provide
loading, empty, expired-result, access-denied and query-error states. It must be
usable in a narrow inline card and an expanded in-chat view, with keyboard access
and readable light/dark themes.

For “read/show this dashboard,” preserve its titles, explanatory text, widget
sequence and meaningful grouping while adapting the layout to the chat width.
Show its KPIs, tables and charts in the conversation. Exact canvas pixel positions
are metadata, not a requirement to reproduce a wide editor canvas in a narrow chat.
For “summarize this dashboard,” select relevant sections and state coverage.
Provide source attribution back to the dashboard and widget; an optional link
to the original does not replace the in-chat result. Label any user-requested
chart transformation so it is distinguishable from the saved dashboard view.

Render numbers from authorized server results. `render_chart` takes result and
field references, not arbitrary assistant-supplied numeric arrays. Presentation
options must not redefine calculations. User filter changes call the same
authorized tools and update model-visible context so follow-up answers describe
the chart actually on screen.

Choose charts according to the data:

| Presentation | Use |
|---|---|
| Bar | Compare categories; provide an explicit top-N label when shortened |
| Line | Ordered time series using model calendar order |
| Pie | Small, non-negative part-to-whole datasets with a meaningful total |
| Scatter | Two numeric measures at the same observation grain |
| Histogram | Numeric observations with explicit bin boundaries and population |
| Table / KPI | Exact values, exceptions, mixed units or insufficient chart data |

Always provide a readable data table and concise description. Preserve nulls,
units and comparison definitions. Do not draw incompatible currencies on a
single unlabeled axis, or turn a partially returned dataset into an unlabeled
whole-population histogram.

The host writes narrative analysis from the tool results. Commentary supplied
to `render_report` is assistant-authored, not engine-verified fact; keep numeric
claims traceable to referenced result fields. Prefer structural explanations
such as “Region A accounts for this much of the difference” to unsupported
causal assertions.

Bundle chart assets with the component. Treat form text, model names and labels
as untrusted content: render as text, sanitize any supported markup, and do not
execute supplied HTML, JavaScript, URLs or instructions. UI resources and result
metadata must not contain tokens or integration credentials. Use a restrictive
resource CSP and avoid arbitrary third-party asset/network requests.

When custom UI is unavailable, retain text/tables and return a static chart image
through the host-supported MCP content mechanism. Verify that fallback in each
host. A raw JSON chart specification or an external dashboard URL alone does
not pass the chart requirement. Do not depend on an optional host code-execution
feature to make the baseline feature work.

## 9. Implementation sequence

1. **Confirm reach and contracts.** Trace data, dashboard and operational reads
   for business users, business administrators, developers and tenant administrators.
   Document effective restrictions, role combinations and resource capabilities.
   Identify any permission change requiring a separate decision.
2. **Build shared reporting reads.** Add authorized source/dashboard discovery,
   typed grid queries, complete form pagination/aggregation and dashboard-widget
   reads with saved/synchronized context. Add workflow/task, trigger/execution and
   related-resource adapters with their own read policies and complete aggregates.
   Reuse calculation, assignment and access services.
   Add generic REST contracts where needed and regenerate clients.
3. **Add MCP and OAuth.** Implement `/mcp`, metadata, discovery and data tools.
   Prove the complete per-user login/read/revoke flow with each client.
4. **Add comparison and presentation.** Implement authorized result references,
   deterministic comparisons, shared chart/report components and static fallback.
5. **Verify end to end.** Run the scenarios below on both a model with no dashboards
   and a model with developer-created dashboards, workflows and triggers, using
   the actual scoped roles and both chat clients. Exercise live permission changes.
6. **Document operation.** Provide connection instructions for ChatGPT and Claude,
   identity-provider configuration, deployment configuration, query limits,
   disconnect behavior, troubleshooting and the tested host capability matrix.

Intermediate milestones are useful; shipping only JSON tools does not complete
this brief. Do not publish to an app directory or deploy production changes as
an incidental part of implementation. Public distribution is a separate release
step; a configured custom connection is sufficient for acceptance.

Log the actor, tenant/model/revision/source IDs, tool, result status, duration
and bounded result size using the existing audit/observability conventions.
Avoid raw record bodies, entire prompts, access tokens and secrets in logs.
Set per-user/per-tenant rate and concurrency limits, apply query deadlines, and
verify cleanup of temporary results. Document the behavior when a host retries.

## 10. Verification and acceptance

Use meaningful integration tests against the gateway and a real test database.
Create business fixtures through supported APIs, following repository rules.
For protocol/authentication tests, use controlled OAuth/JWKS fixtures and real
request verification. Never claim a mocked provider proves host compatibility.

| Scenario | Required evidence |
|---|---|
| Ordinary business user, no dashboards | Discover permitted grids/forms, query them, and view a report and chart in both ChatGPT and Claude |
| Developer-created dashboard | Discover and read its definition, KPIs, chart, grid, form and explanatory text inside both hosts without entering the app's dashboard screen |
| Dashboard grants | List/detail/widget reads agree with the approved role policy; a forged dashboard/widget ID and a revoked dashboard assignment cannot expose results |
| Saved and synchronized filters | Grid defaults, chart context, `sync_context`, KPI total/sync/pin modes and legacy defaults match canonical runtime values for the same context |
| Temporary dashboard filters | Override only applicable widgets; show effective filters and leave saved definitions unchanged |
| Large/partial dashboard | Widget pagination covers every permitted widget; missing, unsupported or failed widgets are reported without falsely claiming complete coverage |
| Dashboard definition change | Results identify the configuration read; concurrent edits cannot mix stale titles with different sources silently |
| Action widgets | Automation, integration and import controls remain descriptive and cause no domain writes |
| Restrictions configured by each authority | A restriction set through the business-admin, developer or tenant-admin capability is enforced identically in the application and connector wherever that setting applies |
| Combined roles and scope | Business/admin/developer role combinations retain the engine's policy precedence; roles held in another workspace/tenant confer no unintended access |
| Shared connector setup | Users connecting to a tenant-installed connector receive their own permitted data, never the installer's credentials or privileges |
| Permission change during a connection | Remove a role, dashboard grant, member/metric permission, model grant or tenant access and verify subsequent reads, cursors and cached renders obey the new policy without reconnecting |
| Disabled user or policy lookup failure | Refuse data even with a previously valid token/session/cache; do not use the last successful allowed response |
| Workflow catalog versus full definition | An ordinary reader receives only permitted published summary fields; full/draft definitions require the actual builder read capability |
| Workflow activity | Requester, assignee, notification recipient, unrelated user and scoped business administrator receive exactly their permitted views; subject/context data stays independently protected |
| Definition edited after instance start | Historical steps and context use the captured definition, with current access checks |
| Trigger/schedule reads | Rule configuration, schedule and observed execution remain distinct; reads cause no execution or schedule mutation |
| More than 50 operational records | Workflow/trigger/notification queries paginate completely; aggregate totals and rates match the full permitted population |
| Workflow and trigger charts | Task versus instance grain, duration population, due-date logic, retry counting, timezone and test-run handling are explicit and correct |
| Integration configuration and errors | Safe projections and sanitized errors contain no credentials or hidden-source data, including for privileged readers |
| Notifications and audit/history | Only permitted recipient/scoped records are read; licence/role gates remain enforced and notification read state does not change |
| Another application/model/tenant | Forged IDs cannot reach data, metadata, cached results or counts |
| Hidden metric/member/ancestor | Excluded everywhere, including derived values, form references, filters, totals and rankings |
| Explicit inaccessible filter | Rejected without silently selecting a visible default |
| Read-only data | Query succeeds without granting write capability |
| More than 100 form records | Pagination reaches every permitted row once; full aggregates match the full visible dataset |
| Hidden recent form records | Visible older records remain reachable; counts/aggregates agree with visibility policy |
| Calculations | Inputs, formulas, ratios, means, time summaries and hierarchy totals match canonical engine results |
| Rollup grid and multi-dimensional slice | Native dimensions and inherited metric lists resolve correctly without double counting |
| Null/error/withheld data | Distinct from zero in tables, comparisons and charts |
| Revision change | Existing results retain their declared context; foreign revision IDs are rejected |
| Comparison | Zero/negative baselines, missing keys and percentage-point measures handled explicitly |
| Expired/revoked access | New requests and cached-result access fail or require reauthorization as designed |
| Large query/timeout | Bounded work, cancellation and honest completeness status |
| Malicious record text | Displayed as content; no script execution, credential exposure or new tool authority |
| In-chat interaction | Changing filters refreshes authorized data and updates conversational context |
| UI unavailable | Text/table and static chart fallback are demonstrated |
| Read-only contract | No domain writes, new dashboards, integration runs or model changes occur |

Add tests near the shared services and existing gateway authorization tests;
cover MCP schemas, cursor/result ownership and UI rendering separately. Exercise
new query paths with relevant existing tests in `internal/query`,
`internal/calculation`, `internal/rollup`, `internal/readset`, `internal/workflow`,
`internal/workflow/assignee`, `internal/identity` and
`internal/gateway`. Run the repository's required generation, Go, frontend build
and lint checks for the files changed. A documentation-only change does not
require running the application test suite.

Record manual verification for each supported ChatGPT/Claude surface: account
and workspace conditions, connection/authentication, discovery, grid/form and
dashboard reports, workflow/trigger activity reports, related resources, charts,
follow-up filters, live access changes, fallback behavior and disconnect. Availability
can depend on host account/workspace policy; state any untested surface explicitly.
Automated tests alone do not establish that either host renders the integration.

The feature is complete when an authorized business user can connect from
either chat client, read permitted model data, dashboards, workflows, triggers and
related resources, receive accurate reports/charts and refine them conversationally
without entering the application screens. Every read must honor the restrictions
configured by business administrators, developers and tenant administrators, under
the signed-in user's current effective permissions. Models without dashboards
remain fully supported.

## 11. Deliverables and supporting references

Deliver the reporting services and resource adapters, effective-access contract
and parity tests, MCP/OAuth integration, in-chat report
and chart components, tests, configuration examples, connection guide and actual
host verification evidence. Update [docs/API.md](docs/API.md),
[docs/README.md](docs/README.md), and implementation status after the work ships.
Record unresolved findings in [docs/OBSERVATIONS.md](docs/OBSERVATIONS.md), using
the private observations file for exploitable issues as required by repository rules.

Repository references:

- [Gateway and role contracts](docs/API.md)
- [Gateway implementation](internal/gateway/handler.go)
- [Grid chart resolver](internal/query/chart.go)
- [Dashboard widget rendering and KPI context](web/src/consoles/business/DashboardWidgets.tsx)
- [Dashboard selector synchronization](web/src/consoles/DashboardContextSyncProvider.tsx)
- [Form record access](internal/gateway/form_record_access.go)
- [Form storage](internal/crudapp/store.go)
- [Workflow definitions, instances and triggers](internal/workflow/store.go)
- [Shared task-assignment policy](internal/workflow/assignee/assignee.go)
- [Existing workflow/inbox scope observations](docs/OBSERVATIONS.md#workflow-history-and-the-workflow-inbox-are-scoped-differently)

External setup references, checked 2026-10-02; verify again when implementing:

- [Connect an MCP server to ChatGPT](https://developers.openai.com/plugins/deploy/connect-chatgpt)
- [OpenAI authentication](https://developers.openai.com/plugins/build/auth)
- [OpenAI in-chat UI](https://developers.openai.com/plugins/build/chatgpt-ui)
- [Claude custom remote connectors](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp)
- [Claude interactive connectors](https://support.claude.com/en/articles/13454812-use-interactive-connectors-in-claude)
- [MCP authorization](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization)
- [MCP Apps](https://modelcontextprotocol.io/extensions/apps/overview)
