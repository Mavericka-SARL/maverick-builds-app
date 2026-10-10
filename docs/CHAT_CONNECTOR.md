# Chat connector (ChatGPT, Claude)

> **Classification:** Current — the connector for **grid data only**: charts
> and reports are made in ChatGPT or Claude from grids, never from dashboards,
> and are not saved in maverickbuilds.app; the one change it makes is entering
> values into the input cells the person may edit.

> **Last verified:** 2026-10-10 — `go test ./internal/gateway -run 'TestMCPConnector|TestConnectorTokens|TestDelegatedReadGate|TestChatWritesCells|TestCellsBatchRoute|TestChatPluginReviewCases'`
> (cell writes, 2026-10-10); 2026-10-02 for the rest (shared and dedicated
> tenant databases);
> `MAVERICKS_KEYCLOAK_IT=1 go test ./internal/gateway -run TestConnectorClientsAgainstKeycloak`
> (Keycloak 24, both hosts' sign-in flows); the in-chat view in Chromium with a
> simulated host. **Live on the hosted service** (app.maverickbuilds.app): the
> metadata, the 401 challenge and both hosts' sign-in pages checked from the
> internet. **Not yet verified inside ChatGPT or Claude themselves.**

A person connects ChatGPT or Claude to maverickbuilds.app once. The assistant
can then read the grids of the models they open, as them, and make tables,
charts, comparisons and reports in the conversation — and enter values into
the input cells they may edit, when they ask it to. On the hosted service it
is available to every workspace, the free Community plan included, and to
every person in it: the account menu (top right) → **Connect ChatGPT or
Claude** shows the connector URL and each host's client ID and secret.

## Tools

| Tool | What it does |
|---|---|
| `get_connection_access` | your roles and what decides what you read (no name or address) |
| `list_models` | models you can open, across your workspaces |
| `list_sources` | a model's grids you can read |
| `describe_source` | a grid's metrics (input or calculated, aggregation, time summary, format) and dimensions |
| `list_members` | members of a grid dimension you can see, for filters and breakdowns |
| `query_grid` | engine totals for a filtered slice, or values per member of one dimension |
| `compare_grid` | two readings member by member (actual vs budget, one period vs another): difference and percent change |
| `render_chart` | a bar, line, pie, scatter or histogram chart of a grid query |
| `render_report` | up to 10 sections — KPIs, tables, charts, comparisons — each a grid query |
| `write_cells` | enters up to 500 input cells of the active revision as you — numbers, texts, dates, pick-list choices, or clears — all or nothing; `dry_run` checks without writing |

Charts and reports take **grid queries, never numbers**: each call reads the
grids as the person, now, so a chart can only show values the engine resolved
for them. Nothing is stored — no dashboard, widget or saved result. A host with
MCP Apps (Claude, ChatGPT) shows an interactive view (switch chart type,
refresh); every host also receives a text summary with the data table and a
PNG of each chart.

Not read through the connector: forms, dashboards, workflows, triggers,
notifications, integrations, audit. Nothing but input cells can be changed:
not the model, its members, forms, workflows or anyone's access.

### Entering values

`write_cells` writes through `POST /api/cells/batch`, which checks every cell
as the console checks a typed one before writing any: an input metric (never a
calculated one or a total), a leaf member of each of its dimensions, the
person's access rules (read-only and hidden members and metrics), workflow
locks, a system-managed revision, and the workspace's plan limits. One refused
cell refuses the call, listing each refused cell by its index, and nothing is
written; otherwise every cell is written in one transaction, the grid is
recalculated once, and `grid_change` automation rules fire as for a typed
cell. Each value is entered as the person (the cell history shows them), and
the audit log records one `cells.written` event per call, with the host's
client as `via`.

The tool is annotated as a write (`readOnlyHint: false`,
`destructiveHint: true`: it overwrites what a cell held), so ChatGPT and Claude
ask the person before a call unless they chose to allow it always; the
server's instructions also tell the assistant to confirm the values first.

## Security model

- **Separate tokens.** `/mcp` accepts a token only when it was issued to one
  of the registered host clients (`MCP_CLIENTS`, default `chatgpt-connector`,
  `claude-connector`), names the connector as its audience, and carries
  `models:read`. The REST API accepts only console tokens
  (`GATEWAY_TOKEN_CLIENTS`, default `mavericks-web`), so a host's token opens
  nothing but the connector.
- **Writing is a separate grant.** `write_cells` writes only for a token that
  also carries `grids:write`, which the person grants on the consent screen. A
  connection made before that scope existed holds `models:read` only and
  writes nothing until the person disconnects and connects again.
- **The tenant decides.** A tenant administrator (or the platform
  administrator, for a tenant they pick) turns chat writes off under
  **Tenant admin › Chat connector** (`PUT /api/admin/connector-settings`); on
  by default. Reads through a connection, and writes in the console, are not
  affected.
- **Every read is the person's own.** A tool runs the console's own grid
  routes in-process, through the same middleware and handlers, as the token's
  subject: application and model grants, hidden and read-only members and
  metrics apply as in the console, decided again on every call. A disabled
  account or a removed grant or rule applies to the next call.
- **Grid routes only.** The gateway refuses a connector's request to anything
  but `/api/me`, `/api/apps`, `/api/demo`, `/api/grid`, `/api/grids` and
  `/api/grid/series` before a handler runs — and `POST /api/cells/batch` for
  `write_cells` alone: a model link's reads, which run through the same
  delegated path, cannot reach it.
- **Active revision only.** Reads and writes use each model's active
  revision, as every user does (the REST API enforces the same: other
  revisions are their builders').
- **The context is confirmed.** The application and model a tool names are
  checked against what the person may open. A filter on a member they cannot
  see is refused exactly like one that does not exist, never swapped for
  another member. Refusals disclose nothing; formulas and raw access rules are
  never returned.
- **Bounded.** Two requests per second per person (burst 60), 30 seconds per
  read or write, 500 members per breakdown, 500 cells per write, 5 series and
  200 categories per chart, 10 sections per report. Each call is logged with
  person, tool, duration and outcome — never arguments or results.

Anything returned to a chat stays in that chat's history; revoking access
stops later reads only.

## Setting it up

### 1. The gateway

| Variable | Meaning |
|---|---|
| `MCP_ENABLED` | `true` serves `/mcp` and its protected-resource metadata |
| `MCP_RESOURCE_URL` | the connector's public URL, default `CONSOLE_URL` + `/mcp` — what people paste into their chat app |
| `MCP_CLIENTS` | the registered host clients, default `chatgpt-connector,claude-connector` |
| `MCP_CLIENT_SECRETS` | those clients' secrets as `client-id=secret` pairs, comma-separated, from the optional secret `mavericks-connector`; shown to every signed-in person in the account menu (`GET /api/connector`) |
| `MCP_AUTHORIZATION_SERVER` | issuer named in the metadata, default this deployment's Keycloak realm |
| `MCP_SCOPE` | required scope, default `models:read` |
| `MCP_WRITE_SCOPE` | scope a token also needs for `write_cells`, default `grids:write` |
| `GATEWAY_TOKEN_CLIENTS` | clients whose tokens the REST API accepts, default `mavericks-web` |
| `MCP_OPENAI_APPS_CHALLENGE` | the ChatGPT plugin submission's domain-verification token, served at `/.well-known/openai-apps-challenge` when listing the connector in ChatGPT's app directory |

The ingress must route `/mcp`, `/.well-known/oauth-protected-resource` and
`/.well-known/oauth-protected-resource/mcp` on the console host to the gateway.
Use `pathType: Prefix` for `/mcp`: Traefik ranks routers by rule length, and an
`Exact` `Path(`/mcp`)` is shorter than the console's `PathPrefix(`/`)`, so the
console answered `/mcp` instead.
Keycloak must be reachable from the hosts (Anthropic's egress is
`160.79.104.0/21`).

### 2. Keycloak: one registered client per host

```sh
KEYCLOAK_ADMIN=… KEYCLOAK_ADMIN_PASSWORD=… go run ./cmd/connector-clients \
  -keycloak https://auth.example.com -resource https://app.example.com/mcp
```

It signs in as a master-realm administrator (the gateway's own service account
deliberately cannot change clients) and, idempotently, creates or updates:

- the client scope `models:read`, shown on the consent screen, whose tokens
  carry `MCP_RESOURCE_URL` as audience;
- the client scope `grids:write`, shown on the consent screen ("Change the
  grid values you can change…"); both scopes are default scopes of each
  client, so every new connection asks for both;
- `chatgpt-connector` — callbacks `https://chatgpt.com/connector_platform_oauth_redirect`
  and `https://chatgpt.com/connector/oauth/*`;
- `claude-connector` — callback `https://claude.ai/api/mcp/auth_callback`.

Both are confidential clients: authorization code with S256 PKCE only, consent
required, no realm roles in tokens, five-minute access tokens, refresh and
offline tokens allowed (the hosts keep the connection until the person revokes
it). It prints each client's id, and its secret when created (`-show-secrets`
reprints them). Put the secrets in the gateway's optional secret
`mavericks-connector` as `MCP_CLIENT_SECRETS=chatgpt-connector=…,claude-connector=…`
(sealed, in production) so the account menu can show them. The secrets
are shared by every workspace on the deployment — each person still signs in
as themselves — so treat them as a host credential, not a person's: rotating
one (Keycloak → the client → Credentials) means updating the secret and
reconnecting every chat that uses it. Run the command again after changing the
connector's URL, and once after upgrading to a release with `write_cells`: it
adds `grids:write` to both clients, and until then every connection reads only.

Offline access needs the realm's default role `offline_access`, which every
account created through the console, sign-up, SSO or SCIM has; accounts
imported from a realm file with explicit roles (the dev stack's `pat`, `sam`, …)
do not.

### 3. In each host (every user)

- **Claude** (claude.ai, Desktop, mobile): add a custom connector with the URL
  `MCP_RESOURCE_URL`, and under its advanced settings enter the
  `claude-connector` client id and secret.
- **ChatGPT** (developer mode / custom app): add the MCP server URL with OAuth,
  using the `chatgpt-connector` client id and secret.

Each person then signs in once with their own maverickbuilds.app account and
consents. Revoking that consent in Keycloak's account console, or disabling
the account, ends the connection. To allow writes on a connection made before
`grids:write` existed, remove the connector in the host and add it again.

## Local testing

With `DEV_MODE=true` and no JWKS, `/mcp` accepts `Authorization: Bearer dev:<persona>`
(an `X-Dev-User` value) instead of a real token, e.g. with the MCP Inspector
against `http://localhost:8080/mcp`; `dev-read:<persona>` stands for a
connection granted reading only.
